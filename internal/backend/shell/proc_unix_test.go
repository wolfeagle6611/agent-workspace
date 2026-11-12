//go:build unix

package shell

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/runtime"
)

// TestCancelKillsTheProcessGroup is the step-4 acceptance test: nothing the
// command started may outlive the cancel.
func TestCancelKillsTheProcessGroup(t *testing.T) {
	dir := t.TempDir()
	b := New()
	b.Grace = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// The shell is the group leader, so its pid is the group id. It writes
	// that id first; the sleep is the grandchild that must not survive.
	res, err := runWith(ctx, t, b, dir, "echo $$ > pgid; sleep 30 & wait", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want DeadlineExceeded", err)
	}
	if res.Status != runtime.Cancelled {
		t.Fatalf("status %v, want Cancelled", res.Status)
	}
	pgid := readInt(t, filepath.Join(dir, "pgid"))
	waitGone(t, pgid)
}

// TestGraceThenKill covers the escalation: a group that ignores SIGTERM is
// removed after the grace period, not left behind.
func TestGraceThenKill(t *testing.T) {
	dir := t.TempDir()
	b := New()
	b.Grace = 300 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	// The shell ignores TERM and keeps a child running; only SIGKILL ends it.
	res, err := runWith(ctx, t, b, dir, "trap '' TERM; while :; do sleep 0.05; done", nil)
	took := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want DeadlineExceeded", err)
	}
	if res.Status != runtime.Cancelled || res.ExitCode != -1 {
		t.Fatalf("status %v exit %d, want Cancelled -1", res.Status, res.ExitCode)
	}
	if took < b.Grace {
		t.Fatalf("command ended in %s, before the %s grace period: escalation did not run", took, b.Grace)
	}
	if took > 3*time.Second {
		t.Fatalf("command ended in %s, want the grace period plus a moment", took)
	}
}

func readInt(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return n
}

// waitGone polls until no process in the group is left.
func waitGone(t *testing.T, pgid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process group %d outlived the cancel", pgid)
}
