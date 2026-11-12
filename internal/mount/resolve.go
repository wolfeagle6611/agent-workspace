package mount

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

// MountID names the mount point of a database.
//
// The name carries a hash of the absolute database path. Manager.Prepare clears
// whatever it finds at a mount point, so two databases that share a file name
// must not share a point: the second Open would unmount the first and route its
// writes to the wrong store.
func MountID(dbPath string) (string, error) {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return "", vfs.WrapError(vfs.CodeInvalid, dbPath, err)
	}
	base := strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "", vfs.NewError(vfs.CodeInvalid, dbPath)
	}
	sum := sha256.Sum256([]byte(abs))
	return base + "-" + hex.EncodeToString(sum[:4]), nil
}

// Resolver maps a workspace path to a real path under the mount point.
//
// It is the one place a workspace path becomes a real path, so the rules live
// here once: reject a relative path or "..", require the path to exist,
// require a directory, and mount on demand (go-mapping §5 step 3). The host
// and a peer that runs commands both need it.
func Resolver(store *vfs.Store, session *Session) func(ctx context.Context, path string) (string, error) {
	return func(ctx context.Context, path string) (string, error) {
		if path == "" {
			path = "/"
		}
		clean, err := vfs.Clean(path)
		if err != nil {
			return "", err
		}
		st, err := store.Stat(ctx, clean)
		if err != nil {
			return "", err
		}
		if st.Type != vfs.TypeDir {
			return "", vfs.NewError(vfs.CodeNotDir, path)
		}
		point, err := session.Mount(ctx)
		if err != nil {
			return "", err
		}
		if clean == "/" {
			return point, nil
		}
		return filepath.Join(point, clean), nil
	}
}
