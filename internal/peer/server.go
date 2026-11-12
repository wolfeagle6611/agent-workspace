package peer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

// Server answers requests against one store. Both sides run one: the host
// serves its authoritative store, the peer serves its mirror, so the same two
// operations cover push and pull.
//
// A server built with a Runner also runs commands. A server without one
// refuses exec requests, which is what a sync-only peer needs.
type Server struct {
	store *vfs.Store
	run   *Runner
}

// NewServer wraps a store that serves sync only.
func NewServer(store *vfs.Store) *Server {
	return &Server{store: store}
}

// NewServerWithRunner wraps a store that also runs commands.
func NewServerWithRunner(store *vfs.Store, run *Runner) *Server {
	return &Server{store: store, run: run}
}

// Serve reads requests until the connection ends.
//
// A refused request gets a failure frame and ends the session: a refusal means
// one side believes something the other does not, and continuing would let the
// two drift while both think they agree.
func (s *Server) Serve(ctx context.Context, conn net.Conn) error {
	w := &frameWriter{w: conn}
	for {
		msg, err := readFrame(conn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if err := s.dispatch(ctx, w, msg); err != nil {
			var refusal *refused
			if errors.As(err, &refusal) {
				_ = w.Frame(opFailure, Failure{Code: refusal.code, Message: refusal.err.Error()})
			}
			return err
		}
	}
}

// refused is a deliberate refusal, reported to the peer as a failure frame.
type refused struct {
	code string
	err  error
}

func (r *refused) Error() string { return r.err.Error() }
func (r *refused) Unwrap() error { return r.err }

func refuse(code, msg string) error {
	return &refused{code: code, err: errors.New(msg)}
}

func (s *Server) dispatch(ctx context.Context, w *frameWriter, msg Message) error {
	switch msg.Op {
	case opHello:
		return s.hello(ctx, w, msg)
	case opChanges:
		return s.changes(ctx, w, msg)
	case opMissing:
		return s.missing(ctx, w, msg)
	case opBlobs:
		return s.blobs(ctx, w, msg)
	case opStage:
		return s.stage(ctx, w, msg)
	case opApply:
		return s.apply(ctx, w, msg)
	case opPing:
		return s.ping(ctx, w)
	case opExec:
		return s.exec(ctx, w, msg)
	case opKill:
		return s.kill(ctx, w, msg)
	}
	return refuse(vfs.CodeInvalid, "unknown op "+msg.Op)
}

func (s *Server) hello(ctx context.Context, w *frameWriter, msg Message) error {
	var h Hello
	if err := json.Unmarshal(msg.Body, &h); err != nil {
		return refuse(vfs.CodeInvalid, "bad hello")
	}
	if h.Proto != protoVersion {
		return refuse(vfs.CodeInvalid, "protocol version "+strconv.Itoa(h.Proto)+" does not match "+strconv.Itoa(protoVersion))
	}
	if err := reconcile(ctx, s.store, h); err != nil {
		return err
	}
	own, err := s.store.SyncState(ctx)
	if err != nil {
		return err
	}
	rev, err := s.store.CurrentRev(ctx)
	if err != nil {
		return err
	}
	return w.Frame(opHello, Hello{
		Proto:      protoVersion,
		Generation: own.Generation,
		CurrentRev: rev,
		Applied:    toWireCursor(own.Applied),
	})
}

func (s *Server) changes(ctx context.Context, w *frameWriter, msg Message) error {
	var req changesRequest
	if err := json.Unmarshal(msg.Body, &req); err != nil {
		return refuse(vfs.CodeInvalid, "bad changes request")
	}
	changes, next, err := s.store.Changes(ctx, fromWireCursor(req.After), req.Bound, req.Limit)
	if err != nil {
		return err
	}
	return w.Frame(opChanges, changesResponse{Changes: toWireChanges(changes), Next: toWireCursor(next)})
}

func (s *Server) missing(ctx context.Context, w *frameWriter, msg Message) error {
	var req hashesRequest
	if err := json.Unmarshal(msg.Body, &req); err != nil {
		return refuse(vfs.CodeInvalid, "bad missing request")
	}
	missing, err := s.store.MissingBlobs(ctx, req.Hashes)
	if err != nil {
		return err
	}
	return w.Frame(opMissing, hashesResponse{Hashes: missing})
}

func (s *Server) blobs(ctx context.Context, w *frameWriter, msg Message) error {
	var req hashesRequest
	if err := json.Unmarshal(msg.Body, &req); err != nil {
		return refuse(vfs.CodeInvalid, "bad blobs request")
	}
	blobs, err := s.store.Blobs(ctx, req.Hashes)
	if err != nil {
		return err
	}
	return w.Frame(opBlobs, blobsResponse{Blobs: toWireBlobs(blobs)})
}

func (s *Server) stage(ctx context.Context, w *frameWriter, msg Message) error {
	var req stageRequest
	if err := json.Unmarshal(msg.Body, &req); err != nil {
		return refuse(vfs.CodeInvalid, "bad stage request")
	}
	if err := s.store.StageBlobs(ctx, fromWireBlobs(req.Blobs)); err != nil {
		return err
	}
	return w.Frame(opStage, ack{})
}

func (s *Server) apply(ctx context.Context, w *frameWriter, msg Message) error {
	var req applyRequest
	if err := json.Unmarshal(msg.Body, &req); err != nil {
		return refuse(vfs.CodeInvalid, "bad apply request")
	}
	rev, changed, err := s.store.Apply(ctx, fromWireChanges(req.Changes), fromWireCursor(req.UpTo))
	if err != nil {
		return err
	}
	return w.Frame(opApply, appliedResponse{Rev: rev, Changed: changed})
}

// ping reports the applied cursor, so a sender can assert its watermark covers
// what it just sent (computer.md §5.1: a regressed watermark must not pass
// silently).
func (s *Server) ping(ctx context.Context, w *frameWriter) error {
	st, err := s.store.SyncState(ctx)
	if err != nil {
		return err
	}
	return w.Frame(opPing, cursorResponse{Applied: toWireCursor(st.Applied)})
}

func (s *Server) exec(ctx context.Context, w *frameWriter, msg Message) error {
	if s.run == nil {
		return refuse(vfs.CodeInvalid, "this peer runs no commands")
	}
	var req ExecRequest
	if err := json.Unmarshal(msg.Body, &req); err != nil {
		return refuse(vfs.CodeInvalid, "bad exec request")
	}
	return s.run.Exec(ctx, w, req)
}

func (s *Server) kill(ctx context.Context, w *frameWriter, msg Message) error {
	var req killRequest
	if err := json.Unmarshal(msg.Body, &req); err != nil {
		return refuse(vfs.CodeInvalid, "bad kill request")
	}
	if s.run == nil || !s.run.Kill(req.ExecID) {
		// The id belongs to a process this peer no longer has. Saying "lost"
		// is different from saying "unknown" (go-mapping §6).
		return refuse(vfs.CodeExecLost, "execution lost: "+req.ExecID)
	}
	return w.Frame(opKill, ack{})
}
