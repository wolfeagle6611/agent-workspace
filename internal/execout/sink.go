// Package execout holds the output policy shared by all backends: a bounded
// tail in memory, the whole text in the workspace store.
//
// The policy is one place on purpose. A backend writes bytes into an
// io.Writer and knows nothing about limits, files, or partial failure. See
// docs/architecture/computer.md §6.4.
package execout

import (
	"context"
	"io"
	"sync"
)

// Store is the slice of the workspace the sink needs. It is an interface so
// the tail policy can be tested without a database.
type Store interface {
	WriteAt(ctx context.Context, path string, off int64, data []byte) error
}

// Stream is the summary of one stream: the bounded tail, where the whole
// text is, and how much there was. It is a value, not a live flow.
type Stream struct {
	Tail      []byte
	Path      string
	Bytes     int64
	Lines     int64
	Truncated bool // Tail does not hold the whole stream
}

// Result is the output half of an execution result.
type Result struct {
	Stdout Stream
	Stderr Stream
	// Err says the full text did not reach the store. Status and ExitCode
	// still describe the command: a broken log must not rewrite a result.
	Err error
}

// Sink streams two streams into the store and keeps a tail of each.
//
// Write never returns an error: a failing store is recorded in Result.Err and
// the stream keeps going in memory, so the command is not killed by its own
// logging.
type Sink struct {
	out  *streamWriter
	errw *streamWriter

	mu  sync.Mutex
	err error
}

// streamWriter is the live side: it owns one window and appends to one file.
type streamWriter struct {
	win    window
	limits Limits

	store  Store
	ctx    context.Context // creation context: appends outlive the caller's cancel
	path   string
	off    int64
	pend   []byte
	broken bool
	onErr  func(error)

	mu sync.Mutex // one copy goroutine per stream, but Finalize may read
}

// NewSink builds a sink for one execution. It creates both files up front so
// the paths in Result are real even when the command prints nothing.
//
// A nil store keeps the tails only; that is the degraded mode used by tests
// and by a host that cannot write.
func NewSink(ctx context.Context, store Store, limits Limits, stdoutPath, stderrPath string) *Sink {
	limits = limits.withDefaults()
	// Appends outlive the caller's cancel: an interrupted command still
	// needs its last bytes on disk. Finalize adds its own deadline.
	base := context.WithoutCancel(ctx)
	s := &Sink{}
	s.out = newStream(base, store, limits, stdoutPath, s.record)
	s.errw = newStream(base, store, limits, stderrPath, s.record)
	return s
}

// record keeps the first failure. Later ones rarely add information.
func (s *Sink) record(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

func newStream(ctx context.Context, store Store, limits Limits, path string, onErr func(error)) *streamWriter {
	st := &streamWriter{
		win:    newWindow(limits),
		limits: limits,
		store:  store,
		ctx:    ctx,
		path:   path,
		onErr:  onErr,
	}
	if store == nil || path == "" {
		return st
	}
	if err := store.WriteAt(ctx, path, 0, nil); err != nil {
		st.broken = true
		st.path = ""
		onErr(err)
	}
	return st
}

// Stdout is the writer a backend hands to its stdout pipe.
func (s *Sink) Stdout() io.Writer { return s.out }

// Stderr is the writer a backend hands to its stderr pipe.
func (s *Sink) Stderr() io.Writer { return s.errw }

// Finalize flushes what is pending and returns the tails.
//
// ctx should carry its own deadline. It is not the command's context, which
// is already done when a command is cancelled.
func (s *Sink) Finalize(ctx context.Context) (Result, error) {
	res := Result{Stdout: s.out.finalize(ctx), Stderr: s.errw.finalize(ctx)}
	s.mu.Lock()
	res.Err = s.err
	s.mu.Unlock()
	return res, nil
}

func (st *streamWriter) Write(p []byte) (int, error) {
	n := len(p)

	st.mu.Lock()
	defer st.mu.Unlock()

	_, _ = st.win.Write(p)
	if st.broken || st.store == nil {
		return n, nil
	}
	for len(p) > 0 {
		take := st.limits.FlushBytes - len(st.pend)
		if take > len(p) {
			take = len(p)
		}
		st.pend = append(st.pend, p[:take]...)
		p = p[take:]
		if len(st.pend) == st.limits.FlushBytes {
			st.flushLocked(st.ctx)
		}
	}
	return n, nil
}

func (st *streamWriter) finalize(ctx context.Context) Stream {
	st.mu.Lock()
	defer st.mu.Unlock()

	if !st.broken {
		st.flushLocked(ctx)
	}
	tail := st.win.snapshot()
	return Stream{
		Tail:      tail,
		Path:      st.path,
		Bytes:     st.win.total,
		Lines:     st.win.lines,
		Truncated: st.win.total > int64(len(tail)),
	}
}

func (st *streamWriter) flushLocked(ctx context.Context) {
	if len(st.pend) == 0 || st.broken {
		return
	}
	if err := st.store.WriteAt(ctx, st.path, st.off, st.pend); err != nil {
		st.broken = true
		st.onErr(err)
		st.pend = nil
		return
	}
	st.off += int64(len(st.pend))
	st.pend = st.pend[:0]
}
