// Package remote runs commands on a peer that keeps its own store.
//
// The backend owns no filesystem code and no output text: it sends the
// command, forwards the bytes it gets to the caller's sink, and reports how
// the command ended. Paths are workspace paths; the peer resolves them through
// its own mount (go-mapping §3.2).
package remote

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"sync"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/peer"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/runtime"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

// Backend dials a peer's unix socket.
type Backend struct {
	// Socket is the peer's unix socket path.
	Socket string
}

// New builds a backend for one peer socket.
func New(socket string) *Backend { return &Backend{Socket: socket} }

// ID is the routing name. Routing is not authorisation (ADR 0002).
func (b *Backend) ID() string { return "remote" }

// Kind is for diagnostics only.
func (b *Backend) Kind() string { return "container" }

// dial opens one connection to the peer.
func (b *Backend) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", b.Socket)
}

// Connect opens the execution connection. Each execution gets its own, so a
// cancelled stream never disturbs another command.
func (b *Backend) Connect(ctx context.Context, host runtime.Host) (runtime.Handle, error) {
	conn, err := b.dial(ctx)
	if err != nil {
		return nil, vfs.WrapError(vfs.CodeNoBackend, b.Socket, err)
	}
	return &handle{
		client: peer.NewExecClient(conn, b.dial),
		ctx:    ctx,
		closed: make(chan struct{}),
	}, nil
}

type handle struct {
	client *peer.ExecClient
	ctx    context.Context

	once   sync.Once
	closed chan struct{}
}

var (
	_ runtime.Backend = (*Backend)(nil)
	_ runtime.Handle  = (*handle)(nil)
)

// Sync is remote: the peer keeps its own store, so push and pull are real
// work (ADR 0008).
func (h *handle) Sync() runtime.SyncMode { return runtime.SyncRemote }

// Close ends the execution connection and reports it dead.
func (h *handle) Close() error {
	h.once.Do(func() { close(h.closed) })
	return h.client.Close()
}

// Closed fires when the connection is gone.
func (h *handle) Closed() <-chan struct{} { return h.closed }

// Exec starts the command on the peer.
//
// The command's lifetime follows the context Exec received, not the one a
// caller passes to Result: this is where the timeout is known.
func (h *handle) Exec(ctx context.Context, req runtime.Request) (*runtime.Exec, error) {
	id := mintID()
	dir := req.Path
	if dir == "" {
		dir = "/"
	}
	err := h.client.Start(ctx, peer.ExecRequest{
		ExecID: id,
		Source: req.Source,
		Dir:    dir,
		Env:    req.Env,
	})
	if err != nil {
		// A command that never started leaves the session unusable, and a
		// start failure is an error, not a result.
		_ = h.Close()
		return nil, vfs.WrapError(vfs.CodeInvalid, dir, err)
	}

	return &runtime.Exec{Result: func(context.Context) (runtime.Result, error) {
		stdout, stderr := req.Output.Stdout(), req.Output.Stderr()
		status, err := h.client.Wait(ctx, stdout, stderr)
		if err != nil {
			// A broken stream leaves half a frame behind; the connection can
			// no longer carry a command.
			_ = h.Close()
			return runtime.Result{}, err
		}
		return fromStatus(status), nil
	}}, nil
}

// fromStatus maps the wire status onto the runtime's vocabulary.
func fromStatus(st peer.ExecStatus) runtime.Result {
	res := runtime.Result{ExitCode: st.ExitCode}
	switch st.Status {
	case peer.StatusCompleted:
		res.Status = runtime.Completed
	case peer.StatusCancelled:
		res.Status = runtime.Cancelled
	default:
		res.Status = runtime.Failed
	}
	return res
}

// mintID gives one execution a name the peer can use for a kill.
func mintID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "remote-" + hex.EncodeToString(b[:])
}
