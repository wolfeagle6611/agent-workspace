package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/workspace"
)

// startServe runs serve until the returned stop function is called.
func startServe(t *testing.T, db, socket string) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := serve(ctx, []string{"--db", db, "--socket", socket}); err != nil && ctx.Err() == nil {
			t.Errorf("serve: %v", err)
		}
	}()
	waitForSocket(t, socket)
	return func() {
		cancel()
		<-done
	}
}

// waitForSocket blocks until the peer listens, so the test never races the
// listener.
func waitForSocket(t *testing.T, socket string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", socket)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("socket %s never accepted a connection", socket)
}

// TestServeSurvivesKillRestartsAndResumes is the step 6b acceptance:
// push through the real socket, kill the peer, restart it, push again, and
// check that the mirror holds the content once.
func TestServeSurvivesKillRestartsAndResumes(t *testing.T) {
	// A unix socket path is limited to about a hundred bytes, and t.TempDir()
	// is longer than that on some systems.
	dir, err := os.MkdirTemp("", "peer")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	dbPath := filepath.Join(dir, "mirror.db")
	socket := filepath.Join(dir, "peer.sock")

	stop := startServe(t, dbPath, socket)

	w, err := workspace.Open(filepath.Join(dir, "host.db"), workspace.Options{PeerSocket: socket})
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}
	if err := w.WriteFile(t.Context(), "/a.txt", []byte("alpha"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	first, err := w.Push(t.Context())
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if first.Writes != 1 {
		t.Fatalf("want 1 write, got %d", first.Writes)
	}

	// Kill the peer without letting it flush anything.
	stop()

	// A push to a dead peer must fail, even when there is nothing to send:
	// the watermark assertion needs the peer to answer.
	if _, err := w.Push(t.Context()); err == nil {
		t.Fatalf("push to a dead peer succeeded")
	}

	// A change written while the peer is down has to survive the restart.
	if err := w.WriteFile(t.Context(), "/b.txt", []byte("beta"), nil); err != nil {
		t.Fatalf("write while down: %v", err)
	}
	if _, err := w.Push(t.Context()); err == nil {
		t.Fatalf("push with pending changes to a dead peer succeeded")
	}

	stop = startServe(t, dbPath, socket)
	defer stop()

	second, err := w.Push(t.Context())
	if err != nil {
		t.Fatalf("push after restart: %v", err)
	}
	if second.Writes != 1 {
		t.Fatalf("restart applied %d writes, want 1", second.Writes)
	}
	third, err := w.Push(t.Context())
	if err != nil {
		t.Fatalf("third push: %v", err)
	}
	if third.Writes != 0 {
		t.Fatalf("converged push wrote %d", third.Writes)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("close workspace: %v", err)
	}

	// Read the mirror directly: the peer stopped, so nothing else holds it.
	mirror, err := vfs.Open(dbPath)
	if err != nil {
		t.Fatalf("open mirror: %v", err)
	}
	defer mirror.Close()
	got, err := mirror.ReadFile(t.Context(), "/a.txt")
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	if string(got.Data) != "alpha" {
		t.Fatalf("mirror holds %q", got.Data)
	}
	beta, err := mirror.ReadFile(t.Context(), "/b.txt")
	if err != nil {
		t.Fatalf("read mirror /b.txt: %v", err)
	}
	if string(beta.Data) != "beta" {
		t.Fatalf("mirror /b.txt holds %q", beta.Data)
	}
}
