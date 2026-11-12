//go:build linux

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/mount"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/peer"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/runtime"
)

// peerWorkspace opens a mounted workspace that syncs with m and runs commands
// on it.
func peerWorkspace(t *testing.T, m *mirror) *Workspace {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "ws.db"), Options{
		MountRoot:  filepath.Join(dir, "mnt"),
		PeerSocket: m.socket,
		Remote:     true,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// TestExecBracketPushesBeforeSpawnAndPullsAfter is the step 6b bracket: the
// command sees the pushed mirror, and the mirror gets the command's writes.
func TestExecBracketPushesBeforeSpawnAndPullsAfter(t *testing.T) {
	m := startMirror(t)
	w := peerWorkspace(t, m)
	ctx := t.Context()

	if err := w.WriteFile(ctx, "/a.txt", []byte("alpha"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	ex, err := w.Exec(ctx, "cat /a.txt > /from-command.txt", runtime.ExecOptions{Backend: "shell"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	res, err := ex.Result(ctx)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Status != runtime.Completed {
		t.Fatalf("status %v", res.Status)
	}
	if res.Sync != runtime.SyncComplete {
		t.Fatalf("sync outcome %v, want complete", res.Sync)
	}

	// The command's write reached the mirror through the pull.
	got, err := m.store.ReadFile(ctx, "/from-command.txt")
	if err != nil {
		t.Fatalf("mirror read: %v", err)
	}
	if string(got.Data) != "alpha" {
		t.Fatalf("mirror holds %q", got.Data)
	}
}

// TestExecGateStopsWhenThePushFails: no spawn against a stale mirror.
func TestExecGateStopsWhenThePushFails(t *testing.T) {
	m := startMirror(t)
	w := peerWorkspace(t, m)
	ctx := t.Context()

	if _, err := w.Push(ctx); err != nil {
		t.Fatalf("first push: %v", err)
	}
	m.stop(t)

	marker := filepath.Join(t.TempDir(), "spawned")
	if _, err := w.Exec(ctx, "touch "+marker, runtime.ExecOptions{Backend: "shell"}); err == nil {
		t.Fatalf("exec started with a dead mirror")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("command ran after a failed push")
	}
}

// TestPullFailureLeavesTheCommandResultPending: the command result stands, and
// the missing pull is reported, not hidden (go-mapping §6).
func TestPullFailureLeavesTheCommandResultPending(t *testing.T) {
	m := startMirror(t)
	w := peerWorkspace(t, m)
	ctx := t.Context()

	ex, err := w.Exec(ctx, "echo done", runtime.ExecOptions{Backend: "shell"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	// The push already happened; kill the mirror before the pull runs.
	m.stop(t)

	res, err := ex.Result(ctx)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Status != runtime.Completed {
		t.Fatalf("command status %v, want completed", res.Status)
	}
	if res.Sync != runtime.SyncPending {
		t.Fatalf("sync outcome %v, want pending", res.Sync)
	}
}

// TestRemoteExecBracket is the full 6b path with a remote backend: push, run
// the command inside the mirror's mount, pull the result back.
func TestRemoteExecBracket(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	m := startMirror(t)
	session := mount.NewSession(mount.NewVolume(m.store), mount.NewManager(filepath.Join(t.TempDir(), "mnt")), "peer-test")
	t.Cleanup(func() { _ = session.Unmount(context.Background()) })

	// Restart the mirror with a runner, so commands run against its mount.
	m.stop(t)
	m.run = peer.NewRunner(mount.Resolver(m.store, session))
	m.start(t)

	w := peerWorkspace(t, m)
	ctx := t.Context()
	if err := w.WriteFile(ctx, "/a.txt", []byte("alpha"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	ex, err := w.Exec(ctx, "cat /a.txt > /copy.txt", runtime.ExecOptions{Backend: "remote"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	res, err := ex.Result(ctx)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Status != runtime.Completed {
		t.Fatalf("status %v", res.Status)
	}
	if res.Sync != runtime.SyncComplete {
		t.Fatalf("sync outcome %v, want complete", res.Sync)
	}

	got, err := w.ReadFile(ctx, "/copy.txt")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got.Data) != "alpha" {
		t.Fatalf("/copy.txt holds %q", got.Data)
	}
}
