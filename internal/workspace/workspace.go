// Package workspace owns one workspace: the store, the mount session, and
// the single execution entry.
//
// The pieces stay separate on purpose. mount.Session owns the mount and its
// order, the runtime holds the execution contract, and this package wires
// them and owns the store (docs/architecture/go-mapping.md §5 step 3).
package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"sync"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/backend/shell"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/mount"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/runtime"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

// Options tunes where the workspace appears. The zero value is usable.
type Options struct {
	// MountRoot is the directory mount points live in. Empty means
	// mount.DefaultRoot(). It must be absolute.
	MountRoot string
}

// Workspace is one workspace: store, mount session, and execution entry.
type Workspace struct {
	store   *vfs.Store
	session *mount.Session
	rt      *runtime.Runtime

	mu     sync.Mutex
	closed bool
}

// Workspace is the only Host the runtime gets, so a drifted Resolve
// signature must fail here and not at a call site.
var _ runtime.Host = (*Workspace)(nil)

// Open opens the workspace whose data lives at dbPath.
//
// The mount point is <root>/<mountID>, and mounting is lazy: the first Exec
// mounts, so a caller that uses only the fs methods never needs FUSE.
func Open(dbPath string, opts Options) (*Workspace, error) {
	root := opts.MountRoot
	if root == "" {
		root = mount.DefaultRoot()
	}
	if !filepath.IsAbs(root) {
		return nil, vfs.NewError(vfs.CodeInvalid, root)
	}
	id, err := mountID(dbPath)
	if err != nil {
		return nil, err
	}

	store, err := vfs.Open(dbPath)
	if err != nil {
		return nil, err
	}
	w := &Workspace{
		store:   store,
		session: mount.NewSession(mount.NewVolume(store), mount.NewManager(root), id),
	}
	w.rt = runtime.New(w, shell.New())
	return w, nil
}

// mountID names the mount point of a database.
//
// The name carries a hash of the absolute database path. Manager.Prepare
// clears whatever it finds at a mount point, so two databases that share a
// file name must not share a point: the second Open would unmount the first
// and route its writes to the wrong store.
func mountID(dbPath string) (string, error) {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return "", vfs.WrapError(vfs.CodeInvalid, dbPath, err)
	}
	base := strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "", vfs.NewError(vfs.CodeInvalid, dbPath)
	}
	sum := sha256.Sum256([]byte(abs))
	return base + "-" + hex.EncodeToString(sum[:4]), nil
}

// Exec runs source through the single execution entry.
func (w *Workspace) Exec(ctx context.Context, source string, opts runtime.ExecOptions) (*runtime.Exec, error) {
	return w.rt.Exec(ctx, source, opts)
}

// MountPoint returns the real directory the workspace is visible at. It is
// empty until the first Exec mounts it.
func (w *Workspace) MountPoint() string {
	return w.session.Path()
}

// Resolve turns a workspace path into a real path under the mount point.
//
// It is the only place where a workspace path becomes a real path, so the
// rules live here once: reject a relative path or "..", require the path to
// exist, require a directory, and mount on demand (go-mapping §5 step 3).
func (w *Workspace) Resolve(ctx context.Context, p string) (string, error) {
	if err := w.ensureOpen(); err != nil {
		return "", err
	}
	if p == "" {
		p = "/"
	}
	clean, err := vfs.Clean(p)
	if err != nil {
		return "", err
	}
	st, err := w.store.Stat(ctx, clean)
	if err != nil {
		return "", err
	}
	if st.Type != vfs.TypeDir {
		return "", vfs.NewError(vfs.CodeNotDir, p)
	}

	point, err := w.session.Mount(ctx)
	if err != nil {
		return "", err
	}
	if clean == "/" {
		return point, nil
	}
	return filepath.Join(point, clean), nil
}

// ensureOpen refuses work on a closed workspace before anything else reads
// the store, so the caller sees ECLOSED and not a closed-database error.
func (w *Workspace) ensureOpen() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return vfs.NewError(vfs.CodeClosed, "")
	}
	return nil
}

// Close flushes open write buffers, unmounts, removes the mount point, and
// closes the store. Close is safe to call twice and does not mount.
func (w *Workspace) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()

	ctx := context.Background()
	errs := []error{w.session.Unmount(ctx)}
	if err := w.store.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Stat returns a path's metadata. See vfs.Store.Stat.
func (w *Workspace) Stat(ctx context.Context, path string) (*vfs.Stat, error) {
	return w.store.Stat(ctx, path)
}

// ReadFile returns a file's bytes. See vfs.Store.ReadFile.
func (w *Workspace) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return w.store.ReadFile(ctx, path)
}

// WriteFile creates or replaces a file. See vfs.Store.WriteFile.
func (w *Workspace) WriteFile(ctx context.Context, path string, data []byte, opts *vfs.WriteOptions) error {
	return w.store.WriteFile(ctx, path, data, opts)
}

// ReadDir lists a directory. See vfs.Store.ReadDir.
func (w *Workspace) ReadDir(ctx context.Context, path string) ([]vfs.Dirent, error) {
	return w.store.ReadDir(ctx, path)
}

// Mkdir creates one directory. See vfs.Store.Mkdir.
func (w *Workspace) Mkdir(ctx context.Context, path string) error {
	return w.store.Mkdir(ctx, path)
}

// MkdirAll creates a directory and its parents. See vfs.Store.MkdirAll.
func (w *Workspace) MkdirAll(ctx context.Context, path string) error {
	return w.store.MkdirAll(ctx, path)
}

// Remove deletes a file or an empty directory. See vfs.Store.Remove.
func (w *Workspace) Remove(ctx context.Context, path string) error {
	return w.store.Remove(ctx, path)
}

// Truncate sets a file's length. See vfs.Store.Truncate.
func (w *Workspace) Truncate(ctx context.Context, path string, size int64) error {
	return w.store.Truncate(ctx, path, size)
}
