package peer

import (
	"context"
	"fmt"
	"io"
	"sync"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/backend/shell"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/execout"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/runtime"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

// Runner runs commands for exec requests on the mirror side.
//
// It owns no storage and no output text: bytes go straight onto the
// connection, and the host decides what to keep (go-mapping §6). What it does
// own is the set of running commands, so a kill can find one by id.
type Runner struct {
	resolve func(ctx context.Context, path string) (string, error)
	shell   *shell.Backend

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

// NewRunner builds a runner. resolve turns a workspace path into a real path
// on this side; production wires the mount, tests wire a directory.
func NewRunner(resolve func(ctx context.Context, path string) (string, error)) *Runner {
	return &Runner{
		resolve: resolve,
		shell:   shell.New(),
		running: make(map[string]context.CancelFunc),
	}
}

// Kill cancels one running command. It reports false when no command owns the
// id, which the caller turns into EEXEC_LOST.
func (r *Runner) Kill(id string) bool {
	r.mu.Lock()
	cancel, ok := r.running[id]
	r.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// Exec runs one command and streams its output.
//
// The command starts before the run frame is written, so a client that has
// seen the run frame can always kill it.
func (r *Runner) Exec(parent context.Context, w *frameWriter, req ExecRequest) error {
	if req.ExecID == "" || req.Source == "" {
		return refuse(vfs.CodeInvalid, "an exec request needs an id and a source")
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	r.mu.Lock()
	if _, busy := r.running[req.ExecID]; busy {
		r.mu.Unlock()
		return refuse(vfs.CodeInvalid, "exec id "+req.ExecID+" is already running")
	}
	r.running[req.ExecID] = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.running, req.ExecID)
		r.mu.Unlock()
	}()

	dir, err := r.resolve(ctx, req.Dir)
	if err != nil {
		// The command never started; report it as a failed execution so the
		// caller gets a result, not a broken session.
		return w.Frame(opExit, ExecStatus{Status: StatusFailed, ExitCode: -1, Err: err.Error()})
	}

	handle, err := r.shell.Connect(ctx, nil)
	if err != nil {
		return w.Frame(opExit, ExecStatus{Status: StatusFailed, ExitCode: -1, Err: err.Error()})
	}
	defer handle.Close()

	sink := &streamSink{writer: w, cancel: cancel}
	ex, err := handle.Exec(ctx, runtime.Request{
		Source: req.Source,
		Dir:    dir,
		Env:    req.Env,
		Output: sink,
	})
	if err != nil {
		return w.Frame(opExit, ExecStatus{Status: StatusFailed, ExitCode: -1, Err: err.Error()})
	}
	if err := w.Frame(opRun, ack{}); err != nil {
		return err
	}

	// The command's own context carries the cancellation; Result's argument is
	// not the one the process group follows.
	res, resErr := ex.Result(context.Background())
	status := ExecStatus{Status: StatusFailed, ExitCode: res.ExitCode}
	switch res.Status {
	case runtime.Completed:
		status.Status = StatusCompleted
	case runtime.Cancelled:
		status.Status = StatusCancelled
	}
	if resErr != nil {
		status.Err = resErr.Error()
	}
	return w.Frame(opExit, status)
}

// streamSink turns the two output writers into frames.
type streamSink struct {
	writer *frameWriter
	cancel context.CancelFunc
}

// Stdout and Stderr are cheap views; the writers live as long as one command.
func (s *streamSink) Stdout() io.Writer { return &streamStream{sink: s, op: opStdout} }
func (s *streamSink) Stderr() io.Writer { return &streamStream{sink: s, op: opStderr} }

// Finalize returns nothing: trimming and the output files belong to the host.
func (s *streamSink) Finalize(context.Context) (execout.Result, error) {
	return execout.Result{}, nil
}

type streamStream struct {
	sink *streamSink
	op   string
}

// Write frames one piece of output. A failed write cancels the command: the
// client is gone, and an unread process is an orphan.
func (s *streamStream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := s.sink.writer.Frame(s.op, streamChunk{Data: p}); err != nil {
		s.sink.cancel()
		return 0, fmt.Errorf("send output: %w", err)
	}
	return len(p), nil
}
