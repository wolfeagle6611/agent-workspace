//go:build linux

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
		t.Fatalf("status %v exit %d stderr %q", res.Status, res.ExitCode, res.Output.Stderr.Tail)
	}

	got, err := w.ReadFile(ctx, "/sub/f")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got.Data) != "hi" {
		t.Fatalf("f = %q, want hi", got.Data)
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
	if got, err := w.ReadFile(ctx, "/copy"); err != nil || string(got.Data) != "hi" {
		t.Fatalf("copy = %q (%v), want hi", got.Data, err)
	}

	point := w.MountPoint()
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(point); !os.IsNotExist(err) {
		t.Fatalf("mount point %s still present after close: %v", point, err)
	}
}

// TestExecChmodThroughTheMount is the step-7a acceptance test: a command that
// sets the execute bit gets a file that really runs. Before this step the
// kernel's mode was dropped and chmod reported success anyway.
func TestExecChmodThroughTheMount(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	ctx := context.Background()
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "ws.db"), Options{MountRoot: filepath.Join(dir, "mnt")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	cmd := `printf '#!/bin/sh\necho ran\n' > s.sh && chmod +x s.sh && ./s.sh`
	ex, err := w.Exec(ctx, cmd, runtime.ExecOptions{})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	res, err := ex.Result(ctx)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Status != runtime.Completed || res.ExitCode != 0 {
		t.Fatalf("status %v exit %d stderr %q", res.Status, res.ExitCode, res.Output.Stderr.Tail)
	}
	if !strings.Contains(string(res.Output.Stdout.Tail), "ran") {
		t.Fatalf("stdout %q, want ran", res.Output.Stdout.Tail)
	}

	st, err := w.Stat(ctx, "/s.sh")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode&0o111 == 0 {
		t.Fatalf("mode = %o, execute bit did not survive chmod", st.Mode)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
