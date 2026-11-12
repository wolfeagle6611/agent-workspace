package vfs

import (
	"context"
	"database/sql"
)

// WriteAt writes data at an offset, creating the file when off is zero.
//
// FUSE turns every write() into one of these. Only the chunks the range
// touches are rewritten; the rest keep their content and their rev.
func (s *Store) WriteAt(ctx context.Context, path string, off int64, data []byte) error {
	clean, err := Clean(path)
	if err != nil {
		return err
	}
	if off < 0 {
		return newError(CodeInvalid, path)
	}
	if clean == "/" {
		return newError(CodeIsDir, path)
	}
	dir, base := parentOf(clean)
	now := s.lastSeen()

	return s.withWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		parent, err := resolveInode(ctx, tx, dir)
		if err != nil {
			return notFoundPath(err, path)
		}
		if err := requireDir(ctx, tx, parent, dir); err != nil {
			return err
		}

		inode, _, err := lookupEntry(ctx, tx, parent, base)
		if err != nil {
			return err
		}

		var size int64
		if inode == 0 {
			if off != 0 {
				return newError(CodeNotFound, path)
			}
			inode, err = createNode(ctx, tx, parent, base, TypeFile, 0o644, now)
			if err != nil {
				return err
			}
		} else {
			n, err := nodeRow(ctx, tx, inode)
			if err != nil {
				return err
			}
			if n.Type == TypeDir {
				return newError(CodeIsDir, path)
			}
			size = n.Size
		}

		rev, err := mintRev(ctx, tx)
		if err != nil {
			return err
		}
		newSize, err := chunks.ReplaceRange(ctx, tx, inode, off, data, size, now)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE vfs_nodes SET size = ?, mtime = ?, rev = ? WHERE inode = ?`,
			newSize, now, rev, inode,
		)
		if err != nil {
			return wrapError(CodeInvalid, path, err)
		}
		return nil
	})
}

// Truncate sets a file's length. Growing zero-fills; shrinking drops the tail.
// This one rewrites the whole chunk list: truncation is rare, and the simple
// path is easier to keep correct than a partial-chunk special case.
func (s *Store) Truncate(ctx context.Context, path string, size int64) error {
	clean, err := Clean(path)
	if err != nil {
		return err
	}
	if size < 0 {
		return newError(CodeInvalid, path)
	}
	if clean == "/" {
		return newError(CodeIsDir, path)
	}
	dir, base := parentOf(clean)
	now := s.lastSeen()

	return s.withWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		parent, err := resolveInode(ctx, tx, dir)
		if err != nil {
			return notFoundPath(err, path)
		}
		inode, err := childInode(ctx, tx, parent, base)
		if err != nil {
			return notFoundPath(err, path)
		}
		n, err := nodeRow(ctx, tx, inode)
		if err != nil {
			return err
		}
		if n.Type == TypeDir {
			return newError(CodeIsDir, path)
		}

		body, err := chunks.Bytes(ctx, tx, inode)
		if err != nil {
			return err
		}
		if int64(len(body)) > size {
			body = body[:size]
		} else if int64(len(body)) < size {
			grown := make([]byte, size)
			copy(grown, body)
			body = grown
		}

		rev, err := mintRev(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM vfs_chunks WHERE inode = ?`, inode); err != nil {
			return wrapError(CodeInvalid, path, err)
		}
		if err := chunks.Put(ctx, tx, inode, body, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE vfs_nodes SET size = ?, mtime = ?, rev = ? WHERE inode = ?`,
			int64(len(body)), now, rev, inode,
		); err != nil {
			return wrapError(CodeInvalid, path, err)
		}
		return nil
	})
}

// Remove deletes a file or an empty directory.
func (s *Store) Remove(ctx context.Context, path string) error {
	clean, err := Clean(path)
	if err != nil {
		return err
	}
	if clean == "/" {
		return newError(CodePerm, path)
	}
	dir, base := parentOf(clean)

	return s.withWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		parent, err := resolveInode(ctx, tx, dir)
		if err != nil {
			return notFoundPath(err, path)
		}
		inode, err := childInode(ctx, tx, parent, base)
		if err != nil {
			return notFoundPath(err, path)
		}
		n, err := nodeRow(ctx, tx, inode)
		if err != nil {
			return err
		}
		if n.Type == TypeDir {
			var children int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM vfs_dirents WHERE parent_inode = ?`, inode,
			).Scan(&children); err != nil {
				return wrapError(CodeInvalid, path, err)
			}
			if children > 0 {
				return newError(CodeNotEmpty, path)
			}
		}

		if _, err := mintRev(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM vfs_dirents WHERE parent_inode = ? AND name = ?`, parent, base,
		); err != nil {
			return wrapError(CodeInvalid, path, err)
		}
		// vfs_chunks rows go with the node through ON DELETE CASCADE.
		if _, err := tx.ExecContext(ctx, `DELETE FROM vfs_nodes WHERE inode = ?`, inode); err != nil {
			return wrapError(CodeInvalid, path, err)
		}
		return nil
	})
}

// createNode inserts a node and its directory entry.
func createNode(ctx context.Context, tx *sql.Tx, parent int64, name string, typ NodeType, mode uint32, now int64) (int64, error) {
	rev, err := mintRev(ctx, tx)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO vfs_nodes(type, mode, size, mtime, rev) VALUES (?, ?, 0, ?, ?)`,
		string(typ), mode, now, rev,
	)
	if err != nil {
		return 0, wrapError(CodeInvalid, name, err)
	}
	inode, err := res.LastInsertId()
	if err != nil {
		return 0, wrapError(CodeInvalid, name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO vfs_dirents(parent_inode, name, child_inode) VALUES (?, ?, ?)`,
		parent, name, inode,
	); err != nil {
		return 0, wrapError(CodeInvalid, name, err)
	}
	return inode, nil
}
