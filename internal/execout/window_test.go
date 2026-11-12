package execout

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestWindowKeepsShortOutputWhole(t *testing.T) {
	w := newWindow(DefaultLimits)
	w.Write([]byte("hello\nworld\n"))
	got := w.snapshot()
	if string(got) != "hello\nworld\n" {
		t.Fatalf("tail %q, want the whole output", got)
	}
	if w.total != 12 || w.lines != 2 {
		t.Fatalf("total %d lines %d, want 12 and 2", w.total, w.lines)
	}
}

func TestWindowKeepsTheLastBytes(t *testing.T) {
	w := newWindow(DefaultLimits)
	chunk := bytes.Repeat([]byte("a"), 1000)
	for i := 0; i < 200; i++ {
		w.Write(chunk)
	}
	got := w.snapshot()
	if len(got) != DefaultLimits.TailBytes {
		t.Fatalf("tail is %d bytes, want %d", len(got), DefaultLimits.TailBytes)
	}
	if w.total != 200_000 {
		t.Fatalf("total %d, want 200000", w.total)
	}
}

func TestWindowTrimsToTheLastLines(t *testing.T) {
	w := newWindow(DefaultLimits)
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&w, "line-%d\n", i)
	}
	got := w.snapshot()
	lines := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
	if len(lines) != DefaultLimits.TailLines {
		t.Fatalf("kept %d lines, want %d", len(lines), DefaultLimits.TailLines)
	}
	if lines[0] != "line-3001" || lines[len(lines)-1] != "line-5000" {
		t.Fatalf("kept from %q to %q, want line-3001 to line-5000", lines[0], lines[len(lines)-1])
	}
}

func TestWindowCapsOneHugeLine(t *testing.T) {
	w := newWindow(DefaultLimits)
	w.Write(bytes.Repeat([]byte("x"), 10<<20))
	got := w.snapshot()
	if len(got) != DefaultLimits.TailBytes {
		t.Fatalf("tail is %d bytes, want %d", len(got), DefaultLimits.TailBytes)
	}
	if w.lines != 0 {
		t.Fatalf("lines %d, want 0: the output has no newline", w.lines)
	}
}

func TestWindowAtTheExactLimit(t *testing.T) {
	w := newWindow(DefaultLimits)
	w.Write(bytes.Repeat([]byte("b"), DefaultLimits.TailBytes))
	got := w.snapshot()
	if len(got) != DefaultLimits.TailBytes {
		t.Fatalf("tail is %d bytes, want %d", len(got), DefaultLimits.TailBytes)
	}
}

func lastLinesCase(t *testing.T, in string, n int, want string) {
	t.Helper()
	if got := string(lastLines([]byte(in), n)); got != want {
		t.Fatalf("lastLines(%q, %d) = %q, want %q", in, n, got, want)
	}
}

func TestLastLines(t *testing.T) {
	// A trailing newline closes the last line; it does not add an empty one.
	lastLinesCase(t, "a\nb\nc\n", 3, "a\nb\nc\n")
	lastLinesCase(t, "a\nb\nc\n", 2, "b\nc\n")
	lastLinesCase(t, "a\nb\nc\n", 1, "c\n")
	lastLinesCase(t, "a\nb\nc\n", 9, "a\nb\nc\n")
	lastLinesCase(t, "a\nb\nc", 2, "b\nc")
	lastLinesCase(t, "no newline", 2, "no newline")
}

func TestZeroLimitsTakeTheDefaults(t *testing.T) {
	if got := (Limits{}).withDefaults(); got != DefaultLimits {
		t.Fatalf("defaults %+v, want %+v", got, DefaultLimits)
	}
}

func TestWindowHonoursSmallLimits(t *testing.T) {
	w := newWindow(Limits{TailBytes: 16, TailLines: 3, FlushBytes: 8})
	_, _ = w.Write([]byte("aaaa\nbbbb\ncccc\ndddd\n"))
	if got := string(w.snapshot()); got != "bbbb\ncccc\ndddd\n" {
		t.Fatalf("tail %q, want the last three lines inside 16 bytes", got)
	}
}
