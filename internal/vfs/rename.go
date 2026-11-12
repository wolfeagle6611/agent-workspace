package vfs

import (
	"context"
	"database/sql"
	"strings"
)

// Rename moves src to dst. It is the only mutation that can replace a target,
// so it is the only one with two preconditions: BaseRev names the source
// node, TargetBaseRev names the target node.
//
// Checks run in a fixed order, structure before version, so a caller can tell
// "this rename is malformed" from "re-read and retry":
//
//  1. path shape, root as source, target inside source → EINVAL / EPERM
//  2. both parents exist and are directories → ENOENT / ENOTDIR
//  3. type pairing → EISDIR / ENOTDIR
//  4. source BaseRev → ECONFLICT
//  5. target: absent is fine; present without TargetBaseRev → EEXIST;
//     present with a stale one → ECONFLICT; a directory target must be empty
//     → ENOTEMPTY
//
// A rename mints one rev and writes it to the moved node, so the old revision
// no longer names the new path and a caller holding it cannot overwrite a file
// it never read.
func (s *Store) Rename(ctx context.Context, src, dst string, opts *MutateOptions) error {
	cleanSrc, err := Clean(src)
	if err != nil {
		return err
	}
	cleanDst, err := Clean(dst)
	if err != nil {
		return err
	}
	if cleanSrc == "/" {
		return newError(CodePerm, src)
	}
	if strings.HasPrefix(cleanDst, cleanSrc+"/") {
		return newError(CodeInvalid, src)
	}

	srcDir, srcBase := parentOf(cleanSrc)
	dstDir, dstBase := parentOf(cleanDst)
	wantSrc := opts.baseRev()
	wantDst := opts.targetRev()

	return s.withWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		srcParent, err := resolveInode(ctx, tx, srcDir)
		if err != nil {
			return notFoundPath(err, src)
		}
		if err := requireDir(ctx, tx, srcParent, srcDir); err != nil {
			return err
		}
		dstParent, err := resolveInode(ctx, tx, dstDir)
		if err != nil {
			return notFoundPath(err, dst)
		}
		if err := requireDir(ctx, tx, dstParent, dstDir); err != nil {
			return err
		}

		srcNode, err := requireNode(ctx, tx, srcParent, srcBase, cleanSrc)
		if err != nil {
			return err
		}
		dstNode, err := lookupNode(ctx, tx, dstParent, dstBase)
		if err != nil {
			return err
		}

		// Renaming a path onto itself is a no-op, not a conflict. The
		// precondition still has to hold, or the caller's read was stale.
		if dstNode != nil && dstNode.Inode == srcNode.Inode {
			return checkBaseRev(cleanSrc, wantSrc, srcNode)
		}
		if dstNode != nil && dstNode.Type != srcNode.Type {
			if dstNode.Type == TypeDir {
				return newError(CodeIsDir, cleanDst)
			}
			return newError(CodeNotDir, cleanDst)
		}

		if err := checkBaseRev(cleanSrc, wantSrc, srcNode); err != nil {
			return err
		}
		if dstNode != nil {
			if wantDst == nil {
				return &Error{Code: CodeExist, Path: cleanSrc, TargetPath: cleanDst}
			}
			if err := checkBaseRev(cleanDst, wantDst, dstNode); err != nil {
				return err
			}
			if dstNode.Type == TypeDir {
				empty, err := dirEmpty(ctx, tx, dstNode.Inode)
				if err != nil {
					return err
				}
				if !empty {
					return newError(CodeNotEmpty, cleanDst)
				}
			}
		}

		rev, err := mintRev(ctx, tx)
		if err != nil {
			return err
		}

		// A rename mints one rev and bills it by subtree size (ADR 0003):
		// every path that disappears gets a tombstone, every path that
		// appears gets an entry, all at this rev.
		if err := subtreePaths(ctx, tx, srcNode.Inode, cleanSrc, func(p string, st *Stat) error {
			return logChange(ctx, tx, rev, p, KindDelete, st.Mode, st.MTime)
		}); err != nil {
			return err
		}

		if dstNode != nil {
			// The replaced node goes away; its chunks go with it through
			// ON DELETE CASCADE. It logs no tombstone: the entry below names
			// the same path in the same rev, and the receiver applies state.
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM vfs_dirents WHERE parent_inode = ? AND name = ?`, dstParent, dstBase,
			); err != nil {
				return wrapError(CodeInvalid, cleanDst, err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM vfs_nodes WHERE inode = ?`, dstNode.Inode); err != nil {
				return wrapError(CodeInvalid, cleanDst, err)
			}
		}
		// (parent_inode, name) is the primary key, so this moves exactly the
		// one entry the source path named.
		if _, err := tx.ExecContext(ctx,
			`UPDATE vfs_dirents SET parent_inode = ?, name = ? WHERE parent_inode = ? AND name = ?`,
			dstParent, dstBase, srcParent, srcBase,
		); err != nil {
			return wrapError(CodeInvalid, cleanDst, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE vfs_nodes SET rev = ? WHERE inode = ?`, rev, srcNode.Inode,
		); err != nil {
			return wrapError(CodeInvalid, cleanSrc, err)
		}

		return subtreePaths(ctx, tx, srcNode.Inode, cleanDst, func(p string, st *Stat) error {
			kind := KindFile
			if st.Type == TypeDir {
				kind = KindDir
			}
			return logChange(ctx, tx, rev, p, kind, st.Mode, st.MTime)
		})
	})
}
