// Package runtime is the single execution entry: one Exec call, one backend
// picked by id, one Result.
//
// It holds no filesystem semantics. The workspace that owns the runtime is
// the Host: it turns a workspace path into a real path. See
// docs/architecture/go-mapping.md §3.2 and ADR 0002.
package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

// SyncMode says whether a backend keeps its own storage.
type SyncMode int

const (
	// SyncRemote means the backend has its own store, so push and pull are
	// real work. Phase two only.
	SyncRemote SyncMode = iota
	// SyncNone means the backend reads and writes the authoritative store
	// directly, so push and pull are no-ops (ADR 0006).
	SyncNone
)

// Host is what a backend may ask of the workspace that owns it.
type Host interface {
	// Resolve turns an absolute workspace path into a real path under the
	// mount point. A relative path, "..", a missing path, or a non-directory
	// fails with a coded error.
	Resolve(ctx context.Context, path string) (string, error)
}

// Backend turns itself into a Handle. It holds no execution semantics and
// no route back to the caller.
type Backend interface {
	ID() string
	Kind() string // diagnostics only: "subprocess", "container", "module"
	Connect(ctx context.Context, host Host) (Handle, error)
}

// Handle is one connection to a backend. It lives for one Exec.
type Handle interface {
	// Sync reports whether the backend keeps its own storage.
	Sync() SyncMode
	// Exec starts the work and returns once it has started. Result waits for
	// the end.
	Exec(ctx context.Context, req Request) (*Exec, error)
	Close() error
}

// ExecOptions is a caller's request.
type ExecOptions struct {
	ID      string            // optional; Runtime mints one when empty
	Backend string            // routing, not authorisation (ADR 0002)
	Cwd     string            // absolute workspace path; "" means "/"
	Env     map[string]string // overrides on top of the process environment
}

// Request is what a backend receives. Dir is already a real path, so no
// backend parses workspace paths.
type Request struct {
	Source string
	Dir    string
	Env    map[string]string
}

// Status is how an execution ended. The backend decides it; Runtime does not
// interpret.
type Status int

const (
	Completed Status = iota
	Failed
	Cancelled
)

// Result is the end of one execution.
type Result struct {
	Status   Status
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// Exec is one execution: an id and a result to wait for.
//
// Result may be called more than once; every call returns the same result.
// The first call waits (go-mapping §3.2: one consumer, so a repeated call
// must not race the first).
type Exec struct {
	ID      string
	Backend string
	Result  func(context.Context) (Result, error)
}

// Runtime routes the one entry point to the registered backends.
type Runtime struct {
	host     Host
	backends map[string]Backend
	order    []string
}

// New builds a Runtime. The last registration for an id wins; ids are
// configuration, and a duplicate is a wiring mistake no caller can see.
func New(host Host, backends ...Backend) *Runtime {
	r := &Runtime{host: host, backends: make(map[string]Backend, len(backends))}
	for _, b := range backends {
		r.backends[b.ID()] = b
		r.order = append(r.order, b.ID())
	}
	sort.Strings(r.order)
	return r
}

// Exec runs source on the named backend.
func (r *Runtime) Exec(ctx context.Context, source string, opts ExecOptions) (*Exec, error) {
	backend, err := r.pick(opts.Backend)
	if err != nil {
		return nil, err
	}
	h, err := backend.Connect(ctx, r.host)
	if err != nil {
		return nil, err
	}
	if h.Sync() != SyncNone {
		_ = h.Close()
		return nil, vfs.WrapError(vfs.CodeInvalid, backend.ID(),
			errors.New("backend declares sync remote: phase two is not implemented"))
	}
	cwd := opts.Cwd
	if cwd == "" {
		cwd = "/"
	}
	dir, err := r.host.Resolve(ctx, cwd)
	if err != nil {
		_ = h.Close()
		return nil, err
	}
	if opts.ID == "" {
		opts.ID = mintID()
	}
	ex, err := h.Exec(ctx, Request{Source: source, Dir: dir, Env: opts.Env})
	if err != nil {
		_ = h.Close()
		return nil, err
	}
	return keep(h, backend.ID(), opts.ID, ex), nil
}

// keep makes the result repeatable and ties the handle to the execution.
func keep(h Handle, backendID, id string, ex *Exec) *Exec {
	inner := ex.Result
	var mu sync.Mutex
	var once sync.Once
	var res Result
	var resErr error

	repeat := func(ctx context.Context) (Result, error) {
		mu.Lock()
		defer mu.Unlock()
		once.Do(func() {
			res, resErr = inner(ctx)
			// One Exec, one handle: the connection dies with the work.
			_ = h.Close()
		})
		return res, resErr
	}
	return &Exec{ID: id, Backend: backendID, Result: repeat}
}

// pick resolves the routing field. backend is never authorisation: see
// ADR 0002.
func (r *Runtime) pick(id string) (Backend, error) {
	if len(r.order) == 0 {
		return nil, vfs.NewError(vfs.CodeNoBackend, id)
	}
	if id == "" {
		if len(r.order) > 1 {
			return nil, vfs.WrapError(vfs.CodeInvalid, "",
				errors.New("more than one backend is registered: name one in ExecOptions.Backend"))
		}
		return r.backends[r.order[0]], nil
	}
	b, ok := r.backends[id]
	if !ok {
		return nil, vfs.NewError(vfs.CodeNoBackend, id)
	}
	return b, nil
}

// mintID gives an execution an id before any caller asks for one, so phase
// two can add getExec and killExec without changing this shape.
func mintID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // never fails on the platforms this builds for
	return "exec-" + hex.EncodeToString(b[:])
}
