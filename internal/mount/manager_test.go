package mount

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newManager(t *testing.T) (*Manager, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "run")
	return NewManager(root), root
}

func TestPrepareCreatesMountPoint(t *testing.T) {
	ctx := context.Background()
	m, root := newManager(t)

	point, err := m.Prepare(ctx, "ws-1")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if point != filepath.Join(root, "ws-1") {
		t.Fatalf("point %q", point)
	}
	st, err := os.Stat(point)
	if err != nil || !st.IsDir() {
		t.Fatalf("mount point missing: %v", err)
	}

	// Idempotent: a live workspace can prepare twice without harm.
	if _, err := m.Prepare(ctx, "ws-1"); err != nil {
		t.Fatalf("second prepare: %v", err)
	}
}

func TestPrepareClearsStaleMount(t *testing.T) {
	ctx := context.Background()
	m, _ := newManager(t)

	point, _ := m.Prepare(ctx, "ws-1")

	state := true
	calls := 0
	m.mounted = func(p string) bool { return p == point && state }
	m.unmount = func(p string) error { calls++; state = false; return nil }

	if _, err := m.Prepare(ctx, "ws-1"); err != nil {
		t.Fatalf("prepare over stale mount: %v", err)
	}
	if calls != 1 {
		t.Fatalf("unmount calls: %d, want 1", calls)
	}
}

func TestPrepareFailsWhenMountStays(t *testing.T) {
	ctx := context.Background()
	m, _ := newManager(t)

	m.mounted = func(string) bool { return true }
	m.unmount = func(string) error { return errors.New("device busy") }

	_, err := m.Prepare(ctx, "ws-1")
	if err == nil {
		t.Fatal("want an error when the stale mount cannot be cleared")
	}
}

func TestReleaseUnmountsAndRemoves(t *testing.T) {
	ctx := context.Background()
	m, root := newManager(t)

	point, _ := m.Prepare(ctx, "ws-1")

	unmounted := 0
	m.mounted = func(string) bool { return false }
	m.unmount = func(string) error { unmounted++; return nil }

	if err := m.Release(ctx, "ws-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(point); !os.IsNotExist(err) {
		t.Fatalf("mount point still present: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("root should stay: %v", err)
	}
	if unmounted != 0 {
		t.Fatalf("unmount called %d times for an unmounted point", unmounted)
	}

	// Release twice is not an error.
	if err := m.Release(ctx, "ws-1"); err != nil {
		t.Fatalf("second release: %v", err)
	}
}

func TestValidID(t *testing.T) {
	for _, id := range []string{"ws-1", "a.b", "UPPER_2"} {
		if err := validID(id); err != nil {
			t.Errorf("validID(%q) = %v", id, err)
		}
	}
	for _, id := range []string{"", ".", "..", "a/b", "a\\b", "a\x00b"} {
		if err := validID(id); err == nil {
			t.Errorf("validID(%q) accepted", id)
		}
	}
}

func TestIsMountPointOnPlainDir(t *testing.T) {
	if isMountPoint(t.TempDir()) {
		t.Fatal("temp dir reported as a mount point")
	}
}
