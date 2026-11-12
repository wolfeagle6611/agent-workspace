//go:build !linux && !darwin

package mount

import (
	"fmt"
	"path/filepath"
	"runtime"
)

func defaultRoot() string { return filepath.Join("/tmp", "ws") }

func unmountPath(path string) error {
	return fmt.Errorf("mount: unmount is not implemented on %s", runtime.GOOS)
}
