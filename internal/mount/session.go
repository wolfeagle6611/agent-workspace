package mount

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

// Session owns one mounted volume: the mount point, the FUSE server serving
// it, and the volume behind it.
//
// It exists because the pieces must run in one order: prepare the directory,
// mount, flush the write buffers, unmount, remove the directory. A caller
// that wires them by hand gets that order wrong once and loses buffered
// writes.
type Session struct {
	vol *Volume
	mgr *Manager
	id  string

	mu     sync.Mutex
	point  string
	server *Server
	closed bool
}

// NewSession builds a session for id under the manager's root. It does not
// mount.
func NewSession(vol *Volume, mgr *Manager, id string) *Session {
	return &Session{vol: vol, mgr: mgr, id: id}
}

// Mount prepares the mount point and mounts the volume. Concurrent callers
// wait for the same mount, a repeated call returns the same point, and a
// closed session refuses.
func (s *Session) Mount(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", vfs.NewError(vfs.CodeClosed, "")
	}
	if s.server != nil {
		return s.point, nil
	}

	point, err := s.mgr.Prepare(ctx, s.id)
	if err != nil {
		return "", fmt.Errorf("prepare mount point: %w", err)
	}
	srv, err := Mount(s.vol, point, false)
	if err != nil {
		// An environment fault, not a path fault: no FsError code fits.
		return "", fmt.Errorf("mount %s: %w", point, err)
	}
	s.point, s.server = point, srv
	return point, nil
}

// Path returns the mount point. It is empty before Mount and after Unmount.
func (s *Session) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.point
}

// Unmount flushes the volume's write buffers, unmounts, and removes the
// directory. It is safe to call twice and safe before Mount.
//
// The flush comes first on purpose: an unmount before it would lose the
// buffered writes.
func (s *Session) Unmount(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	srv, point := s.server, s.point
	s.server, s.point = nil, ""
	s.mu.Unlock()

	var errs []error
	if err := s.vol.Close(ctx); err != nil {
		errs = append(errs, err)
	}
	if srv != nil {
		if err := srv.Unmount(); err != nil {
			errs = append(errs, fmt.Errorf("unmount %s: %w", point, err))
		}
	}
	if err := s.mgr.Release(ctx, s.id); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
