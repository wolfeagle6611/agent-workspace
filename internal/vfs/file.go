package vfs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// WriteOptions carries the optional precondition.
//
// BaseRev is the revision the caller read. A mismatch fails with
// ErrConflict and a *Error carrying CurrentRev. BaseRev == 0 means
// "I expect this path not to exist".
//
// exec writers do not carry BaseRev and always win: see ADR 0007.
type WriteOptions struct {
	BaseRev *int64
	Mode    uint32
}

// WriteFile creates or replaces a file.
func (s *Store) WriteFile(ctx context.Context, path string, data []byte, opts *WriteOptions) error {
	clean, err := Clean(path)
	if err != nil {
		return err
	}
	if clean == "/" {
		return newError(CodeIsDir, path)
	}

	mode := uint32(0o644)
	var want *int64
	if opts != nil {
		if opts.BaseRev != nil {
			v := *opts.BaseRev
			want = &v
		}
		if opts.Mode != 0 {
			mode = opts.Mode
		}
	}
	dir, base := parentOf(clean)
	now := s.lastSeen()
	size := int64(len(data))

	return s.withWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		parent, err := resolveInode(ctx, tx, dir)
		if err != nil {
			return notFoundPath(err, path)
		}
		if err := requireDir(ctx, tx, parent, dir); err != nil {
			return err
		}

		inode, currentRev, err := lookupEntry(ctx, tx, parent, base)
		if err != nil {
			return err
		}
		if inode != 0 {
			n, err := nodeRow(ctx, tx, inode)
			if err != nil {
				return err
			}
			if n.Type == TypeDir {
				return newError(CodeIsDir, path)
			}
		}
		if err := checkBaseRev(clean, want, currentRev); err != nil {
			return err
		}

		rev, err := mintRev(ctx, tx)
		if err != nil {
			return err
		}

		if inode == 0 {
			res, err := tx.ExecContext(ctx,
				`INSERT INTO vfs_nodes(type, mode, size, mtime, rev) VALUES ('file', ?, ?, ?, ?)`,
				mode, size, now, rev,
			)
			if err != nil {
				return wrapError(CodeInvalid, path, err)
			}
			inode, err = res.LastInsertId()
			if err != nil {
				return wrapError(CodeInvalid, path, err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO vfs_dirents(parent_inode, name, child_inode) VALUES (?, ?, ?)`,
				parent, base, inode,
			); err != nil {
				return wrapError(CodeInvalid, path, err)
			}
		} else {
			if _, err := tx.ExecContext(ctx, `DELETE FROM vfs_chunks WHERE inode = ?`, inode); err != nil {
				return wrapError(CodeInvalid, path, err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE vfs_nodes SET mode = ?, size = ?, mtime = ?, rev = ? WHERE inode = ?`,
				mode, size, now, rev, inode,
			); err != nil {
				return wrapError(CodeInvalid, path, err)
			}
		}

		return chunks.Put(ctx, tx, inode, data, now)
	})
}

// ReadFile returns the file's bytes.
func (s *Store) ReadFile(ctx context.Context, path string) ([]byte, error) {
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
	if st.Type == TypeDir {
		return nil, newError(CodeIsDir, path)
	}

	out, err := chunks.Bytes(ctx, s.db, inode)
	if err != nil {
		return nil, err
	}
	if int64(len(out)) != st.Size {
		return nil, &Error{
			Code: CodeInvalid, Path: path,
			Err: fmt.Errorf("size mismatch: node says %d, chunks hold %d", st.Size, len(out)),
		}
	}
	return out, nil
}

// Mkdir creates one directory. It is idempotent when the target already
// exists as a directory (go-mapping §6, Q34).
func (s *Store) Mkdir(ctx context.Context, path string) error {
	clean, err := Clean(path)
	if err != nil {
		return err
	}
	if clean == "/" {
		return nil
	}
	now := s.lastSeen()
	return s.withWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return mkdirInTx(ctx, tx, clean, now)
	})
}

// MkdirAll creates every missing ancestor in one transaction.
func (s *Store) MkdirAll(ctx context.Context, path string) error {
	clean, err := Clean(path)
	if err != nil {
		return err
	}
	if clean == "/" {
		return nil
	}
	now := s.lastSeen()
	return s.withWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		inode := int64(rootInode)
		walked := ""
		for _, seg := range segments(clean) {
			walked += "/" + seg
			child, err := childInode(ctx, tx, inode, seg)
			if err != nil {
				var e *Error
				if !errors.As(err, &e) || e.Code != CodeNotFound {
					return notFoundPath(err, walked)
				}
				if child, err = createDir(ctx, tx, inode, seg, now); err != nil {
					return err
				}
				inode = child
				continue
			}
			if err := requireDir(ctx, tx, child, walked); err != nil {
				return err
			}
			inode = child
		}
		return nil
	})
}

// mkdirInTx creates the final segment, requiring the parent to exist.
func mkdirInTx(ctx context.Context, tx *sql.Tx, clean string, now int64) error {
	dir, base := parentOf(clean)
	parent, err := resolveInode(ctx, tx, dir)
	if err != nil {
		return notFoundPath(err, clean)
	}
	if err := requireDir(ctx, tx, parent, dir); err != nil {
		return err
	}
	child, err := childInode(ctx, tx, parent, base)
	if err != nil {
		var e *Error
		if !errors.As(err, &e) || e.Code != CodeNotFound {
			return err
		}
		_, err = createDir(ctx, tx, parent, base, now)
		return err
	}
	return requireDir(ctx, tx, child, clean)
}

func createDir(ctx context.Context, tx *sql.Tx, parent int64, name string, now int64) (int64, error) {
	return createNode(ctx, tx, parent, name, TypeDir, 0o755, now)
}

// lookupEntry returns the child inode and its current revision. inode == 0
// with a nil error means "absent". currentRev is nil when the entry is absent.
func lookupEntry(ctx context.Context, q querier, parent int64, name string) (int64, *int64, error) {
	child, err := childInode(ctx, q, parent, name)
	if err != nil {
		var e *Error
		if errors.As(err, &e) && e.Code == CodeNotFound {
			return 0, nil, nil
		}
		return 0, nil, err
	}
	n, err := nodeRow(ctx, q, child)
	if err != nil {
		return 0, nil, err
	}
	rev := n.Rev
	return child, &rev, nil
}
