package vfs

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func distinct(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func TestReadDirListsAndOrders(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.MkdirAll(ctx, "/w/sub"); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := s.WriteFile(ctx, "/w/b.txt", []byte("b"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.WriteFile(ctx, "/w/a.txt", []byte("aa"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	entries, err := s.ReadDir(ctx, "/w")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries: %d, want 3", len(entries))
	}
	for i, want := range []string{"a.txt", "b.txt", "sub"} {
		if entries[i].Name != want {
			t.Fatalf("entry %d = %q, want %q", i, entries[i].Name, want)
		}
	}
	if entries[0].Type != TypeFile || entries[0].Size != 2 {
		t.Fatalf("a.txt: %+v", entries[0])
	}
	if entries[2].Type != TypeDir {
		t.Fatalf("sub: %+v", entries[2])
	}

	if _, err := s.ReadDir(ctx, "/w/a.txt"); !errors.Is(err, ErrNotDir) {
		t.Fatalf("readdir on file = %v, want ErrNotDir", err)
	}
}

func TestRemove(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.MkdirAll(ctx, "/w/dir"); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := s.WriteFile(ctx, "/w/file", []byte("x"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.WriteFile(ctx, "/w/dir/inner", []byte("y"), nil); err != nil {
		t.Fatalf("write inner: %v", err)
	}

	if err := s.Remove(ctx, "/w/dir", nil); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("remove non-empty dir = %v, want ErrNotEmpty", err)
	}
	if err := s.Remove(ctx, "/w/file", nil); err != nil {
		t.Fatalf("remove file: %v", err)
	}
	if _, err := s.Stat(ctx, "/w/file"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat after remove = %v, want ErrNotFound", err)
	}
	if err := s.Remove(ctx, "/w/dir/inner", nil); err != nil {
		t.Fatalf("remove inner: %v", err)
	}
	if err := s.Remove(ctx, "/w/dir", nil); err != nil {
		t.Fatalf("remove empty dir: %v", err)
	}
	if err := s.Remove(ctx, "/", nil); !errors.Is(err, ErrPerm) {
		t.Fatalf("remove root = %v, want ErrPerm", err)
	}
}

func TestWriteAtPatchesOnlyTouchedChunks(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	body := distinct(ChunkSize*2 + 512)
	if err := s.WriteFile(ctx, "/big", body, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	st, _ := s.Stat(ctx, "/big")
	before, err := chunks.Chunks(ctx, s.db, st.Inode)
	if err != nil {
		t.Fatalf("chunks: %v", err)
	}
	if len(before) != 3 {
		t.Fatalf("chunks: %d, want 3", len(before))
	}

	patch := []byte("PATCHED")
	if err := s.WriteAt(ctx, "/big", 64, patch); err != nil {
		t.Fatalf("writeat: %v", err)
	}
	got, err := s.ReadFile(ctx, "/big")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := append([]byte(nil), body...)
	copy(want[64:], patch)
	if !bytes.Equal(got.Data, want) {
		t.Fatalf("content mismatch after patch")
	}

	after, err := chunks.Chunks(ctx, s.db, st.Inode)
	if err != nil {
		t.Fatalf("chunks: %v", err)
	}
	if before[1].Hash != after[1].Hash || before[2].Hash != after[2].Hash {
		t.Fatalf("untouched chunks were rewritten")
	}
	if before[0].Hash == after[0].Hash {
		t.Fatalf("touched chunk kept its old hash")
	}
}

func TestWriteAtCrossesChunkBoundary(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	body := distinct(ChunkSize + 128)
	if err := s.WriteFile(ctx, "/edge", body, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	patch := []byte("0123456789")
	off := int64(ChunkSize - 4)
	if err := s.WriteAt(ctx, "/edge", off, patch); err != nil {
		t.Fatalf("writeat: %v", err)
	}

	want := append([]byte(nil), body...)
	copy(want[off:], patch)
	got, err := s.ReadFile(ctx, "/edge")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got.Data, want) {
		t.Fatalf("boundary patch mismatch")
	}
}

func TestWriteAtExtendsWithZeros(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.WriteFile(ctx, "/f", []byte("abc"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.WriteAt(ctx, "/f", 6, []byte("ghi")); err != nil {
		t.Fatalf("writeat: %v", err)
	}
	got, err := s.ReadFile(ctx, "/f")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got.Data, []byte("abc\x00\x00\x00ghi")) {
		t.Fatalf("got %q", got.Data)
	}
	st, _ := s.Stat(ctx, "/f")
	if st.Size != 9 {
		t.Fatalf("size %d, want 9", st.Size)
	}
}

func TestWriteAtCreatesAtZeroOnly(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.WriteAt(ctx, "/new", 0, []byte("hi")); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _ := s.ReadFile(ctx, "/new")
	if string(got.Data) != "hi" {
		t.Fatalf("got %q", got.Data)
	}

	if err := s.WriteAt(ctx, "/missing", 10, []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("write past end of missing file = %v, want ErrNotFound", err)
	}
}

func TestWriteAtRejectsNegativeOffset(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.WriteAt(ctx, "/f", -1, []byte("x")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestTruncate(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	body := distinct(ChunkSize + 700)
	if err := s.WriteFile(ctx, "/t", body, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Truncate(ctx, "/t", 100, nil); err != nil {
		t.Fatalf("shrink: %v", err)
	}
	got, _ := s.ReadFile(ctx, "/t")
	if !bytes.Equal(got.Data, body[:100]) {
		t.Fatalf("shrink content mismatch")
	}

	if err := s.Truncate(ctx, "/t", ChunkSize+5, nil); err != nil {
		t.Fatalf("grow: %v", err)
	}
	got, _ = s.ReadFile(ctx, "/t")
	if int64(len(got.Data)) != ChunkSize+5 {
		t.Fatalf("grown to %d", len(got.Data))
	}
	if !bytes.Equal(got.Data[:100], body[:100]) || got.Data[ChunkSize] != 0 {
		t.Fatalf("grow content mismatch")
	}
	st, _ := s.Stat(ctx, "/t")
	if st.Size != ChunkSize+5 {
		t.Fatalf("size %d", st.Size)
	}
}

func TestReadAtRanges(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	body := distinct(ChunkSize*2 + 100)
	if err := s.WriteFile(ctx, "/r", body, nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	cases := []struct {
		off  int64
		n    int
		want []byte
	}{
		{0, 5, body[:5]},
		{ChunkSize - 3, 6, body[ChunkSize-3 : ChunkSize+3]},
		{ChunkSize * 2, 50, body[ChunkSize*2 : ChunkSize*2+50]},
		{ChunkSize*2 + 90, 100, body[ChunkSize*2+90:]},
		{ChunkSize * 3, 10, nil},
	}
	for _, c := range cases {
		got, err := s.ReadAt(ctx, "/r", c.off, c.n)
		if err != nil {
			t.Fatalf("readat(%d,%d): %v", c.off, c.n, err)
		}
		if !bytes.Equal(got, c.want) {
			t.Fatalf("readat(%d,%d) = %d bytes, want %d", c.off, c.n, len(got), len(c.want))
		}
	}
}
