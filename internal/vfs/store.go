package vfs

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store is an open workspace filesystem.
//
// Writes run on one goroutine, in arrival order. Reads use the pool and see
// committed data through WAL. This makes "one writer at a time" structural
// rather than a convention (docs/architecture/computer.md §2, I2).
type Store struct {
	db  *sql.DB
	now func() time.Time

	writeCh chan writeOp
	mu      sync.Mutex // guards closed and the send below
	closed  chan struct{}
	drain   chan struct{}
	once    sync.Once
}

type writeOp struct {
	fn   func(context.Context, *sql.Tx) error
	done chan error
}

// Open opens or creates a workspace at path.
func Open(path string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	dsn := "file:" + path + "?" + q.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One writer goroutine, several readers. WAL keeps readers off the
	// writer's back.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)

	if _, err := db.ExecContext(context.Background(), schemaDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	if _, err := db.ExecContext(context.Background(),
		`INSERT OR IGNORE INTO vfs_meta(key, value) VALUES ('schema_version', ?), ('rev', 0)`,
		schemaVersion,
	); err != nil {
		db.Close()
		return nil, fmt.Errorf("seed meta: %w", err)
	}
	if _, err := db.ExecContext(context.Background(),
		`INSERT OR IGNORE INTO vfs_nodes(inode, type, mode, size, mtime, rev) VALUES (?, 'dir', ?, 0, 0, 0)`,
		rootInode, 0o755,
	); err != nil {
		db.Close()
		return nil, fmt.Errorf("seed root: %w", err)
	}

	s := &Store{
		db:      db,
		now:     time.Now,
		writeCh: make(chan writeOp, 64),
		closed:  make(chan struct{}),
		drain:   make(chan struct{}),
	}
	go s.run()
	return s, nil
}

func (s *Store) run() {
	defer close(s.drain)
	for op := range s.writeCh {
		op.done <- s.exec(op)
	}
}

// exec runs one write operation in a transaction.
func (s *Store) exec(op writeOp) error {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return wrapError(CodeInvalid, "", err)
	}
	if err := op.fn(ctx, tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return wrapError(CodeInvalid, "", err)
	}
	return nil
}

// withWrite queues fn on the writer goroutine.
//
// ctx bounds the wait, not the write: once fn is queued it commits even if
// the caller has gone away, and the caller gets ctx.Err() as if the write
// failed. A caller that retries with the same precondition therefore gets
// ECONFLICT rather than a second write, so nothing lands twice.
func (s *Store) withWrite(ctx context.Context, fn func(context.Context, *sql.Tx) error) error {
	op := writeOp{fn: fn, done: make(chan error, 1)}

	s.mu.Lock()
	select {
	case <-s.closed:
		s.mu.Unlock()
		return newError(CodeClosed, "")
	default:
	}
	s.writeCh <- op
	s.mu.Unlock()

	select {
	case err := <-op.done:
		return err
	case <-ctx.Done():
		// The transaction still commits; the caller just stops waiting.
		return ctx.Err()
	}
}

// Close stops accepting writes, drains the queue, and closes the database.
func (s *Store) Close() error {
	var err error
	s.once.Do(func() {
		s.mu.Lock()
		close(s.closed)
		close(s.writeCh)
		s.mu.Unlock()

		<-s.drain
		err = s.db.Close()
	})
	return err
}

// metaRev returns the workspace revision counter.
func (s *Store) metaRev(ctx context.Context) (int64, error) {
	var rev int64
	err := s.db.QueryRowContext(ctx, `SELECT value FROM vfs_meta WHERE key = 'rev'`).Scan(&rev)
	if err != nil {
		return 0, wrapError(CodeInvalid, "", err)
	}
	return rev, nil
}

// mintRev bumps the workspace revision and returns the new value. Every
// committed mutation gets exactly one revision (I3).
func mintRev(ctx context.Context, tx *sql.Tx) (int64, error) {
	var rev int64
	err := tx.QueryRowContext(ctx,
		`UPDATE vfs_meta SET value = value + 1 WHERE key = 'rev' RETURNING value`,
	).Scan(&rev)
	if err != nil {
		return 0, wrapError(CodeInvalid, "", err)
	}
	return rev, nil
}

func (s *Store) lastSeen() int64 { return s.now().UnixMilli() }
