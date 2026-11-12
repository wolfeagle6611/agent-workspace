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

// Stat is the metadata view of one node: what a stat() reports, and what
// the internal lookups carry around. It has no path, because one node can be
// reached through any path a dirent names.
type Stat struct {
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
	return nodeRow(ctx, s.db, inode)
}

// lookupNode returns the node one directory entry names. A nil node with a
// nil error means the entry is absent.
func lookupNode(ctx context.Context, q querier, parent int64, name string) (*Stat, error) {
	child, err := childInode(ctx, q, parent, name)
	if err != nil {
		var e *Error
		if errors.As(err, &e) && e.Code == CodeNotFound {
			return nil, nil
		}
		return nil, err
	}
	return nodeRow(ctx, q, child)
}

// requireNode is lookupNode for callers that cannot work on an absent path.
func requireNode(ctx context.Context, q querier, parent int64, name, path string) (*Stat, error) {
	node, err := lookupNode(ctx, q, parent, name)
	if err != nil {
		return nil, err
	}
	if node == nil {
		return nil, newError(CodeNotFound, path)
	}
	return node, nil
}

// dirEmpty reports whether a directory has no children.
func dirEmpty(ctx context.Context, q querier, inode int64) (bool, error) {
	var children int
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM vfs_dirents WHERE parent_inode = ?`, inode,
	).Scan(&children)
	if err != nil {
		return false, wrapError(CodeInvalid, "", err)
	}
	return children == 0, nil
}

// checkBaseRev implements the precondition in one place for every mutation.
//
//   - want == nil: no precondition
//   - node == nil: the path is absent, so only want == 0 matches
//   - node != nil: want must equal the node's revision
func checkBaseRev(path string, want *int64, node *Stat) error {
	if want == nil {
		return nil
	}
	var current int64
	if node != nil {
		current = node.Rev
	}
	if *want == current {
		return nil
	}
	return &Error{Code: CodeConflict, Path: path, BaseRev: *want, CurrentRev: current}
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
