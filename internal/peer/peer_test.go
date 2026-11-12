package peer

import (
	"bytes"
	"context"
	"io"
	"net"
	"path/filepath"
	"testing"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

func newStore(t *testing.T) *vfs.Store {
	t.Helper()
	s, err := vfs.Open(filepath.Join(t.TempDir(), "ws.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// cutConn kills the link after a number of frame writes. One frame is one
// Write, so each count is a named protocol point rather than a byte offset.
type cutConn struct {
	net.Conn
	allow  int
	writes int
}

func (c *cutConn) Write(b []byte) (int, error) {
	if c.writes >= c.allow {
		c.Conn.Close()
		return 0, io.ErrClosedPipe
	}
	c.writes++
	return c.Conn.Write(b)
}

func (c *cutConn) hit() bool { return c.writes >= c.allow }

const unlimited = 1 << 20

// session wires a client on host to a server on mirror. allowClient and
// allowServer count frame writes before the link dies; unlimited disables it.
func session(t *testing.T, host, mirror *vfs.Store, allowClient, allowServer int) (*Client, *cutConn, *cutConn) {
	t.Helper()
	local, remote := net.Pipe()
	cc := &cutConn{Conn: local, allow: allowClient}
	sc := &cutConn{Conn: remote, allow: allowServer}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = NewServer(mirror).Serve(ctx, sc)
	}()

	c, err := Open(t.Context(), cc, host)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = c.Close()
		_ = sc.Close()
		<-done
	})
	return c, cc, sc
}

func write(t *testing.T, s *vfs.Store, path, body string) {
	t.Helper()
	dir := path[:lastSlash(path)]
	if dir == "" {
		dir = "/"
	}
	if err := s.MkdirAll(t.Context(), dir); err != nil {
		t.Fatalf("mkdirall %s: %v", dir, err)
	}
	if err := s.WriteFile(t.Context(), path, []byte(body), nil); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func lastSlash(p string) int {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return i
		}
	}
	return 0
}

func read(t *testing.T, s *vfs.Store, path string) string {
	t.Helper()
	got, err := s.ReadFile(t.Context(), path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(got.Data)
}

func TestPushThenPull(t *testing.T) {
	host, mirror := newStore(t), newStore(t)
	write(t, host, "/a.txt", "alpha")
	write(t, host, "/dir/b.txt", "beta")

	c, _, _ := session(t, host, mirror, unlimited, unlimited)
	out, err := c.Push(t.Context())
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if out.Writes != 2 {
		t.Fatalf("want 2 writes, got %d", out.Writes)
	}
	if got := read(t, mirror, "/dir/b.txt"); got != "beta" {
		t.Fatalf("mirror has %q", got)
	}

	// A second push is the echo: same state must write nothing.
	out, err = c.Push(t.Context())
	if err != nil {
		t.Fatalf("echo push: %v", err)
	}
	if out.Writes != 0 {
		t.Fatalf("echo wrote %d", out.Writes)
	}

	// The other direction.
	write(t, mirror, "/back.txt", "from mirror")
	if err := c.Handshake(t.Context()); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	out, err = c.Pull(t.Context())
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if out.Writes != 1 {
		t.Fatalf("want 1 write, got %d", out.Writes)
	}
	if got := read(t, host, "/back.txt"); got != "from mirror" {
		t.Fatalf("host has %q", got)
	}

	// And the echo of the pull terminates too.
	out, err = c.Push(t.Context())
	if err != nil {
		t.Fatalf("push after pull: %v", err)
	}
	if out.Writes != 0 {
		t.Fatalf("push after pull wrote %d", out.Writes)
	}
}

func TestPullIsResumable(t *testing.T) {
	host, mirror := newStore(t), newStore(t)
	write(t, mirror, "/one", "1")
	write(t, mirror, "/two", "2")
	write(t, mirror, "/three", "3")

	c, _, _ := session(t, host, mirror, unlimited, unlimited)
	c.limit = 1
	if _, err := c.Pull(t.Context()); err != nil {
		t.Fatalf("pull: %v", err)
	}
	st, err := host.SyncState(t.Context())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.Fetch.Rev == 0 {
		t.Fatalf("fetch cursor did not advance")
	}
	if got := read(t, host, "/three"); got != "3" {
		t.Fatalf("host has %q", got)
	}
}

// TestBreakpointMatrix kills the link at each named protocol point, then
// reconnects and syncs again. Convergence plus "total writes == paths" is the
// "resume without applying twice" check from go-mapping §5.
//
// Frame counts per side: handshake is write 1 on both. A push then writes the
// missing probe, the stage, and the apply.
func TestBreakpointMatrix(t *testing.T) {
	cases := []struct {
		name                  string
		mode                  string
		limit                 int
		allowClient, allowSrv int
	}{
		{"push: after missing probe", "push", Batch, 2, unlimited},
		{"push: half the objects staged", "push", Batch, 3, unlimited},
		{"push: after first batch applied", "push", 1, 4, unlimited},
		{"push: apply committed, reply lost", "push", Batch, unlimited, 2},
		{"pull: after first page applied", "pull", 1, 2, unlimited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, mirror := newStore(t), newStore(t)
			src, dst := host, mirror
			if tc.mode == "pull" {
				src, dst = mirror, host
			}
			write(t, src, "/a.txt", "alpha")
			write(t, src, "/b.txt", "beta")

			c, _, _ := session(t, host, mirror, tc.allowClient, tc.allowSrv)
			c.limit = tc.limit
			first, err := c.sync(tc.mode)
			if err == nil {
				t.Fatalf("cut point never reached")
			}
			t.Logf("first %s stopped: %v", tc.mode, err)

			c2, _, _ := session(t, host, mirror, unlimited, unlimited)
			c2.limit = tc.limit
			second, err := c2.sync(tc.mode)
			if err != nil {
				t.Fatalf("resume %s: %v", tc.mode, err)
			}
			if got := read(t, dst, "/a.txt"); got != "alpha" {
				t.Fatalf("/a.txt is %q", got)
			}
			if got := read(t, dst, "/b.txt"); got != "beta" {
				t.Fatalf("/b.txt is %q", got)
			}
			if total := first.Writes + second.Writes; total != 2 {
				t.Fatalf("applied %d writes for 2 paths", total)
			}

			third, err := c2.sync(tc.mode)
			if err != nil {
				t.Fatalf("third %s: %v", tc.mode, err)
			}
			if third.Writes != 0 {
				t.Fatalf("converged %s wrote %d", tc.mode, third.Writes)
			}
		})
	}
}

// sync runs one direction.
func (c *Client) sync(mode string) (Outcome, error) {
	if mode == "pull" {
		return c.Pull(context.Background())
	}
	return c.Push(context.Background())
}

func TestNewMirrorResetsFetchCursor(t *testing.T) {
	host, mirror := newStore(t), newStore(t)
	write(t, mirror, "/m1", "one")

	c, _, _ := session(t, host, mirror, unlimited, unlimited)
	if _, err := c.Pull(t.Context()); err != nil {
		t.Fatalf("pull: %v", err)
	}
	before, err := host.SyncState(t.Context())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if before.Fetch.Rev == 0 {
		t.Fatalf("fetch cursor did not advance")
	}

	// A different mirror: new generation, rev space restarts.
	fresh := newStore(t)
	_, _, _ = session(t, host, fresh, unlimited, unlimited)
	after, err := host.SyncState(t.Context())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if after.Fetch != (vfs.Cursor{}) {
		t.Fatalf("fetch cursor survived a generation change: %+v", after.Fetch)
	}
	freshState, err := fresh.SyncState(t.Context())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if after.PeerGeneration != freshState.Generation {
		t.Fatalf("peer generation not recorded: %+v", after)
	}
}

func TestServerRefusesWrongProtocol(t *testing.T) {
	_, mirror := newStore(t), newStore(t)
	local, remote := net.Pipe()
	defer local.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = NewServer(mirror).Serve(context.Background(), remote)
	}()

	if err := writeFrame(local, opHello, Hello{Proto: protoVersion + 1}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	msg, err := readFrame(local)
	if err != nil {
		t.Fatalf("read failure: %v", err)
	}
	if msg.Op != opFailure {
		t.Fatalf("want a failure frame, got %s", msg.Op)
	}
	<-done
}

func TestTakeBytesSplitsAtBudget(t *testing.T) {
	blobs := []vfs.Blob{
		{Data: bytes.Repeat([]byte("a"), 4)},
		{Data: bytes.Repeat([]byte("b"), 4)},
		{Data: bytes.Repeat([]byte("c"), 4)},
	}
	take, rest := takeBytes(blobs, 10)
	if len(take) != 2 || len(rest) != 1 {
		t.Fatalf("split into %d and %d", len(take), len(rest))
	}
}
