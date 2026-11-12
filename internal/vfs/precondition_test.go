package vfs

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
)

// staleRead returns the revision a caller would keep after reading path.
func staleRead(t *testing.T, s *Store, path string) int64 {
	t.Helper()
	st, err := s.Stat(context.Background(), path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.Rev
}

func wantConflict(t *testing.T, err error, base int64) *Error {
	t.Helper()
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	var fe *Error
	if !errors.As(err, &fe) {
		t.Fatalf("want *Error, got %T", err)
	}
	if fe.BaseRev != base {
		t.Fatalf("conflict carries base %d, want %d", fe.BaseRev, base)
	}
	if fe.CurrentRev == base {
		t.Fatalf("conflict current %d equals base, retry would fail again", fe.CurrentRev)
	}
	return fe
}

func TestPreconditionsOnEveryMutation(t *testing.T) {
	ctx := context.Background()

	ops := map[string]func(s *Store, rev *int64) error{
		"WriteFile": func(s *Store, rev *int64) error {
			return s.WriteFile(ctx, "/w/f", []byte("mine"), &MutateOptions{BaseRev: rev})
		},
		"Truncate": func(s *Store, rev *int64) error {
			return s.Truncate(ctx, "/w/f", 1, &MutateOptions{BaseRev: rev})
		},
		"Remove": func(s *Store, rev *int64) error {
			return s.Remove(ctx, "/w/f", &MutateOptions{BaseRev: rev})
		},
		"Chmod": func(s *Store, rev *int64) error {
			return s.Chmod(ctx, "/w/f", 0o600, &MutateOptions{BaseRev: rev})
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			if err := s.MkdirAll(ctx, "/w"); err != nil {
				t.Fatalf("mkdirall: %v", err)
			}
			if err := s.WriteFile(ctx, "/w/f", []byte("one"), nil); err != nil {
				t.Fatalf("write: %v", err)
			}
			base := staleRead(t, s, "/w/f")
			// Someone else moves the file on between our read and our write.
			if err := s.WriteFile(ctx, "/w/f", []byte("two"), nil); err != nil {
				t.Fatalf("other write: %v", err)
			}

			stale := base
			fe := wantConflict(t, op(s, &stale), stale)
			fresh := fe.CurrentRev
			if err := op(s, &fresh); err != nil {
				t.Fatalf("retry with current rev %d: %v", fresh, err)
			}
		})
	}
}

func TestMkdirPreconditionNamesTheTarget(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	zero := int64(0)
	if err := s.Mkdir(ctx, "/d", &MutateOptions{BaseRev: &zero}); err != nil {
		t.Fatalf("create with base 0: %v", err)
	}
	// A second create-if-absent must report the dir's own rev, which is
	// only possible if the check names the target and not the parent.
	fe := wantConflict(t, s.Mkdir(ctx, "/d", &MutateOptions{BaseRev: &zero}), zero)
	if fe.Path != "/d" {
		t.Fatalf("conflict path %q, want /d", fe.Path)
	}

	// The parent's rev does not move when a child appears, so a
	// create-if-absent of a sibling still passes.
	if err := s.Mkdir(ctx, "/e", &MutateOptions{BaseRev: &zero}); err != nil {
		t.Fatalf("sibling create with base 0: %v", err)
	}

	// A matching revision does not make mkdir a no-op: the name is taken.
	current := fe.CurrentRev
	if err := s.Mkdir(ctx, "/d", &MutateOptions{BaseRev: &current}); !errors.Is(err, ErrExist) {
		t.Fatalf("mkdir with current rev = %v, want ErrExist", err)
	}
	if err := s.Mkdir(ctx, "/d", nil); !errors.Is(err, ErrExist) {
		t.Fatalf("mkdir on existing dir = %v, want ErrExist", err)
	}
	// MkdirAll is the "make sure it exists" form.
	if err := s.MkdirAll(ctx, "/d"); err != nil {
		t.Fatalf("mkdirall on existing dir: %v", err)
	}
}

func TestBaseRevZeroOnDeleteNeverDeletes(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.WriteFile(ctx, "/f", []byte("x"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	current := staleRead(t, s, "/f")
	zero := int64(0)
	wantConflict(t, s.Remove(ctx, "/f", &MutateOptions{BaseRev: &zero}), zero)
	if _, err := s.Stat(ctx, "/f"); err != nil {
		t.Fatalf("file gone after a zero-rev remove: %v", err)
	}
	rev := current
	if err := s.Remove(ctx, "/f", &MutateOptions{BaseRev: &rev}); err != nil {
		t.Fatalf("remove with current rev: %v", err)
	}
}

// TestReadFileIsOneSnapshot pairs the bytes with the revision of the same
// commit. A writer keeps the body equal to the revision that carries it;
// a reader that took bytes and metadata from two snapshots would see a
// mismatch.
func TestReadFileIsOneSnapshot(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.WriteFile(ctx, "/n", []byte("1"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	first := staleRead(t, s, "/n")

	const rounds = 200
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i <= rounds; i++ {
			if err := s.WriteFile(ctx, "/n", []byte(fmt.Sprintf("%d", first+int64(i))), nil); err != nil {
				t.Errorf("write %d: %v", i, err)
				return
			}
		}
	}()

	for i := 0; i < rounds; i++ {
		c, err := s.ReadFile(ctx, "/n")
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		body, err := strconv.ParseInt(string(c.Data), 10, 64)
		if err != nil {
			t.Fatalf("body %q: %v", c.Data, err)
		}
		if body != c.Stat.Rev {
			t.Fatalf("body says rev %d, metadata says %d", body, c.Stat.Rev)
		}
		if int64(len(c.Data)) != c.Stat.Size {
			t.Fatalf("size %d, body %d bytes", c.Stat.Size, len(c.Data))
		}
	}
	<-done
}

func TestConcurrentReadModifyWriteOneWinner(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.WriteFile(ctx, "/counter", []byte("0"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	c, err := s.ReadFile(ctx, "/counter")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	base := c.Stat.Rev

	const writers = 2
	var wg sync.WaitGroup
	results := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rev := base
			results <- s.WriteFile(ctx, "/counter",
				[]byte(fmt.Sprintf("%d", i+1)), &MutateOptions{BaseRev: &rev})
		}(i)
	}
	wg.Wait()
	close(results)

	var ok, conflicts int
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || conflicts != writers-1 {
		t.Fatalf("%d writes won, %d conflicted; want 1 and %d", ok, conflicts, writers-1)
	}
}

// TestMutationStructureFirst pins the shared check order: a path whose parent
// is a file is ENOTDIR for every mutation, before any version is compared.
func TestMutationStructureFirst(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.WriteFile(ctx, "/file", []byte("x"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	bad := "/file/child"
	zero := int64(0)

	cases := map[string]func() error{
		"WriteFile": func() error { return s.WriteFile(ctx, bad, []byte("y"), nil) },
		"Mkdir":     func() error { return s.Mkdir(ctx, bad, nil) },
		"Truncate":  func() error { return s.Truncate(ctx, bad, 0, nil) },
		"Remove":    func() error { return s.Remove(ctx, bad, nil) },
		"Rename":    func() error { return s.Rename(ctx, bad, "/moved", nil) },
		"WriteAt":   func() error { return s.WriteAt(ctx, bad, 0, []byte("y")) },
	}
	for name, op := range cases {
		if err := op(); !errors.Is(err, ErrNotDir) {
			t.Fatalf("%s = %v, want ErrNotDir", name, err)
		}
	}

	// A precondition never hides the structure error: the same call with a
	// stale revision still reports ENOTDIR.
	if err := s.WriteFile(ctx, bad, []byte("y"), &MutateOptions{BaseRev: &zero}); !errors.Is(err, ErrNotDir) {
		t.Fatalf("WriteFile with base 0 = %v, want ErrNotDir", err)
	}
}
