//go:build linux

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/runtime"
)

// execWorkspace opens a workspace for tests that need a real mount.
func execWorkspace(t *testing.T) *Workspace {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "ws.db"), Options{MountRoot: filepath.Join(dir, "mnt")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// TestExecOutputLandsInTheWorkspace is the step-4 acceptance case: a command
// that prints 100k lines keeps a bounded tail and stores the whole text.
func TestExecOutputLandsInTheWorkspace(t *testing.T) {
	ctx := context.Background()
	w := execWorkspace(t)

	ex, err := w.Exec(ctx, "seq 1 100000", runtime.ExecOptions{})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	res, err := ex.Result(ctx)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	out := res.Output.Stdout
	if res.Status != runtime.Completed || out.Bytes == 0 {
		t.Fatalf("status %v bytes %d", res.Status, out.Bytes)
	}
	if !out.Truncated {
		t.Fatal("truncated false, want true")
	}
	if len(out.Tail) > 64<<10 {
		t.Fatalf("tail is %d bytes, want a bounded window", len(out.Tail))
	}
	if !strings.HasSuffix(string(out.Tail), "100000\n") {
		t.Fatalf("tail ends %q, want the last line", string(out.Tail[len(out.Tail)-10:]))
	}
	segs, err := w.ReadDir(ctx, "/.computer/output")
	if err != nil {
		t.Fatalf("readdir output: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("output files %d, want stdout and stderr", len(segs))
	}
	st, err := w.Stat(ctx, out.Path)
	if err != nil {
		t.Fatalf("stat %s: %v", out.Path, err)
	}
	if st.Size != out.Bytes {
		t.Fatalf("stored %d bytes, counted %d", st.Size, out.Bytes)
	}
}

// TestExecOutputIsPruned keeps 50 output files, newest by rev.
func TestExecOutputIsPruned(t *testing.T) {
	ctx := context.Background()
	w := execWorkspace(t)

	for i := 0; i < 30; i++ {
		ex, err := w.Exec(ctx, "true", runtime.ExecOptions{})
		if err != nil {
			t.Fatalf("exec %d: %v", i, err)
		}
		if _, err := ex.Result(ctx); err != nil {
			t.Fatalf("result %d: %v", i, err)
		}
	}
	entries, err := w.ReadDir(ctx, "/.computer/output")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 50 {
		t.Fatalf("kept %d files after 60 writes, want 50", len(entries))
	}
}

// TestCloseStopsARunningCommand covers the join between Close and an
// execution in flight (go-mapping §5 step 4).
func TestCloseStopsARunningCommand(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "ws.db"), Options{MountRoot: filepath.Join(dir, "mnt")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}

	if _, err := w.Exec(ctx, "sleep 30", runtime.ExecOptions{}); err != nil {
		t.Fatalf("exec: %v", err)
	}

	start := time.Now()
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("close took %s, want the cancel to end the command", took)
	}
}
