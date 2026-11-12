package vfs

import (
	"context"
	"database/sql"
)

// ChangeKind is the kind of one change entry. Same vocabulary as the wire
// form in docs/architecture/go-mapping.md §3.3.
type ChangeKind string

const (
	KindFile   ChangeKind = "file"
	KindDir    ChangeKind = "dir"
	KindDelete ChangeKind = "delete"
)

// Change is one entry of the change log, and one entry on the wire.
//
// Rev names the commit that produced it. Chunks is empty for anything but a
// file, and nil on a delete. Mode and MTime make an applied change
// comparable: the receiver skips a change whose target already matches, so a
// push/pull round trip terminates instead of echoing forever (ADR 0008).
type Change struct {
	Rev    int64
	Path   string
	Kind   ChangeKind
	Mode   uint32
	MTime  int64
	Chunks []ChunkRef
}

// Cursor is a position in a change stream: a rev, and a path inside that rev.
// An empty Path means the rev is complete. It never holds a nil path the way
// the upstream cursor does, because SQLite's primary keys admit NULL and an
// empty string does not (go-mapping §6).
type Cursor struct {
	Rev  int64
	Path string
}

// After reports whether c is past the other cursor.
func (c Cursor) After(o Cursor) bool {
	if c.Rev != o.Rev {
		return c.Rev > o.Rev
	}
	return c.Path > o.Path
}

// logChange appends one row to the log inside the caller's transaction, so
// the log and the data it describes commit together.
func logChange(ctx context.Context, tx *sql.Tx, rev int64, path string, kind ChangeKind, mode uint32, mtime int64) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO vfs_changes(rev, path, kind, mode, mtime) VALUES (?, ?, ?, ?, ?)`,
		rev, path, string(kind), int64(mode), mtime,
	); err != nil {
		return wrapError(CodeInvalid, path, err)
	}
	return nil
}

// subtreePaths calls fn for the node at base and every path under it, in
// name order.
//
// Rename moves a whole subtree with one dirent update, so the paths that
// disappear and the paths that appear have to be enumerated. ADR 0003 accepts
// this cost: a directory rename is billed by subtree size.
func subtreePaths(ctx context.Context, q querier, inode int64, base string, fn func(path string, st *Stat) error) error {
	st, err := nodeRow(ctx, q, inode)
	if err != nil {
		return err
	}
	if err := fn(base, st); err != nil {
		return err
	}
	if st.Type != TypeDir {
		return nil
	}

	rows, err := q.QueryContext(ctx,
		`SELECT name, child_inode FROM vfs_dirents WHERE parent_inode = ? ORDER BY name`,
		inode,
	)
	if err != nil {
		return wrapError(CodeInvalid, base, err)
	}
	type child struct {
		name  string
		inode int64
	}
	var children []child
	for rows.Next() {
		var c child
		if err := rows.Scan(&c.name, &c.inode); err != nil {
			rows.Close()
			return wrapError(CodeInvalid, base, err)
		}
		children = append(children, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return wrapError(CodeInvalid, base, err)
	}

	for _, c := range children {
		p := joinPath(base, c.name)
		if err := subtreePaths(ctx, q, c.inode, p, fn); err != nil {
			return err
		}
	}
	return nil
}

// joinPath appends one segment to a cleaned absolute path.
func joinPath(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}
