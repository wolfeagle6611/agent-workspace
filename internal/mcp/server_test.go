package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/workspace"
)

func newServer(t *testing.T) (*Server, *workspace.Workspace) {
	t.Helper()
	dir := t.TempDir()
	ws, err := workspace.Open(filepath.Join(dir, "ws.db"), workspace.Options{MountRoot: filepath.Join(dir, "mnt")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return NewServer(ws), ws
}

// roundTrip answers every request and returns the replies in order.
func roundTrip(t *testing.T, s *Server, reqs ...string) []response {
	t.Helper()
	var out bytes.Buffer
	in := strings.NewReader(strings.Join(reqs, "\n") + "\n")
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	dec := json.NewDecoder(&out)
	var got []response
	for {
		var r response
		err := dec.Decode(&r)
		if errors.Is(err, io.EOF) {
			return got
		}
		if err != nil {
			t.Fatalf("decode reply: %v", err)
		}
		got = append(got, r)
	}
}

func callTool(t *testing.T, s *Server, name, args string) toolResult {
	t.Helper()
	req := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
	got := roundTrip(t, s, req)
	if len(got) != 1 {
		t.Fatalf("replies = %d, want 1", len(got))
	}
	if got[0].Error != nil {
		t.Fatalf("protocol error: %+v", got[0].Error)
	}
	body, err := json.Marshal(got[0].Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var res toolResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("result shape: %v", err)
	}
	if len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("content = %+v, want one text item", res.Content)
	}
	return res
}

func TestInitializeAndList(t *testing.T) {
	s, _ := newServer(t)
	got := roundTrip(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)
	if len(got) != 2 {
		t.Fatalf("replies = %d, want 2 (the notification takes none)", len(got))
	}

	init, ok := got[0].Result.(map[string]any)
	if !ok {
		t.Fatalf("initialize result = %T", got[0].Result)
	}
	if init["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocolVersion = %v", init["protocolVersion"])
	}

	list, ok := got[1].Result.(map[string]any)
	if !ok {
		t.Fatalf("tools/list result = %T", got[1].Result)
	}
	raw, _ := json.Marshal(list["tools"])
	var names []string
	var defs []struct {
		Name   string `json:"name"`
		Schema struct {
			Type string `json:"type"`
		} `json:"inputSchema"`
	}
	if err := json.Unmarshal(raw, &defs); err != nil {
		t.Fatalf("tools: %v", err)
	}
	for _, d := range defs {
		names = append(names, d.Name)
		if d.Schema.Type != "object" {
			t.Fatalf("tool %s has no object schema", d.Name)
		}
	}
	if strings.Join(names, ",") != "exec,read,write,edit,ls" {
		t.Fatalf("tools = %v, want read,write,edit,ls", names)
	}
}

func TestReadReturnsBodyAndRev(t *testing.T) {
	s, ws := newServer(t)
	ctx := context.Background()
	if err := ws.WriteFile(ctx, "/a.txt", []byte("one\ntwo\nthree\n"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	res := callTool(t, s, "read", `{"path":"/a.txt"}`)
	if res.IsError {
		t.Fatalf("read failed: %s", res.Content[0].Text)
	}
	text := res.Content[0].Text
	if !strings.HasPrefix(text, "one\ntwo\nthree\n") {
		t.Fatalf("body = %q", text)
	}
	if !strings.Contains(text, "rev=") || !strings.Contains(text, "3 lines") {
		t.Fatalf("footer = %q", text[strings.LastIndex(text, "["):])
	}
}

func TestReadWindowsLines(t *testing.T) {
	s, ws := newServer(t)
	ctx := context.Background()
	body := "l1\nl2\nl3\nl4\nl5\n"
	if err := ws.WriteFile(ctx, "/a.txt", []byte(body), nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	res := callTool(t, s, "read", `{"path":"/a.txt","offset":2,"limit":2}`)
	text := res.Content[0].Text
	if !strings.HasPrefix(text, "l2\nl3\n") {
		t.Fatalf("window = %q", text)
	}
	if strings.Contains(text, "l4") {
		t.Fatalf("window leaked past the limit: %q", text)
	}
	if !strings.Contains(text, "lines 2-3 of 5") {
		t.Fatalf("footer = %q", text)
	}
}

func TestReadMissingIsToolError(t *testing.T) {
	s, _ := newServer(t)
	res := callTool(t, s, "read", `{"path":"/nope"}`)
	if !res.IsError {
		t.Fatal("missing file did not report isError")
	}
	if got := res.Content[0].Text; got != "ENOENT path=/nope" {
		t.Fatalf("text = %q", got)
	}
}

func TestLsListsOneLevel(t *testing.T) {
	s, ws := newServer(t)
	ctx := context.Background()
	if err := ws.MkdirAll(ctx, "/sub/notes"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := ws.WriteFile(ctx, "/sub/a.txt", []byte("hi\n"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	res := callTool(t, s, "ls", `{"path":"/sub"}`)
	text := res.Content[0].Text
	want := "f a.txt 3\nd notes/\n"
	if !strings.HasPrefix(text, want) {
		t.Fatalf("listing = %q, want prefix %q", text, want)
	}
	if !strings.Contains(text, "2 entries") {
		t.Fatalf("footer = %q", text)
	}
}

func TestUnknownToolIsProtocolError(t *testing.T) {
	s, _ := newServer(t)
	got := roundTrip(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	if len(got) != 1 || got[0].Error == nil {
		t.Fatalf("want one protocol error, got %+v", got)
	}
	if got[0].Error.Code != -32602 {
		t.Fatalf("code = %d, want -32602", got[0].Error.Code)
	}
}
