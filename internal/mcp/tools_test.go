package mcp

import (
	"bytes"
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// revOf reads the rev out of a tool's metadata line.
func revOf(t *testing.T, text string) int64 {
	t.Helper()
	m := regexp.MustCompile(`rev=(\d+)`).FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no rev in %q", text)
	}
	rev, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("rev %q: %v", m[1], err)
	}
	return rev
}

func TestWriteCreatesAndReturnsRev(t *testing.T) {
	s, ws := newServer(t)
	res := callTool(t, s, "write", `{"path":"/a.md","content":"hello\n"}`)
	if res.IsError {
		t.Fatalf("write failed: %s", res.Content[0].Text)
	}
	if !strings.HasPrefix(res.Content[0].Text, "wrote /a.md\n[rev=") {
		t.Fatalf("text = %q", res.Content[0].Text)
	}
	if revOf(t, res.Content[0].Text) == 0 {
		t.Fatal("write reported rev 0")
	}

	got, err := ws.ReadFile(context.Background(), "/a.md")
	if err != nil || string(got.Data) != "hello\n" {
		t.Fatalf("body = %q (%v)", got.Data, err)
	}
}

func TestWriteStaleRevConflicts(t *testing.T) {
	s, _ := newServer(t)
	first := callTool(t, s, "write", `{"path":"/a.md","content":"one\n"}`)
	stale := revOf(t, first.Content[0].Text)

	second := callTool(t, s, "write", `{"path":"/a.md","content":"two\n"}`)
	current := revOf(t, second.Content[0].Text)

	res := callTool(t, s, "write", `{"path":"/a.md","content":"mine\n","rev":`+strconv.FormatInt(stale, 10)+`}`)
	if !res.IsError {
		t.Fatal("stale write succeeded")
	}
	want := "ECONFLICT path=/a.md baseRev=" + strconv.FormatInt(stale, 10) +
		" currentRev=" + strconv.FormatInt(current, 10)
	if got := res.Content[0].Text; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}

	retry := callTool(t, s, "write", `{"path":"/a.md","content":"mine\n","rev":`+strconv.FormatInt(current, 10)+`}`)
	if retry.IsError {
		t.Fatalf("retry with currentRev failed: %s", retry.Content[0].Text)
	}
}

func TestEditReplacesOne(t *testing.T) {
	s, ws := newServer(t)
	ctx := context.Background()
	if err := ws.WriteFile(ctx, "/a.md", []byte("a\nb\n"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	res := callTool(t, s, "edit", `{"path":"/a.md","old":"b","new":"c"}`)
	if res.IsError {
		t.Fatalf("edit failed: %s", res.Content[0].Text)
	}
	if !strings.HasPrefix(res.Content[0].Text, "edited /a.md · 1 replacement\n[rev=") {
		t.Fatalf("text = %q", res.Content[0].Text)
	}
	got, err := ws.ReadFile(ctx, "/a.md")
	if err != nil || string(got.Data) != "a\nc\n" {
		t.Fatalf("body = %q (%v)", got.Data, err)
	}
}

func TestEditMissAndAmbiguity(t *testing.T) {
	s, ws := newServer(t)
	ctx := context.Background()
	if err := ws.WriteFile(ctx, "/a.md", []byte("x\nx\n"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	res := callTool(t, s, "edit", `{"path":"/a.md","old":"zzz","new":"y"}`)
	if !res.IsError {
		t.Fatal("edit of missing text succeeded")
	}
	if got := res.Content[0].Text; got != "ENOMATCH path=/a.md occurrences=0" {
		t.Fatalf("text = %q", got)
	}

	res = callTool(t, s, "edit", `{"path":"/a.md","old":"x","new":"y"}`)
	if !res.IsError {
		t.Fatal("ambiguous edit succeeded without replace_all")
	}
	if got := res.Content[0].Text; got != "ENOMATCH path=/a.md occurrences=2" {
		t.Fatalf("text = %q", got)
	}

	res = callTool(t, s, "edit", `{"path":"/a.md","old":"x","new":"y","replace_all":true}`)
	if res.IsError {
		t.Fatalf("replace_all failed: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "2 replacements") {
		t.Fatalf("text = %q", res.Content[0].Text)
	}
	got, err := ws.ReadFile(ctx, "/a.md")
	if err != nil || string(got.Data) != "y\ny\n" {
		t.Fatalf("body = %q (%v)", got.Data, err)
	}
}

// A stale rev must report the version, not the text: the caller has to re-read
// before any match against the new bytes means anything.
func TestEditStaleRevIsConflictNotNoMatch(t *testing.T) {
	s, _ := newServer(t)
	first := callTool(t, s, "write", `{"path":"/a.md","content":"one\n"}`)
	stale := revOf(t, first.Content[0].Text)
	second := callTool(t, s, "write", `{"path":"/a.md","content":"two\n"}`)
	current := revOf(t, second.Content[0].Text)

	res := callTool(t, s, "edit", `{"path":"/a.md","old":"gone","new":"x","rev":`+strconv.FormatInt(stale, 10)+`}`)
	if !res.IsError {
		t.Fatal("stale edit succeeded")
	}
	want := "ECONFLICT path=/a.md baseRev=" + strconv.FormatInt(stale, 10) +
		" currentRev=" + strconv.FormatInt(current, 10)
	if got := res.Content[0].Text; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

func TestEditMissingFileIsNotFound(t *testing.T) {
	s, _ := newServer(t)
	res := callTool(t, s, "edit", `{"path":"/nope","old":"a","new":"b"}`)
	if !res.IsError || res.Content[0].Text != "ENOENT path=/nope" {
		t.Fatalf("text = %q", res.Content[0].Text)
	}
}

// A malformed call must not truncate a file. Only an explicit "" empties it.
func TestWriteAndEditRequireTheirContentFields(t *testing.T) {
	s, ws := newServer(t)
	ctx := context.Background()
	if err := ws.WriteFile(ctx, "/a.md", []byte("keep\n"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	res := callTool(t, s, "write", `{"path":"/a.md"}`)
	if !res.IsError || res.Content[0].Text != "EINVAL path=/a.md" {
		t.Fatalf("write without content: %q (isError=%v)", res.Content[0].Text, res.IsError)
	}
	res = callTool(t, s, "edit", `{"path":"/a.md","old":"keep"}`)
	if !res.IsError || res.Content[0].Text != "EINVAL path=/a.md" {
		t.Fatalf("edit without new: %q (isError=%v)", res.Content[0].Text, res.IsError)
	}
	got, err := ws.ReadFile(ctx, "/a.md")
	if err != nil || string(got.Data) != "keep\n" {
		t.Fatalf("file changed: %q (%v)", got.Data, err)
	}

	// An explicit empty content is still a legitimate truncation.
	if res := callTool(t, s, "write", `{"path":"/a.md","content":""}`); res.IsError {
		t.Fatalf("explicit empty write failed: %s", res.Content[0].Text)
	}
	if got, err := ws.ReadFile(ctx, "/a.md"); err != nil || len(got.Data) != 0 {
		t.Fatalf("explicit empty write = %q (%v)", got.Data, err)
	}
}

// The footer must name the lines that came back, not the lines requested: a
// model computes its next offset from that number.
func TestReadCutReportsActualLines(t *testing.T) {
	s, ws := newServer(t)
	ctx := context.Background()
	line := strings.Repeat("x", 39) + "\n"
	body := strings.Repeat(line, 3000)
	if err := ws.WriteFile(ctx, "/big.txt", []byte(body), nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	res := callTool(t, s, "read", `{"path":"/big.txt"}`)
	text := res.Content[0].Text
	if !strings.Contains(text, "cut to 64 KiB") {
		t.Fatalf("no cut marker in footer: %q", text)
	}
	parts := strings.SplitN(text, "\n\n[", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected shape: %q", text)
	}
	// The split consumes the newline that ended the body's last line.
	returned := strings.Count(parts[0], "\n") + 1
	m := regexp.MustCompile(`lines 1-(\d+) of 3000`).FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("footer does not name a range: %q", text)
	}
	end, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("range %q: %v", m[1], err)
	}
	if end != returned {
		t.Fatalf("footer says %d lines, body holds %d", end, returned)
	}
	if end >= 2000 {
		t.Fatalf("cut kept %d lines, no cut happened", end)
	}
}

func TestReadLimits(t *testing.T) {
	s, ws := newServer(t)
	ctx := context.Background()

	t.Run("oversized file is refused before it is read", func(t *testing.T) {
		big := bytes.Repeat([]byte("x"), maxReadFile+1)
		if err := ws.WriteFile(ctx, "/huge.bin", big, nil); err != nil {
			t.Fatalf("write: %v", err)
		}
		res := callTool(t, s, "read", `{"path":"/huge.bin"}`)
		if !res.IsError {
			t.Fatal("oversized read succeeded")
		}
		if got := res.Content[0].Text; !strings.HasPrefix(got, "EINVAL path=/huge.bin file is") ||
			!strings.Contains(got, "use exec") {
			t.Fatalf("text = %q", got)
		}
	})

	t.Run("one giant line is cut and labelled", func(t *testing.T) {
		if err := ws.WriteFile(ctx, "/line.txt", bytes.Repeat([]byte("y"), 100*1024), nil); err != nil {
			t.Fatalf("write: %v", err)
		}
		res := callTool(t, s, "read", `{"path":"/line.txt"}`)
		if !strings.Contains(res.Content[0].Text, "partial line") {
			t.Fatalf("text tail = %q", res.Content[0].Text[len(res.Content[0].Text)-120:])
		}
	})
}

func TestLsCapsEntries(t *testing.T) {
	s, ws := newServer(t)
	ctx := context.Background()
	if err := ws.MkdirAll(ctx, "/many"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for i := 0; i <= maxLsEntries; i++ {
		path := "/many/f" + strconv.Itoa(i)
		if err := ws.WriteFile(ctx, path, nil, nil); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	res := callTool(t, s, "ls", `{"path":"/many"}`)
	text := res.Content[0].Text
	if !strings.Contains(text, "1001 entries") || !strings.Contains(text, "first 1000 shown") {
		t.Fatalf("footer = %q", text[strings.LastIndex(text, "["):])
	}
	listed := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "f ") || strings.HasPrefix(line, "d ") {
			listed++
		}
	}
	if listed != maxLsEntries {
		t.Fatalf("listed %d entries, want %d", listed, maxLsEntries)
	}
}
