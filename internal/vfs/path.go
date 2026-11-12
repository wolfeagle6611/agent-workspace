package vfs

import (
	"path"
	"strings"
)

// Clean validates and canonicalises an absolute workspace path.
//
// Rules (docs/architecture/computer.md §4, and the upstream conventions):
//   - must start with "/"
//   - must not contain a NUL byte
//   - must not contain a ".." segment
//   - "." segments are dropped
//   - no trailing slash, except for the root
func Clean(p string) (string, error) {
	if p == "" || p[0] != '/' {
		return "", newError(CodeInvalid, p)
	}
	if strings.ContainsRune(p, 0) {
		return "", newError(CodeInvalid, p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", newError(CodeInvalid, p)
		}
	}
	clean := path.Clean(p)
	if clean != "/" && strings.HasSuffix(clean, "/") {
		clean = strings.TrimSuffix(clean, "/")
	}
	return clean, nil
}

// segments splits a cleaned path into its components. The root yields none.
func segments(clean string) []string {
	if clean == "/" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(clean, "/"), "/")
}

// parentOf returns the directory path containing clean, and its base name.
// For "/a" it returns "/", "a".
func parentOf(clean string) (dir, base string) {
	i := strings.LastIndex(clean, "/")
	if i == 0 {
		return "/", clean[1:]
	}
	return clean[:i], clean[i+1:]
}
