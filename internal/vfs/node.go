package vfs

import (
	"context"
	"database/sql"
	"errors"
)

// NodeType is the kind of a VFS object. Only files and directories exist
// in step 1.
type NodeType string

const (
	TypeFile NodeType = "file"
	TypeDir  NodeType = "dir"
)

// Stat is the metadata view of one path.
type Stat struct {
	Path  string
	Inode int64
	Type  NodeType
	Mode  uint32
	Size  int64
	MTime int64 // epoch milliseconds
	Rev   int64
}

// querier lets the resolution and chunk helpers run against the pool or a
// transaction. Both *sql.DB and *sql.Tx satisfy it.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// resolveInode walks a cleaned path from the root.
func resolveInode(ctx context.Context, q querier, clean string) (int64, error) {
	inode := int64(rootInode)
	for _, seg := range segments(clean) {
		child, err := childInode(ctx, q, inode, seg)
		if err != nil {
			return 0, err
		}
		inode = child
	}
	return inode, nil
}

// childInode looks up one directory entry.
func childInode(ctx context.Context, q querier, parent int64, name string) (int64, error) {
	var child int64
	err := q.QueryRowContext(ctx,
		`SELECT child_inode FROM vfs_dirents WHERE parent_inode = ? AND name = ?`,
		parent, name,
	).Scan(&child)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, newError(CodeNotFound, name)
	}
	if err != nil {
		return 0, wrapError(CodeInvalid, name, err)
	}
	return child, nil
}

// nodeRow reads one node.
func nodeRow(ctx context.Context, q querier, inode int64) (*Stat, error) {
	var (
		typ   string
		mode  int64
		size  int64
		mtime int64
		rev   int64
	)
	err := q.QueryRowContext(ctx,
		`SELECT type, mode, size, mtime, rev FROM vfs_nodes WHERE inode = ?`, inode,
	).Scan(&typ, &mode, &size, &mtime, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, newError(CodeNotFound, "")
	}
	if err != nil {
		return nil, wrapError(CodeInvalid, "", err)
	}
	return &Stat{
		Inode: inode,
		Type:  NodeType(typ),
		Mode:  uint32(mode),
		Size:  size,
		MTime: mtime,
		Rev:   rev,
	}, nil
}

// requireDir rejects a non-directory where a directory must be.
func requireDir(ctx context.Context, q querier, inode int64, path string) error {
	n, err := nodeRow(ctx, q, inode)
	if err != nil {
		return err
	}
	if n.Type != TypeDir {
		return newError(CodeNotDir, path)
	}
	return nil
}

// Stat resolves path and returns its metadata.
func (s *Store) Stat(ctx context.Context, path string) (*Stat, error) {
	clean, err := Clean(path)
	if err != nil {
		return nil, err
	}
	inode, err := resolveInode(ctx, s.db, clean)
	if err != nil {
		return nil, notFoundPath(err, path)
	}
	st, err := nodeRow(ctx, s.db, inode)
	if err != nil {
		return nil, err
	}
	st.Path = clean
	return st, nil
}

// checkBaseRev implements the precondition from
// docs/architecture/go-mapping.md §6 and Q34.
//
//   - want == nil: no precondition
//   - target absent, want == 0: caller expected absence, OK
//   - target absent, want != 0: conflict, current 0
//   - target present, want == rev: OK
//   - target present, want != rev: conflict
func checkBaseRev(path string, want *int64, current *int64) error {
	if want == nil {
		return nil
	}
	if current == nil {
		if *want == 0 {
			return nil
		}
		return &Error{Code: CodeConflict, Path: path, BaseRev: *want, CurrentRev: 0}
	}
	if *want == *current {
		return nil
	}
	return &Error{Code: CodeConflict, Path: path, BaseRev: *want, CurrentRev: *current}
}

// notFoundPath reattaches a full path to a not-found error raised while
// walking segments.
func notFoundPath(err error, path string) error {
	var e *Error
	if errors.As(err, &e) && e.Code == CodeNotFound {
		return &Error{Code: e.Code, Path: path, Err: e.Err}
	}
	return err
}
