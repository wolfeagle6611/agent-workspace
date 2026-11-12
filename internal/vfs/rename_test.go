package vfs

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestRenameMovesAndMintsOneRev(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.MkdirAll(ctx, "/a"); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := s.WriteFile(ctx, "/a/f", []byte("body"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	before, _ := s.metaRev(ctx)
	oldRev := staleRead(t, s, "/a/f")

	if err := s.Rename(ctx, "/a/f", "/a/g", nil); err != nil {
		t.Fatalf("rename: %v", err)
	}

	after, _ := s.metaRev(ctx)
	if after != before+1 {
		t.Fatalf("rev moved by %d, want 1", after-before)
	}
	if _, err := s.Stat(ctx, "/a/f"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("source still there: %v", err)
	}
	c, err := s.ReadFile(ctx, "/a/g")
	if err != nil {
		t.Fatalf("read moved file: %v", err)
	}
	if !bytes.Equal(c.Data, []byte("body")) {
		t.Fatalf("content %q", c.Data)
	}
	// The moved node carries the rename rev: a reader that took the old one
	// must fail loudly rather than write over a path it no longer owns.
	if c.Stat.Rev != after {
		t.Fatalf("moved node rev %d, want the rename rev %d", c.Stat.Rev, after)
	}
	stale := oldRev
	if err := s.WriteFile(ctx, "/a/g", []byte("x"), &MutateOptions{BaseRev: &stale}); !errors.Is(err, ErrConflict) {
		t.Fatalf("write with pre-rename rev = %v, want ErrConflict", err)
	}
}

func TestRenameOntoExistingFile(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.WriteFile(ctx, "/src", []byte("src"), nil); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := s.WriteFile(ctx, "/dst", []byte("dst"), nil); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	// Without a target precondition the replace is refused.
	err := s.Rename(ctx, "/src", "/dst", nil)
	if !errors.Is(err, ErrExist) {
		t.Fatalf("rename without TargetBaseRev = %v, want ErrExist", err)
	}
	var ee *Error
	if !errors.As(err, &ee) || ee.Path != "/src" || ee.TargetPath != "/dst" {
		t.Fatalf("EEXIST carries path %q target %q, want /src and /dst", ee.Path, ee.TargetPath)
	}

	// A stale one names the target and its current revision.
	dstRev := staleRead(t, s, "/dst")
	if err := s.WriteFile(ctx, "/src", []byte("src2"), nil); err != nil {
		t.Fatalf("rewrite src: %v", err)
	}
	if err := s.WriteFile(ctx, "/dst", []byte("dst2"), nil); err != nil {
		t.Fatalf("rewrite dst: %v", err)
	}
	stale := dstRev
	err = s.Rename(ctx, "/src", "/dst", &MutateOptions{TargetBaseRev: &stale})
	fe := wantConflict(t, err, stale)
	if fe.Path != "/dst" {
		t.Fatalf("target conflict path %q, want /dst", fe.Path)
	}

	// The current one replaces the target.
	fresh := fe.CurrentRev
	if err := s.Rename(ctx, "/src", "/dst", &MutateOptions{TargetBaseRev: &fresh}); err != nil {
		t.Fatalf("rename with current target rev: %v", err)
	}
	c, err := s.ReadFile(ctx, "/dst")
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(c.Data) != "src2" {
		t.Fatalf("dst holds %q, want src2", c.Data)
	}
	if _, err := s.Stat(ctx, "/src"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("source still there: %v", err)
	}
}

func TestRenameSourcePrecondition(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.WriteFile(ctx, "/f", []byte("one"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	stale := staleRead(t, s, "/f")
	if err := s.WriteFile(ctx, "/f", []byte("two"), nil); err != nil {
		t.Fatalf("other write: %v", err)
	}
	err := s.Rename(ctx, "/f", "/g", &MutateOptions{BaseRev: &stale})
	if fe := wantConflict(t, err, stale); fe.Path != "/f" {
		t.Fatalf("source conflict path %q, want /f", fe.Path)
	}
	if _, err := s.Stat(ctx, "/g"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed rename left a target: %v", err)
	}
}

func TestRenameDirectories(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.MkdirAll(ctx, "/from/inner"); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := s.WriteFile(ctx, "/from/inner/f", []byte("x"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Mkdir(ctx, "/to", nil); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// A directory onto an empty directory is allowed.
	toRev := staleRead(t, s, "/to")
	if err := s.Rename(ctx, "/from", "/to", &MutateOptions{TargetBaseRev: &toRev}); err != nil {
		t.Fatalf("rename dir onto empty dir: %v", err)
	}
	c, err := s.ReadFile(ctx, "/to/inner/f")
	if err != nil {
		t.Fatalf("read moved child: %v", err)
	}
	if string(c.Data) != "x" {
		t.Fatalf("child holds %q", c.Data)
	}

	// A non-empty directory as target is refused.
	if err := s.MkdirAll(ctx, "/other"); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	busyRev := staleRead(t, s, "/to")
	err = s.Rename(ctx, "/other", "/to", &MutateOptions{TargetBaseRev: &busyRev})
	if !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("rename onto non-empty dir = %v, want ErrNotEmpty", err)
	}
	if _, err := s.Stat(ctx, "/other"); err != nil {
		t.Fatalf("source gone after a refused rename: %v", err)
	}
}

func TestRenameRejectsWrongShapes(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.MkdirAll(ctx, "/d/sub"); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := s.WriteFile(ctx, "/d/f", []byte("x"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Mkdir(ctx, "/empty", nil); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := s.WriteFile(ctx, "/plain", []byte("x"), nil); err != nil {
		t.Fatalf("write plain: %v", err)
	}

	cases := []struct {
		name   string
		src    string
		dst    string
		target bool // pass TargetBaseRev
		want   error
	}{
		{"root source", "/", "/x", false, ErrPerm},
		{"into own subtree", "/d", "/d/sub", false, ErrInvalid},
		{"file onto dir", "/d/f", "/empty", true, ErrIsDir},
		{"missing source", "/nope", "/x", false, ErrNotFound},
		{"missing target parent", "/d/f", "/nope/f", false, ErrNotFound},
		{"target parent is a file", "/d/f", "/plain/x", false, ErrNotDir},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var opts *MutateOptions
			if c.target {
				rev := int64(0)
				if st, err := s.Stat(ctx, c.dst); err == nil {
					rev = st.Rev
				}
				opts = &MutateOptions{TargetBaseRev: &rev}
			}
			err := s.Rename(ctx, c.src, c.dst, opts)
			if !errors.Is(err, c.want) {
				t.Fatalf("rename(%q,%q) = %v, want %v", c.src, c.dst, err, c.want)
			}
		})
	}

	// A directory source onto a file target names the type mismatch.
	rev := staleRead(t, s, "/plain")
	err := s.Rename(ctx, "/d/sub", "/plain", &MutateOptions{TargetBaseRev: &rev})
	if !errors.Is(err, ErrNotDir) {
		t.Fatalf("dir onto file = %v, want ErrNotDir", err)
	}
}

func TestRenameToItselfIsNoOp(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.WriteFile(ctx, "/f", []byte("x"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	before, _ := s.metaRev(ctx)
	rev := staleRead(t, s, "/f")

	if err := s.Rename(ctx, "/f", "/f", &MutateOptions{BaseRev: &rev}); err != nil {
		t.Fatalf("rename onto itself: %v", err)
	}
	after, _ := s.metaRev(ctx)
	if after != before {
		t.Fatalf("no-op rename moved rev by %d", after-before)
	}
	if _, err := s.ReadFile(ctx, "/f"); err != nil {
		t.Fatalf("file gone: %v", err)
	}

	// The precondition still applies: a stale self-rename is a conflict.
	if err := s.WriteFile(ctx, "/f", []byte("y"), nil); err != nil {
		t.Fatalf("other write: %v", err)
	}
	if err := s.Rename(ctx, "/f", "/f", &MutateOptions{BaseRev: &rev}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale self-rename = %v, want ErrConflict", err)
	}
}
