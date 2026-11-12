package remote

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/execout"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/peer"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/runtime"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

// proc is a peer process: one store, one socket, restartable so a test can
// kill it the way a host would.
type proc struct {
	dir    string
	socket string
	store  *vfs.Store

	mu       sync.Mutex
	listener net.Listener
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	done     chan struct{}
	accepts  int
}

// startPeer opens a peer process that runs commands in dir.
func startPeer(t *testing.T, dir string) *proc {
	t.Helper()
	tmp, err := os.MkdirTemp("", "remote")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	store, err := vfs.Open(filepath.Join(tmp, "mirror.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	p := &proc{dir: dir, socket: filepath.Join(tmp, "peer.sock"), store: store}
	t.Cleanup(func() {
		p.stop(t)
		_ = store.Close()
		_ = os.RemoveAll(tmp)
	})
	p.start(t)
	return p
}

func (p *proc) start(t *testing.T) {
	t.Helper()
	_ = os.Remove(p.socket)
	listener, err := net.Listen("unix", p.socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	runner := peer.NewRunner(func(context.Context, string) (string, error) { return p.dir, nil })
	server := peer.NewServerWithRunner(p.store, runner)

	ctx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	p.listener, p.conns, p.done = listener, make(map[net.Conn]struct{}), make(chan struct{})
	done := p.done
	p.mu.Unlock()

	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				break
			}
			p.mu.Lock()
			p.conns[conn] = struct{}{}
			p.accepts++
			p.mu.Unlock()
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				defer conn.Close()
				defer func() {
					p.mu.Lock()
					delete(p.conns, conn)
					p.mu.Unlock()
				}()
				_ = server.Serve(ctx, conn)
			}()
		}
		p.wg.Wait()
	}()
	t.Cleanup(cancel)
}

// stop kills the listener and every connection, including an idle cached one.
func (p *proc) stop(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	listener, done := p.listener, p.done
	p.listener, p.done = nil, nil
	p.mu.Unlock()
	if listener == nil {
		return
	}
	_ = listener.Close()
	p.mu.Lock()
	for conn := range p.conns {
		_ = conn.Close()
	}
	p.mu.Unlock()
	<-done
}

// accepted is how many connections this peer has seen.
func (p *proc) accepted() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accepts
}

// hostStub resolves every path to the same real directory.
type hostStub struct{ dir string }

func (h hostStub) Resolve(_ context.Context, p string) (string, error) { return h.dir, nil }

// sink collects the two streams.
type sink struct{ out, err bytes.Buffer }

func (s *sink) Stdout() io.Writer { return &s.out }
func (s *sink) Stderr() io.Writer { return &s.err }
func (s *sink) Finalize(context.Context) (execout.Result, error) {
	return execout.Result{}, nil
}

type outputs struct{ last *sink }

func (o *outputs) For(context.Context, string, string) runtime.OutputSink {
	o.last = &sink{}
	return o.last
}

func newRuntime(t *testing.T, socket, dir string) (*runtime.Runtime, *outputs) {
	t.Helper()
	outs := &outputs{}
	return runtime.New(hostStub{dir: dir}, outs, &Backend{Socket: socket}), outs
}

func TestRemoteBackendRunsAndStreams(t *testing.T) {
	dir := t.TempDir()
	p := startPeer(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "in.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rt, outs := newRuntime(t, p.socket, dir)

	ex, err := rt.Exec(t.Context(), "cat in.txt", runtime.ExecOptions{Cwd: "/"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	res, err := ex.Result(t.Context())
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Status != runtime.Completed || res.ExitCode != 0 {
		t.Fatalf("status %v code %d", res.Status, res.ExitCode)
	}
	if outs.last.out.String() != "alpha" {
		t.Fatalf("stdout %q", outs.last.out.String())
	}
}

func TestRemoteBackendReportsExitCode(t *testing.T) {
	p := startPeer(t, t.TempDir())
	rt, _ := newRuntime(t, p.socket, t.TempDir())

	ex, err := rt.Exec(t.Context(), "echo bad >&2; exit 7", runtime.ExecOptions{})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	res, err := ex.Result(t.Context())
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Status != runtime.Failed || res.ExitCode != 7 {
		t.Fatalf("status %v code %d", res.Status, res.ExitCode)
	}
}

// TestRemoteBackendTimeoutCancels is the cancellation contract from phase one:
// the whole process group ends and the result says Cancelled.
func TestRemoteBackendTimeoutCancels(t *testing.T) {
	p := startPeer(t, t.TempDir())
	rt, _ := newRuntime(t, p.socket, t.TempDir())

	ex, err := rt.Exec(t.Context(), "sleep 30", runtime.ExecOptions{Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	start := time.Now()
	res, err := ex.Result(t.Context())
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Status != runtime.Cancelled || res.ExitCode != -1 {
		t.Fatalf("status %v code %d", res.Status, res.ExitCode)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
}

func TestRemoteBackendUnreachableIsCoded(t *testing.T) {
	rt, _ := newRuntime(t, filepath.Join(t.TempDir(), "nope.sock"), t.TempDir())
	_, err := rt.Exec(t.Context(), "true", runtime.ExecOptions{})
	if err == nil {
		t.Fatalf("exec against a missing peer succeeded")
	}
	if !strings.Contains(err.Error(), "ENOBACKEND") {
		t.Fatalf("error %v is not coded", err)
	}
}

// TestRemoteBackendReusesOneConnection pins the cache: the second execution
// does not dial again.
func TestRemoteBackendReusesOneConnection(t *testing.T) {
	p := startPeer(t, t.TempDir())
	rt, _ := newRuntime(t, p.socket, t.TempDir())

	for i := 0; i < 2; i++ {
		ex, err := rt.Exec(t.Context(), "true", runtime.ExecOptions{})
		if err != nil {
			t.Fatalf("exec %d: %v", i, err)
		}
		if _, err := ex.Result(t.Context()); err != nil {
			t.Fatalf("result %d: %v", i, err)
		}
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := p.accepted(); got != 1 {
		t.Fatalf("peer saw %d connections for two execs, want 1", got)
	}
}

// TestStaleHandleFailsOnceThenRecovers pins the cost of caching: a peer that
// restarts while the host is idle leaves a stale handle. The next command
// fails, the command is not replayed (§5.5), and the one after that reconnects.
func TestStaleHandleFailsOnceThenRecovers(t *testing.T) {
	p := startPeer(t, t.TempDir())
	rt, _ := newRuntime(t, p.socket, t.TempDir())

	first, err := rt.Exec(t.Context(), "true", runtime.ExecOptions{})
	if err != nil {
		t.Fatalf("first exec: %v", err)
	}
	if _, err := first.Result(t.Context()); err != nil {
		t.Fatalf("first result: %v", err)
	}

	p.stop(t)
	p.start(t)

	if _, err := rt.Exec(t.Context(), "true", runtime.ExecOptions{}); err == nil {
		t.Fatalf("a stale handle ran a command")
	}

	second, err := rt.Exec(t.Context(), "true", runtime.ExecOptions{})
	if err != nil {
		t.Fatalf("exec after recovery: %v", err)
	}
	if _, err := second.Result(t.Context()); err != nil {
		t.Fatalf("result after recovery: %v", err)
	}
	if got := p.accepted(); got != 2 {
		t.Fatalf("peer saw %d connections, want 2", got)
	}
}
