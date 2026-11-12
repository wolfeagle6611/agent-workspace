//go:build linux

package mcp

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestExecToolRunsThroughTheMount is the step-7d acceptance test: the tool
// runs a command, reports its exit code, keeps the two tails, and the full log
// is readable through the workspace. It needs a kernel mount, so it stays on
// Linux CI.
func TestExecToolRunsThroughTheMount(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	s, ws := newServer(t)

	res := callTool(t, s, "exec", `{"command":"printf 'hi\\n'; printf 'oops\\n' >&2; printf body > f.txt; exit 3"}`)
	if res.IsError {
		t.Fatalf("exec failed: %s", res.Content[0].Text)
	}
	text := res.Content[0].Text
	for _, want := range []string{"hi\n", "--- stderr ---\noops\n", "status=failed exit=3", "sync=complete"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}

	// A write the command made is visible to the next tool call.
	read := callTool(t, s, "read", `{"path":"/f.txt"}`)
	if read.IsError || !strings.HasPrefix(read.Content[0].Text, "body") {
		t.Fatalf("read after exec = %q", read.Content[0].Text)
	}

	// The full log lives in the workspace, at the path the result named.
	path := stdoutPath(t, text)
	full, err := ws.ReadFile(context.Background(), path)
	if err != nil {
		t.Fatalf("read log %s: %v", path, err)
	}
	if !strings.Contains(string(full.Data), "hi\n") {
		t.Fatalf("log body = %q", full.Data)
	}
}

// stdoutPath pulls the full-log path out of the metadata line.
func stdoutPath(t *testing.T, text string) string {
	t.Helper()
	const marker = "stdout="
	i := strings.LastIndex(text, marker)
	if i < 0 {
		t.Fatalf("no stdout path in %q", text)
	}
	rest := text[i+len(marker):]
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}
