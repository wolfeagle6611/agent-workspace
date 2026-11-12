package workspace

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/runtime"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

func open(t *testing.T) *Workspace {
	t.Helper()
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "ws.db"), Options{MountRoot: filepath.Join(dir, "mnt")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func TestFsForwarding(t *testing.T) {
	ctx := context.Background()
	w := open(t)

	if err := w.MkdirAll(ctx, "/a/b"); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := w.WriteFile(ctx, "/a/b/f.txt", []byte("hello"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := w.ReadFile(ctx, "/a/b/f.txt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("read %q, want hello", got)
	}
	st, err := w.Stat(ctx, "/a/b/f.txt")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size != 5 || st.Rev == 0 {
		t.Fatalf("size %d rev %d, want 5 and a minted rev", st.Size, st.Rev)
	}
	entries, err := w.ReadDir(ctx, "/a/b")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "f.txt" {
		t.Fatalf("entries %+v", entries)
	}
	if err := w.Truncate(ctx, "/a/b/f.txt", 2); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := w.Remove(ctx, "/a/b/f.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
}

func TestMountPointIsEmptyUntilFirstExec(t *testing.T) {
	w := open(t)
	if got := w.MountPoint(); got != "" {
		t.Fatalf("mount point %q before any exec, want empty", got)
	}
}

// Resolve rejects a bad cwd before it mounts, so these cases need no FUSE.
func TestResolveRejectsBadPaths(t *testing.T) {
	ctx := context.Background()
	w := open(t)
	if err := w.WriteFile(ctx, "/file.txt", []byte("x"), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for _, tc := range []struct {
		path string
		want error
	}{
		{"relative", vfs.ErrInvalid},
		{"/../escape", vfs.ErrInvalid},
		{"/missing", vfs.ErrNotFound},
		{"/file.txt", vfs.ErrNotDir},
	} {
		if _, err := w.Resolve(ctx, tc.path); !errors.Is(err, tc.want) {
			t.Fatalf("resolve %q = %v, want %v", tc.path, err, tc.want)
		}
	}
}

func TestExecAfterCloseIsClosed(t *testing.T) {
	ctx := context.Background()
	w := open(t)
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := w.Exec(ctx, "true", runtime.ExecOptions{}); !errors.Is(err, vfs.ErrClosed) {
		t.Fatalf("exec after close = %v, want ErrClosed", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	w := open(t)
	if err := w.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestRelativeMountRootIsInvalid(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(filepath.Join(dir, "ws.db"), Options{MountRoot: "relative/path"}); !errors.Is(err, vfs.ErrInvalid) {
		t.Fatalf("err %v, want ErrInvalid", err)
	}
}

func TestMountIDDiffersForSameDatabaseName(t *testing.T) {
	first, err := mountID(filepath.Join(t.TempDir(), "ws.db"))
	if err != nil {
		t.Fatalf("mountID: %v", err)
	}
	second, err := mountID(filepath.Join(t.TempDir(), "ws.db"))
	if err != nil {
		t.Fatalf("mountID: %v", err)
	}
	// Same file name, different databases: Prepare clears whatever it finds
	// at a mount point, so these must not share one.
	if first == second {
		t.Fatalf("both databases use mount id %q", first)
	}
	if !strings.HasPrefix(first, "ws-") {
		t.Fatalf("mount id %q does not start with the file name", first)
	}
}
