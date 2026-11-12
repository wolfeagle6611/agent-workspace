// Package vfs is the first step of the Go workspace: a SQLite-backed
// virtual filesystem with inode indirection, content-addressed chunks,
// and a monotonic revision counter.
//
// Design references, in read order:
//
//   - docs/architecture/computer.md §2 (invariants I1, I3, I5)
//   - docs/architecture/computer.md §4 (data model)
//   - docs/architecture/go-mapping.md §5 (steps 1, 5, and 6a of the landing order)
//   - docs/adr/0006-local-backend-sync-none-first.md
//   - docs/adr/0008-second-store-shape.md
//
// What this package deliberately does not have yet:
//
//   - symlinks and mounts
//   - the wire protocol, the peer process, and the push/pull orchestration.
//     The store side of 6a lives here: the change log, the watermarks, the
//     coalescing read, and apply.
//
// Watermarks, push and pull are not features of a store on its own. A store
// can list its changes, apply a batch, and hold the bookkeeping; only a peer
// makes them a protocol (ADR 0008).
//
// Every mutation takes a *MutateOptions precondition, so a caller that read a
// revision can write only if that revision still holds. exec and FUSE writers
// pass nil and always win: see ADR 0007 and go-mapping §6.
//
// Writes are serialised through a single goroutine (go-mapping §6,
// "SQLite 并发"). Reads go straight to the pool under WAL.
package vfs
