// Package shell runs shell source as a local subprocess.
//
// The backend keeps no storage of its own: the subprocess works inside the
// mount point, so every file it writes reaches the authoritative store
// through FUSE (ADR 0006).
package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/runtime"
)

// DefaultGrace is how long the process group gets between SIGTERM and
// SIGKILL when an execution is cancelled.
const DefaultGrace = 5 * time.Second

// Backend runs /bin/sh. It needs no transport, so connecting cannot fail.
type Backend struct {
	// Grace overrides DefaultGrace. Tests set it short.
	Grace time.Duration
}

// New builds the backend.
func New() *Backend { return &Backend{} }

func (b *Backend) grace() time.Duration {
	if b.Grace > 0 {
		return b.Grace
	}
	return DefaultGrace
}

// ID is the routing name.
func (b *Backend) ID() string { return "shell" }

// Kind is for diagnostics only.
func (b *Backend) Kind() string { return "subprocess" }

// Connect returns a handle. A subprocess backend needs no handshake.
func (b *Backend) Connect(ctx context.Context, host runtime.Host) (runtime.Handle, error) {
	return &handle{grace: b.grace()}, nil
}

type handle struct{ grace time.Duration }

var (
	_ runtime.Backend = (*Backend)(nil)
	_ runtime.Handle  = (*handle)(nil)
)

// Sync is none: writes go straight into the authoritative store.
func (h *handle) Sync() runtime.SyncMode { return runtime.SyncNone }

// Close is a no-op. The handle owns nothing, and Result reaps the process.
func (h *handle) Close() error { return nil }

// Exec starts /bin/sh -c source in req.Dir.
//
// The environment is the process environment plus req.Env, stdin is the null
// device so a reading command gets EOF instead of a hang. Output goes to the
// handle's sink, never to a buffer of ours: the command may print more than
// memory holds (go-mapping §5 step 4).
func (h *handle) Exec(ctx context.Context, req runtime.Request) (*runtime.Exec, error) {
	cmd := exec.Command("/bin/sh", "-c", req.Source)
	cmd.Dir = req.Dir
	cmd.Env = environ(req.Env)
	cmd.Stdin = nil
	stdout, stderr := io.Discard, io.Discard
	if req.Output != nil {
		stdout, stderr = req.Output.Stdout(), req.Output.Stderr()
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	startGroup(cmd)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %q in %s: %w", req.Source, req.Dir, err)
	}
	// The command's lifetime follows the context Exec received, not the one
	// a caller passes to Result: timeouts are set up once, at start.
	return &runtime.Exec{Result: func(context.Context) (runtime.Result, error) {
		return wait(ctx, cmd, h.grace)
	}}, nil
}

// wait reaps the process and classifies the end.
//
// A cancelled context terminates the whole process group: SIGTERM, then
// SIGKILL after the grace period. The group, not the direct child, is the
// target because a shell command spawns children of its own.
func wait(ctx context.Context, cmd *exec.Cmd, grace time.Duration) (runtime.Result, error) {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return classify(err)
	case <-ctx.Done():
	}

	terminateGroup(cmd)
	var err error
	select {
	case err = <-done:
	case <-time.After(grace):
		killGroup(cmd)
		err = <-done
	}
	if err == nil {
		// The command finished on its own during the grace period.
		return classify(nil)
	}
	return runtime.Result{Status: runtime.Cancelled, ExitCode: -1}, ctx.Err()
}

// classify turns the wait error into a Result. A non-zero exit is a normal
// outcome of the command, so it is not an error of the call. A command killed
// by a signal reports ExitCode -1.
func classify(err error) (runtime.Result, error) {
	res := runtime.Result{}
	if err == nil {
		res.Status = runtime.Completed
		return res, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		res.Status = runtime.Failed
		res.ExitCode = exit.ExitCode()
		return res, nil
	}
	return runtime.Result{}, err
}

// environ merges the process environment with overrides. Later values win,
// and the result is sorted so the child sees one entry per name.
func environ(overrides map[string]string) []string {
	env := make(map[string]string)
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	for k, v := range overrides {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
