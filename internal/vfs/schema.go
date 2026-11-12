package vfs

// schemaVersion is stored in vfs_meta and bumped by migrations.
const schemaVersion = 1

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
`
