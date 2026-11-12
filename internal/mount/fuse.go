//go:build !windows

package mount

import (
	"context"
	"errors"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

// Server is a mounted volume.
type Server struct{ inner *fuse.Server }

// Unmount detaches the filesystem.
func (s *Server) Unmount() error { return s.inner.Unmount() }

// Wait blocks until the mount is torn down.
func (s *Server) Wait() { s.inner.Wait() }

// Mount serves vol at mountPoint. Every syscall is translated into a
// Volume call; this file holds no rules of its own.
//
// Entry and attribute timeouts are zero on purpose (go-mapping §6): a
// cached attribute would let the mount disagree with the store.
func Mount(vol *Volume, mountPoint string, debug bool) (*Server, error) {
	zero := time.Duration(0)
	opts := &fs.Options{
		EntryTimeout:    &zero,
		AttrTimeout:     &zero,
		NegativeTimeout: &zero,
	}
	opts.Debug = debug
	opts.MountOptions.Name = "agent-design"

	root := &node{vol: vol, path: "/"}
	srv, err := fs.Mount(mountPoint, root, opts)
	if err != nil {
		return nil, err
	}
	return &Server{inner: srv}, nil
}

// node is one path in the mounted tree.
type node struct {
	fs.Inode
	vol  *Volume
	path string
}

var (
	_ = (fs.NodeLookuper)((*node)(nil))
	_ = (fs.NodeGetattrer)((*node)(nil))
	_ = (fs.NodeReaddirer)((*node)(nil))
	_ = (fs.NodeMkdirer)((*node)(nil))
	_ = (fs.NodeCreater)((*node)(nil))
	_ = (fs.NodeUnlinker)((*node)(nil))
	_ = (fs.NodeRmdirer)((*node)(nil))
	_ = (fs.NodeOpener)((*node)(nil))
	_ = (fs.NodeSetattrer)((*node)(nil))
)

func (n *node) join(name string) string {
	if n.path == "/" {
		return "/" + name
	}
	return n.path + "/" + name
}

func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	st, err := n.vol.Lookup(ctx, n.join(name))
	if err != nil {
		return nil, errno(err)
	}
	fillAttr(&out.Attr, st)
	return n.NewInode(ctx, &node{vol: n.vol, path: n.join(name)},
		fs.StableAttr{Mode: stableMode(st), Ino: uint64(st.Inode)}), 0
}

func (n *node) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	st, err := n.vol.Lookup(ctx, n.path)
	if err != nil {
		return errno(err)
	}
	fillAttr(&out.Attr, st)
	return 0
}

func (n *node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	entries, err := n.vol.ReadDir(ctx, n.path)
	if err != nil {
		return nil, errno(err)
	}
	list := make([]fuse.DirEntry, 0, len(entries))
	for _, e := range entries {
		mode := uint32(syscall.S_IFREG)
		if e.Type == vfs.TypeDir {
			mode = syscall.S_IFDIR
		}
		list = append(list, fuse.DirEntry{Name: e.Name, Mode: mode, Ino: uint64(e.Inode)})
	}
	return fs.NewListDirStream(list), 0
}

func (n *node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	path := n.join(name)
	if err := n.vol.Mkdir(ctx, path, mode&0o7777); err != nil {
		return nil, errno(err)
	}
	st, err := n.vol.Lookup(ctx, path)
	if err != nil {
		return nil, errno(err)
	}
	fillAttr(&out.Attr, st)
	return n.NewInode(ctx, &node{vol: n.vol, path: path},
		fs.StableAttr{Mode: syscall.S_IFDIR, Ino: uint64(st.Inode)}), 0
}

func (n *node) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	path := n.join(name)
	id, err := n.vol.Create(ctx, path, mode&0o7777)
	if err != nil {
		return nil, nil, 0, errno(err)
	}
	st, err := n.vol.Lookup(ctx, path)
	if err != nil {
		return nil, nil, 0, errno(err)
	}
	fillAttr(&out.Attr, st)
	child := n.NewInode(ctx, &node{vol: n.vol, path: path},
		fs.StableAttr{Mode: syscall.S_IFREG, Ino: uint64(st.Inode)})
	return child, &file{vol: n.vol, id: id}, 0, 0
}

func (n *node) Unlink(ctx context.Context, name string) syscall.Errno {
	return errno(n.vol.Remove(ctx, n.join(name)))
}

func (n *node) Rmdir(ctx context.Context, name string) syscall.Errno {
	return errno(n.vol.Remove(ctx, n.join(name)))
}

func (n *node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	writable := flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0
	var (
		id  uint64
		err error
	)
	if flags&syscall.O_TRUNC != 0 && writable {
		id, err = n.vol.Create(ctx, n.path, 0)
	} else {
		id, err = n.vol.Open(ctx, n.path, writable)
	}
	if err != nil {
		return nil, 0, errno(err)
	}
	return &file{vol: n.vol, id: id}, 0, 0
}

func (n *node) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if mode, ok := in.GetMode(); ok {
		if err := n.vol.Chmod(ctx, n.path, mode); err != nil {
			return errno(err)
		}
	}
	if size, ok := in.GetSize(); ok {
		if err := n.vol.Truncate(ctx, n.path, int64(size)); err != nil {
			return errno(err)
		}
	}
	st, err := n.vol.Lookup(ctx, n.path)
	if err != nil {
		return errno(err)
	}
	fillAttr(&out.Attr, st)
	return 0
}

// file is one open handle.
type file struct {
	vol *Volume
	id  uint64
}

var (
	_ = (fs.FileReader)((*file)(nil))
	_ = (fs.FileWriter)((*file)(nil))
	_ = (fs.FileFlusher)((*file)(nil))
	_ = (fs.FileReleaser)((*file)(nil))
)

func (f *file) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	body, err := f.vol.Read(ctx, f.id, off, len(dest))
	if err != nil {
		return nil, errno(err)
	}
	return fuse.ReadResultData(body), 0
}

func (f *file) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	n, err := f.vol.Write(ctx, f.id, off, data)
	if err != nil {
		return 0, errno(err)
	}
	return uint32(n), 0
}

func (f *file) Flush(ctx context.Context) syscall.Errno { return errno(f.vol.Flush(ctx, f.id)) }

func (f *file) Release(ctx context.Context) syscall.Errno { return errno(f.vol.Release(ctx, f.id)) }

func fillAttr(out *fuse.Attr, st *vfs.Stat) {
	out.Ino = uint64(st.Inode)
	out.Size = uint64(st.Size)
	out.Mode = stableMode(st) | st.Mode
	out.Mtime = uint64(st.MTime / 1000)
	out.Nlink = 1
	out.Blksize = uint32(vfs.ChunkSize)
}

func stableMode(st *vfs.Stat) uint32 {
	if st.Type == vfs.TypeDir {
		return syscall.S_IFDIR
	}
	return syscall.S_IFREG
}

// errno translates a store error into what the kernel reports. Anything
// unrecognised becomes EIO rather than a silent success.
func errno(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	switch {
	case errors.Is(err, vfs.ErrNotFound):
		return syscall.ENOENT
	case errors.Is(err, vfs.ErrExist):
		return syscall.EEXIST
	case errors.Is(err, vfs.ErrNotDir):
		return syscall.ENOTDIR
	case errors.Is(err, vfs.ErrIsDir):
		return syscall.EISDIR
	case errors.Is(err, vfs.ErrNotEmpty):
		return syscall.ENOTEMPTY
	case errors.Is(err, vfs.ErrReadOnly):
		return syscall.EROFS
	case errors.Is(err, vfs.ErrInvalid):
		return syscall.EINVAL
	case errors.Is(err, vfs.ErrPerm):
		return syscall.EPERM
	case errors.Is(err, vfs.ErrConflict):
		return syscall.EAGAIN
	case errors.Is(err, ErrBadHandle):
		return syscall.EBADF
	default:
		return syscall.EIO
	}
}
