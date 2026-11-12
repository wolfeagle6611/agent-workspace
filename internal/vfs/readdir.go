package vfs

import "context"

// Dirent is one directory entry.
type Dirent struct {
	Name  string
	Inode int64
	Type  NodeType
	Size  int64
	Mode  uint32
	MTime int64
	Rev   int64
}

// ReadDir lists a directory, ordered by name.
func (s *Store) ReadDir(ctx context.Context, path string) ([]Dirent, error) {
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
	if st.Type != TypeDir {
		return nil, newError(CodeNotDir, path)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT d.name, n.inode, n.type, n.size, n.mode, n.mtime, n.rev
		   FROM vfs_dirents d
		   JOIN vfs_nodes n ON n.inode = d.child_inode
		  WHERE d.parent_inode = ?
		  ORDER BY d.name`,
		inode,
	)
	if err != nil {
		return nil, wrapError(CodeInvalid, path, err)
	}
	defer rows.Close()

	var out []Dirent
	for rows.Next() {
		var (
			d    Dirent
			typ  string
			mode int64
		)
		if err := rows.Scan(&d.Name, &d.Inode, &typ, &d.Size, &mode, &d.MTime, &d.Rev); err != nil {
			return nil, wrapError(CodeInvalid, path, err)
		}
		d.Type = NodeType(typ)
		d.Mode = uint32(mode)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapError(CodeInvalid, path, err)
	}
	return out, nil
}

// ReadAt returns up to n bytes starting at off, reading only the chunks the
// range touches. FUSE turns every read() into one of these.
func (s *Store) ReadAt(ctx context.Context, path string, off int64, n int) ([]byte, error) {
	if off < 0 || n < 0 {
		return nil, newError(CodeInvalid, path)
	}
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
	if off >= st.Size || n == 0 {
		return nil, nil
	}
	if end := off + int64(n); end > st.Size {
		n = int(st.Size - off)
	}

	refs, err := chunks.Chunks(ctx, s.db, inode)
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, n)
	want := int64(n)
	for idx := off / ChunkSize; idx < int64(len(refs)) && want > 0; idx++ {
		start := idx * ChunkSize
		body, err := chunks.Object(ctx, s.db, refs[idx].Hash)
		if err != nil {
			return nil, err
		}
		from := int64(0)
		if off > start {
			from = off - start
		}
		if from >= int64(len(body)) {
			continue
		}
		piece := body[from:]
		if int64(len(piece)) > want {
			piece = piece[:want]
		}
		out = append(out, piece...)
		want -= int64(len(piece))
	}
	return out, nil
}
