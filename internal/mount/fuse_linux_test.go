//go:build linux

package mount

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestMountRoundTrip is the only test that needs a kernel mount. It runs on
// Linux CI and skips everywhere else, per go-mapping §6: FUSE integration
// tests stay on Linux, everything else hits Volume.
func TestMountRoundTrip(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	ctx := context.Background()
	vol, store := newVolume(t)
	m := NewManager(t.TempDir())

	point, err := m.Prepare(ctx, "ws")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := store.MkdirAll(ctx, "/dir"); err != nil {
		t.Fatalf("seed dir: %v", err)
	}

	// The store root is "/", so mount at a path the volume can serve.
	srv, err := Mount(vol, point, false)
	if err != nil {
		t.Skipf("mount unavailable: %v", err)
	}
	defer func() { _ = srv.Unmount() }()

	if err := os.WriteFile(filepath.Join(point, "dir", "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write through mount: %v", err)
	}
	got, err := store.ReadFile(ctx, "/dir/hello.txt")
	if err != nil {
		t.Fatalf("read from store: %v", err)
	}
	if string(got) != "hi" {
		t.Fatalf("store saw %q", got)
	}

	entries, err := os.ReadDir(point)
	if err != nil {
		t.Fatalf("readdir through mount: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "dir" {
		t.Fatalf("entries: %v", entries)
	}

	if err := os.Remove(filepath.Join(point, "dir", "hello.txt")); err != nil {
		t.Fatalf("remove through mount: %v", err)
	}
	if _, err := store.Stat(ctx, "/dir/hello.txt"); err == nil {
		t.Fatal("file survived removal")
	}
}
