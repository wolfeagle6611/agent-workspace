package peer

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

// Client drives one peer session against the local store.
//
// It owns two pieces of state across calls: the peer's handshake (its rev
// bound and generation) and how far the peer has applied this side's changes.
// Both are re-established by every Handshake, which is what makes a reconnect
// a resume rather than a guess.
type Client struct {
	conn  net.Conn
	store *vfs.Store
	limit int

	remote      Hello
	peerApplied vfs.Cursor
}

// Open connects to a serving peer and completes the handshake.
//
// There is no way to build a client without one: Pull reads the peer's rev
// bound out of the handshake, so a client that skipped it would silently pull
// nothing.
func Open(ctx context.Context, conn net.Conn, store *vfs.Store) (*Client, error) {
	c := &Client{conn: conn, store: store, limit: Batch}
	if err := c.Handshake(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// Close ends the session. It does not close the store.
func (c *Client) Close() error { return c.conn.Close() }

// Handshake exchanges identities and reconciles watermarks.
func (c *Client) Handshake(ctx context.Context) error {
	own, err := c.store.SyncState(ctx)
	if err != nil {
		return err
	}
	rev, err := c.store.CurrentRev(ctx)
	if err != nil {
		return err
	}
	sent := Hello{Proto: protoVersion, Generation: own.Generation, CurrentRev: rev, Applied: toWireCursor(own.Applied)}

	var got Hello
	if err := c.call(ctx, opHello, sent, &got); err != nil {
		return err
	}
	if err := reconcile(ctx, c.store, got); err != nil {
		return err
	}
	c.remote = got
	c.peerApplied = fromWireCursor(got.Applied)
	return nil
}

// Outcome is what one push or pull did.
type Outcome struct {
	Changes int   // entries read from the sender
	Writes  int   // entries that changed the receiver
	Rev     int64 // the receiver's last minted rev, zero when nothing wrote
}

// Push sends every change the peer has not applied yet.
//
// Two round trips per probe: hashes first, bytes only for what is missing
// (computer.md §5.1). Bytes travel before the change that names them, which is
// why an interrupted push leaves unreferenced blobs behind for GC.
func (c *Client) Push(ctx context.Context) (Outcome, error) {
	var out Outcome
	bound, err := c.store.CurrentRev(ctx)
	if err != nil {
		return out, err
	}
	at := c.peerApplied
	for {
		// The sender coalesces its own log locally; only the entries and the
		// missing-bytes probe cross the link.
		changes, next, err := c.store.Changes(ctx, at, bound, c.limit)
		if err != nil {
			return out, err
		}
		if next == at {
			break
		}
		if len(changes) > 0 {
			applied, err := c.send(ctx, changes, next)
			if err != nil {
				return out, err
			}
			out.Changes += len(changes)
			out.Writes += applied.Changed
			if applied.Rev != 0 {
				out.Rev = applied.Rev
			}
		}
		at = next
	}
	// One round trip even when nothing was sent: the peer reports how far it
	// has applied, and a dead peer or a regressed watermark fails the bracket
	// here, not after a command has already run (computer.md §5.1).
	var confirmed cursorResponse
	if err := c.call(ctx, opPing, pingRequest{UpTo: toWireCursor(at)}, &confirmed); err != nil {
		return out, err
	}
	if at.After(fromWireCursor(confirmed.Applied)) {
		return out, fmt.Errorf("peer watermark regressed: applied %+v, sent %+v", confirmed.Applied, at)
	}
	c.peerApplied = at
	if err := c.store.SetPushRev(ctx, at.Rev); err != nil {
		return out, err
	}
	return out, nil
}

// send probes the peer for missing chunks, stages them, then applies.
func (c *Client) send(ctx context.Context, changes []vfs.Change, upTo vfs.Cursor) (appliedResponse, error) {
	var zero appliedResponse

	hashes := chunkHashes(changes)
	if len(hashes) > 0 {
		var probe hashesResponse
		if err := c.call(ctx, opMissing, hashesRequest{Hashes: hashes}, &probe); err != nil {
			return zero, err
		}
		if err := c.stage(ctx, probe.Hashes); err != nil {
			return zero, err
		}
	}

	var applied appliedResponse
	req := applyRequest{Changes: toWireChanges(changes), UpTo: toWireCursor(upTo)}
	if err := c.call(ctx, opApply, req, &applied); err != nil {
		return zero, err
	}
	return applied, nil
}

// stage sends the missing bytes in budget-sized requests.
func (c *Client) stage(ctx context.Context, missing [][32]byte) error {
	for len(missing) > 0 {
		blobs, err := c.store.Blobs(ctx, missing)
		if err != nil {
			return err
		}
		if len(blobs) == 0 {
			return fmt.Errorf("peer asked for %d chunks this store does not hold", len(missing))
		}
		batch, _ := takeBytes(blobs, stageBudget)
		var ack ack
		if err := c.call(ctx, opStage, stageRequest{Blobs: toWireBlobs(batch)}, &ack); err != nil {
			return err
		}
		missing = without(missing, batch)
	}
	return nil
}

// Pull reads every change the peer has past the local fetch cursor.
func (c *Client) Pull(ctx context.Context) (Outcome, error) {
	var out Outcome
	bound := c.remote.CurrentRev

	st, err := c.store.SyncState(ctx)
	if err != nil {
		return out, err
	}
	at := st.Fetch
	for {
		var page changesResponse
		req := changesRequest{After: toWireCursor(at), Bound: bound, Limit: c.limit}
		if err := c.call(ctx, opChanges, req, &page); err != nil {
			return out, err
		}
		next := fromWireCursor(page.Next)
		if next == at {
			break
		}
		changes := fromWireChanges(page.Changes)
		if len(changes) > 0 {
			if err := c.receive(ctx, changes); err != nil {
				return out, err
			}
			rev, changed, err := c.store.Apply(ctx, changes, next)
			if err != nil {
				return out, err
			}
			out.Changes += len(changes)
			out.Writes += changed
			if rev != 0 {
				out.Rev = rev
			}
		}
		// The cursor commits after the batch applies, so an interrupted pull
		// replays at most one page.
		if err := c.store.SetFetchCursor(ctx, next); err != nil {
			return out, err
		}
		at = next
	}
	return out, nil
}

// receive asks the peer for the bytes this store lacks and stages them.
func (c *Client) receive(ctx context.Context, changes []vfs.Change) error {
	hashes := chunkHashes(changes)
	if len(hashes) == 0 {
		return nil
	}
	missing, err := c.store.MissingBlobs(ctx, hashes)
	if err != nil {
		return err
	}
	for len(missing) > 0 {
		probe := missing
		if len(probe) > c.limit {
			probe = probe[:c.limit]
		}
		var got blobsResponse
		if err := c.call(ctx, opBlobs, hashesRequest{Hashes: probe}, &got); err != nil {
			return err
		}
		if len(got.Blobs) == 0 {
			return fmt.Errorf("peer holds none of the %d chunks asked for", len(probe))
		}
		blobs := fromWireBlobs(got.Blobs)
		if err := c.store.StageBlobs(ctx, blobs); err != nil {
			return err
		}
		missing = without(missing, blobs)
	}
	return nil
}

// call sends one request and decodes one response.
func (c *Client) call(ctx context.Context, op string, req, resp any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeFrame(c.conn, op, req); err != nil {
		return err
	}
	msg, err := readFrame(c.conn)
	if err != nil {
		return err
	}
	if msg.Op == opFailure {
		var f Failure
		if err := json.Unmarshal(msg.Body, &f); err != nil {
			return fmt.Errorf("peer refused %s: %s", op, string(msg.Body))
		}
		return &Refusal{Op: op, Code: f.Code, Message: f.Message}
	}
	if msg.Op != op {
		return fmt.Errorf("peer answered %s with %s", op, msg.Op)
	}
	if resp == nil {
		return nil
	}
	if err := json.Unmarshal(msg.Body, resp); err != nil {
		return fmt.Errorf("decode %s response: %w", op, err)
	}
	return nil
}

// Refusal is the peer rejecting a request on purpose, as opposed to a broken
// connection. Callers do not retry these.
type Refusal struct {
	Op      string
	Code    string
	Message string
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("%s refused: %s: %s", r.Op, r.Code, r.Message)
}

// chunkHashes lists every chunk hash a set of changes names, once.
func chunkHashes(changes []vfs.Change) [][32]byte {
	seen := make(map[[32]byte]bool)
	var out [][32]byte
	for _, ch := range changes {
		for _, ref := range ch.Chunks {
			if seen[ref.Hash] {
				continue
			}
			seen[ref.Hash] = true
			out = append(out, ref.Hash)
		}
	}
	return out
}

// takeBytes splits blobs at a byte budget.
func takeBytes(blobs []vfs.Blob, budget int) (take, rest []vfs.Blob) {
	n := 0
	for i, b := range blobs {
		if n > 0 && n+len(b.Data) > budget {
			return blobs[:i], blobs[i:]
		}
		n += len(b.Data)
	}
	return blobs, nil
}

// without drops the hashes that came back, keeping the order.
func without(missing [][32]byte, got []vfs.Blob) [][32]byte {
	if len(got) == 0 {
		return missing
	}
	have := make(map[[32]byte]bool, len(got))
	for _, b := range got {
		have[b.Hash] = true
	}
	out := make([][32]byte, 0, len(missing))
	for _, h := range missing {
		if !have[h] {
			out = append(out, h)
		}
	}
	return out
}
