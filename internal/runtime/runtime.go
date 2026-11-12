// Package runtime is the single execution entry: one Exec call, one backend
// picked by id, one Result.
//
// It holds no filesystem semantics. Two collaborator interfaces carry the
// knowledge it must not have: Host answers what a backend asks, Outputs
// answers where the output of one execution goes. See
// docs/architecture/go-mapping.md §3.2 and ADR 0002.
package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"sync"
	"time"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/execout"
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

// LocalPath is a capability: a backend that runs commands on this host needs a
// real path, and Runtime resolves one before it starts the command.
//
// A backend that runs elsewhere leaves this out. Runtime then skips the
// resolution, so no local mount pays for a command it never runs, and the
// backend reads Request.Path only (go-mapping §6, "路径跨传输").
type LocalPath interface {
	Local() bool
}

// Host is what a backend may ask of the workspace that owns it.
type Host interface {
	// Resolve turns an absolute workspace path into a real path under the
	// mount point. A relative path, "..", a missing path, or a non-directory
	// fails with a coded error.
	Resolve(ctx context.Context, path string) (string, error)
}

// Outputs allocates the output target of one execution. Runtime asks for it;
// a backend never does, so it stays out of Host.
type Outputs interface {
	// For names the target for one execution. The ids reach the file names,
	// because the whole text lands in the workspace under those names. It
	// never returns nil.
	For(ctx context.Context, backendID, execID string) OutputSink
}

// OutputSink is the per-execution output target. Backends only see
// io.Writer; Runtime calls Finalize.
type OutputSink interface {
	Stdout() io.Writer
	Stderr() io.Writer
	// Finalize flushes pending bytes and returns the tails. Runtime calls it
	// once, after the backend's Result returns.
	Finalize(ctx context.Context) (execout.Result, error)
}

// Backend turns itself into a Handle. It holds no execution semantics and
// no route back to the caller.
type Backend interface {
	ID() string
	Kind() string // diagnostics only: "subprocess", "container", "module"
	Connect(ctx context.Context, host Host) (Handle, error)
}

// Handle is one connection to a backend. Runtime caches one per backend id.
//
// The base interface stays narrow. A capability that only some backends have
// (sync client, event stream, session reuse) is a separate interface the
// caller asserts with a type switch, not a new method here: a new method
// breaks every backend, including the ones that do not need it.
//
// Cancellation is the backend's job. A backend must end the whole process
// set, not only the direct child, and report Status Cancelled with
// ExitCode -1 and ctx.Err().
type Handle interface {
	// Sync reports whether the backend keeps its own storage.
	Sync() SyncMode
	// Exec starts the work and returns once it has started. Result waits for
	// the end.
	Exec(ctx context.Context, req Request) (*Exec, error)
	Close() error
	// Closed reports the connection dying on its own. Runtime caches handles,
	// so it needs a signal that does not wait for the next command to fail.
	// A backend whose connection cannot die returns a channel that never
	// closes.
	Closed() <-chan struct{}
}

// ExecOptions is a caller's request.
type ExecOptions struct {
	ID      string            // optional; Runtime mints one when empty
	Backend string            // routing, not authorisation (ADR 0002)
	Cwd     string            // absolute workspace path; "" means "/"
	Env     map[string]string // overrides on top of the process environment
	Timeout time.Duration     // the command only; zero means no limit
}

// Request is what a backend receives. Dir is already a real path, so no
// backend parses workspace paths.
type Request struct {
	Source string
	// Dir is a real path on this host. It is set only for a backend that
	// implements LocalPath, and is empty otherwise.
	Dir string
	// Path is the workspace path Dir was resolved from. It is the only form
	// of a location that means the same thing on the far side of a transport.
	Path   string
	Env    map[string]string
	Output OutputSink // never nil; runtime asks Outputs for it before the start
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
	Output   execout.Result
	// Sync is written by the bracket that owns the execution (the workspace
	// that pushed before it and pulls after it). Runtime carries the field; it
	// does not interpret it.
	Sync SyncOutcome
}

// SyncOutcome says whether the pull after one execution landed.
//
// There is no “lost” value: the authoritative side holds every change, so a
// replaced peer means a full resend, not a loss (ADR 0008).
type SyncOutcome int

const (
	// SyncComplete means the bracket's pull landed.
	SyncComplete SyncOutcome = iota
	// SyncPending means the pull failed. The command result stands, and the
	// next bracket resumes from the committed cursor.
	SyncPending
)

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
	outputs  Outputs
	backends map[string]Backend
	order    []string

	mu      sync.Mutex
	handles map[string]*session
}

// session is one cached handle. Its mutex makes "one handle, one command
// stream" true: a connection carries one execution at a time, so two callers
// queue instead of interleaving frames.
type session struct {
	mu sync.Mutex
	h  Handle
}

// dead reports whether the handle ended on its own.
func (s *session) dead() bool {
	select {
	case <-s.h.Closed():
		return true
	default:
		return false
	}
}

// New builds a Runtime. The last registration for an id wins; ids are
// configuration, and a duplicate is a wiring mistake no caller can see.
func New(host Host, outputs Outputs, backends ...Backend) *Runtime {
	r := &Runtime{
		host:     host,
		outputs:  outputs,
		backends: make(map[string]Backend, len(backends)),
		handles:  make(map[string]*session),
	}
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
	s, err := r.connect(ctx, backend)
	if err != nil {
		return nil, err
	}
	// One connection, one command stream.
	s.mu.Lock()

	cwd := opts.Cwd
	if cwd == "" {
		cwd = "/"
	}
	// Only a backend that runs here needs a real path. Resolving for a remote
	// backend would mount a volume the command never touches.
	dir := ""
	if local, ok := backend.(LocalPath); ok && local.Local() {
		dir, err = r.host.Resolve(ctx, cwd)
		if err != nil {
			// A caller error, not a transport error: keep the session.
			s.mu.Unlock()
			return nil, err
		}
	}
	if opts.ID == "" {
		opts.ID = mintID()
	}
	out := r.outputs.For(ctx, backend.ID(), opts.ID)
	// The clock starts at the command, not at the mount: a slow first mount
	// must not be reported as a command timeout (go-mapping §5 step 4).
	ctx, cancel := ctx, context.CancelFunc(func() {})
	if opts.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
	}
	ex, err := s.h.Exec(ctx, Request{Source: source, Dir: dir, Path: cwd, Env: opts.Env, Output: out})
	if err != nil {
		cancel()
		s.mu.Unlock()
		r.drop(backend.ID(), s)
		return nil, err
	}
	return keep(r, backend.ID(), s, opts.ID, ex, out, cancel), nil
}

// connect returns the cached session for a backend, building one when the
// cache is empty or the cached handle died.
func (r *Runtime) connect(ctx context.Context, backend Backend) (*session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if s, ok := r.handles[backend.ID()]; ok {
		if s.dead() {
			_ = s.h.Close()
			delete(r.handles, backend.ID())
		} else {
			return s, nil
		}
	}
	h, err := backend.Connect(ctx, r.host)
	if err != nil {
		return nil, err
	}
	s := &session{h: h}
	r.handles[backend.ID()] = s
	return s, nil
}

// drop forgets a session and closes its handle.
func (r *Runtime) drop(backendID string, s *session) {
	r.mu.Lock()
	if r.handles[backendID] == s {
		delete(r.handles, backendID)
	}
	r.mu.Unlock()
	_ = s.h.Close()
}

// Close closes every cached handle. Executions must have finished first.
func (r *Runtime) Close() error {
	r.mu.Lock()
	handles := r.handles
	r.handles = make(map[string]*session)
	r.mu.Unlock()
	for _, s := range handles {
		_ = s.h.Close()
	}
	return nil
}

// finalizeTimeout bounds the flush of an execution that has just ended.
const finalizeTimeout = 5 * time.Second

// keep makes the result repeatable and ties the handle to the execution.
//
// Finalize runs here, not in the backend, so every backend gets the output
// contract for free.
func keep(r *Runtime, backendID string, s *session, id string, ex *Exec, out OutputSink, cancel context.CancelFunc) *Exec {
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
			// The command context may already be done, so the flush gets
			// its own deadline and no inherited cancel.
			fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), finalizeTimeout)
			outRes, ferr := out.Finalize(fctx)
			fcancel()
			cancel()
			// The session goes back to the cache, unless the handle died on
			// its own or the command failed at the transport.
			s.mu.Unlock()
			if resErr != nil || s.dead() {
				r.drop(backendID, s)
			}
			res.Output = outRes
			if ferr != nil {
				res.Output.Err = errors.Join(res.Output.Err, ferr)
			}
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
