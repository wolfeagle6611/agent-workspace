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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/backend/shell"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/execout"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/mount"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/runtime"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

const (
	// outputDir is the reserved directory for output text
	// (go-mapping §6). Everything in it is system output.
	outputDir = "/.computer/output"
	// keepOutput is how many output files survive a prune.
	keepOutput = 50
	// closeGrace bounds how long Close waits for running executions.
	closeGrace = 5 * time.Second
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

	// root cancels every running execution. Close cancels it, then waits a
	// bounded time before the store goes away.
	root    context.Context
	stop    context.CancelFunc
	running sync.WaitGroup

	mu     sync.Mutex
	closed bool
}

// Workspace is the only Host and Outputs the runtime gets, so a drifted
// signature must fail here and not at a call site.
var (
	_ runtime.Host    = (*Workspace)(nil)
	_ runtime.Outputs = (*Workspace)(nil)
)

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
	// The reserved directory is created here, not lazily: a listing of "/"
	// must look the same whether or not a command has run yet.
	if err := store.MkdirAll(context.Background(), outputDir); err != nil {
		_ = store.Close()
		return nil, err
	}
	rootCtx, stop := context.WithCancel(context.Background())
	w := &Workspace{
		store:   store,
		session: mount.NewSession(mount.NewVolume(store), mount.NewManager(root), id),
		root:    rootCtx,
		stop:    stop,
	}
	w.rt = runtime.New(w, w, shell.New())
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
//
// The workspace adds two things the runtime cannot: a root cancel that
// Close can pull, and the output prune after the text has landed.
func (w *Workspace) Exec(ctx context.Context, source string, opts runtime.ExecOptions) (*runtime.Exec, error) {
	if err := w.begin(); err != nil {
		return nil, err
	}
	ectx, cancel := context.WithCancel(ctx)
	stopRoot := context.AfterFunc(w.root, cancel)

	ex, err := w.rt.Exec(ectx, source, opts)
	if err != nil {
		stopRoot()
		cancel()
		w.running.Done()
		return nil, err
	}

	inner := ex.Result
	var once sync.Once
	ex.Result = func(ctx context.Context) (runtime.Result, error) {
		res, err := inner(ctx)
		once.Do(func() {
			stopRoot()
			// The command's context may be done; the prune still needs to
			// reach the store before Close does.
			w.prune(context.WithoutCancel(ctx))
			cancel()
			w.running.Done()
		})
		return res, err
	}
	return ex, nil
}

// begin admits one execution. It refuses a closed workspace and counts the
// execution in the same lock, so Close never closes the store under a start
// that has already been admitted.
func (w *Workspace) begin() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return vfs.NewError(vfs.CodeClosed, "")
	}
	w.running.Add(1)
	return nil
}

// For names the two output files for one execution.
//
// A repeated id gets a numeric suffix instead of overwriting the previous
// run's text; the exact path comes back in the result.
func (w *Workspace) For(ctx context.Context, backendID, execID string) runtime.OutputSink {
	base := outputDir + "/" + segment(backendID) + "." + segment(execID)
	for n := 0; ; n++ {
		suffix := ""
		if n > 0 {
			suffix = "-" + strconv.Itoa(n)
		}
		stdout := base + suffix + ".stdout.log"
		stderr := base + suffix + ".stderr.log"
		if w.taken(ctx, stdout) || w.taken(ctx, stderr) {
			continue
		}
		return execout.NewSink(ctx, w.store, execout.DefaultLimits, stdout, stderr)
	}
}

func (w *Workspace) taken(ctx context.Context, p string) bool {
	_, err := w.store.Stat(ctx, p)
	return err == nil
}

// segment keeps one path segment. An id with a slash would otherwise leave
// the output directory.
func segment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// prune keeps the newest keepOutput files by rev. The directory is reserved,
// so every entry in it is output (go-mapping §6).
func (w *Workspace) prune(ctx context.Context) {
	entries, err := w.store.ReadDir(ctx, outputDir)
	if err != nil || len(entries) <= keepOutput {
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Rev > entries[j].Rev })
	for _, e := range entries[keepOutput:] {
		_ = w.store.Remove(ctx, outputDir+"/"+e.Name, nil)
	}
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
//
// Running executions are cancelled first, then given a bounded time to
// finish, so the store is never closed under a live writer.
func (w *Workspace) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()

	w.stop()
	w.waitRunning(closeGrace)

	ctx := context.Background()
	errs := []error{w.session.Unmount(ctx)}
	if err := w.store.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (w *Workspace) waitRunning(d time.Duration) {
	done := make(chan struct{})
	go func() {
		w.running.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// Stat returns a path's metadata. See vfs.Store.Stat.
func (w *Workspace) Stat(ctx context.Context, path string) (*vfs.Stat, error) {
	return w.store.Stat(ctx, path)
}

// ReadFile returns a file's bytes and metadata as one snapshot. See
// vfs.Store.ReadFile.
func (w *Workspace) ReadFile(ctx context.Context, path string) (*vfs.Content, error) {
	return w.store.ReadFile(ctx, path)
}

// WriteFile creates or replaces a file. See vfs.Store.WriteFile.
func (w *Workspace) WriteFile(ctx context.Context, path string, data []byte, opts *vfs.MutateOptions) error {
	return w.store.WriteFile(ctx, path, data, opts)
}

// ReadDir lists a directory. See vfs.Store.ReadDir.
func (w *Workspace) ReadDir(ctx context.Context, path string) ([]vfs.Dirent, error) {
	return w.store.ReadDir(ctx, path)
}

// Mkdir creates one directory. See vfs.Store.Mkdir.
func (w *Workspace) Mkdir(ctx context.Context, path string, opts *vfs.MutateOptions) error {
	return w.store.Mkdir(ctx, path, opts)
}

// MkdirAll creates a directory and its parents. See vfs.Store.MkdirAll.
func (w *Workspace) MkdirAll(ctx context.Context, path string) error {
	return w.store.MkdirAll(ctx, path)
}

// Remove deletes a file or an empty directory. See vfs.Store.Remove.
func (w *Workspace) Remove(ctx context.Context, path string, opts *vfs.MutateOptions) error {
	return w.store.Remove(ctx, path, opts)
}

// Truncate sets a file's length. See vfs.Store.Truncate.
func (w *Workspace) Truncate(ctx context.Context, path string, size int64, opts *vfs.MutateOptions) error {
	return w.store.Truncate(ctx, path, size, opts)
}

// Rename moves a path. See vfs.Store.Rename: it takes two preconditions,
// BaseRev for the source and TargetBaseRev for the target it replaces.
func (w *Workspace) Rename(ctx context.Context, src, dst string, opts *vfs.MutateOptions) error {
	return w.store.Rename(ctx, src, dst, opts)
}
