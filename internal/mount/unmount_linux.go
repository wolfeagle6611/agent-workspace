//go:build linux

package mount

import "os/exec"

func defaultRoot() string { return "/run/ws" }

// unmountPath clears a FUSE mount. -z (lazy) is the point: a crashed
// process leaves the mount busy, and waiting for it would hang Prepare.
func unmountPath(path string) error {
	if err := exec.Command("fusermount", "-u", "-z", path).Run(); err == nil {
		return nil
	}
	return exec.Command("umount", "-l", path).Run()
}
