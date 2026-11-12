package mount

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Manager owns the directories volumes are mounted on: creating them,
// detecting a stale mount left by a crashed process, and removing them.
//
// One directory per workspace id. The stale-mount check runs on every
// Prepare, because a FUSE mount outliving its process is the failure mode
// that otherwise needs a human.
type Manager struct {
	root    string
	mounted func(string) bool
	unmount func(string) error
}

// NewManager builds a Manager rooted at root (see DefaultRoot).
func NewManager(root string) *Manager {
	return &Manager{root: root, mounted: isMountPoint, unmount: unmountPath}
}

// DefaultRoot is where mount points live for this platform.
func DefaultRoot() string {
	return defaultRoot()
}

// Root reports the configured root.
func (m *Manager) Root() string { return m.root }

// Prepare returns the mount point for id, creating it and clearing a stale
// mount left behind by an earlier process.
func (m *Manager) Prepare(ctx context.Context, id string) (string, error) {
	if err := validID(id); err != nil {
		return "", err
	}
	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return "", fmt.Errorf("create mount root: %w", err)
	}
	point := filepath.Join(m.root, id)
	if err := os.MkdirAll(point, 0o755); err != nil {
		return "", fmt.Errorf("create mount point: %w", err)
	}
	if m.mounted(point) {
		if err := m.unmount(point); err != nil {
			return "", fmt.Errorf("mount point %s is stale and could not be cleared: %w", point, err)
		}
		if m.mounted(point) {
			return "", fmt.Errorf("mount point %s is still busy", point)
		}
	}
	return point, nil
}

// Release unmounts point (when mounted) and removes the directory.
func (m *Manager) Release(ctx context.Context, id string) error {
	if err := validID(id); err != nil {
		return err
	}
	point := filepath.Join(m.root, id)
	if m.mounted(point) {
		if err := m.unmount(point); err != nil {
			return fmt.Errorf("unmount %s: %w", point, err)
		}
	}
	if err := os.Remove(point); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// validID keeps an id from escaping the root.
func validID(id string) error {
	if id == "" || id == "." || id == ".." {
		return fmt.Errorf("invalid workspace id %q", id)
	}
	if strings.ContainsAny(id, "/\\\x00") {
		return fmt.Errorf("invalid workspace id %q", id)
	}
	return nil
}
