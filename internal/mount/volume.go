package mount

import (
	"context"
	"errors"
	"sync"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

// ErrBadHandle means the id does not name an open handle: already released,
// or never issued by this volume.
var ErrBadHandle = errors.New("mount: unknown file handle")

// Volume is the operations layer a FUSE mount needs: open handles, buffered
// writes, ranged reads, and the directory calls.
//
// It exists so the protocol adapter stays thin. The go-fuse adapter in
// fuse.go only translates syscalls into these calls; every rule that can be
// wrong lives here, where tests reach it without a kernel mount.
type Volume struct {
	store *vfs.Store

	mu      sync.Mutex
	handles map[uint64]*handle
	next    uint64
}

type handle struct {
	mu       sync.Mutex
	path     string
	writable bool
	closed   bool
	off      int64
	buf      []byte
}

// NewVolume wraps a store.
func NewVolume(store *vfs.Store) *Volume {
	return &Volume{store: store, handles: make(map[uint64]*handle)}
}

// Open returns a handle for path.
func (v *Volume) Open(ctx context.Context, path string, writable bool) (uint64, error) {
	if _, err := v.store.Stat(ctx, path); err != nil {
		return 0, err
	}
	return v.add(path, writable), nil
}

// Create makes path empty and returns a writable handle. mode is the
// permission bits the caller asked for; zero keeps the current mode, or 0644
// for a new file.
func (v *Volume) Create(ctx context.Context, path string, mode uint32) (uint64, error) {
	if err := v.store.WriteFile(ctx, path, nil, &vfs.MutateOptions{Mode: mode}); err != nil {
		return 0, err
	}
	return v.add(path, true), nil
}

func (v *Volume) add(path string, writable bool) uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.next++
	v.handles[v.next] = &handle{path: path, writable: writable}
	return v.next
}

// Read returns up to n bytes at off. A pending write overlapping the range
// is committed first, so a read after a write sees the write.
func (v *Volume) Read(ctx context.Context, id uint64, off int64, n int) ([]byte, error) {
	h, err := v.handle(id)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.buf != nil && off < h.off+int64(len(h.buf)) && off+int64(n) > h.off {
		if err := v.flushLocked(ctx, h); err != nil {
			return nil, err
		}
	}
	return v.store.ReadAt(ctx, h.path, off, n)
}

// Write buffers data at off and commits when the buffer fills or the
// handle flushes. See go-mapping §6: buffering, because FUSE delivers
// small writes, and the commit must happen before Flush returns.
func (v *Volume) Write(ctx context.Context, id uint64, off int64, data []byte) (int, error) {
	h, err := v.handle(id)
	if err != nil {
		return 0, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return v.writeLocked(ctx, h, off, data)
}

// writeLocked buffers data on a handle the caller already holds. A handle
// closed since the lookup refuses the write: buffering it would lose the
// bytes silently, because nothing flushes that handle any more.
func (v *Volume) writeLocked(ctx context.Context, h *handle, off int64, data []byte) (int, error) {
	if h.closed {
		return 0, ErrBadHandle
	}
	if !h.writable {
		return 0, vfs.ErrReadOnly
	}

	contiguous := h.buf != nil && off == h.off+int64(len(h.buf))
	if contiguous && len(h.buf)+len(data) <= vfs.ChunkSize {
		h.buf = append(h.buf, data...)
		return len(data), nil
	}
	if err := v.flushLocked(ctx, h); err != nil {
		return 0, err
	}
	h.off = off
	h.buf = append([]byte(nil), data...)
	if len(h.buf) >= vfs.ChunkSize {
		if err := v.flushLocked(ctx, h); err != nil {
			return 0, err
		}
	}
	return len(data), nil
}

// Flush commits the handle's buffer.
func (v *Volume) Flush(ctx context.Context, id uint64) error {
	h, err := v.handle(id)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return v.flushLocked(ctx, h)
}

// Release flushes and forgets the handle. A write that already holds the
// handle sees closed and refuses, so no byte lands in a buffer nobody
// flushes.
func (v *Volume) Release(ctx context.Context, id uint64) error {
	v.mu.Lock()
	h, ok := v.handles[id]
	delete(v.handles, id)
	v.mu.Unlock()
	if !ok {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	return v.flushLocked(ctx, h)
}

// Close flushes every open handle and forgets them all.
//
// It returns the joined flush errors and clears the table either way: Close
// means no further write arrives, so keeping a handle would only delay the
// loss to a moment nobody watches.
func (v *Volume) Close(ctx context.Context) error {
	v.mu.Lock()
	handles := make([]*handle, 0, len(v.handles))
	for _, h := range v.handles {
		handles = append(handles, h)
	}
	v.handles = make(map[uint64]*handle)
	v.mu.Unlock()

	var errs []error
	for _, h := range handles {
		h.mu.Lock()
		h.closed = true
		err := v.flushLocked(ctx, h)
		h.mu.Unlock()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (v *Volume) flushLocked(ctx context.Context, h *handle) error {
	if len(h.buf) == 0 {
		return nil
	}
	off, buf := h.off, h.buf
	h.buf, h.off = nil, 0
	return v.store.WriteAt(ctx, h.path, off, buf)
}

func (v *Volume) handle(id uint64) (*handle, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	h, ok := v.handles[id]
	if !ok {
		return nil, ErrBadHandle
	}
	return h, nil
}

// Lookup returns a path's metadata.
func (v *Volume) Lookup(ctx context.Context, path string) (*vfs.Stat, error) {
	return v.store.Stat(ctx, path)
}

// ReadDir lists a directory.
func (v *Volume) ReadDir(ctx context.Context, path string) ([]vfs.Dirent, error) {
	return v.store.ReadDir(ctx, path)
}

// Mkdir creates one directory with the permission bits the kernel asked for.
// FUSE writers carry no precondition: the kernel cannot express one (see
// go-mapping §6).
func (v *Volume) Mkdir(ctx context.Context, path string, mode uint32) error {
	return v.store.Mkdir(ctx, path, &vfs.MutateOptions{Mode: mode})
}

// Remove deletes a file or an empty directory.
func (v *Volume) Remove(ctx context.Context, path string) error {
	return v.store.Remove(ctx, path, nil)
}

// Truncate sets a file's length.
func (v *Volume) Truncate(ctx context.Context, path string, size int64) error {
	return v.store.Truncate(ctx, path, size, nil)
}

// OpenHandles reports how many handles are open. Used by tests and by the
// leak counter the mount reports on Close.
func (v *Volume) OpenHandles() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.handles)
}
