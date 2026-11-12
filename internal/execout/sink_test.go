package execout

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// fakeStore records what the sink appends and can fail on purpose.
type fakeStore struct {
	mu        sync.Mutex
	created   []string
	appends   int
	data      []byte
	failAt    int   // append number that fails; 0 never
	failStore error // returned when failing
}

func (f *fakeStore) WriteAt(ctx context.Context, path string, off int64, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if off == 0 && len(data) == 0 {
		f.created = append(f.created, path)
		return nil
	}
	if f.failStore != nil && (f.failAt == 0 || f.appends+1 >= f.failAt) {
		return f.failStore
	}
	f.appends++
	if off != int64(len(f.data)) {
		return fmt.Errorf("offset %d, want %d", off, len(f.data))
	}
	f.data = append(f.data, data...)
	return nil
}

func TestSinkCreatesBothFiles(t *testing.T) {
	st := &fakeStore{}
	s := NewSink(context.Background(), st, DefaultLimits, "/o/a.stdout.log", "/o/a.stderr.log")
	res, err := s.Finalize(context.Background())
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if len(st.created) != 2 {
		t.Fatalf("created %v, want both files", st.created)
	}
	if res.Stdout.Path != "/o/a.stdout.log" || res.Stderr.Path != "/o/a.stderr.log" {
		t.Fatalf("paths %q / %q", res.Stdout.Path, res.Stderr.Path)
	}
	if res.Err != nil {
		t.Fatalf("err %v, want nil", res.Err)
	}
}

// TestSinkStreamsInChunks is the "100k lines must not grow memory" case: the
// full text leaves through appends, the tail stays bounded.
func TestSinkStreamsInChunks(t *testing.T) {
	st := &fakeStore{}
	s := NewSink(context.Background(), st, DefaultLimits, "/o/a.stdout.log", "/o/a.stderr.log")

	total := 0
	for i := 1; i <= 100_000; i++ {
		line := fmt.Sprintf("line-%d\n", i)
		if _, err := s.Stdout().Write([]byte(line)); err != nil {
			t.Fatalf("write: %v", err)
		}
		total += len(line)
	}
	res, err := s.Finalize(context.Background())
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}

	out := res.Stdout
	if out.Bytes != int64(total) || out.Lines != 100_000 {
		t.Fatalf("bytes %d lines %d, want %d and 100000", out.Bytes, out.Lines, total)
	}
	if len(out.Tail) > DefaultLimits.TailBytes || len(out.Tail) > len(st.data) {
		t.Fatalf("tail is %d bytes", len(out.Tail))
	}
	if !out.Truncated {
		t.Fatal("truncated false, want true")
	}
	if !strings.HasSuffix(string(out.Tail), "line-100000\n") {
		t.Fatalf("tail ends %q, want the last line", string(out.Tail[len(out.Tail)-20:]))
	}
	if st.appends < 2 {
		t.Fatalf("appends %d, want the text split into batches", st.appends)
	}
	if len(st.data) != total {
		t.Fatalf("stored %d bytes, want %d", len(st.data), total)
	}
	if !strings.HasPrefix(string(st.data), "line-1\n") {
		t.Fatalf("stored text starts %q, want line-1", string(st.data[:7]))
	}
}

func TestSinkShortOutputIsNotTruncated(t *testing.T) {
	st := &fakeStore{}
	s := NewSink(context.Background(), st, DefaultLimits, "/o/a.stdout.log", "/o/a.stderr.log")
	_, _ = s.Stdout().Write([]byte("hi"))
	res, _ := s.Finalize(context.Background())
	if res.Stdout.Truncated {
		t.Fatal("truncated true, want false")
	}
	if string(res.Stdout.Tail) != "hi" || string(st.data) != "hi" {
		t.Fatalf("tail %q stored %q, want hi twice", res.Stdout.Tail, st.data)
	}
}

// TestSinkRecordsAppendFailure: a broken log must not fail the command.
func TestSinkRecordsAppendFailure(t *testing.T) {
	st := &fakeStore{failStore: errors.New("disk full")}
	s := NewSink(context.Background(), st, DefaultLimits, "/o/a.stdout.log", "/o/a.stderr.log")

	n, err := s.Stdout().Write([]byte("hello\n"))
	if n != 6 || err != nil {
		t.Fatalf("write %d %v, want 6 nil", n, err)
	}
	res, _ := s.Finalize(context.Background())
	if !errors.Is(res.Err, st.failStore) {
		t.Fatalf("err %v, want the store error", res.Err)
	}
	if res.Stdout.Path != "/o/a.stdout.log" {
		t.Fatalf("path %q, want the created file", res.Stdout.Path)
	}
	if string(res.Stdout.Tail) != "hello\n" {
		t.Fatalf("tail %q, want the bytes kept in memory", res.Stdout.Tail)
	}
}

func TestSinkWithoutStoreKeepsTheTail(t *testing.T) {
	s := NewSink(context.Background(), nil, DefaultLimits, "", "")
	_, _ = s.Stdout().Write([]byte("only memory\n"))
	res, _ := s.Finalize(context.Background())
	if res.Err != nil {
		t.Fatalf("err %v, want nil", res.Err)
	}
	if string(res.Stdout.Tail) != "only memory\n" || res.Stdout.Path != "" {
		t.Fatalf("tail %q path %q", res.Stdout.Tail, res.Stdout.Path)
	}
}

// TestSinkHonoursSmallLimits is why Limits is a parameter: a test (or a
// future per-backend budget) can shrink every number at once.
func TestSinkHonoursSmallLimits(t *testing.T) {
	st := &fakeStore{}
	small := Limits{TailBytes: 16, TailLines: 3, FlushBytes: 8}
	s := NewSink(context.Background(), st, small, "/o/a.stdout.log", "/o/a.stderr.log")

	in := "aaaa\nbbbb\ncccc\ndddd\n"
	if _, err := s.Stdout().Write([]byte(in)); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, _ := s.Finalize(context.Background())
	if string(res.Stdout.Tail) != "bbbb\ncccc\ndddd\n" {
		t.Fatalf("tail %q, want three lines inside 16 bytes", res.Stdout.Tail)
	}
	if string(st.data) != in {
		t.Fatalf("stored %q, want the whole text", st.data)
	}
	// 20 bytes at 8 per batch is two full batches and one final flush.
	if st.appends != 3 {
		t.Fatalf("appends %d, want 3", st.appends)
	}
}
