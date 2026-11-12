package vfs

// schemaVersion is stored in vfs_meta and bumped by migrations.
//
// 2 adds the change log and the sync bookkeeping (ADR 0008). The DDL is
// idempotent, so an older database gains the new tables on Open.
const schemaVersion = 2

// rootInode is always 1 and always a directory. Invariant from
// docs/architecture/computer.md §4.3.
const rootInode = 1

const schemaDDL = `
CREATE TABLE IF NOT EXISTS vfs_meta (
	key   TEXT PRIMARY KEY,
	value INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS vfs_nodes (
	inode  INTEGER PRIMARY KEY AUTOINCREMENT,
	type   TEXT    NOT NULL CHECK (type IN ('file', 'dir')),
	mode   INTEGER NOT NULL,
	size   INTEGER NOT NULL DEFAULT 0,
	mtime  INTEGER NOT NULL,
	rev    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS vfs_dirents (
	parent_inode INTEGER NOT NULL REFERENCES vfs_nodes(inode) ON DELETE CASCADE,
	name         TEXT    NOT NULL,
	child_inode  INTEGER NOT NULL REFERENCES vfs_nodes(inode) ON DELETE CASCADE,
	PRIMARY KEY (parent_inode, name)
);

-- Content-addressed chunks. Bytes are stored once per hash, so identical
-- content at two paths costs one row pair (I5).
CREATE TABLE IF NOT EXISTS vfs_blobs (
	hash      BLOB    PRIMARY KEY,
	size      INTEGER NOT NULL,
	last_seen INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS vfs_blob_bytes (
	hash  BLOB PRIMARY KEY REFERENCES vfs_blobs(hash) ON DELETE CASCADE,
	bytes BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS vfs_chunks (
	inode INTEGER NOT NULL REFERENCES vfs_nodes(inode) ON DELETE CASCADE,
	idx   INTEGER NOT NULL,
	hash  BLOB    NOT NULL REFERENCES vfs_blobs(hash),
	size  INTEGER NOT NULL,
	PRIMARY KEY (inode, idx)
);

CREATE INDEX IF NOT EXISTS vfs_dirents_child ON vfs_dirents(child_inode);
CREATE INDEX IF NOT EXISTS vfs_chunks_hash   ON vfs_chunks(hash);

-- Append-only log of committed changes. One row per (rev, path): a single
-- mutation names each path once. Deletes write a tombstone here, because a
-- removed dirent leaves no trace the peer could compare against.
-- Chunk refs are not stored; coalesce reads the live node instead.
CREATE TABLE IF NOT EXISTS vfs_changes (
	rev   INTEGER NOT NULL,
	path  TEXT    NOT NULL,
	kind  TEXT    NOT NULL CHECK (kind IN ('file', 'dir', 'delete')),
	mode  INTEGER NOT NULL,
	mtime INTEGER NOT NULL,
	PRIMARY KEY (rev, path)
);

CREATE INDEX IF NOT EXISTS vfs_changes_path ON vfs_changes(path);

-- Sync bookkeeping for one peer pair. One row, both roles: the host uses
-- push_rev / fetch_rev / fetch_path, the mirror uses applied_rev /
-- applied_path (ADR 0008). Both record their own generation and the
-- generation they last saw from the peer.
CREATE TABLE IF NOT EXISTS vfs_sync (
	id              TEXT PRIMARY KEY,
	push_rev        INTEGER NOT NULL DEFAULT 0,
	fetch_rev       INTEGER NOT NULL DEFAULT 0,
	fetch_path      TEXT    NOT NULL DEFAULT '',
	applied_rev     INTEGER NOT NULL DEFAULT 0,
	applied_path    TEXT    NOT NULL DEFAULT '',
	generation      TEXT    NOT NULL DEFAULT '',
	peer_generation TEXT    NOT NULL DEFAULT ''
);
`
