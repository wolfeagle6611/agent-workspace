package vfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "ws.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestWriteReadRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.MkdirAll(ctx, "/workspace/notes"); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	want := []byte("- [ ] ship it\n")
	if err := s.WriteFile(ctx, "/workspace/notes/todo.md", want, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := s.ReadFile(ctx, "/workspace/notes/todo.md")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got.Data, want) {
		t.Fatalf("got %q, want %q", got.Data, want)
	}

	st, err := s.Stat(ctx, "/workspace/notes/todo.md")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Type != TypeFile || st.Size != int64(len(want)) || st.Rev == 0 {
		t.Fatalf("unexpected stat: %+v", st)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ws.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.MkdirAll(ctx, "/workspace"); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := s.WriteFile(ctx, "/workspace/keep.txt", []byte("survives"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	got, err := reopened.ReadFile(ctx, "/workspace/keep.txt")
	if err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	if string(got.Data) != "survives" {
		t.Fatalf("got %q", got.Data)
	}
}

func TestRevStrictlyIncreases(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	before, err := s.metaRev(ctx)
	if err != nil {
		t.Fatalf("metaRev: %v", err)
	}
	if err := s.WriteFile(ctx, "/a.txt", []byte("one"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	first, _ := s.metaRev(ctx)
	if first != before+1 {
		t.Fatalf("first write: rev %d -> %d, want +1", before, first)
	}

	if err := s.WriteFile(ctx, "/a.txt", []byte("two"), nil); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	second, _ := s.metaRev(ctx)
	if second != first+1 {
		t.Fatalf("second write: rev %d -> %d, want +1", first, second)
	}

	st, err := s.Stat(ctx, "/a.txt")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Rev != second {
		t.Fatalf("node rev %d, meta rev %d", st.Rev, second)
	}
}

func TestChunkingAndDedupe(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	// Distinct bytes everywhere, so the three chunks are three blobs. A
	// repeating payload would dedupe inside the file itself.
	big := make([]byte, ChunkSize*2+1024)
	for i := range big {
		big[i] = byte(i*7 + i/251)
	}
	if err := s.WriteFile(ctx, "/big.bin", big, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	st, err := s.Stat(ctx, "/big.bin")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	refs, err := chunks.Chunks(ctx, s.db, st.Inode)
	if err != nil {
		t.Fatalf("chunks: %v", err)
	}
	if len(refs) != 3 {
		t.Fatalf("chunks: %d, want 3", len(refs))
	}
	if refs[0].Size != ChunkSize || refs[2].Size != 1024 {
		t.Fatalf("chunk sizes: %d, %d", refs[0].Size, refs[2].Size)
	}

	// Same content at a second path shares the blobs.
	if err := s.WriteFile(ctx, "/copy.bin", big, nil); err != nil {
		t.Fatalf("copy: %v", err)
	}
	counts, err := chunks.Counts(ctx, s.db)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Blobs != 3 {
		t.Fatalf("blobs: %d, want 3 after dedupe", counts.Blobs)
	}
	if counts.Chunks != 6 {
		t.Fatalf("chunk rows: %d, want 6 (two files, three chunks each)", counts.Chunks)
	}

	// Rewriting with the same bytes must not grow the blob set.
	if err := s.WriteFile(ctx, "/big.bin", big, nil); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	counts, err = chunks.Counts(ctx, s.db)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Blobs != 3 {
		t.Fatalf("blobs after rewrite: %d, want 3", counts.Blobs)
	}

	got, err := s.ReadFile(ctx, "/copy.bin")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got.Data, big) {
		t.Fatalf("round trip mismatch: %d bytes", len(got.Data))
	}
}

func TestEmptyFileHasNoChunks(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.WriteFile(ctx, "/empty", nil, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	st, err := s.Stat(ctx, "/empty")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	refs, err := chunks.Chunks(ctx, s.db, st.Inode)
	if err != nil {
		t.Fatalf("chunks: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("chunks: %d, want 0", len(refs))
	}
	got, err := s.ReadFile(ctx, "/empty")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Data) != 0 {
		t.Fatalf("got %d bytes", len(got.Data))
	}
}

func TestInvalidPaths(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	for _, p := range []string{"", "relative/path", "/workspace/../etc/passwd", "/a\x00b"} {
		err := s.WriteFile(ctx, p, []byte("x"), nil)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("WriteFile(%q) = %v, want ErrInvalid", p, err)
		}
	}
	if _, err := s.ReadFile(ctx, "nope"); !errors.Is(err, ErrInvalid) {
		t.Errorf("ReadFile(relative) = %v, want ErrInvalid", err)
	}
}

func TestWriteToRootIsDir(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.WriteFile(ctx, "/", []byte("x"), nil); !errors.Is(err, ErrIsDir) {
		t.Fatalf("write to root = %v, want ErrIsDir", err)
	}
}

func TestMissingParent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	err := s.WriteFile(ctx, "/nope/deep/file.txt", []byte("x"), nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestParentIsFileIsNotDir(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.WriteFile(ctx, "/file", []byte("x"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	err := s.WriteFile(ctx, "/file/child", []byte("y"), nil)
	if !errors.Is(err, ErrNotDir) {
		t.Fatalf("want ErrNotDir, got %v", err)
	}
}

func TestBaseRevConflictAndRetry(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.WriteFile(ctx, "/counter.txt", []byte("5"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	read, err := s.Stat(ctx, "/counter.txt")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// Someone else changes the file between our read and our write.
	if err := s.WriteFile(ctx, "/counter.txt", []byte("6"), nil); err != nil {
		t.Fatalf("other write: %v", err)
	}

	stale := read.Rev
	err = s.WriteFile(ctx, "/counter.txt", []byte("7"), &MutateOptions{BaseRev: &stale})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	var fe *Error
	if !errors.As(err, &fe) {
		t.Fatalf("want *Error, got %T", err)
	}
	if fe.BaseRev != stale || fe.CurrentRev <= stale {
		t.Fatalf("conflict carries base %d current %d", fe.BaseRev, fe.CurrentRev)
	}

	// Re-read and retry with the current revision.
	fresh := fe.CurrentRev
	if err = s.WriteFile(ctx, "/counter.txt", []byte("7"), &MutateOptions{BaseRev: &fresh}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	got, _ := s.ReadFile(ctx, "/counter.txt")
	if string(got.Data) != "7" {
		t.Fatalf("got %q", got.Data)
	}
}

func TestBaseRevZeroMeansExpectAbsent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	zero := int64(0)
	if err := s.WriteFile(ctx, "/new.txt", []byte("a"), &MutateOptions{BaseRev: &zero}); err != nil {
		t.Fatalf("create with base 0: %v", err)
	}
	err := s.WriteFile(ctx, "/new.txt", []byte("b"), &MutateOptions{BaseRev: &zero})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second create with base 0 = %v, want ErrConflict", err)
	}
}

func TestBaseRevOnAbsentWithNonZeroRev(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	nine := int64(9)
	err := s.WriteFile(ctx, "/ghost", []byte("a"), &MutateOptions{BaseRev: &nine})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

func TestMkdirStrictAndMkdirAll(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.MkdirAll(ctx, "/a/b/c"); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := s.Mkdir(ctx, "/a/b/c", nil); !errors.Is(err, ErrExist) {
		t.Fatalf("mkdir on existing dir = %v, want ErrExist", err)
	}
	if err := s.MkdirAll(ctx, "/a/b/c"); err != nil {
		t.Fatalf("mkdirall idempotent: %v", err)
	}

	if err := s.Mkdir(ctx, "/a/b/c/d", nil); err != nil {
		t.Fatalf("mkdir child: %v", err)
	}

	// A file is not a directory.
	if err := s.WriteFile(ctx, "/a/file", []byte("x"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Mkdir(ctx, "/a/file", nil); !errors.Is(err, ErrExist) {
		t.Fatalf("mkdir over file = %v, want ErrExist", err)
	}
}

func TestConcurrentWritesAreSerialized(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.MkdirAll(ctx, "/w"); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	before, _ := s.metaRev(ctx)

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := []byte(fmt.Sprintf("body-%d", i))
			if err := s.WriteFile(ctx, fmt.Sprintf("/w/f%02d.txt", i), body, nil); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent write: %v", err)
	}

	after, _ := s.metaRev(ctx)
	if after-before != n {
		t.Fatalf("rev advanced by %d, want %d", after-before, n)
	}
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("/w/f%02d.txt", i)
		got, err := s.ReadFile(ctx, path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if want := fmt.Sprintf("body-%d", i); string(got.Data) != want {
			t.Fatalf("%s = %q, want %q", path, got.Data, want)
		}
	}
}
