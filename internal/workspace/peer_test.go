package workspace

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/peer"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/runtime"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

// mirror is a peer process shrunk to a listener: same server, same store, no
// separate binary. It runs everywhere, including platforms without FUSE.
type mirror struct {
	store  *vfs.Store
	socket string
	// run makes this mirror run commands. Nil means sync only.
	run *peer.Runner

	mu       sync.Mutex
	listener net.Listener
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	done     chan struct{}
}

func startMirror(t *testing.T) *mirror {
	t.Helper()
	// A unix socket path is limited to about a hundred bytes, and t.TempDir()
	// is longer than that on some systems.
	dir, err := os.MkdirTemp("", "mirror")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	store, err := vfs.Open(filepath.Join(dir, "mirror.db"))
	if err != nil {
		t.Fatalf("open mirror: %v", err)
	}
	m := &mirror{store: store, socket: filepath.Join(dir, "peer.sock")}
	t.Cleanup(func() { _ = store.Close() })
	m.start(t)
	return m
}

// start listens and serves until stop.
func (m *mirror) start(t *testing.T) {
	t.Helper()
	_ = os.Remove(m.socket)
	listener, err := net.Listen("unix", m.socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	m.mu.Lock()
	m.listener = listener
	m.conns = make(map[net.Conn]struct{})
	m.done = make(chan struct{})
	done := m.done
	m.mu.Unlock()

	server := peer.NewServer(m.store)
	if m.run != nil {
		server = peer.NewServerWithRunner(m.store, m.run)
	}
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				break
			}
			m.mu.Lock()
			m.conns[conn] = struct{}{}
			m.mu.Unlock()
			m.wg.Add(1)
			go func() {
				defer m.wg.Done()
				defer conn.Close()
				defer func() {
					m.mu.Lock()
					delete(m.conns, conn)
					m.mu.Unlock()
				}()
				_ = server.Serve(context.Background(), conn)
			}()
		}
		m.wg.Wait()
	}()
}

// stop kills the listener and every live connection, the way SIGKILL would.
func (m *mirror) stop(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	listener, done := m.listener, m.done
	m.listener, m.done = nil, nil
	m.mu.Unlock()
	if listener == nil {
		return
	}
	_ = listener.Close()
	m.mu.Lock()
	for conn := range m.conns {
		_ = conn.Close()
	}
	m.mu.Unlock()
	<-done
}

func openWorkspace(t *testing.T, opts Options) *Workspace {
	t.Helper()
	w, err := Open(filepath.Join(t.TempDir(), "host.db"), opts)
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func TestPushAndPullWithoutAPeer(t *testing.T) {
	w := openWorkspace(t, Options{})
	if err := w.WriteFile(t.Context(), "/a.txt", []byte("alpha"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := w.Push(t.Context())
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if out.Writes != 0 {
		t.Fatalf("a workspace without a peer wrote %d", out.Writes)
	}
	if _, err := w.Pull(t.Context()); err != nil {
		t.Fatalf("pull: %v", err)
	}
}

// TestReconnectPullsBeforePush pins the reconnect order: the mirror's
// unpulled write wins over the host's, because the host drains the mirror
// before it pushes anything (ADR 0008).
func TestReconnectPullsBeforePush(t *testing.T) {
	m := startMirror(t)
	w := openWorkspace(t, Options{PeerSocket: m.socket})

	// Establish the session so the next one counts as a reconnect.
	if _, err := w.Push(t.Context()); err != nil {
		t.Fatalf("first push: %v", err)
	}

	if err := m.store.WriteFile(t.Context(), "/x", []byte("mirror"), nil); err != nil {
		t.Fatalf("mirror write: %v", err)
	}
	if err := w.WriteFile(t.Context(), "/x", []byte("host"), nil); err != nil {
		t.Fatalf("host write: %v", err)
	}

	// The peer dies and comes back with the same database.
	m.stop(t)
	m.start(t)

	if _, err := w.Push(t.Context()); err != nil {
		t.Fatalf("push after reconnect: %v", err)
	}

	host, err := w.ReadFile(t.Context(), "/x")
	if err != nil {
		t.Fatalf("host read: %v", err)
	}
	if string(host.Data) != "mirror" {
		t.Fatalf("pull-first order lost: host holds %q", host.Data)
	}
	peerSide, err := m.store.ReadFile(t.Context(), "/x")
	if err != nil {
		t.Fatalf("mirror read: %v", err)
	}
	if string(peerSide.Data) != "mirror" {
		t.Fatalf("mirror holds %q", peerSide.Data)
	}
}

// TestPushRetriesOnceOnAFreshSession covers the retry rule from
// computer.md §5.5: a broken session is replaced, not reported.
func TestPushRetriesOnceOnAFreshSession(t *testing.T) {
	m := startMirror(t)
	w := openWorkspace(t, Options{PeerSocket: m.socket})

	if _, err := w.Push(t.Context()); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if err := w.WriteFile(t.Context(), "/a.txt", []byte("alpha"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	m.stop(t)
	m.start(t)

	out, err := w.Push(t.Context())
	if err != nil {
		t.Fatalf("push after restart: %v", err)
	}
	if out.Writes != 1 {
		t.Fatalf("want 1 write, got %d", out.Writes)
	}
	got, err := m.store.ReadFile(t.Context(), "/a.txt")
	if err != nil {
		t.Fatalf("mirror read: %v", err)
	}
	if string(got.Data) != "alpha" {
		t.Fatalf("mirror holds %q", got.Data)
	}
}

func TestPushFailsWhenThePeerStaysDown(t *testing.T) {
	m := startMirror(t)
	w := openWorkspace(t, Options{PeerSocket: m.socket})
	if _, err := w.Push(t.Context()); err != nil {
		t.Fatalf("first push: %v", err)
	}
	m.stop(t)

	if _, err := w.Push(t.Context()); err == nil {
		t.Fatalf("push to a dead peer succeeded")
	}
	// The workspace stays usable, and a push without a peer gate still works
	// on the store itself.
	if err := w.WriteFile(t.Context(), "/b.txt", []byte("beta"), nil); err != nil {
		t.Fatalf("write after failed push: %v", err)
	}
}

// TestBracketAroundARemoteCommand runs the whole bracket without FUSE: the
// push gate, a command on the peer, and the pull that carries its output. The
// command runs in a plain directory, so this covers the bracket itself rather
// than the mount.
func TestBracketAroundARemoteCommand(t *testing.T) {
	m := startMirror(t)
	dir := t.TempDir()
	m.stop(t)
	m.run = peer.NewRunner(func(context.Context, string) (string, error) { return dir, nil })
	m.start(t)

	w := openWorkspace(t, Options{MountRoot: t.TempDir(), PeerSocket: m.socket, Remote: true})
	if err := w.WriteFile(t.Context(), "/a.txt", []byte("alpha"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	ex, err := w.Exec(t.Context(), "echo hi", runtime.ExecOptions{Backend: "remote"})
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
	if res.Sync != runtime.SyncComplete {
		t.Fatalf("sync outcome %v, want complete", res.Sync)
	}
	if got := string(res.Output.Stdout.Tail); got != "hi\n" {
		t.Fatalf("stdout tail %q", got)
	}
	// The output text and the pushed file both live in the host's store.
	if _, err := w.ReadFile(t.Context(), res.Output.Stdout.Path); err != nil {
		t.Fatalf("output file: %v", err)
	}
}
