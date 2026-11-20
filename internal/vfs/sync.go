package vfs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SyncState is the sync bookkeeping of one store.
//
// The host reads PushRev / Fetch / PeerGeneration; the mirror reads Applied /
// Generation. One row holds both halves (ADR 0008).
type SyncState struct {
	PushRev        int64  // the last rev this side successfully pushed
	Fetch          Cursor // what this side has pulled from the peer
	Applied        Cursor // what the peer has pushed here, as applied
	Generation     string // this store's identity, minted once
	PeerGeneration string // the peer generation seen on the last handshake
}

// CurrentRev returns the workspace revision counter.
func (s *Store) CurrentRev(ctx context.Context) (int64, error) {
	return s.metaRev(ctx)
}

// newGeneration mints the identity of one store. A wiped mirror gets a new
// one, which is how the host learns the rev space restarted (ADR 0008).
func newGeneration() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint generation: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// SyncState reads the sync row.
func (s *Store) SyncState(ctx context.Context) (SyncState, error) {
	var (
		st                     SyncState
		fetchRev, appliedRev   int64
		fetchPath, appliedPath string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT push_rev, fetch_rev, fetch_path, applied_rev, applied_path,
		        generation, peer_generation
		   FROM vfs_sync WHERE id = ''`,
	).Scan(&st.PushRev, &fetchRev, &fetchPath, &appliedRev, &appliedPath,
		&st.Generation, &st.PeerGeneration)
	if err != nil {
		return st, wrapError(CodeInvalid, "", err)
	}
	st.Fetch = Cursor{Rev: fetchRev, Path: fetchPath}
	st.Applied = Cursor{Rev: appliedRev, Path: appliedPath}
	return st, nil
}

// SetPushRev records the last rev this side pushed.
func (s *Store) SetPushRev(ctx context.Context, rev int64) error {
	return s.execSync(ctx, `UPDATE vfs_sync SET push_rev = ? WHERE id = ''`, rev)
}

// SetFetchCursor records how far this side has pulled.
func (s *Store) SetFetchCursor(ctx context.Context, c Cursor) error {
	return s.execSync(ctx, `UPDATE vfs_sync SET fetch_rev = ?, fetch_path = ? WHERE id = ''`, c.Rev, c.Path)
}

// SetPeerGeneration records the generation the peer reported.
func (s *Store) SetPeerGeneration(ctx context.Context, gen string) error {
	return s.execSync(ctx, `UPDATE vfs_sync SET peer_generation = ? WHERE id = ''`, gen)
}

// ResetSync zeroes both watermarks and keeps both generations. The harness
// calls it when a generation mismatch says the rev space restarted.
func (s *Store) ResetSync(ctx context.Context) error {
	return s.execSync(ctx,
		`UPDATE vfs_sync
		    SET push_rev = 0, fetch_rev = 0, fetch_path = '',
		        applied_rev = 0, applied_path = ''
		  WHERE id = ''`)
}

// execSync runs one small sync write on the writer goroutine, so watermarks
// obey the same single-writer rule as every other mutation.
func (s *Store) execSync(ctx context.Context, query string, args ...any) error {
	return s.withWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return wrapError(CodeInvalid, "", err)
		}
		return nil
	})
}

// Changes reads up to limit log rows after the cursor, considering only revs
// up to bound, and returns the changes plus the cursor those rows cover.
//
// Coalescing is per path: a row is skipped when a later row for the same path
// is also in range. The walk still visits every row, so the cursor keeps
// moving and no other path's change is skipped. Entries are ordered by
// (rev, path); a path whose current state is gone reports a delete.
func (s *Store) Changes(ctx context.Context, after Cursor, bound int64, limit int) ([]Change, Cursor, error) {
	if limit <= 0 {
		return nil, after, newError(CodeInvalid, "")
	}
	if bound <= 0 {
		return nil, after, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.rev, c.path FROM vfs_changes c
		  WHERE c.rev <= ? AND (c.rev > ? OR (c.rev = ? AND c.path > ?))
		    AND NOT EXISTS (
		          SELECT 1 FROM vfs_changes d
		           WHERE d.path = c.path AND d.rev > c.rev AND d.rev <= ?)
		  ORDER BY c.rev, c.path
		  LIMIT ?`,
		bound, after.Rev, after.Rev, after.Path, bound, limit,
	)
	if err != nil {
		return nil, after, wrapError(CodeInvalid, "", err)
	}
	defer rows.Close()

	type row struct {
		rev  int64
		path string
	}
	var raw []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.rev, &r.path); err != nil {
			return nil, after, wrapError(CodeInvalid, "", err)
		}
		raw = append(raw, r)
	}
	if err := rows.Err(); err != nil {
		return nil, after, wrapError(CodeInvalid, "", err)
	}

	// Skipped rows shorten the page, so the caller cannot tell a short page
	// from the end. Report the cursor of the last row examined instead.
	out := make([]Change, 0, len(raw))
	for _, r := range raw {
		ch, err := s.materialize(ctx, r.rev, r.path)
		if err != nil {
			return nil, after, err
		}
		out = append(out, ch)
	}
	next := after
	if len(raw) > 0 {
		next = Cursor{Rev: raw[len(raw)-1].rev, Path: raw[len(raw)-1].path}
	}
	return out, next, nil
}

// materialize turns one log row into the path's current state.
//
// The log holds no chunk refs and no history, so the state is read now. That
// is what makes coalesce cheap and what makes the cursor a resume point
// rather than a snapshot (computer.md §5.2).
func (s *Store) materialize(ctx context.Context, rev int64, path string) (Change, error) {
	inode, err := resolveInode(ctx, s.db, path)
	if err != nil {
		if missing(err) {
			return Change{Rev: rev, Path: path, Kind: KindDelete}, nil
		}
		return Change{}, err
	}
	st, err := nodeRow(ctx, s.db, inode)
	if err != nil {
		return Change{}, err
	}
	if st.Type == TypeDir {
		return Change{Rev: rev, Path: path, Kind: KindDir, Mode: st.Mode, MTime: st.MTime}, nil
	}
	refs, err := chunks.Chunks(ctx, s.db, inode)
	if err != nil {
		return Change{}, err
	}
	return Change{Rev: rev, Path: path, Kind: KindFile, Mode: st.Mode, MTime: st.MTime, Chunks: refs}, nil
}

// missing reports the codes that mean "this path does not resolve".
func missing(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Code == CodeNotFound || e.Code == CodeNotDir)
}

// Apply writes a batch of changes in one transaction, together with the
// applied cursor it now covers.
//
// The batch mints at most one rev. A change whose target already matches
// writes nothing, so a push echoed back as a pull mints no rev and the
// round trip terminates. MTime is the sender's, not this clock's; that is
// what makes the comparison work.
//
// Apply is the third unguarded writer: FUSE, exec, and pull all pass no
// baseRev (ADR 0007, ADR 0008).
func (s *Store) Apply(ctx context.Context, changes []Change, upTo Cursor) (rev int64, changed int, err error) {
	err = s.withWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := s.lastSeen()
		writes := make([]Change, 0, len(changes))
		for _, ch := range changes {
			need, err := differs(ctx, tx, ch)
			if err != nil {
				return err
			}
			if need {
				writes = append(writes, ch)
			}
		}
		if len(writes) > 0 {
			r, err := mintRev(ctx, tx)
			if err != nil {
				return err
			}
			rev = r
			for _, ch := range writes {
				if err := applyOne(ctx, tx, r, ch, now); err != nil {
					return err
				}
				changed++
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE vfs_sync SET applied_rev = ?, applied_path = ? WHERE id = ''`,
			upTo.Rev, upTo.Path,
		); err != nil {
			return wrapError(CodeInvalid, "", err)
		}
		return nil
	})
	return rev, changed, err
}

// differs reports whether applying ch would change anything.
func differs(ctx context.Context, tx *sql.Tx, ch Change) (bool, error) {
	clean, err := Clean(ch.Path)
	if err != nil {
		return false, err
	}
	if clean == "/" {
		return false, nil
	}
	inode, err := resolveInode(ctx, tx, clean)
	if err != nil {
		if missing(err) {
			return ch.Kind != KindDelete, nil
		}
		return false, err
	}
	st, err := nodeRow(ctx, tx, inode)
	if err != nil {
		return false, err
	}
	switch ch.Kind {
	case KindDelete:
		return true, nil
	case KindDir:
		return st.Type != TypeDir || st.Mode != ch.Mode || st.MTime != ch.MTime, nil
	case KindFile:
		if st.Type != TypeFile || st.Mode != ch.Mode || st.MTime != ch.MTime {
			return true, nil
		}
		refs, err := chunks.Chunks(ctx, tx, inode)
		if err != nil {
			return false, err
		}
		return !sameChunks(refs, ch.Chunks), nil
	}
	return false, newError(CodeInvalid, ch.Path)
}

// sameChunks compares two chunk lists.
func sameChunks(a, b []ChunkRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Hash != b[i].Hash || a[i].Size != b[i].Size {
			return false
		}
	}
	return true
}

func applyOne(ctx context.Context, tx *sql.Tx, rev int64, ch Change, now int64) error {
	clean, err := Clean(ch.Path)
	if err != nil {
		return err
	}
	if clean == "/" {
		return nil
	}
	switch ch.Kind {
	case KindDelete:
		return applyDelete(ctx, tx, rev, clean)
	case KindDir:
		return applyDir(ctx, tx, rev, clean, ch, now)
	case KindFile:
		return applyFile(ctx, tx, rev, clean, ch, now)
	}
	return newError(CodeInvalid, ch.Path)
}

// applyDelete removes a path and, on a type conflict, its local subtree.
func applyDelete(ctx context.Context, tx *sql.Tx, rev int64, clean string) error {
	dir, base := parentOf(clean)
	parent, err := resolveInode(ctx, tx, dir)
	if err != nil {
		if missing(err) {
			return nil
		}
		return err
	}
	node, err := lookupNode(ctx, tx, parent, base)
	if err != nil {
		return err
	}
	if node == nil {
		return nil
	}
	if err := removeSubtree(ctx, tx, parent, base, node.Inode); err != nil {
		return err
	}
	return logChange(ctx, tx, rev, clean, KindDelete, node.Mode, node.MTime)
}

// applyDir materialises one directory entry.
func applyDir(ctx context.Context, tx *sql.Tx, rev int64, clean string, ch Change, now int64) error {
	dir, base := parentOf(clean)
	if err := ensureDir(ctx, tx, dir, rev, now); err != nil {
		return err
	}
	parent, err := resolveInode(ctx, tx, dir)
	if err != nil {
		return err
	}
	node, err := lookupNode(ctx, tx, parent, base)
	if err != nil {
		return err
	}
	if node != nil && node.Type == TypeDir {
		if _, err := tx.ExecContext(ctx,
			`UPDATE vfs_nodes SET mode = ?, mtime = ?, rev = ? WHERE inode = ?`,
			ch.Mode, ch.MTime, rev, node.Inode,
		); err != nil {
			return wrapError(CodeInvalid, clean, err)
		}
		return logChange(ctx, tx, rev, clean, KindDir, ch.Mode, ch.MTime)
	}
	if node != nil {
		if err := removeSubtree(ctx, tx, parent, base, node.Inode); err != nil {
			return err
		}
	}
	if _, err := insertNode(ctx, tx, parent, base, TypeDir, ch.Mode, ch.MTime, rev); err != nil {
		return err
	}
	return logChange(ctx, tx, rev, clean, KindDir, ch.Mode, ch.MTime)
}

// applyFile materialises one file from blobs that must already be staged.
func applyFile(ctx context.Context, tx *sql.Tx, rev int64, clean string, ch Change, now int64) error {
	dir, base := parentOf(clean)
	if err := ensureDir(ctx, tx, dir, rev, now); err != nil {
		return err
	}
	parent, err := resolveInode(ctx, tx, dir)
	if err != nil {
		return err
	}
	node, err := lookupNode(ctx, tx, parent, base)
	if err != nil {
		return err
	}

	var inode int64
	switch {
	case node == nil:
		if inode, err = insertNode(ctx, tx, parent, base, TypeFile, ch.Mode, ch.MTime, rev); err != nil {
			return err
		}
	case node.Type == TypeDir:
		if err := removeSubtree(ctx, tx, parent, base, node.Inode); err != nil {
			return err
		}
		if inode, err = insertNode(ctx, tx, parent, base, TypeFile, ch.Mode, ch.MTime, rev); err != nil {
			return err
		}
	default:
		inode = node.Inode
		if _, err := tx.ExecContext(ctx, `DELETE FROM vfs_chunks WHERE inode = ?`, inode); err != nil {
			return wrapError(CodeInvalid, clean, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE vfs_nodes SET mode = ?, size = ?, mtime = ?, rev = ? WHERE inode = ?`,
			ch.Mode, chunkBytes(ch.Chunks), ch.MTime, rev, inode,
		); err != nil {
			return wrapError(CodeInvalid, clean, err)
		}
	}
	if err := chunks.Link(ctx, tx, inode, ch.Chunks, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE vfs_nodes SET size = ? WHERE inode = ?`, chunkBytes(ch.Chunks), inode,
	); err != nil {
		return wrapError(CodeInvalid, clean, err)
	}
	return logChange(ctx, tx, rev, clean, KindFile, ch.Mode, ch.MTime)
}

// chunkBytes is the size a file's chunk list implies.
func chunkBytes(refs []ChunkRef) int64 {
	var n int64
	for _, r := range refs {
		n += r.Size
	}
	return n
}

// ensureDir creates the missing ancestors of dir.
//
// Apply creates directories implicitly because a directory creation writes no
// change entry (go-mapping §6): the peer's files bring their parents along.
// Implicit directories log nothing, or the next pull would echo them back.
func ensureDir(ctx context.Context, tx *sql.Tx, dir string, rev, now int64) error {
	if dir == "/" {
		return nil
	}
	walked := ""
	inode := int64(rootInode)
	for _, seg := range segments(dir) {
		walked = joinPath(walked, seg)
		if walked == "" {
			walked = "/" + seg
		}
		child, err := childInode(ctx, tx, inode, seg)
		if err != nil {
			if !missing(err) {
				return err
			}
			if child, err = insertNode(ctx, tx, inode, seg, TypeDir, 0o755, now, rev); err != nil {
				return err
			}
			inode = child
			continue
		}
		st, err := nodeRow(ctx, tx, child)
		if err != nil {
			return err
		}
		if st.Type != TypeDir {
			// Type conflict: the peer says directory, this side says file.
			if err := removeSubtree(ctx, tx, inode, seg, child); err != nil {
				return err
			}
			if child, err = insertNode(ctx, tx, inode, seg, TypeDir, 0o755, now, rev); err != nil {
				return err
			}
		}
		inode = child
	}
	return nil
}

// removeSubtree deletes a node and every node under it. Deleting a node
// cascades its own dirents and chunks, but not the children those dirents
// name, so they go depth first.
func removeSubtree(ctx context.Context, tx *sql.Tx, parent int64, base string, inode int64) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT name, child_inode FROM vfs_dirents WHERE parent_inode = ?`, inode,
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
		if err := removeSubtree(ctx, tx, inode, c.name, c.inode); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM vfs_dirents WHERE parent_inode = ? AND name = ?`, parent, base,
	); err != nil {
		return wrapError(CodeInvalid, base, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM vfs_nodes WHERE inode = ?`, inode); err != nil {
		return wrapError(CodeInvalid, base, err)
	}
	return nil
}

// Blob is one content-addressed object in flight.
type Blob struct {
	Hash [sha256.Size]byte
	Data []byte
}

// StageBlobs stores bytes that no file references yet.
//
// Push sends objects after the receiver says which hashes it lacks, so the
// bytes land before the change that uses them. A crash in between leaves an
// unreferenced blob; the GC safety window (1 hour, matching ADR 0008) covers
// it. Callers pass the window to GC; nothing in this package assumes a
// shorter one.
func (s *Store) StageBlobs(ctx context.Context, objects []Blob) error {
	return s.withWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := s.lastSeen()
		for _, o := range objects {
			sum := sha256.Sum256(o.Data)
			if sum != o.Hash {
				return &Error{Code: CodeInvalid, Err: fmt.Errorf("blob hash mismatch: sent %x, got %x", o.Hash, sum)}
			}
			if err := stageBlob(ctx, tx, o.Hash, o.Data, now); err != nil {
				return err
			}
		}
		return nil
	})
}

func stageBlob(ctx context.Context, tx *sql.Tx, hash [sha256.Size]byte, data []byte, now int64) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO vfs_blobs(hash, size, last_seen) VALUES (?, ?, ?)
		 ON CONFLICT(hash) DO UPDATE SET last_seen = excluded.last_seen`,
		hash[:], int64(len(data)), now,
	); err != nil {
		return wrapError(CodeInvalid, "", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO vfs_blob_bytes(hash, bytes) VALUES (?, ?)`,
		hash[:], data,
	); err != nil {
		return wrapError(CodeInvalid, "", err)
	}
	return nil
}

// MissingBlobs returns the hashes the store does not hold. This is the probe
// that keeps a push at two round trips regardless of file count.
func (s *Store) MissingBlobs(ctx context.Context, hashes [][sha256.Size]byte) ([][sha256.Size]byte, error) {
	if len(hashes) == 0 {
		return nil, nil
	}
	have, err := s.heldHashes(ctx, hashes)
	if err != nil {
		return nil, err
	}
	var out [][sha256.Size]byte
	for _, h := range hashes {
		if !have[h] {
			out = append(out, h)
		}
	}
	return out, nil
}

// heldHashes answers which of the given hashes exist.
func (s *Store) heldHashes(ctx context.Context, hashes [][sha256.Size]byte) (map[[sha256.Size]byte]bool, error) {
	args := make([]any, 0, len(hashes))
	for _, h := range hashes {
		args = append(args, h[:])
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT hash FROM vfs_blob_bytes WHERE hash IN (`+placeholders(len(hashes))+`)`,
		args...,
	)
	if err != nil {
		return nil, wrapError(CodeInvalid, "", err)
	}
	defer rows.Close()

	have := make(map[[sha256.Size]byte]bool, len(hashes))
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, wrapError(CodeInvalid, "", err)
		}
		var h [sha256.Size]byte
		copy(h[:], raw)
		have[h] = true
	}
	if err := rows.Err(); err != nil {
		return nil, wrapError(CodeInvalid, "", err)
	}
	return have, nil
}

// Blobs returns the bytes for the hashes the store holds. Absent hashes are
// skipped, so the caller compares counts.
func (s *Store) Blobs(ctx context.Context, hashes [][sha256.Size]byte) ([]Blob, error) {
	if len(hashes) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(hashes))
	for _, h := range hashes {
		args = append(args, h[:])
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT hash, bytes FROM vfs_blob_bytes WHERE hash IN (`+placeholders(len(hashes))+`)`,
		args...,
	)
	if err != nil {
		return nil, wrapError(CodeInvalid, "", err)
	}
	defer rows.Close()

	var out []Blob
	for rows.Next() {
		var (
			raw  []byte
			data []byte
		)
		if err := rows.Scan(&raw, &data); err != nil {
			return nil, wrapError(CodeInvalid, "", err)
		}
		var b Blob
		copy(b.Hash[:], raw)
		b.Data = data
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapError(CodeInvalid, "", err)
	}
	return out, nil
}

// placeholders builds "?, ?, ?" for an IN clause.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// GCResult counts what one collection removed.
type GCResult struct {
	Blobs   int
	Changes int
}

// GC removes blobs that nothing references and that have been idle longer
// than window, and change-log rows at or below pruneBelow.
//
// GC mints no rev: it is not a change, and a rev it moved would push every
// cursor past rows nobody has seen. pruneBelow is the peer's confirmed
// progress in *this* store's rev space, so each side passes its own.
func (s *Store) GC(ctx context.Context, now time.Time, window time.Duration, pruneBelow int64) (GCResult, error) {
	var res GCResult
	err := s.withWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		cutoff := now.Add(-window).UnixMilli()
		out, err := tx.ExecContext(ctx,
			`DELETE FROM vfs_blobs
			  WHERE last_seen < ?
			    AND hash NOT IN (SELECT hash FROM vfs_chunks)`,
			cutoff,
		)
		if err != nil {
			return wrapError(CodeInvalid, "", err)
		}
		if n, err := out.RowsAffected(); err == nil {
			res.Blobs = int(n)
		}
		if pruneBelow <= 0 {
			return nil
		}
		out, err = tx.ExecContext(ctx, `DELETE FROM vfs_changes WHERE rev <= ?`, pruneBelow)
		if err != nil {
			return wrapError(CodeInvalid, "", err)
		}
		if n, err := out.RowsAffected(); err == nil {
			res.Changes = int(n)
		}
		return nil
	})
	return res, err
}
