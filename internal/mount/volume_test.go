package mount

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

func newVolume(t *testing.T) (*Volume, *vfs.Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := vfs.Open(dir + "/ws.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return NewVolume(s), s
}

// TestVolumePassesMode pins the wiring: the permission bits the kernel hands
// to create() and mkdir() reach the node, so stat() does not report a lie.
func TestVolumePassesMode(t *testing.T) {
	ctx := context.Background()
	vol, store := newVolume(t)

	if _, err := vol.Create(ctx, "/p.txt", 0o600); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := vol.Mkdir(ctx, "/private", 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for path, want := range map[string]uint32{"/p.txt": 0o600, "/private": 0o700} {
		st, err := store.Stat(ctx, path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if st.Mode != want {
			t.Fatalf("%s mode %#o, want %#o", path, st.Mode, want)
		}
	}

	// A create without a mode keeps the mode the file already had.
	if err := vol.Truncate(ctx, "/p.txt", 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := vol.Create(ctx, "/p.txt", 0); err != nil {
		t.Fatalf("re-create: %v", err)
	}
	st, err := store.Stat(ctx, "/p.txt")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode != 0o600 {
		t.Fatalf("mode reset to %#o on replace, want it kept at 0600", st.Mode)
	}
}

func TestVolumeWriteFlushRead(t *testing.T) {
	ctx := context.Background()
	vol, _ := newVolume(t)

	h, err := vol.Create(ctx, "/hello.txt", 0o644)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := vol.Write(ctx, h, 0, []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := vol.Flush(ctx, h); err != nil {
		t.Fatalf("flush: %v", err)
	}

	got, err := vol.Read(ctx, h, 0, 5)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q", got)
	}
	if err := vol.Release(ctx, h); err != nil {
		t.Fatalf("release: %v", err)
	}
	if vol.OpenHandles() != 0 {
		t.Fatalf("handles leaked: %d", vol.OpenHandles())
	}
}

func TestVolumeReadSeesUnflushedWrite(t *testing.T) {
	ctx := context.Background()
	vol, _ := newVolume(t)

	h, err := vol.Create(ctx, "/live.txt", 0o644)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := vol.Write(ctx, h, 0, []byte("abcdef")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// No Flush: a read on the same handle must still see the bytes.
	got, err := vol.Read(ctx, h, 2, 3)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "cde" {
		t.Fatalf("got %q, want cde", got)
	}
}

func TestVolumeFlushesAtChunkBoundary(t *testing.T) {
	ctx := context.Background()
	vol, store := newVolume(t)

	h, err := vol.Create(ctx, "/big.bin", 0o644)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Two writes that together exceed one chunk: the first fills the buffer
	// and commits, the second starts a new buffer.
	half := bytes.Repeat([]byte("a"), vfs.ChunkSize/2)
	if _, err := vol.Write(ctx, h, 0, half); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if _, err := vol.Write(ctx, h, int64(len(half)), half); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	if _, err := vol.Write(ctx, h, int64(len(half))*2, []byte("tail")); err != nil {
		t.Fatalf("write 3: %v", err)
	}
	if err := vol.Release(ctx, h); err != nil {
		t.Fatalf("release: %v", err)
	}

	st, err := store.Stat(ctx, "/big.bin")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if want := int64(len(half))*2 + 4; st.Size != want {
		t.Fatalf("size %d, want %d", st.Size, want)
	}
	body, err := store.ReadFile(ctx, "/big.bin")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(body.Data[len(body.Data)-4:], []byte("tail")) {
		t.Fatalf("tail missing: %q", body.Data[len(body.Data)-4:])
	}
}

func TestVolumeReadOnlyHandleRejectsWrite(t *testing.T) {
	ctx := context.Background()
	vol, store := newVolume(t)

	if err := store.WriteFile(ctx, "/ro", []byte("x"), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h, err := vol.Open(ctx, "/ro", false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := vol.Write(ctx, h, 0, []byte("y")); !errors.Is(err, vfs.ErrReadOnly) {
		t.Fatalf("write on read-only handle = %v, want ErrReadOnly", err)
	}
}

func TestVolumeReleaseFlushesWithoutExplicitFlush(t *testing.T) {
	ctx := context.Background()
	vol, store := newVolume(t)

	h, err := vol.Create(ctx, "/tail.txt", 0o644)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := vol.Write(ctx, h, 0, []byte("kept")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := vol.Release(ctx, h); err != nil {
		t.Fatalf("release: %v", err)
	}
	got, err := store.ReadFile(ctx, "/tail.txt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got.Data) != "kept" {
		t.Fatalf("got %q", got.Data)
	}
}

func TestVolumeDirectoryOps(t *testing.T) {
	ctx := context.Background()
	vol, _ := newVolume(t)

	if err := vol.Mkdir(ctx, "/dir", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	h, err := vol.Create(ctx, "/dir/file", 0o644)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := vol.Write(ctx, h, 0, []byte("abc")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := vol.Release(ctx, h); err != nil {
		t.Fatalf("release: %v", err)
	}

	entries, err := vol.ReadDir(ctx, "/dir")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "file" {
		t.Fatalf("entries: %+v", entries)
	}

	if err := vol.Remove(ctx, "/dir"); !errors.Is(err, vfs.ErrNotEmpty) {
		t.Fatalf("remove non-empty = %v, want ErrNotEmpty", err)
	}
	if err := vol.Truncate(ctx, "/dir/file", 1); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	st, err := vol.Lookup(ctx, "/dir/file")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if st.Size != 1 {
		t.Fatalf("size %d, want 1", st.Size)
	}

	if err := vol.Remove(ctx, "/dir/file"); err != nil {
		t.Fatalf("remove file: %v", err)
	}
	if err := vol.Remove(ctx, "/dir"); err != nil {
		t.Fatalf("remove dir: %v", err)
	}
}

func TestVolumeUnknownHandle(t *testing.T) {
	ctx := context.Background()
	vol, _ := newVolume(t)

	if _, err := vol.Read(ctx, 42, 0, 1); !errors.Is(err, ErrBadHandle) {
		t.Fatalf("read = %v, want ErrBadHandle", err)
	}
	if err := vol.Flush(ctx, 42); !errors.Is(err, ErrBadHandle) {
		t.Fatalf("flush = %v, want ErrBadHandle", err)
	}
	if err := vol.Release(ctx, 42); err != nil {
		t.Fatalf("release of unknown handle = %v, want nil", err)
	}
}

func TestVolumeCloseFlushesEveryHandle(t *testing.T) {
	ctx := context.Background()
	vol, store := newVolume(t)

	first, err := vol.Create(ctx, "/one", 0o644)
	if err != nil {
		t.Fatalf("create one: %v", err)
	}
	second, err := vol.Create(ctx, "/two", 0o644)
	if err != nil {
		t.Fatalf("create two: %v", err)
	}
	// Neither handle flushes: Close must commit both, in any order.
	if _, err := vol.Write(ctx, first, 0, []byte("1")); err != nil {
		t.Fatalf("write one: %v", err)
	}
	if _, err := vol.Write(ctx, second, 0, []byte("2")); err != nil {
		t.Fatalf("write two: %v", err)
	}

	if err := vol.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	for path, want := range map[string]string{"/one": "1", "/two": "2"} {
		got, err := store.ReadFile(ctx, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got.Data) != want {
			t.Fatalf("%s = %q, want %q", path, got.Data, want)
		}
	}
	if vol.OpenHandles() != 0 {
		t.Fatalf("handles kept after close: %d", vol.OpenHandles())
	}
	if _, err := vol.Write(ctx, first, 0, []byte("x")); !errors.Is(err, ErrBadHandle) {
		t.Fatalf("write after close = %v, want ErrBadHandle", err)
	}
}

func TestVolumeRefusesWriteToHandleItClosed(t *testing.T) {
	ctx := context.Background()
	vol, _ := newVolume(t)

	id, err := vol.Create(ctx, "/late.txt", 0o644)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A write that resolved the handle before Close holds this pointer. The
	// close lands between the lookup and the lock, so replay that order: the
	// write must fail loudly, not vanish into a buffer nobody flushes.
	held, err := vol.handle(id)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if err := vol.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}

	held.mu.Lock()
	defer held.mu.Unlock()
	if _, err := vol.writeLocked(ctx, held, 0, []byte("x")); !errors.Is(err, ErrBadHandle) {
		t.Fatalf("write after close = %v, want ErrBadHandle", err)
	}
}
