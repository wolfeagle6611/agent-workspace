//go:build darwin

package mount

import (
	"os/exec"
	"path/filepath"
)

func defaultRoot() string { return filepath.Join("/tmp", "ws") }

// unmountPath clears a macFUSE mount.
func unmountPath(path string) error {
	if err := exec.Command("umount", path).Run(); err == nil {
		return nil
	}
	return exec.Command("diskutil", "unmount", "force", path).Run()
}
