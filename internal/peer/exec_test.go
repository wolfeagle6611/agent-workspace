package peer

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

// execPeer serves commands with a resolver that answers with dir. No mount is
// involved, so the exec wire can be tested on any platform.
func execPeer(t *testing.T, dir string) (socket string, stop func()) {
	t.Helper()
	// A unix socket path is limited to about a hundred bytes.
	tmp, err := os.MkdirTemp("", "exec")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	socket = filepath.Join(tmp, "peer.sock")

	store, err := vfs.Open(filepath.Join(tmp, "mirror.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	runner := NewRunner(func(ctx context.Context, _ string) (string, error) {
		return dir, nil
	})
	server := NewServerWithRunner(store, runner)

	var wg sync.WaitGroup
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				break
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				_ = server.Serve(ctx, conn)
			}()
		}
		wg.Wait()
	}()

	stop = func() {
		cancel()
		_ = listener.Close()
		<-done
		_ = os.RemoveAll(tmp)
		_ = store.Close()
	}
	t.Cleanup(stop)
	return socket, stop
}

func execDialer(t *testing.T, socket string) (*ExecClient, func()) {
	t.Helper()
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}
	conn, err := dial(t.Context())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := NewExecClient(conn, dial)
	return c, func() { _ = c.Close() }
}

func TestExecStreamsBothStreams(t *testing.T) {
	socket, _ := execPeer(t, t.TempDir())
	c, closeConn := execDialer(t, socket)
	defer closeConn()

	var stdout, stderr bytes.Buffer
	if err := c.Start(t.Context(), ExecRequest{
		ExecID: "e1", Source: "echo hello; echo oops >&2", Dir: "/",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	got, err := c.Wait(t.Context(), &stdout, &stderr)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if got.Status != StatusCompleted || got.ExitCode != 0 {
		t.Fatalf("status %+v", got)
	}
	if stdout.String() != "hello\n" {
		t.Fatalf("stdout %q", stdout.String())
	}
	if stderr.String() != "oops\n" {
		t.Fatalf("stderr %q", stderr.String())
	}
}

func TestExecReportsAFailingCommand(t *testing.T) {
	socket, _ := execPeer(t, t.TempDir())
	c, closeConn := execDialer(t, socket)
	defer closeConn()

	var stdout, stderr bytes.Buffer
	if err := c.Start(t.Context(), ExecRequest{
		ExecID: "e2", Source: "exit 3", Dir: "/",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	got, err := c.Wait(t.Context(), &stdout, &stderr)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if got.Status != StatusFailed || got.ExitCode != 3 {
		t.Fatalf("status %+v", got)
	}
}

func TestExecKillStopsTheCommand(t *testing.T) {
	socket, _ := execPeer(t, t.TempDir())
	c, closeConn := execDialer(t, socket)
	defer closeConn()

	var stdout, stderr bytes.Buffer
	if err := c.Start(t.Context(), ExecRequest{
		ExecID: "e3", Source: "sleep 30", Dir: "/",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	start := time.Now()
	if err := c.Kill(t.Context(), "e3"); err != nil {
		t.Fatalf("kill: %v", err)
	}
	got, err := c.Wait(t.Context(), &stdout, &stderr)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if got.Status != StatusCancelled || got.ExitCode != -1 {
		t.Fatalf("status %+v", got)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("kill took %s", elapsed)
	}
}

func TestKillUnknownIDIsNotAnError(t *testing.T) {
	socket, _ := execPeer(t, t.TempDir())
	c, closeConn := execDialer(t, socket)
	defer closeConn()

	if err := c.Kill(t.Context(), "never-started"); err != nil {
		t.Fatalf("kill of an unknown id: %v", err)
	}
}

func TestExecResolverFailureIsAStatus(t *testing.T) {
	tmp, err := os.MkdirTemp("", "exec")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(tmp)
	socket := filepath.Join(tmp, "peer.sock")
	store, err := vfs.Open(filepath.Join(tmp, "mirror.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	runner := NewRunner(func(context.Context, string) (string, error) {
		return "", vfs.NewError(vfs.CodeNotFound, "/missing")
	})
	server := NewServerWithRunner(store, runner)
	go func() { _ = server.Serve(context.Background(), mustAccept(t, listener)) }()

	c, closeConn := execDialer(t, socket)
	defer closeConn()

	err = c.Start(t.Context(), ExecRequest{ExecID: "e4", Source: "true", Dir: "/missing"})
	var startErr *StartError
	if !errors.As(err, &startErr) {
		t.Fatalf("err %v, want a start error", err)
	}
	if startErr.Status.Status != StatusFailed || !strings.Contains(startErr.Status.Err, "ENOENT") {
		t.Fatalf("status %+v", startErr.Status)
	}
}

func mustAccept(t *testing.T, listener net.Listener) net.Conn {
	t.Helper()
	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	return conn
}
