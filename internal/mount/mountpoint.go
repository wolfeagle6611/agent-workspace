package mount

import (
	"os"
	"path/filepath"
	"syscall"
)

// isMountPoint compares the device of path with the device of its parent.
// A mount point is where they differ. This works on Linux and on macOS
// without reading /proc or calling into FUSE.
func isMountPoint(path string) bool {
	child, err := os.Stat(path)
	if err != nil {
		return false
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return false
	}
	cs, ok := child.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	ps, ok := parent.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return cs.Dev != ps.Dev
}
