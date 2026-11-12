//go:build linux

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/runtime"
)

// TestExecWritesThroughTheMount is the step-3 acceptance test: a subprocess
// writes to the mount point, and the authoritative store sees the bytes as
// soon as the command exits (go-mapping §5 step 3). It needs a kernel mount,
// so it stays on Linux CI and skips everywhere else.
func TestExecWritesThroughTheMount(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	ctx := context.Background()
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "ws.db"), Options{MountRoot: filepath.Join(dir, "mnt")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	ex, err := w.Exec(ctx, "mkdir -p sub && printf hi > sub/f", runtime.ExecOptions{})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	res, err := ex.Result(ctx)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Status != runtime.Completed || res.ExitCode != 0 {
		t.Fatalf("status %v exit %d stderr %q", res.Status, res.ExitCode, res.Stderr)
	}

	got, err := w.ReadFile(ctx, "/sub/f")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "hi" {
		t.Fatalf("f = %q, want hi", got)
	}
	st, err := w.Stat(ctx, "/sub/f")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Rev == 0 {
		t.Fatal("exec write minted no rev")
	}

	// A command that reads from its own write must see it too.
	ex, err = w.Exec(ctx, "cat sub/f > copy", runtime.ExecOptions{})
	if err != nil {
		t.Fatalf("second exec: %v", err)
	}
	if _, err := ex.Result(ctx); err != nil {
		t.Fatalf("second result: %v", err)
	}
	if got, err := w.ReadFile(ctx, "/copy"); err != nil || string(got) != "hi" {
		t.Fatalf("copy = %q (%v), want hi", got, err)
	}

	point := w.MountPoint()
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(point); !os.IsNotExist(err) {
		t.Fatalf("mount point %s still present after close: %v", point, err)
	}
}
