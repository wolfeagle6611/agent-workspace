package vfs

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
)

// ChunkSize is the fixed chunk boundary. Upstream uses 512 KiB
// (docs/architecture/go-mapping.md §1).
const ChunkSize = 512 * 1024

// ChunkRef names one chunk of a file's content.
type ChunkRef struct {
	Hash [sha256.Size]byte
	Size int64
}

// ChunkCounts is a diagnostic view of the blob store. Tests use it to
// assert the dedupe invariant (I5) without reading tables directly.
type ChunkCounts struct {
	Blobs  int
	Chunks int
}

// chunkStore owns how bytes become blobs and chunks: the fixed-size
// split, the content hash, deduplication, and last_seen.
//
// The zero value is ready to use. Transactions belong to the caller:
// WriteFile must commit the chunk rows in the same transaction as the
// node row, so this module takes a *sql.Tx for writes and a querier for
// reads instead of opening its own.
//
// Reads are split on purpose. Bytes joins a file back together for the
// callers that want bytes today. Chunks exposes the refs, which is what
// a ranged read needs: a FUSE read at an offset resolves to a chunk,
// not to a whole file in memory.
type chunkStore struct{}

// chunks is the package's single value of the module.
var chunks chunkStore

// Put writes data as chunks, storing identical bytes once.
func (chunkStore) Put(ctx context.Context, tx *sql.Tx, inode int64, data []byte, now int64) error {
	for idx, off := 0, 0; off < len(data); idx, off = idx+1, off+ChunkSize {
		end := off + ChunkSize
		if end > len(data) {
			end = len(data)
		}
		chunk := data[off:end]
		sum := sha256.Sum256(chunk)

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO vfs_blobs(hash, size, last_seen) VALUES (?, ?, ?)
			 ON CONFLICT(hash) DO UPDATE SET last_seen = excluded.last_seen`,
			sum[:], int64(len(chunk)), now,
		); err != nil {
			return wrapError(CodeInvalid, "", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO vfs_blob_bytes(hash, bytes) VALUES (?, ?)`,
			sum[:], chunk,
		); err != nil {
			return wrapError(CodeInvalid, "", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO vfs_chunks(inode, idx, hash, size) VALUES (?, ?, ?, ?)`,
			inode, idx, sum[:], int64(len(chunk)),
		); err != nil {
			return wrapError(CodeInvalid, "", err)
		}
	}
	return nil
}

// Chunks returns the file's chunks in order. An empty file has none.
func (chunkStore) Chunks(ctx context.Context, q querier, inode int64) ([]ChunkRef, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT hash, size FROM vfs_chunks WHERE inode = ? ORDER BY idx`,
		inode,
	)
	if err != nil {
		return nil, wrapError(CodeInvalid, "", err)
	}
	defer rows.Close()

	var refs []ChunkRef
	for rows.Next() {
		var (
			hash []byte
			size int64
		)
		if err := rows.Scan(&hash, &size); err != nil {
			return nil, wrapError(CodeInvalid, "", err)
		}
		if len(hash) != sha256.Size {
			return nil, &Error{Code: CodeInvalid, Err: fmt.Errorf("chunk hash is %d bytes", len(hash))}
		}
		var ref ChunkRef
		copy(ref.Hash[:], hash)
		ref.Size = size
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapError(CodeInvalid, "", err)
	}
	return refs, nil
}

// Bytes joins a file's chunks back into one buffer.
func (chunkStore) Bytes(ctx context.Context, q querier, inode int64) ([]byte, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT b.bytes FROM vfs_chunks c
		   JOIN vfs_blob_bytes b ON b.hash = c.hash
		  WHERE c.inode = ?
		  ORDER BY c.idx`,
		inode,
	)
	if err != nil {
		return nil, wrapError(CodeInvalid, "", err)
	}
	defer rows.Close()

	var out []byte
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, wrapError(CodeInvalid, "", err)
		}
		out = append(out, b...)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapError(CodeInvalid, "", err)
	}
	return out, nil
}

// Counts reports how many blobs and chunk rows exist.
func (chunkStore) Counts(ctx context.Context, q querier) (ChunkCounts, error) {
	var c ChunkCounts
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM vfs_blobs`).Scan(&c.Blobs); err != nil {
		return c, wrapError(CodeInvalid, "", err)
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM vfs_chunks`).Scan(&c.Chunks); err != nil {
		return c, wrapError(CodeInvalid, "", err)
	}
	return c, nil
}

// Object returns one chunk's bytes.
func (chunkStore) Object(ctx context.Context, q querier, hash [sha256.Size]byte) ([]byte, error) {
	var b []byte
	err := q.QueryRowContext(ctx, `SELECT bytes FROM vfs_blob_bytes WHERE hash = ?`, hash[:]).Scan(&b)
	if err != nil {
		return nil, wrapError(CodeNotFound, "", err)
	}
	return b, nil
}

// ReplaceRange writes data at off without rewriting unaffected chunks. It
// returns the file's new size.
//
// FUSE delivers small writes at arbitrary offsets; rewriting the whole file
// for each one would be quadratic. Only the chunks the range touches are
// rehashed and replaced.
func (chunkStore) ReplaceRange(ctx context.Context, tx *sql.Tx, inode, off int64, data []byte, size, now int64) (int64, error) {
	if len(data) == 0 {
		return size, nil
	}
	newSize := size
	if end := off + int64(len(data)); end > newSize {
		newSize = end
	}

	first := off / ChunkSize
	last := (off + int64(len(data)) - 1) / ChunkSize

	for idx := first; idx <= last; idx++ {
		chunkStart := idx * ChunkSize
		// Desired length of this chunk after the write. Chunks before the
		// last one are full; the last one follows the new size.
		want := int64(ChunkSize)
		if remain := newSize - chunkStart; remain < want {
			want = remain
		}
		if want < 0 {
			want = 0
		}

		current, err := chunkAt(ctx, tx, inode, idx)
		if err != nil {
			return 0, err
		}
		buf := make([]byte, want)
		copy(buf, current)

		// Copy the part of data that lands in this chunk. data is cut, not
		// shifted: bytes before this chunk's start belong to an earlier one.
		rest := data
		dst := off - chunkStart
		if dst < 0 {
			rest = rest[-dst:]
			dst = 0
		}
		if dst < int64(len(buf)) {
			copy(buf[dst:], rest)
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM vfs_chunks WHERE inode = ? AND idx = ?`, inode, idx); err != nil {
			return 0, wrapError(CodeInvalid, "", err)
		}
		if len(buf) == 0 {
			continue
		}
		if err := chunks.putChunk(ctx, tx, inode, idx, buf, now); err != nil {
			return 0, err
		}
	}
	return newSize, nil
}

// chunkAt reads one chunk's current bytes, or nil when it does not exist.
func chunkAt(ctx context.Context, q querier, inode, idx int64) ([]byte, error) {
	var b []byte
	err := q.QueryRowContext(ctx,
		`SELECT b.bytes FROM vfs_chunks c
		   JOIN vfs_blob_bytes b ON b.hash = c.hash
		  WHERE c.inode = ? AND c.idx = ?`,
		inode, idx,
	).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapError(CodeInvalid, "", err)
	}
	return b, nil
}

// putChunk stores one chunk and its chunk row.
func (chunkStore) putChunk(ctx context.Context, tx *sql.Tx, inode, idx int64, buf []byte, now int64) error {
	sum := sha256.Sum256(buf)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO vfs_blobs(hash, size, last_seen) VALUES (?, ?, ?)
		 ON CONFLICT(hash) DO UPDATE SET last_seen = excluded.last_seen`,
		sum[:], int64(len(buf)), now,
	); err != nil {
		return wrapError(CodeInvalid, "", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO vfs_blob_bytes(hash, bytes) VALUES (?, ?)`,
		sum[:], buf,
	); err != nil {
		return wrapError(CodeInvalid, "", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO vfs_chunks(inode, idx, hash, size) VALUES (?, ?, ?, ?)`,
		inode, idx, sum[:], int64(len(buf)),
	); err != nil {
		return wrapError(CodeInvalid, "", err)
	}
	return nil
}

// Link points a file at chunks whose bytes are already in the blob store.
//
// Sync sends hashes first and bytes only for what the peer lacks, so an
// applied file finds its blobs staged already. A missing blob is an
// integrity error, not a slow path: it means the probe and the apply
// disagreed about the same hash.
func (chunkStore) Link(ctx context.Context, tx *sql.Tx, inode int64, refs []ChunkRef, now int64) error {
	for idx, ref := range refs {
		res, err := tx.ExecContext(ctx,
			`UPDATE vfs_blobs SET last_seen = ? WHERE hash = ?`, now, ref.Hash[:],
		)
		if err != nil {
			return wrapError(CodeInvalid, "", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return wrapError(CodeInvalid, "", err)
		}
		if n == 0 {
			return &Error{Code: CodeInvalid, Err: fmt.Errorf("chunk %x is not staged", ref.Hash)}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO vfs_chunks(inode, idx, hash, size) VALUES (?, ?, ?, ?)`,
			inode, int64(idx), ref.Hash[:], ref.Size,
		); err != nil {
			return wrapError(CodeInvalid, "", err)
		}
	}
	return nil
}
