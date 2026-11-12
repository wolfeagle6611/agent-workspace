//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestThirdPhaseAcceptance is the step-7e acceptance test: one agent session
// over MCP, end to end. Each block below is one numbered step of the scenario
// in go-mapping §5. It needs a kernel mount, so it only runs on Linux.
func TestThirdPhaseAcceptance(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent-design")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build: %v", err)
	}
	db := filepath.Join(dir, "ws.db")
	mnt := filepath.Join(dir, "mnt")

	p := startServer(t, bin, db, mnt)

	// 1. The agent writes a script.
	const script = "#!/bin/sh\necho ran\n"
	mustTool(t, p, "write", map[string]any{"path": "/run.sh", "content": script})

	// 2. chmod really sets the execute bit: the script runs.
	out := mustTool(t, p, "exec", map[string]any{"command": "chmod +x run.sh && ./run.sh"})
	if !strings.Contains(out, "ran") || !strings.Contains(out, "status=completed exit=0") {
		t.Fatalf("step 2: %q", out)
	}

	// 3. Two writers read the same rev. The first write lands, the second gets
	// the conflict and the rev it needs to retry with.
	rev := revOf(t, mustTool(t, p, "read", map[string]any{"path": "/run.sh"}))
	mustTool(t, p, "write", map[string]any{"path": "/run.sh", "content": script + "# one\n", "rev": rev})
	text, isErr := p.tool(t, "write", map[string]any{"path": "/run.sh", "content": script + "# two\n", "rev": rev})
	if !isErr || !strings.Contains(text, "ECONFLICT") || !strings.Contains(text, "currentRev=") {
		t.Fatalf("step 3: %q (isError=%v)", text, isErr)
	}

	// 4. A wrong old string is ENOMATCH; a right one lands.
	text, isErr = p.tool(t, "edit", map[string]any{"path": "/run.sh", "old": "not-there", "new": "x"})
	if !isErr || !strings.Contains(text, "ENOMATCH") {
		t.Fatalf("step 4 miss: %q (isError=%v)", text, isErr)
	}
	mustTool(t, p, "edit", map[string]any{"path": "/run.sh", "old": "# one", "new": "# edited"})
	if got := mustTool(t, p, "read", map[string]any{"path": "/run.sh"}); !strings.Contains(got, "# edited") {
		t.Fatalf("step 4 edit did not land: %q", got)
	}

	// 5. Thirty thousand lines: the tail stays bounded, and the whole log is
	// readable through the workspace.
	out = mustTool(t, p, "exec", map[string]any{"command": "seq 1 30000"})
	if !strings.Contains(out, "exit=0") || !strings.Contains(out, "(tail only)") || !strings.Contains(out, "30000") {
		t.Fatalf("step 5: %q", out)
	}
	logPath := fieldValue(t, out, "stdout=")
	head := mustTool(t, p, "read", map[string]any{"path": logPath, "limit": 5})
	if !strings.HasPrefix(head, "1\n2\n3\n4\n5\n") {
		t.Fatalf("step 5 log head = %q", head)
	}

	// 6. A timeout kills the process group. The pid the command wrote is gone
	// once the tool returns.
	out = mustTool(t, p, "exec", map[string]any{
		"command": "echo $$ > /p.pid; exec sleep 30",
		"timeout": 1,
	})
	if !strings.Contains(out, "status=cancelled exit=-1") {
		t.Fatalf("step 6: %q", out)
	}
	pid := int64Of(t, mustTool(t, p, "read", map[string]any{"path": "/p.pid"}))
	waitGone(t, pid)

	// 7. The host stops and restarts. The workspace is still there, and the
	// mount root holds no leftover mount point.
	p.stop(t)
	assertMountRootEmpty(t, mnt)
	p = startServer(t, bin, db, mnt)
	if got := mustTool(t, p, "read", map[string]any{"path": "/run.sh"}); !strings.Contains(got, "# edited") {
		t.Fatalf("step 7 content lost: %q", got)
	}
	if got := mustTool(t, p, "ls", map[string]any{"path": "/"}); !strings.Contains(got, "f run.sh") {
		t.Fatalf("step 7 listing = %q", got)
	}

	// 8. The second stop leaves nothing behind either.
	p.stop(t)
	assertMountRootEmpty(t, mnt)
}

// client drives the built binary over MCP stdio, the way an agent harness does.
type client struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	dec  *json.Decoder
	next int
}

func startServer(t *testing.T, bin, db, mountRoot string) *client {
	t.Helper()
	cmd := exec.Command(bin, "mcp", "--db", db, "--mount-root", mountRoot)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	c := &client{cmd: cmd, in: in, dec: json.NewDecoder(out)}
	t.Cleanup(func() { c.stop(t) })

	c.call(t, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "acceptance", "version": "1"},
	})
	c.notify(t, "notifications/initialized")
	return c
}

func (c *client) stop(t *testing.T) {
	t.Helper()
	if c.cmd == nil {
		return
	}
	_ = c.in.Close()
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = c.cmd.Process.Kill()
		<-done
		t.Fatalf("server did not stop after stdin closed")
	}
	c.cmd = nil
}

type reply struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *client) call(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	c.next++
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.next, "method": method, "params": params})
	if err != nil {
		t.Fatalf("marshal %s: %v", method, err)
	}
	if _, err := c.in.Write(append(body, '\n')); err != nil {
		t.Fatalf("write %s: %v", method, err)
	}
	var r reply
	if err := c.dec.Decode(&r); err != nil {
		t.Fatalf("read reply to %s: %v", method, err)
	}
	if r.Error != nil {
		t.Fatalf("%s: rpc error %d: %s", method, r.Error.Code, r.Error.Message)
	}
	return r.Result
}

func (c *client) notify(t *testing.T, method string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	if err != nil {
		t.Fatalf("marshal %s: %v", method, err)
	}
	if _, err := c.in.Write(append(body, '\n')); err != nil {
		t.Fatalf("write %s: %v", method, err)
	}
}

func (c *client) tool(t *testing.T, name string, args map[string]any) (string, bool) {
	t.Helper()
	raw := c.call(t, "tools/call", map[string]any{"name": name, "arguments": args})
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("%s: result shape: %v", name, err)
	}
	if len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("%s: content = %+v", name, res.Content)
	}
	return res.Content[0].Text, res.IsError
}

func mustTool(t *testing.T, c *client, name string, args map[string]any) string {
	t.Helper()
	text, isErr := c.tool(t, name, args)
	if isErr {
		t.Fatalf("%s failed: %s", name, text)
	}
	return text
}

// revOf reads the rev out of a metadata line.
func revOf(t *testing.T, text string) int64 {
	t.Helper()
	m := regexp.MustCompile(`rev=(\d+)`).FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no rev in %q", text)
	}
	return int64Of(t, m[1])
}

func int64Of(t *testing.T, s string) int64 {
	t.Helper()
	s = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("int %q: %v", s, err)
	}
	return n
}

// fieldValue pulls one `key=value` token out of a metadata line.
func fieldValue(t *testing.T, text, key string) string {
	t.Helper()
	i := strings.LastIndex(text, key)
	if i < 0 {
		t.Fatalf("no %s in %q", key, text)
	}
	rest := text[i+len(key):]
	if j := strings.IndexAny(rest, " \n"); j >= 0 {
		rest = rest[:j]
	}
	if rest == "" {
		t.Fatalf("empty %s in %q", key, text)
	}
	return rest
}

// waitGone polls until pid is neither running nor an unreaped zombie.
func waitGone(t *testing.T, pid int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if gone(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process %d survived the timeout kill", pid)
}

func gone(pid int64) bool {
	if err := syscall.Kill(int(pid), 0); err != nil {
		return true
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	// Field 3 is the state; a zombie has finished running.
	fields := strings.Fields(string(stat))
	return len(fields) > 2 && fields[2] == "Z"
}

func assertMountRootEmpty(t *testing.T, root string) {
	t.Helper()
	ents, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("read mount root: %v", err)
	}
	if len(ents) != 0 {
		t.Fatalf("mount root %s still holds %d entries", root, len(ents))
	}
}
