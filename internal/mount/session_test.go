package mount

import (
	"context"
	"errors"
	"testing"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

func newSession(t *testing.T) *Session {
	t.Helper()
	vol, _ := newVolume(t)
	return NewSession(vol, NewManager(t.TempDir()), "ws-test")
}

func TestSessionUnmountWithoutMountIsSafe(t *testing.T) {
	ctx := context.Background()
	s := newSession(t)

	if err := s.Unmount(ctx); err != nil {
		t.Fatalf("unmount of an unmounted session: %v", err)
	}
	if err := s.Unmount(ctx); err != nil {
		t.Fatalf("second unmount: %v", err)
	}
	if got := s.Path(); got != "" {
		t.Fatalf("path %q after unmount, want empty", got)
	}
}

// A mount that starts after the unmount must fail, or it would remount a
// volume whose buffers and store are gone.
func TestSessionRefusesMountAfterUnmount(t *testing.T) {
	ctx := context.Background()
	s := newSession(t)
	if err := s.Unmount(ctx); err != nil {
		t.Fatalf("unmount: %v", err)
	}

	if _, err := s.Mount(ctx); !errors.Is(err, vfs.ErrClosed) {
		t.Fatalf("mount after unmount = %v, want ErrClosed", err)
	}
	if _, err := s.Mount(ctx); !errors.Is(err, vfs.ErrClosed) {
		t.Fatalf("second mount = %v, want ErrClosed", err)
	}
}
