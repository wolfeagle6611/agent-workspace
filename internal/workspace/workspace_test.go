package workspace

import (
	"context"
	"errors"
	"fmt"
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

func TestOpenCreatesTheReservedDirectory(t *testing.T) {
	w := open(t)
	st, err := w.Stat(context.Background(), "/.computer/output")
	if err != nil {
		t.Fatalf("stat reserved dir: %v", err)
	}
	if st.Type != vfs.TypeDir {
		t.Fatalf("type %v, want dir", st.Type)
	}
}

func TestOutputNamesAreUnique(t *testing.T) {
	ctx := context.Background()
	w := open(t)

	first := w.For(ctx, "shell", "exec-1")
	second := w.For(ctx, "shell", "exec-1")
	r1, _ := first.Finalize(ctx)
	r2, _ := second.Finalize(ctx)

	if r1.Stdout.Path == r2.Stdout.Path {
		t.Fatalf("both runs write %s: the second overwrote the first", r1.Stdout.Path)
	}
	if r1.Err != nil || r2.Err != nil {
		t.Fatalf("errs %v / %v, want nil", r1.Err, r2.Err)
	}
}

func TestOutputSanitizesTheIDs(t *testing.T) {
	ctx := context.Background()
	w := open(t)

	out := w.For(ctx, "../shell", "../../exec")
	res, _ := out.Finalize(ctx)
	rest, ok := strings.CutPrefix(res.Stdout.Path, "/.computer/output/")
	if !ok || strings.Contains(rest, "/") {
		t.Fatalf("path %q escapes the output directory", res.Stdout.Path)
	}
	if _, err := w.Stat(ctx, res.Stdout.Path); err != nil {
		t.Fatalf("stat %q: %v", res.Stdout.Path, err)
	}
}

// TestOutputStoresTheWholeText mirrors the 100k-line acceptance case: the
// tail is bounded, the file is not.
func TestOutputStoresTheWholeText(t *testing.T) {
	ctx := context.Background()
	w := open(t)

	out := w.For(ctx, "shell", "exec-1")
	total := 0
	for i := 1; i <= 100_000; i++ {
		line := fmt.Sprintf("line-%d\n", i)
		if _, err := out.Stdout().Write([]byte(line)); err != nil {
			t.Fatalf("write: %v", err)
		}
		total += len(line)
	}
	res, err := out.Finalize(ctx)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("output err: %v", res.Err)
	}

	full, err := w.ReadFile(ctx, res.Stdout.Path)
	if err != nil {
		t.Fatalf("read full text: %v", err)
	}
	if len(full) != total || res.Stdout.Bytes != int64(total) {
		t.Fatalf("stored %d bytes, counted %d, want %d", len(full), res.Stdout.Bytes, total)
	}
	if len(res.Stdout.Tail) > 64<<10 {
		t.Fatalf("tail is %d bytes, want a bounded window", len(res.Stdout.Tail))
	}
	if !res.Stdout.Truncated {
		t.Fatal("truncated false, want true")
	}
}

func TestPruneKeepsFifty(t *testing.T) {
	ctx := context.Background()
	w := open(t)

	var last string
	for i := 0; i < 60; i++ {
		last = fmt.Sprintf("/.computer/output/run-%d.stdout.log", i)
		if err := w.WriteFile(ctx, last, []byte("x"), nil); err != nil {
			t.Fatalf("write %s: %v", last, err)
		}
	}
	w.prune(ctx)

	entries, err := w.ReadDir(ctx, "/.computer/output")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 50 {
		t.Fatalf("kept %d files, want 50", len(entries))
	}
	if _, err := w.Stat(ctx, last); err != nil {
		t.Fatalf("newest file was pruned: %v", err)
	}
	if _, err := w.Stat(ctx, "/.computer/output/run-0.stdout.log"); !errors.Is(err, vfs.ErrNotFound) {
		t.Fatalf("oldest file survived: %v", err)
	}
}
