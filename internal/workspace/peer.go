// This file wires the workspace to a mirror over the sync protocol: the push
// that gates an execution, the pull that drains it, and the session between
// them.
//
// Order matters and lives here, not at the call sites (ADR 0008):
//
//	push → command → pull          one execution
//	pull → push                    after a reconnect
//
// The reconnect order is the surprising one: the mirror may hold writes from
// the last command that never came back, and pushing first would overwrite
// them with this side's older state.
package workspace

import (
	"context"
	"fmt"
	"net"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/peer"
)

// dialer opens the link to the mirror. 6b has one implementation, the unix
// socket; a remote mirror swaps it without touching a frame or a cursor.
type dialer func(ctx context.Context) (net.Conn, error)

// unixDial dials a unix socket path.
func unixDial(socket string) dialer {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}
}

// push sends local changes to the mirror.
//
// It is the safety gate, not an optimisation: a failing push stops the
// execution before spawn, so a command never looks at a stale view
// (computer.md §5.1).
func (w *Workspace) push(ctx context.Context) error {
	if w.dial == nil {
		return nil
	}
	_, err := w.withPeer(ctx, func(c *peer.Client) (peer.Outcome, error) {
		return c.Push(ctx)
	})
	return err
}

// pull reads the mirror's changes back.
func (w *Workspace) pull(ctx context.Context) error {
	if w.dial == nil {
		return nil
	}
	_, err := w.withPeer(ctx, func(c *peer.Client) (peer.Outcome, error) {
		return c.Pull(ctx)
	})
	return err
}

// Push sends local changes to the mirror. It does nothing without a peer.
func (w *Workspace) Push(ctx context.Context) (peer.Outcome, error) {
	if err := w.ensureOpen(); err != nil {
		return peer.Outcome{}, err
	}
	if w.dial == nil {
		return peer.Outcome{}, nil
	}
	return w.withPeer(ctx, func(c *peer.Client) (peer.Outcome, error) {
		return c.Push(ctx)
	})
}

// Pull reads the mirror's changes. It does nothing without a peer.
func (w *Workspace) Pull(ctx context.Context) (peer.Outcome, error) {
	if err := w.ensureOpen(); err != nil {
		return peer.Outcome{}, err
	}
	if w.dial == nil {
		return peer.Outcome{}, nil
	}
	return w.withPeer(ctx, func(c *peer.Client) (peer.Outcome, error) {
		return c.Pull(ctx)
	})
}

// withPeer runs fn on a live session and retries once on a fresh one.
//
// The retry is safe because both directions are state-based: a repeat writes
// nothing it did not already write. computer.md §5.5 asks for exactly one
// retry, and a second failure is reported.
func (w *Workspace) withPeer(ctx context.Context, fn func(*peer.Client) (peer.Outcome, error)) (peer.Outcome, error) {
	w.syncMu.Lock()
	defer w.syncMu.Unlock()

	out, err, retry := w.attempt(ctx, fn)
	if err == nil || !retry {
		return out, err
	}
	out, err, _ = w.attempt(ctx, fn)
	return out, err
}

// attempt runs fn once. retry reports whether a fresh session could help: a
// failed dial is final, a broken session is not.
func (w *Workspace) attempt(ctx context.Context, fn func(*peer.Client) (peer.Outcome, error)) (peer.Outcome, error, bool) {
	c, fresh, err := w.peerSession(ctx)
	if err != nil {
		return peer.Outcome{}, err, false
	}
	if fresh {
		// A reconnect drains the mirror before anything is pushed over it.
		if _, err := c.Pull(ctx); err != nil {
			w.dropPeer()
			return peer.Outcome{}, err, true
		}
	}
	out, err := fn(c)
	if err != nil {
		// One session, one consumer, one failure: the next attempt redials
		// rather than trusting a pipe that may hold half a frame.
		w.dropPeer()
		return out, err, true
	}
	return out, nil, false
}

// peerSession returns the cached client, dialing and handshaking when needed.
// fresh reports a session this call established over a previous one.
func (w *Workspace) peerSession(ctx context.Context) (*peer.Client, bool, error) {
	if w.peer != nil {
		return w.peer, false, nil
	}
	conn, err := w.dial(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("dial mirror: %w", err)
	}
	c, err := peer.Open(ctx, conn, w.store)
	if err != nil {
		return nil, false, fmt.Errorf("open mirror: %w", err)
	}
	reconnected := w.everConnected
	w.peer = c
	w.everConnected = true
	return c, reconnected, nil
}

// dropPeer closes and forgets the session.
func (w *Workspace) dropPeer() {
	if w.peer == nil {
		return
	}
	_ = w.peer.Close()
	w.peer = nil
}
