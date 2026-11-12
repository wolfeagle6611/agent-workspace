package vfs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// MutateOptions carries the optional preconditions for a mutation.
//
// BaseRev is the revision the caller read. A mismatch fails with
// ErrConflict and a *Error carrying CurrentRev. BaseRev == 0 means
// "I expect this path not to exist".
//
// Only Rename reads TargetBaseRev: it is the revision of the target path,
// present when the caller expects the target to exist and wants to replace
// it. Only WriteFile reads Mode.
//
// exec and FUSE writers pass nil and always win: see ADR 0007 and
// go-mapping §6.
type MutateOptions struct {
	BaseRev       *int64
	TargetBaseRev *int64
	Mode          uint32
}

// baseRev copies the source precondition, so the write goroutine cannot race
// with the caller reusing the variable. A nil receiver means "no
// precondition", which is how exec and FUSE writers call in.
func (o *MutateOptions) baseRev() *int64 {
	if o == nil {
		return nil
	}
	return copyRev(o.BaseRev)
}

// targetRev copies the TargetBaseRev precondition.
func (o *MutateOptions) targetRev() *int64 {
	if o == nil {
		return nil
	}
	return copyRev(o.TargetBaseRev)
}

func copyRev(rev *int64) *int64 {
	if rev == nil {
		return nil
	}
	v := *rev
	return &v
}

// WriteFile creates or replaces a file.
func (s *Store) WriteFile(ctx context.Context, path string, data []byte, opts *MutateOptions) error {
	clean, err := Clean(path)
	if err != nil {
		return err
	}
	if clean == "/" {
		return newError(CodeIsDir, path)
	}

	// A caller that names no mode keeps the file's current one, or gets
	// 0644 for a new file. A replace must not silently reset permissions.
	var requested uint32
	if opts != nil {
		requested = opts.Mode
	}
	want := opts.baseRev()
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
		node, err := lookupNode(ctx, tx, parent, base)
		if err != nil {
			return err
		}
		if node != nil && node.Type == TypeDir {
			return newError(CodeIsDir, path)
		}
		if err := checkBaseRev(clean, want, node); err != nil {
			return err
		}
		mode := requested
		if mode == 0 {
			mode = 0o644
			if node != nil {
				mode = node.Mode
			}
		}

		rev, err := mintRev(ctx, tx)
		if err != nil {
			return err
		}

		var inode int64
		if node == nil {
			res, err := tx.ExecContext(ctx,
				`INSERT INTO vfs_nodes(type, mode, size, mtime, rev) VALUES ('file', ?, ?, ?, ?)`,
				mode, size, now, rev,
			)
			if err != nil {
				return wrapError(CodeInvalid, path, err)
			}
			if inode, err = res.LastInsertId(); err != nil {
				return wrapError(CodeInvalid, path, err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO vfs_dirents(parent_inode, name, child_inode) VALUES (?, ?, ?)`,
				parent, base, inode,
			); err != nil {
				return wrapError(CodeInvalid, path, err)
			}
		} else {
			inode = node.Inode
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

// Content is one file read: the bytes and the metadata from the same
// snapshot.
//
// Returning both together is what makes a read-modify-write cycle safe.
// A caller that takes the bytes cannot pair them with a newer revision, and
// a caller that takes the revision cannot pair it with older bytes; either
// order of two separate reads can drop a concurrent write silently.
type Content struct {
	Data []byte
	Stat Stat
}

// ReadFile returns the file's bytes and metadata as one snapshot.
func (s *Store) ReadFile(ctx context.Context, path string) (*Content, error) {
	clean, err := Clean(path)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, wrapError(CodeInvalid, path, err)
	}
	defer tx.Rollback()

	inode, err := resolveInode(ctx, tx, clean)
	if err != nil {
		return nil, notFoundPath(err, path)
	}
	st, err := nodeRow(ctx, tx, inode)
	if err != nil {
		return nil, err
	}
	if st.Type == TypeDir {
		return nil, newError(CodeIsDir, path)
	}

	out, err := chunks.Bytes(ctx, tx, inode)
	if err != nil {
		return nil, err
	}
	if int64(len(out)) != st.Size {
		return nil, &Error{
			Code: CodeInvalid, Path: path,
			Err: fmt.Errorf("size mismatch: node says %d, chunks hold %d", st.Size, len(out)),
		}
	}
	return &Content{Data: out, Stat: *st}, nil
}

// Mkdir creates one directory. An existing target is EEXIST, even when the
// caller's revision matched: creating is the only thing this method does.
// Callers that want "make sure it exists" use MkdirAll (go-mapping §6).
//
// BaseRev === 0 is the create-if-absent form: a target that appeared after
// the caller's read reports ECONFLICT instead of EEXIST.
func (s *Store) Mkdir(ctx context.Context, path string, opts *MutateOptions) error {
	clean, err := Clean(path)
	if err != nil {
		return err
	}
	if clean == "/" {
		return nil
	}
	want := opts.baseRev()
	mode := uint32(0o755)
	if opts != nil && opts.Mode != 0 {
		mode = opts.Mode
	}
	now := s.lastSeen()
	return s.withWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return mkdirInTx(ctx, tx, clean, mode, now, want)
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
				if child, err = createDir(ctx, tx, inode, seg, 0o755, now); err != nil {
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
//
// The precondition names the target node, not the parent: a parent's rev
// does not move when a child appears, so checking the parent would pass
// for every concurrent creator.
func mkdirInTx(ctx context.Context, tx *sql.Tx, clean string, mode uint32, now int64, want *int64) error {
	dir, base := parentOf(clean)
	parent, err := resolveInode(ctx, tx, dir)
	if err != nil {
		return notFoundPath(err, clean)
	}
	if err := requireDir(ctx, tx, parent, dir); err != nil {
		return err
	}
	node, err := lookupNode(ctx, tx, parent, base)
	if err != nil {
		return err
	}
	if node != nil {
		if err := checkBaseRev(clean, want, node); err != nil {
			return err
		}
		return newError(CodeExist, clean)
	}
	if err := checkBaseRev(clean, want, nil); err != nil {
		return err
	}
	_, err = createDir(ctx, tx, parent, base, mode, now)
	return err
}

func createDir(ctx context.Context, tx *sql.Tx, parent int64, name string, mode uint32, now int64) (int64, error) {
	return createNode(ctx, tx, parent, name, TypeDir, mode, now)
}
