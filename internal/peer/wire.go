// Package peer carries the two protocols between a host store and a peer that
// keeps its own mirror: sync (push / pull) and remote execution.
//
// Shape and rules come from docs/adr/0008-second-store-shape.md:
//
//   - length-prefixed JSON frames, one message per frame
//   - object bytes travel as frame payload, never padded or escaped
//   - each side runs the same server on its own store, so push and pull are the
//     same two operations mirrored
//   - the protocol has no operation codes for the filesystem: it moves state
//
// The wire types below are this package's own. They are converted from
// internal/vfs on the way out and back on the way in, so a rename inside the
// store cannot silently become a protocol change.
//
// The transport is not part of this package. 6a drives it over net.Pipe; 6b
// dials a unix socket, and a remote peer can bring back reverse dialling
// without touching a frame.
package peer

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

// protoVersion is the wire contract. Both sides are released together (ADR
// 0003), so a mismatch is an operator error and fails fast with EINVAL.
const protoVersion = 1

// maxFrame bounds one message. A stage request is chunked by bytes, so the
// frame stays under this even for a large file.
const maxFrame = 8 << 20

// stageBudget is how many blob bytes one stage request may carry.
const stageBudget = 4 << 20

// Batch is the page size the sender reads changes in (computer.md §5.1).
const Batch = 256

// Ops. One request, one response shape.
const (
	opHello   = "hello"
	opChanges = "changes"
	opMissing = "missing"
	opBlobs   = "blobs"
	opStage   = "stage"
	opApply   = "apply"
	opPing    = "ping"
	opFailure = "failure"

	// Remote execution. One exec request streams stdout / stderr frames and
	// ends with exactly one exit frame.
	opExec   = "exec"
	opKill   = "kill"
	opRun    = "run"
	opStdout = "stdout"
	opStderr = "stderr"
	opExit   = "exit"
)

// Message is one frame: an op name and its JSON body.
type Message struct {
	Op   string          `json:"op"`
	Body json.RawMessage `json:"body,omitempty"`
}

// Hello is the first exchange. Each side reports its identity, its current
// rev, and how far the peer's changes have been applied here.
type Hello struct {
	Proto      int        `json:"proto"`
	Generation string     `json:"generation"`
	CurrentRev int64      `json:"current_rev"`
	Applied    wireCursor `json:"applied"`
}

// ExecRequest starts one command on the peer. Dir is a workspace path: the
// peer resolves it through its own mount, because a real path means nothing
// across a transport (go-mapping §3.2).
type ExecRequest struct {
	ExecID string            `json:"exec_id"`
	Source string            `json:"source"`
	Dir    string            `json:"dir"`
	Env    map[string]string `json:"env,omitempty"`
}

// Failure is the reply to a request this side refuses.
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Execution status values, shared by both sides.
const (
	StatusCompleted = 0
	StatusFailed    = 1
	StatusCancelled = 2
)

// ExecStatus is the single exit frame of one execution.
type ExecStatus struct {
	Status   int    `json:"status"`
	ExitCode int    `json:"exit_code"`
	Err      string `json:"err,omitempty"`
}

// wireCursor is a position in a change stream.
type wireCursor struct {
	Rev  int64  `json:"rev"`
	Path string `json:"path"`
}

// wireRef names one chunk.
type wireRef struct {
	Hash [32]byte `json:"hash"`
	Size int64    `json:"size"`
}

// wireChange is one change entry.
type wireChange struct {
	Rev    int64     `json:"rev"`
	Path   string    `json:"path"`
	Kind   string    `json:"kind"`
	Mode   uint32    `json:"mode"`
	MTime  int64     `json:"mtime"`
	Chunks []wireRef `json:"chunks,omitempty"`
}

// wireBlob is one content-addressed object.
type wireBlob struct {
	Hash [32]byte `json:"hash"`
	Data []byte   `json:"data"`
}

type changesRequest struct {
	After wireCursor `json:"after"`
	Bound int64      `json:"bound"`
	Limit int        `json:"limit"`
}

type changesResponse struct {
	Changes []wireChange `json:"changes"`
	Next    wireCursor   `json:"next"`
}

type hashesRequest struct {
	Hashes [][32]byte `json:"hashes"`
}

type hashesResponse struct {
	Hashes [][32]byte `json:"hashes"`
}

type blobsResponse struct {
	Blobs []wireBlob `json:"blobs"`
}

type stageRequest struct {
	Blobs []wireBlob `json:"blobs"`
}

type applyRequest struct {
	Changes []wireChange `json:"changes"`
	UpTo    wireCursor   `json:"up_to"`
}

type appliedResponse struct {
	Rev     int64 `json:"rev"`
	Changed int   `json:"changed"`
}

type pingRequest struct {
	UpTo wireCursor `json:"up_to"`
}

type cursorResponse struct {
	Applied wireCursor `json:"applied"`
}

type killRequest struct {
	ExecID string `json:"exec_id"`
}

// streamChunk is one piece of output. encoding/json writes a []byte as base64:
// object bytes travel raw (they are the bulk), command output goes through the
// frame codec.
type streamChunk struct {
	Data []byte `json:"data"`
}

type ack struct{}

// toWireCursor converts a store cursor.
func toWireCursor(c vfs.Cursor) wireCursor {
	return wireCursor{Rev: c.Rev, Path: c.Path}
}

// fromWireCursor converts a wire cursor.
func fromWireCursor(c wireCursor) vfs.Cursor {
	return vfs.Cursor{Rev: c.Rev, Path: c.Path}
}

// toWireChanges converts changes on the way out.
func toWireChanges(in []vfs.Change) []wireChange {
	if len(in) == 0 {
		return nil
	}
	out := make([]wireChange, len(in))
	for i, ch := range in {
		out[i] = wireChange{
			Rev:    ch.Rev,
			Path:   ch.Path,
			Kind:   string(ch.Kind),
			Mode:   ch.Mode,
			MTime:  ch.MTime,
			Chunks: toWireRefs(ch.Chunks),
		}
	}
	return out
}

// fromWireChanges converts changes on the way in.
func fromWireChanges(in []wireChange) []vfs.Change {
	if len(in) == 0 {
		return nil
	}
	out := make([]vfs.Change, len(in))
	for i, ch := range in {
		out[i] = vfs.Change{
			Rev:    ch.Rev,
			Path:   ch.Path,
			Kind:   vfs.ChangeKind(ch.Kind),
			Mode:   ch.Mode,
			MTime:  ch.MTime,
			Chunks: fromWireRefs(ch.Chunks),
		}
	}
	return out
}

func toWireRefs(in []vfs.ChunkRef) []wireRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]wireRef, len(in))
	for i, ref := range in {
		out[i] = wireRef{Hash: ref.Hash, Size: ref.Size}
	}
	return out
}

func fromWireRefs(in []wireRef) []vfs.ChunkRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]vfs.ChunkRef, len(in))
	for i, ref := range in {
		out[i] = vfs.ChunkRef{Hash: ref.Hash, Size: ref.Size}
	}
	return out
}

// toWireBlobs converts objects on the way out.
func toWireBlobs(in []vfs.Blob) []wireBlob {
	if len(in) == 0 {
		return nil
	}
	out := make([]wireBlob, len(in))
	for i, b := range in {
		out[i] = wireBlob{Hash: b.Hash, Data: b.Data}
	}
	return out
}

// fromWireBlobs converts objects on the way in.
func fromWireBlobs(in []wireBlob) []vfs.Blob {
	if len(in) == 0 {
		return nil
	}
	out := make([]vfs.Blob, len(in))
	for i, b := range in {
		out[i] = vfs.Blob{Hash: b.Hash, Data: b.Data}
	}
	return out
}

// frameWriter serialises frames on one connection. Two output streams and the
// exit frame write concurrently, and a frame must not interleave with another.
type frameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (f *frameWriter) Frame(op string, body any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return writeFrame(f.w, op, body)
}

// writeFrame writes one message as a single Write call. Single calls matter: a
// half-written frame is exactly the fault the breakpoint matrix injects.
func writeFrame(w io.Writer, op string, body any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("encode %s: %w", op, err)
		}
	}
	msg, err := json.Marshal(Message{Op: op, Body: payload})
	if err != nil {
		return fmt.Errorf("encode %s: %w", op, err)
	}
	if len(msg) > maxFrame {
		return fmt.Errorf("encode %s: %d bytes over the %d frame limit", op, len(msg), maxFrame)
	}
	buf := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(buf, uint32(len(msg)))
	copy(buf[4:], msg)
	if _, err := w.Write(buf); err != nil {
		return err
	}
	return nil
}

// readFrame reads one message.
func readFrame(r io.Reader) (Message, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return Message{}, err
	}
	n := binary.BigEndian.Uint32(head[:])
	if n > maxFrame {
		return Message{}, fmt.Errorf("frame of %d bytes over the %d limit", n, maxFrame)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return Message{}, err
	}
	var msg Message
	if err := json.Unmarshal(buf, &msg); err != nil {
		return Message{}, fmt.Errorf("decode frame: %w", err)
	}
	return msg, nil
}

// reconcile records the peer's generation and voids this side's pull cursor
// when the peer's rev space restarted (ADR 0008).
//
// Only the pull cursor is really lost: a fresh peer also starts its applied
// cursor at zero, so a full resend follows without anyone asking for it.
func reconcile(ctx context.Context, s *vfs.Store, remote Hello) error {
	st, err := s.SyncState(ctx)
	if err != nil {
		return err
	}
	if st.PeerGeneration == remote.Generation && st.Generation != "" {
		return nil
	}
	if err := s.SetPeerGeneration(ctx, remote.Generation); err != nil {
		return err
	}
	return s.ResetSync(ctx)
}
