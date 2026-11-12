// Package vfs is the first step of the Go workspace: a SQLite-backed
// virtual filesystem with inode indirection, content-addressed chunks,
// and a monotonic revision counter.
//
// Design references, in read order:
//
//   - docs/architecture/computer.md §2 (invariants I1, I3, I5)
//   - docs/architecture/computer.md §4 (data model)
//   - docs/architecture/go-mapping.md §5 (step 1 of the landing order)
//   - docs/adr/0006-local-backend-sync-none-first.md
//
// What this package deliberately does not have yet:
//
//   - watermarks, push/pull, or any sync (ADR 0006: phase two)
//   - rename, remove, symlinks, mounts
//   - the FUSE mount (go-mapping §5 step 2)
//   - baseRev on directory operations (only WriteFile carries it)
//
// Writes are serialised through a single goroutine (go-mapping §6,
// "SQLite 并发"). Reads go straight to the pool under WAL.
package vfs
