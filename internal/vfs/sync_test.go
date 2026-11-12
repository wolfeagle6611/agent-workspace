package vfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"
	"time"
)

// drain reads every change from src after cursor, up to src's current rev.
func drain(t *testing.T, src *Store, cursor Cursor, limit int) ([]Change, Cursor) {
	t.Helper()
	ctx := context.Background()
	bound, err := src.CurrentRev(ctx)
	if err != nil {
		t.Fatalf("current rev: %v", err)
	}
	var all []Change
	for {
		page, next, err := src.Changes(ctx, cursor, bound, limit)
		if err != nil {
			t.Fatalf("changes: %v", err)
		}
		if next == cursor {
			return all, cursor
		}
		all = append(all, page...)
		cursor = next
	}
}

// pushOnce copies src's changes into dst the way the bracket does: read
// changes, probe the peer, send the missing bytes, apply.
func pushOnce(t *testing.T, src, dst *Store, limit int) int {
	t.Helper()
	ctx := context.Background()
	cursor, err := dst.SyncState(ctx)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	bound, err := src.CurrentRev(ctx)
	if err != nil {
		t.Fatalf("current rev: %v", err)
	}

	applied := 0
	at := cursor.Applied
	for {
		changes, next, err := src.Changes(ctx, at, bound, limit)
		if err != nil {
			t.Fatalf("changes: %v", err)
		}
		if next == at {
			return applied
		}
		if len(changes) > 0 {
			if err := sendObjects(t, src, dst, changes); err != nil {
				t.Fatalf("objects: %v", err)
			}
			_, changed, err := dst.Apply(ctx, changes, next)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			applied += changed
		}
		at = next
	}
}

// sendObjects stages every chunk the receiver lacks.
func sendObjects(t *testing.T, src, dst *Store, changes []Change) error {
	t.Helper()
	ctx := context.Background()
	var hashes [][32]byte
	for _, ch := range changes {
		for _, ref := range ch.Chunks {
			hashes = append(hashes, ref.Hash)
		}
	}
	if len(hashes) == 0 {
		return nil
	}
	missing, err := dst.MissingBlobs(ctx, hashes)
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		return nil
	}
	blobs, err := src.Blobs(ctx, missing)
	if err != nil {
		return err
	}
	return dst.StageBlobs(ctx, blobs)
}

func TestChangesCoalescePerPath(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	for i := 0; i < 5; i++ {
		if err := s.WriteFile(ctx, "/a.txt", []byte{'0' + byte(i)}, nil); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	got, _ := drain(t, s, Cursor{}, 256)
	if len(got) != 1 {
		t.Fatalf("want one coalesced change, got %d: %+v", len(got), got)
	}
	if got[0].Kind != KindFile || got[0].Path != "/a.txt" {
		t.Fatalf("unexpected change: %+v", got[0])
	}
	body, err := s.ReadFile(ctx, "/a.txt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(body.Data, []byte("4")) {
		t.Fatalf("coalesce reported stale content: %q", body.Data)
	}
}

func TestChangesPageByRevAndPath(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	for _, p := range []string{"/c", "/a", "/b"} {
		if err := s.WriteFile(ctx, p, []byte(p), nil); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	all, _ := drain(t, s, Cursor{}, 1)
	if len(all) != 3 {
		t.Fatalf("want 3 changes, got %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		prev, cur := Cursor{Rev: all[i-1].Rev, Path: all[i-1].Path}, Cursor{Rev: all[i].Rev, Path: all[i].Path}
		if !cur.After(prev) {
			t.Fatalf("changes out of cursor order: %+v then %+v", prev, cur)
		}
	}
}

func TestChangesReportDelete(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.WriteFile(ctx, "/gone", []byte("x"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Remove(ctx, "/gone", nil); err != nil {
		t.Fatalf("remove: %v", err)
	}

	got, _ := drain(t, s, Cursor{}, 256)
	if len(got) != 1 || got[0].Kind != KindDelete || got[0].Path != "/gone" {
		t.Fatalf("want one delete, got %+v", got)
	}
}

func TestMkdirAdvancesRevWithoutChange(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.Mkdir(ctx, "/empty", nil); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := s.MkdirAll(ctx, "/d"); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := s.WriteFile(ctx, "/d/f", []byte("x"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	rev, err := s.CurrentRev(ctx)
	if err != nil {
		t.Fatalf("current rev: %v", err)
	}
	got, _ := drain(t, s, Cursor{}, 256)
	if len(got) != 1 {
		t.Fatalf("want only the file change, got %+v", got)
	}
	if rev < 3 {
		t.Fatalf("mkdir did not advance rev: %d", rev)
	}
}

func TestRenameLogsOldAndNewPaths(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.MkdirAll(ctx, "/d"); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := s.WriteFile(ctx, "/d/x", []byte("x"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.WriteFile(ctx, "/d/y", []byte("y"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	base, _ := drain(t, s, Cursor{}, 256)
	cursor := Cursor{Rev: base[len(base)-1].Rev, Path: base[len(base)-1].Path}

	if err := s.Rename(ctx, "/d", "/e", nil); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got, _ := drain(t, s, cursor, 256)

	want := map[string]ChangeKind{
		"/d": KindDelete, "/d/x": KindDelete, "/d/y": KindDelete,
		"/e": KindDir, "/e/x": KindFile, "/e/y": KindFile,
	}
	if len(got) != len(want) {
		t.Fatalf("want %d changes, got %d: %+v", len(want), len(got), got)
	}
	rev := got[0].Rev
	for _, ch := range got {
		if ch.Rev != rev {
			t.Fatalf("rename minted more than one rev: %+v", got)
		}
		if want[ch.Path] != ch.Kind {
			t.Fatalf("%s: want %s, got %s", ch.Path, want[ch.Path], ch.Kind)
		}
	}
}

func TestApplyCopiesTreeAndTerminates(t *testing.T) {
	ctx := context.Background()
	src, dst := newStore(t), newStore(t)

	if err := src.MkdirAll(ctx, "/deep/dir"); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := src.WriteFile(ctx, "/deep/dir/a.txt", []byte("alpha"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := src.WriteFile(ctx, "/b.txt", []byte("beta"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	if n := pushOnce(t, src, dst, 256); n != 2 {
		t.Fatalf("want 2 writes, got %d", n)
	}
	for _, p := range []string{"/deep/dir/a.txt", "/b.txt"} {
		want, err := src.ReadFile(ctx, p)
		if err != nil {
			t.Fatalf("src read: %v", err)
		}
		got, err := dst.ReadFile(ctx, p)
		if err != nil {
			t.Fatalf("dst read %s: %v", p, err)
		}
		if !bytes.Equal(got.Data, want.Data) {
			t.Fatalf("%s: got %q, want %q", p, got.Data, want.Data)
		}
		if got.Stat.MTime != want.Stat.MTime {
			t.Fatalf("%s: mtime not carried: got %d, want %d", p, got.Stat.MTime, want.Stat.MTime)
		}
	}
	// The parents came along even though mkdir logs nothing.
	if _, err := dst.Stat(ctx, "/deep/dir"); err != nil {
		t.Fatalf("implicit parent missing: %v", err)
	}

	// A second push is the echo: identical state must write nothing, or the
	// two stores would mint revs forever.
	before, err := dst.CurrentRev(ctx)
	if err != nil {
		t.Fatalf("current rev: %v", err)
	}
	if n := pushOnce(t, src, dst, 256); n != 0 {
		t.Fatalf("echo wrote %d changes", n)
	}
	after, err := dst.CurrentRev(ctx)
	if err != nil {
		t.Fatalf("current rev: %v", err)
	}
	if after != before {
		t.Fatalf("echo minted a rev: %d -> %d", before, after)
	}
}

func TestApplyDeleteAndTypeConflict(t *testing.T) {
	ctx := context.Background()
	src, dst := newStore(t), newStore(t)

	// dst holds a file where src has a directory with a child.
	if err := dst.WriteFile(ctx, "/tree", []byte("local"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := src.MkdirAll(ctx, "/tree"); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := src.WriteFile(ctx, "/tree/leaf", []byte("peer"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if n := pushOnce(t, src, dst, 256); n != 1 {
		t.Fatalf("want 1 write, got %d", n)
	}
	if _, err := dst.ReadFile(ctx, "/tree/leaf"); err != nil {
		t.Fatalf("type conflict not resolved: %v", err)
	}

	// A delete of a directory takes the local subtree with it.
	if err := src.Remove(ctx, "/tree/leaf", nil); err != nil {
		t.Fatalf("remove leaf: %v", err)
	}
	if err := src.Remove(ctx, "/tree", nil); err != nil {
		t.Fatalf("remove tree: %v", err)
	}
	if n := pushOnce(t, src, dst, 256); n == 0 {
		t.Fatalf("deletes wrote nothing")
	}
	if _, err := dst.Stat(ctx, "/tree"); err == nil {
		t.Fatalf("deleted path still present")
	}
}

func TestStageBlobsProbeAndFetch(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	body := []byte("object bytes")
	sum := sha256Of(body)
	if missing, err := s.MissingBlobs(ctx, [][32]byte{sum}); err != nil || len(missing) != 1 {
		t.Fatalf("empty store: missing=%v err=%v", missing, err)
	}
	if err := s.StageBlobs(ctx, []Blob{{Hash: sum, Data: body}}); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if missing, err := s.MissingBlobs(ctx, [][32]byte{sum}); err != nil || len(missing) != 0 {
		t.Fatalf("staged store: missing=%v err=%v", missing, err)
	}
	blobs, err := s.Blobs(ctx, [][32]byte{sum})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(blobs) != 1 || !bytes.Equal(blobs[0].Data, body) {
		t.Fatalf("unexpected blobs: %+v", blobs)
	}
	// A hash that does not match its bytes is a protocol error, not a write.
	if err := s.StageBlobs(ctx, []Blob{{Hash: sum, Data: []byte("other")}}); err == nil {
		t.Fatalf("hash mismatch accepted")
	}
}

func TestApplyRejectsUnstagedChunk(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	body := []byte("never staged")
	refs := []ChunkRef{{Hash: sha256Of(body), Size: int64(len(body))}}
	_, _, err := s.Apply(ctx, []Change{{
		Rev: 1, Path: "/f", Kind: KindFile, Mode: 0o644, MTime: 1, Chunks: refs,
	}}, Cursor{Rev: 1})
	if err == nil {
		t.Fatalf("apply without bytes succeeded")
	}
}

func TestGCPrunesIdleBlobsAndLog(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.WriteFile(ctx, "/f", []byte("first"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	orphan := sha256Of([]byte("first"))
	if err := s.WriteFile(ctx, "/f", []byte("second"), nil); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	live := sha256Of([]byte("second"))
	rev, err := s.CurrentRev(ctx)
	if err != nil {
		t.Fatalf("current rev: %v", err)
	}

	// A window of zero still protects anything touched "now"; move the clock
	// forward instead of sleeping.
	res, err := s.GC(ctx, time.Now().Add(time.Hour), time.Minute, rev)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if res.Blobs != 1 {
		t.Fatalf("want 1 blob pruned, got %d", res.Blobs)
	}
	if res.Changes == 0 {
		t.Fatalf("log not pruned below %d", rev)
	}
	missing, err := s.MissingBlobs(ctx, [][32]byte{orphan, live})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(missing) != 1 || missing[0] != orphan {
		t.Fatalf("want only the orphan gone: %v", missing)
	}

	after, err := s.CurrentRev(ctx)
	if err != nil {
		t.Fatalf("current rev: %v", err)
	}
	if after != rev {
		t.Fatalf("gc minted a rev: %d -> %d", rev, after)
	}
}

func TestGCKeepsBlobsInsideWindow(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.StageBlobs(ctx, []Blob{{Hash: sha256Of([]byte("held")), Data: []byte("held")}}); err != nil {
		t.Fatalf("stage: %v", err)
	}
	// Bytes staged for a change that has not arrived yet must survive the
	// probe-and-send gap.
	res, err := s.GC(ctx, time.Now(), time.Minute, 0)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if res.Blobs != 0 {
		t.Fatalf("pruned %d fresh blobs", res.Blobs)
	}
}

func TestSyncStateGenerations(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	st, err := s.SyncState(ctx)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.Generation == "" {
		t.Fatalf("store has no generation")
	}
	if err := s.SetPushRev(ctx, 7); err != nil {
		t.Fatalf("push rev: %v", err)
	}
	if err := s.SetFetchCursor(ctx, Cursor{Rev: 9, Path: "/x"}); err != nil {
		t.Fatalf("fetch cursor: %v", err)
	}
	if err := s.SetPeerGeneration(ctx, "peer"); err != nil {
		t.Fatalf("peer generation: %v", err)
	}
	st, err = s.SyncState(ctx)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.PushRev != 7 || st.Fetch.Rev != 9 || st.Fetch.Path != "/x" || st.PeerGeneration != "peer" {
		t.Fatalf("unexpected state: %+v", st)
	}

	if err := s.ResetSync(ctx); err != nil {
		t.Fatalf("reset: %v", err)
	}
	st, err = s.SyncState(ctx)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.PushRev != 0 || st.Fetch != (Cursor{}) || st.Applied != (Cursor{}) {
		t.Fatalf("reset left watermarks: %+v", st)
	}
	if st.Generation == "" || st.PeerGeneration != "peer" {
		t.Fatalf("reset dropped generations: %+v", st)
	}
}

func TestGenerationSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := dir + "/ws.db"

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	first, err := s.SyncState(ctx)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	again, err := s.SyncState(ctx)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if again.Generation != first.Generation {
		t.Fatalf("generation changed across reopen: %q -> %q", first.Generation, again.Generation)
	}
}

func sha256Of(b []byte) [32]byte {
	return sha256.Sum256(b)
}
