package execout

import "bytes"

// Limits is the output policy for one sink. Callers that want the shared
// policy pass DefaultLimits; a zero field takes the default.
type Limits struct {
	// TailBytes is how much of one stream stays in memory. The model reads
	// the end of the output; the whole text lives in the workspace.
	TailBytes int
	// TailLines is the second bound on the same tail, from
	// docs/architecture/computer.md §6.4.
	TailLines int
	// FlushBytes is one append batch. Keep it at the chunk size, so a batch
	// lands on chunk boundaries instead of rewriting the same tail chunk
	// again and again.
	FlushBytes int
}

// DefaultLimits is the policy shared by all backends.
var DefaultLimits = Limits{
	TailBytes:  64 << 10,
	TailLines:  2000,
	FlushBytes: 512 << 10,
}

// withDefaults fills the zero fields, so a caller can override one number
// without restating the rest.
func (l Limits) withDefaults() Limits {
	if l.TailBytes <= 0 {
		l.TailBytes = DefaultLimits.TailBytes
	}
	if l.TailLines <= 0 {
		l.TailLines = DefaultLimits.TailLines
	}
	if l.FlushBytes <= 0 {
		l.FlushBytes = DefaultLimits.FlushBytes
	}
	return l
}

// window keeps the last limits.TailBytes of one stream and counts
// everything that passed through it.
//
// Bytes are counted on the way in, so the totals stay correct even when the
// tail drops the start. Lines are trimmed once, at snapshot time, because a
// byte ring cannot know which delimiter starts the last N lines.
type window struct {
	limits Limits
	buf    []byte
	pos    int   // next write slot
	full   bool  // buf wrapped at least once
	total  int64 // bytes ever written
	lines  int64 // newlines ever written
}

func newWindow(l Limits) window {
	l = l.withDefaults()
	return window{limits: l, buf: make([]byte, l.TailBytes)}
}

// Write keeps the tail. It never fails.
func (w *window) Write(p []byte) (int, error) {
	size := w.limits.TailBytes
	w.total += int64(len(p))
	w.lines += int64(bytes.Count(p, []byte{'\n'}))

	if len(p) >= size {
		copy(w.buf, p[len(p)-size:])
		w.pos = 0
		w.full = true
		return len(p), nil
	}
	n := copy(w.buf[w.pos:], p)
	if n < len(p) {
		copy(w.buf, p[n:])
		w.pos = len(p) - n
		w.full = true
		return len(p), nil
	}
	w.pos += n
	if w.pos == size {
		w.pos = 0
		w.full = true
	}
	return len(p), nil
}

// snapshot returns the kept bytes, trimmed to the last limits.TailLines.
func (w *window) snapshot() []byte {
	var tail []byte
	if w.full {
		tail = make([]byte, 0, len(w.buf))
		tail = append(tail, w.buf[w.pos:]...)
		tail = append(tail, w.buf[:w.pos]...)
	} else {
		tail = append([]byte(nil), w.buf[:w.pos]...)
	}
	return lastLines(tail, w.limits.TailLines)
}

// lastLines cuts b to its last n lines. A trailing newline does not count as
// starting an empty last line.
func lastLines(b []byte, n int) []byte {
	if n <= 0 {
		return nil
	}
	body := b
	if len(body) > 0 && body[len(body)-1] == '\n' {
		body = body[:len(body)-1]
	}
	seen := 0
	for i := len(body) - 1; i >= 0; i-- {
		if body[i] != '\n' {
			continue
		}
		seen++
		if seen == n {
			return b[i+1:]
		}
	}
	return b
}
