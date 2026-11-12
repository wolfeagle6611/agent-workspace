package vfs

import (
	"errors"
	"fmt"
)

// Error codes. Same strings as the Cloudflare Computer wire vocabulary so
// logs and tests read the same across both systems. ECONFLICT is ours: the
// upstream system has no baseRev.
// See docs/architecture/go-mapping.md §7.
const (
	CodeNotFound = "ENOENT"
	CodeExist    = "EEXIST"
	CodeNotDir   = "ENOTDIR"
	CodeIsDir    = "EISDIR"
	CodeNotEmpty = "ENOTEMPTY"
	CodeReadOnly = "EROFS"
	CodeInvalid  = "EINVAL"
	CodePerm     = "EPERM"
	CodeConflict = "ECONFLICT"
	CodeExecLost = "EEXEC_LOST"
	CodeSyncLost = "ESYNC_LOST"
	CodeClosed   = "ECLOSED"
)

// Sentinels exist so callers can write errors.Is(err, vfs.ErrConflict)
// without parsing strings.
var (
	ErrNotFound = errors.New("not found")
	ErrExist    = errors.New("already exists")
	ErrNotDir   = errors.New("not a directory")
	ErrIsDir    = errors.New("is a directory")
	ErrNotEmpty = errors.New("directory not empty")
	ErrReadOnly = errors.New("read-only")
	ErrInvalid  = errors.New("invalid argument")
	ErrPerm     = errors.New("operation not permitted")
	ErrConflict = errors.New("version conflict")
	ErrExecLost = errors.New("execution lost")
	ErrSyncLost = errors.New("sync lost")
	ErrClosed   = errors.New("store closed")
)

// Error is the structured form. Callers use errors.As to reach Path,
// BaseRev, and CurrentRev, then retry against CurrentRev.
type Error struct {
	Code       string
	Path       string
	TargetPath string // rename only
	BaseRev    int64
	CurrentRev int64
	ExecID     string
	Err        error // cause, may be nil
}

func (e *Error) Error() string {
	msg := e.Code
	if e.Path != "" {
		msg += " " + e.Path
	}
	if e.Code == CodeConflict {
		msg += fmt.Sprintf(" (base %d, current %d)", e.BaseRev, e.CurrentRev)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap returns both the sentinel and the cause so that errors.Is works
// for the sentinel and errors.Is(err, io.EOF) style checks still reach the
// underlying error.
func (e *Error) Unwrap() []error {
	out := make([]error, 0, 2)
	if s := sentinelFor(e.Code); s != nil {
		out = append(out, s)
	}
	if e.Err != nil {
		out = append(out, e.Err)
	}
	return out
}

func sentinelFor(code string) error {
	switch code {
	case CodeNotFound:
		return ErrNotFound
	case CodeExist:
		return ErrExist
	case CodeNotDir:
		return ErrNotDir
	case CodeIsDir:
		return ErrIsDir
	case CodeNotEmpty:
		return ErrNotEmpty
	case CodeReadOnly:
		return ErrReadOnly
	case CodeInvalid:
		return ErrInvalid
	case CodePerm:
		return ErrPerm
	case CodeConflict:
		return ErrConflict
	case CodeExecLost:
		return ErrExecLost
	case CodeSyncLost:
		return ErrSyncLost
	case CodeClosed:
		return ErrClosed
	default:
		return nil
	}
}

func newError(code, path string) *Error {
	return &Error{Code: code, Path: path}
}

func wrapError(code, path string, err error) *Error {
	return &Error{Code: code, Path: path, Err: err}
}
