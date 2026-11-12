package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/execout"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/runtime"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/workspace"
)

// Read limits. They bound what a tool puts into a model's context, the way
// execout bounds command output.
const (
	defaultReadLines = 2000
	maxReadBytes     = 64 * 1024
	// maxReadFile keeps a read from materializing a file far larger than the
	// window it will show. Above it, the model greps through exec instead.
	maxReadFile = 8 << 20
	// maxLsEntries keeps one listing from filling a context. The footer names
	// the real entry count, so nothing is silently hidden.
	maxLsEntries = 1000
)

// tool is one named operation. The call translates arguments into exactly one
// workspace call set.
type tool struct {
	name   string
	desc   string
	schema map[string]any
	call   func(context.Context, json.RawMessage) (string, error)
}

func tools(ws *workspace.Workspace) []tool {
	return []tool{exec(ws), read(ws), write(ws), edit(ws), ls(ws)}
}

// codeNoMatch is the tool-layer code for "the old text is not there", or "it
// is there more than once". It stays out of the vfs error table because the
// store does no content matching (go-mapping §5.1).
const codeNoMatch = "ENOMATCH"

// noMatch carries the occurrence count so the model can tell a typo (0) from
// an ambiguous match (>1). It is a coded error like every other failure, so
// callers reach it with errors.As instead of parsing text.
func noMatch(path string, count int) error {
	return &vfs.Error{Code: codeNoMatch, Path: path, Err: fmt.Errorf("occurrences=%d", count)}
}

// conflict reports a stale rev. The vfs builds the same error for a failed
// precondition; this one covers the check the tool makes before writing.
func conflict(path string, base, current int64) error {
	return &vfs.Error{Code: vfs.CodeConflict, Path: path, BaseRev: base, CurrentRev: current}
}

// footer is the metadata line every mutating tool ends with.
func footer(rev, size int64) string {
	return fmt.Sprintf("[rev=%d · %d bytes]\n", rev, size)
}

func read(ws *workspace.Workspace) tool {
	return tool{
		name: "read",
		desc: "Read a text file. Returns the file body and then one bracketed metadata line " +
			"holding its rev, the line range returned, and the byte count. The bracketed line " +
			"is not file content. Its rev is what write and edit accept as a precondition.",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "Absolute workspace path."},
				"offset": map[string]any{"type": "integer", "description": "First line to return, 1-based. Default 1."},
				"limit":  map[string]any{"type": "integer", "description": "Maximum lines to return. Default 2000."},
			},
			"required": []string{"path"},
		},
		call: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var a struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return "", vfs.NewError(vfs.CodeInvalid, "")
			}
			if a.Path == "" {
				return "", vfs.NewError(vfs.CodeInvalid, "")
			}
			st, err := ws.Stat(ctx, a.Path)
			if err != nil {
				return "", err
			}
			if st.Size > maxReadFile {
				return "", vfs.WrapError(vfs.CodeInvalid, a.Path, fmt.Errorf(
					"file is %d bytes, more than the %d byte read limit; use exec (sed, grep, head) to read a range",
					st.Size, maxReadFile))
			}
			content, err := ws.ReadFile(ctx, a.Path)
			if err != nil {
				return "", err
			}
			return formatRead(content, a.Offset, a.Limit), nil
		},
	}
}

// formatRead windows the body by whole lines, keeping the bytes exact so an
// edit can match text copied out of the output.
func formatRead(c *vfs.Content, offset, limit int) string {
	lines := strings.SplitAfter(string(c.Data), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	total := len(lines)

	start := offset - 1
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	if limit <= 0 {
		limit = defaultReadLines
	}
	end := start + limit
	if end > total {
		end = total
	}

	body := strings.Join(lines[start:end], "")
	cut, partial := false, false
	if len(body) > maxReadBytes {
		body = body[:maxReadBytes]
		if i := strings.LastIndexByte(body, '\n'); i >= 0 {
			body = body[:i+1]
			// Report the lines the cut left, not the lines requested.
			end = start + strings.Count(body, "\n")
		} else {
			end = start + 1
			partial = true
		}
		cut = true
	}

	var b strings.Builder
	b.WriteString(body)
	if body != "" && !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n[")
	fmt.Fprintf(&b, "rev=%d", c.Stat.Rev)
	if total == 0 {
		b.WriteString(" · empty")
	} else if start == 0 && end == total {
		unit := "lines"
		if total == 1 {
			unit = "line"
		}
		fmt.Fprintf(&b, " · %d %s", total, unit)
	} else {
		fmt.Fprintf(&b, " · lines %d-%d of %d", start+1, end, total)
	}
	fmt.Fprintf(&b, " · %d bytes", len(c.Data))
	if cut {
		b.WriteString(" · cut to 64 KiB")
	}
	if partial {
		b.WriteString(" · partial line")
	}
	b.WriteString("]\n")
	return b.String()
}

func ls(ws *workspace.Workspace) tool {
	return tool{
		name: "ls",
		desc: "List one directory level. Each line is `d name/` for a directory or " +
			"`f name size` for a file. Ends with one bracketed metadata line holding the " +
			"directory rev and the entry count.",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Absolute workspace path. Default /."},
			},
		},
		call: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return "", vfs.NewError(vfs.CodeInvalid, "")
			}
			if a.Path == "" {
				a.Path = "/"
			}
			entries, err := ws.ReadDir(ctx, a.Path)
			if err != nil {
				return "", err
			}
			st, err := ws.Stat(ctx, a.Path)
			if err != nil {
				return "", err
			}

			shown := entries
			if len(shown) > maxLsEntries {
				shown = shown[:maxLsEntries]
			}
			var b strings.Builder
			for _, e := range shown {
				if e.Type == vfs.TypeDir {
					fmt.Fprintf(&b, "d %s/\n", e.Name)
					continue
				}
				fmt.Fprintf(&b, "f %s %d\n", e.Name, e.Size)
			}
			unit := "entries"
			if len(entries) == 1 {
				unit = "entry"
			}
			fmt.Fprintf(&b, "\n[rev=%d · %d %s", st.Rev, len(entries), unit)
			if len(shown) < len(entries) {
				fmt.Fprintf(&b, " · first %d shown", len(shown))
			}
			b.WriteString("]\n")
			return b.String(), nil
		},
	}
}

func write(ws *workspace.Workspace) tool {
	return tool{
		name: "write",
		desc: "Create or replace a file. Parent directories must already exist. " +
			"Returns the new rev. Pass the rev you read to make the write conditional: " +
			"a mismatch returns ECONFLICT with currentRev, and you re-read and retry. " +
			"Omit rev to let the write win (last-write-wins).",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "Absolute workspace path."},
				"content": map[string]any{"type": "string", "description": "Full new file body."},
				"rev":     map[string]any{"type": "integer", "description": "Optional precondition: the rev you read."},
			},
			"required": []string{"path", "content"},
		},
		call: func(ctx context.Context, raw json.RawMessage) (string, error) {
			// Content is a pointer so a missing field is an error, not an
			// empty file. A malformed call must not truncate anything.
			var a struct {
				Path    string  `json:"path"`
				Content *string `json:"content"`
				Rev     int64   `json:"rev"`
			}
			if err := json.Unmarshal(raw, &a); err != nil || a.Path == "" || a.Content == nil {
				return "", vfs.NewError(vfs.CodeInvalid, a.Path)
			}
			opts := &vfs.MutateOptions{}
			if a.Rev != 0 {
				opts.BaseRev = &a.Rev
			}
			if err := ws.WriteFile(ctx, a.Path, []byte(*a.Content), opts); err != nil {
				return "", err
			}
			st, err := ws.Stat(ctx, a.Path)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %s\n%s", a.Path, footer(st.Rev, st.Size)), nil
		},
	}
}

func edit(ws *workspace.Workspace) tool {
	return tool{
		name: "edit",
		desc: "Replace text in one file. `old` must match the current bytes exactly. " +
			"It must appear once unless replace_all is true. A missing or ambiguous match " +
			"returns ENOMATCH with occurrences. A stale rev returns ECONFLICT with currentRev; " +
			"re-read and retry. Omitting rev still guards the write against a writer that " +
			"slips in between this tool's read and write.",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        map[string]any{"type": "string", "description": "Absolute workspace path."},
				"old":         map[string]any{"type": "string", "description": "Exact text to replace."},
				"new":         map[string]any{"type": "string", "description": "Replacement text."},
				"replace_all": map[string]any{"type": "boolean", "description": "Replace every occurrence. Default false."},
				"rev":         map[string]any{"type": "integer", "description": "Optional precondition: the rev you read."},
			},
			"required": []string{"path", "old", "new"},
		},
		call: func(ctx context.Context, raw json.RawMessage) (string, error) {
			// New is a pointer: a missing field is a malformed call, while an
			// explicit "" is a legitimate deletion.
			var a struct {
				Path       string  `json:"path"`
				Old        string  `json:"old"`
				New        *string `json:"new"`
				ReplaceAll bool    `json:"replace_all"`
				Rev        int64   `json:"rev"`
			}
			if err := json.Unmarshal(raw, &a); err != nil || a.Path == "" || a.Old == "" || a.New == nil {
				return "", vfs.NewError(vfs.CodeInvalid, a.Path)
			}
			content, err := ws.ReadFile(ctx, a.Path)
			if err != nil {
				return "", err
			}
			// The version check comes before the match: matching text the
			// caller never saw would be a guess, not an edit.
			if a.Rev != 0 && a.Rev != content.Stat.Rev {
				return "", conflict(a.Path, a.Rev, content.Stat.Rev)
			}
			body := string(content.Data)
			n := strings.Count(body, a.Old)
			if n == 0 || (n > 1 && !a.ReplaceAll) {
				return "", noMatch(a.Path, n)
			}
			if a.ReplaceAll {
				body = strings.ReplaceAll(body, a.Old, *a.New)
			} else {
				body = strings.Replace(body, a.Old, *a.New, 1)
				n = 1
			}

			// Carry the rev we just read. A writer that slipped in gets
			// ECONFLICT instead of being overwritten.
			rev := content.Stat.Rev
			if err := ws.WriteFile(ctx, a.Path, []byte(body), &vfs.MutateOptions{BaseRev: &rev}); err != nil {
				return "", err
			}
			st, err := ws.Stat(ctx, a.Path)
			if err != nil {
				return "", err
			}
			unit := "replacements"
			if n == 1 {
				unit = "replacement"
			}
			return fmt.Sprintf("edited %s · %d %s\n%s", a.Path, n, unit, footer(st.Rev, st.Size)), nil
		},
	}
}

func exec(ws *workspace.Workspace) tool {
	return tool{
		name: "exec",
		desc: "Run a shell command with /bin/sh -c inside the workspace. " +
			"Returns the stdout and stderr tails, the exit code, and the workspace paths of the " +
			"full logs. A non-zero exit is a normal result, not a tool error. The command is never " +
			"retried. Writes it makes are visible to later tool calls.",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "Shell command."},
				"cwd":     map[string]any{"type": "string", "description": "Absolute workspace path to start in. Default /."},
				"timeout": map[string]any{"type": "integer", "description": "Seconds before the process group is killed. Default no limit."},
			},
			"required": []string{"command"},
		},
		call: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var a struct {
				Command string `json:"command"`
				Cwd     string `json:"cwd"`
				Timeout int    `json:"timeout"`
			}
			if err := json.Unmarshal(raw, &a); err != nil || a.Command == "" {
				return "", vfs.NewError(vfs.CodeInvalid, "")
			}
			opts := runtime.ExecOptions{Cwd: a.Cwd}
			if a.Timeout > 0 {
				opts.Timeout = time.Duration(a.Timeout) * time.Second
			}
			ex, err := ws.Exec(ctx, a.Command, opts)
			if err != nil {
				return "", err
			}
			res, err := ex.Result(ctx)
			if err != nil {
				return "", err
			}
			return formatExec(res), nil
		},
	}
}

// formatExec turns one execution result into the text a model reads: the two
// tails, then one metadata line with the status, the exit code, the sync
// outcome, and where the full logs live (go-mapping §5.1).
func formatExec(r runtime.Result) string {
	var b strings.Builder
	if tail := r.Output.Stdout.Tail; len(tail) > 0 {
		b.Write(tail)
		if tail[len(tail)-1] != '\n' {
			b.WriteString("\n")
		}
	}
	if tail := r.Output.Stderr.Tail; len(tail) > 0 {
		b.WriteString("--- stderr ---\n")
		b.Write(tail)
		if tail[len(tail)-1] != '\n' {
			b.WriteString("\n")
		}
	}

	status := "completed"
	switch r.Status {
	case runtime.Failed:
		status = "failed"
	case runtime.Cancelled:
		status = "cancelled"
	}
	sync := "complete"
	if r.Sync == runtime.SyncPending {
		sync = "pending"
	}

	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString("[")
	fmt.Fprintf(&b, "status=%s exit=%d · sync=%s", status, r.ExitCode, sync)
	stream(&b, "stdout", r.Output.Stdout)
	stream(&b, "stderr", r.Output.Stderr)
	if r.Output.Err != nil {
		fmt.Fprintf(&b, " · logError=%q", r.Output.Err)
	}
	b.WriteString("]\n")
	return b.String()
}

func stream(b *strings.Builder, name string, s execout.Stream) {
	fmt.Fprintf(b, " · %s=%s %d bytes", name, s.Path, s.Bytes)
	if s.Truncated {
		b.WriteString(" (tail only)")
	}
}
