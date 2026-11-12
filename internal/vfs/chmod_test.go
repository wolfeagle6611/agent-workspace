package vfs

import (
	"context"
	"errors"
	"testing"
)

// cursorNow drains everything logged so far and returns the cursor it covers.
func cursorNow(t *testing.T, s *Store) Cursor {
	t.Helper()
	_, next := drain(t, s, Cursor{}, 256)
	return next
}

// changesSince returns every change logged after cursor.
func changesSince(t *testing.T, s *Store, cursor Cursor) []Change {
	t.Helper()
	all, _ := drain(t, s, cursor, 256)
	return all
}

func TestChmodFileSetsModeAndLogs(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.MkdirAll(ctx, "/w"); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := s.WriteFile(ctx, "/w/f", []byte("#!/bin/sh\n"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	before := staleRead(t, s, "/w/f")
	cursor := cursorNow(t, s)

	if err := s.Chmod(ctx, "/w/f", 0o755, nil); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	st, err := s.Stat(ctx, "/w/f")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode != 0o755 {
		t.Fatalf("mode = %o, want 755", st.Mode)
	}
	if st.Rev <= before {
		t.Fatalf("rev %d did not advance past %d", st.Rev, before)
	}

	changes := changesSince(t, s, cursor)
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(changes))
	}
	c := changes[0]
	if c.Path != "/w/f" || c.Kind != KindFile || c.Mode != 0o755 {
		t.Fatalf("change = %+v, want file /w/f mode 755", c)
	}
	if c.Rev != st.Rev {
		t.Fatalf("change rev %d, node rev %d", c.Rev, st.Rev)
	}
}

func TestChmodSameModeIsNoOp(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.WriteFile(ctx, "/f", []byte("x"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	before := staleRead(t, s, "/f")
	cursor := cursorNow(t, s)
	if err := s.Chmod(ctx, "/f", 0o644, nil); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if after := staleRead(t, s, "/f"); after != before {
		t.Fatalf("rev moved from %d to %d on a no-op chmod", before, after)
	}
	if changes := changesSince(t, s, cursor); len(changes) != 0 {
		t.Fatalf("no-op chmod logged %d changes", len(changes))
	}
}

func TestChmodDirectoryMintsRevWithoutChange(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.Mkdir(ctx, "/d", &MutateOptions{Mode: 0o700}); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	before := staleRead(t, s, "/d")
	cursor := cursorNow(t, s)

	if err := s.Chmod(ctx, "/d", 0o750, nil); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	st, err := s.Stat(ctx, "/d")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode != 0o750 {
		t.Fatalf("mode = %o, want 750", st.Mode)
	}
	if st.Rev <= before {
		t.Fatalf("directory rev %d did not advance past %d", st.Rev, before)
	}
	if changes := changesSince(t, s, cursor); len(changes) != 0 {
		t.Fatalf("directory chmod logged %d changes, want 0", len(changes))
	}
}

func TestChmodRootAndMissing(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.Chmod(ctx, "/", 0o700, nil); err != nil {
		t.Fatalf("chmod root: %v", err)
	}
	st, err := s.Stat(ctx, "/")
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}
	if st.Mode != 0o700 {
		t.Fatalf("root mode = %o, want 700", st.Mode)
	}

	err = s.Chmod(ctx, "/nope", 0o755, nil)
	if err == nil {
		t.Fatal("chmod on a missing path succeeded")
	}
	var fe *Error
	if !errors.As(err, &fe) || fe.Code != CodeNotFound {
		t.Fatalf("code = %v, want ENOENT", err)
	}
}
