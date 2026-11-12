package vfs

import (
	"errors"
	"fmt"
)

// Error codes. Same strings as the Cloudflare Computer wire vocabulary so
// logs and tests read the same across both systems. ECONFLICT is ours: the
// upstream system has no baseRev.
// See docs/architecture/go-mapping.md §7.
//
// The execution surface (internal/workspace, internal/runtime) reports the
// same codes through the same type, so callers use errors.As once. This
// package is where the codes live today, not where all of them belong.
const (
	CodeNotFound  = "ENOENT"
	CodeNoBackend = "ENOBACKEND"
	CodeExist     = "EEXIST"
	CodeNotDir    = "ENOTDIR"
	CodeIsDir     = "EISDIR"
	CodeNotEmpty  = "ENOTEMPTY"
	CodeReadOnly  = "EROFS"
	CodeInvalid   = "EINVAL"
	CodePerm      = "EPERM"
	CodeConflict  = "ECONFLICT"
	CodeExecLost  = "EEXEC_LOST"
	CodeSyncLost  = "ESYNC_LOST"
	CodeClosed    = "ECLOSED"
)

// Sentinels exist so callers can write errors.Is(err, vfs.ErrConflict)
// without parsing strings.
var (
	ErrNotFound  = errors.New("not found")
	ErrExist     = errors.New("already exists")
	ErrNotDir    = errors.New("not a directory")
	ErrIsDir     = errors.New("is a directory")
	ErrNotEmpty  = errors.New("directory not empty")
	ErrReadOnly  = errors.New("read-only")
	ErrInvalid   = errors.New("invalid argument")
	ErrNoBackend = errors.New("unknown backend")
	ErrPerm      = errors.New("operation not permitted")
	ErrConflict  = errors.New("version conflict")
	ErrExecLost  = errors.New("execution lost")
	ErrSyncLost  = errors.New("sync lost")
	ErrClosed    = errors.New("store closed")
)

// Error is the structured form. Callers use errors.As to reach Path,
// BaseRev, and CurrentRev, then retry against CurrentRev.
//
// Code decides which fields carry meaning; every other field stays zero.
// Read the fields by Code, never by presence:
//
//	ENOENT, ENOTDIR, EISDIR, ENOTEMPTY, EROFS, EINVAL, EPERM  Path
//	EEXIST                                                  Path, TargetPath
//	ECONFLICT                                               Path, BaseRev, CurrentRev
//	ENOBACKEND                                              Path (the id)
//	EEXEC_LOST                                              ExecID
//	ESYNC_LOST                                              RuntimeIDs (phase two)
//
// A rename reports the source in Path. When the error names the target, it
// also sets TargetPath, and BaseRev / CurrentRev describe the target node
// (go-mapping §7).
type Error struct {
	Code       string
	Path       string
	TargetPath string // rename only: the target this error names
	BaseRev    int64
	CurrentRev int64
	ExecID     string
	RuntimeIDs []string
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
	case CodeNoBackend:
		return ErrNoBackend
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
	return NewError(code, path)
}

func wrapError(code, path string, err error) *Error {
	return WrapError(code, path, err)
}

// NewError builds a coded error without a cause. It is exported because the
// execution surface reports the same codes to the same callers.
func NewError(code, path string) *Error {
	return &Error{Code: code, Path: path}
}

// WrapError builds a coded error that keeps err reachable through Unwrap.
func WrapError(code, path string, err error) *Error {
	return &Error{Code: code, Path: path, Err: err}
}
