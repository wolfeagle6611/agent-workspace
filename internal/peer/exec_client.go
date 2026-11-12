package peer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

// ExecClient drives one remote execution over a dedicated connection.
//
// One connection per execution: the exec stream is a single request with many
// frames, and a kill travels on a second connection so it never has to
// interleave with that stream.
type ExecClient struct {
	conn    net.Conn
	control func(ctx context.Context) (net.Conn, error)
	id      string
}

// NewExecClient wraps a connection to the peer's exec service. control opens
// a short-lived connection for kills.
func NewExecClient(conn net.Conn, control func(ctx context.Context) (net.Conn, error)) *ExecClient {
	return &ExecClient{conn: conn, control: control}
}

// Close ends the connection. The command, if still running, is cancelled by
// the peer when its output can no longer be delivered.
func (c *ExecClient) Close() error { return c.conn.Close() }

// Start sends the request and waits for the run frame.
//
// The interface has two outcomes, not three: either the command started, or
// Start returns an error saying why it did not. A command that never started is
// a start failure, never a result.
func (c *ExecClient) Start(ctx context.Context, req ExecRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.id = req.ExecID
	if err := writeFrame(c.conn, opExec, req); err != nil {
		return err
	}
	msg, err := readFrame(c.conn)
	if err != nil {
		return err
	}
	switch msg.Op {
	case opRun:
		return nil
	case opExit:
		var status ExecStatus
		if err := json.Unmarshal(msg.Body, &status); err != nil {
			return fmt.Errorf("decode exit frame: %w", err)
		}
		return &StartError{Status: status}
	case opFailure:
		return refusal(opExec, msg)
	}
	return fmt.Errorf("peer answered exec with %s", msg.Op)
}

// StartError is the peer starting nothing: the command never ran, and the
// reason travels in the status.
type StartError struct {
	Status ExecStatus
}

func (e *StartError) Error() string {
	if e.Status.Err != "" {
		return e.Status.Err
	}
	return "the peer did not start the command"
}

// Wait streams the output and returns the exit status.
//
// Cancelling ctx sends a kill and keeps reading: the peer reports how the
// command ended, so a cancelled execution still carries a status and a code.
func (c *ExecClient) Wait(ctx context.Context, stdout, stderr io.Writer) (ExecStatus, error) {
	var status ExecStatus
	stop := context.AfterFunc(ctx, func() {
		// A finished command has no id to kill; that refusal is not an error.
		_ = c.Kill(context.WithoutCancel(ctx), c.id)
	})
	defer stop()

	for {
		msg, err := readFrame(c.conn)
		if err != nil {
			return status, err
		}
		switch msg.Op {
		case opStdout, opStderr:
			var chunk streamChunk
			if err := json.Unmarshal(msg.Body, &chunk); err != nil {
				return status, fmt.Errorf("decode %s frame: %w", msg.Op, err)
			}
			out := stdout
			if msg.Op == opStderr {
				out = stderr
			}
			if _, err := out.Write(chunk.Data); err != nil {
				return status, fmt.Errorf("write %s: %w", msg.Op, err)
			}
		case opExit:
			if err := json.Unmarshal(msg.Body, &status); err != nil {
				return status, fmt.Errorf("decode exit frame: %w", err)
			}
			return status, nil
		case opFailure:
			return status, refusal(opExec, msg)
		default:
			return status, fmt.Errorf("unexpected %s frame during exec", msg.Op)
		}
	}
}

// Kill cancels one execution on the peer. An id the peer no longer knows is
// already gone, so EEXEC_LOST is not an error here (go-mapping §6).
func (c *ExecClient) Kill(ctx context.Context, id string) error {
	if id == "" || c.control == nil {
		return nil
	}
	conn, err := c.control(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := writeFrame(conn, opKill, killRequest{ExecID: id}); err != nil {
		return err
	}
	msg, err := readFrame(conn)
	if err != nil {
		return err
	}
	if msg.Op == opFailure {
		var f Failure
		if err := json.Unmarshal(msg.Body, &f); err != nil {
			return fmt.Errorf("peer refused kill")
		}
		if f.Code == vfs.CodeExecLost {
			return nil
		}
		return &Refusal{Op: opKill, Code: f.Code, Message: f.Message}
	}
	return nil
}

// refusal decodes a failure frame.
func refusal(op string, msg Message) error {
	var f Failure
	if err := json.Unmarshal(msg.Body, &f); err != nil {
		return fmt.Errorf("peer refused %s", op)
	}
	return &Refusal{Op: op, Code: f.Code, Message: f.Message}
}
