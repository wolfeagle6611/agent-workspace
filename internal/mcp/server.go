// Package mcp exposes a workspace to an agent over the Model Context
// Protocol, using the stdio transport.
//
// File semantics stay in the workspace. What lives here is what a model sees:
// the five tools, how arguments become workspace calls, how results are
// bounded and formatted, and how errors become text. That translation is the
// whole job; storage rules stay out (go-mapping §5.1).
//
// Requests are answered one at a time, so a long exec delays later calls on
// the same connection. MCP admits concurrent ids; this server does not use
// them, because one agent drives one workspace.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/workspace"
)

// Supported protocol versions. A client that asks for one of these gets it
// back; anything else gets fallbackVersion.
const fallbackVersion = "2025-06-18"

var supportedVersions = map[string]bool{
	"2025-11-25": true,
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

// Server answers MCP requests for one workspace.
//
// One server is one workspace: the database is fixed when the process starts,
// so no tool takes a workspace argument (go-mapping §5.1).
type Server struct {
	ws    *workspace.Workspace
	tools []tool
}

// NewServer builds a server over ws.
func NewServer(ws *workspace.Workspace) *Server {
	return &Server{ws: ws, tools: tools(ws)}
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// toolResult is what tools/call returns. Errors are results, not protocol
// errors: the model has to read the code and the fields to correct itself
// (go-mapping §5.1).
type toolResult struct {
	Content []contentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Serve answers requests until the input ends. Messages are JSON values, so
// both newline-delimited and concatenated framing work.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	dec := json.NewDecoder(in)
	enc := json.NewEncoder(out)
	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		resp, ok := s.handle(ctx, req)
		if !ok {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
}

// handle answers one request. The second result is false for notifications,
// which take no reply.
func (s *Server) handle(ctx context.Context, req request) (response, bool) {
	if len(req.ID) == 0 {
		return response{}, false
	}
	res := response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		res.Result = initializeResult(req.Params)
	case "ping", "shutdown":
		res.Result = struct{}{}
	case "tools/list":
		res.Result = map[string]any{"tools": s.list()}
	case "tools/call":
		result, err := s.call(ctx, req.Params)
		if err != nil {
			res.Error = &rpcError{Code: -32602, Message: err.Error()}
		} else {
			res.Result = result
		}
	default:
		res.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return res, true
}

func initializeResult(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	version := fallbackVersion
	if supportedVersions[p.ProtocolVersion] {
		version = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "agent-design", "version": "0.1.0"},
	}
}

// call runs one tool. An unknown name is a protocol error: the client asked
// for something that does not exist, and a model cannot recover from it.
func (s *Server) call(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var p callParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return toolResult{}, fmt.Errorf("invalid params: %w", err)
	}
	for _, t := range s.tools {
		if t.name != p.Name {
			continue
		}
		text, err := t.call(ctx, p.Arguments)
		if err != nil {
			return toolResult{
				Content: []contentItem{{Type: "text", Text: describe(err)}},
				IsError: true,
			}, nil
		}
		return toolResult{Content: []contentItem{{Type: "text", Text: text}}}, nil
	}
	return toolResult{}, fmt.Errorf("unknown tool: %s", p.Name)
}

func (s *Server) list() []map[string]any {
	out := make([]map[string]any, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, map[string]any{
			"name":        t.name,
			"description": t.desc,
			"inputSchema": t.schema,
		})
	}
	return out
}

// describe turns a coded error into the text a model reads. The code comes
// first so the model can branch on it without parsing prose, and the fields
// it needs to retry travel with it (go-mapping §7).
func describe(err error) string {
	var fe *vfs.Error
	if !errors.As(err, &fe) {
		return err.Error()
	}
	parts := []string{fe.Code}
	if fe.Path != "" {
		parts = append(parts, "path="+fe.Path)
	}
	if fe.TargetPath != "" {
		parts = append(parts, "target="+fe.TargetPath)
	}
	if fe.Code == vfs.CodeConflict {
		parts = append(parts, fmt.Sprintf("baseRev=%d currentRev=%d", fe.BaseRev, fe.CurrentRev))
	}
	if fe.ExecID != "" {
		parts = append(parts, "execId="+fe.ExecID)
	}
	if fe.Err != nil {
		parts = append(parts, fe.Err.Error())
	}
	return strings.Join(parts, " ")
}
