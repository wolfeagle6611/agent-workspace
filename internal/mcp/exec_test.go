package mcp

import (
	"errors"
	"strings"
	"testing"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/execout"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/runtime"
)

func TestFormatExecCompleted(t *testing.T) {
	r := runtime.Result{
		Status:   runtime.Completed,
		ExitCode: 0,
		Sync:     runtime.SyncComplete,
		Output: execout.Result{
			Stdout: execout.Stream{Tail: []byte("hi\n"), Path: "/.computer/output/shell.A.stdout.log", Bytes: 2},
			Stderr: execout.Stream{Path: "/.computer/output/shell.A.stderr.log"},
		},
	}
	got := formatExec(r)
	if !strings.HasPrefix(got, "hi\n") {
		t.Fatalf("output = %q", got)
	}
	if strings.Contains(got, "--- stderr ---") {
		t.Fatalf("empty stderr got a separator: %q", got)
	}
	for _, want := range []string{
		"status=completed exit=0 · sync=complete",
		"stdout=/.computer/output/shell.A.stdout.log 2 bytes",
		"stderr=/.computer/output/shell.A.stderr.log 0 bytes",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

func TestFormatExecFailedWithStderrAndTail(t *testing.T) {
	r := runtime.Result{
		Status:   runtime.Failed,
		ExitCode: 3,
		Sync:     runtime.SyncPending,
		Output: execout.Result{
			Stdout: execout.Stream{Tail: []byte("partial"), Truncated: true, Bytes: 900000},
			Stderr: execout.Stream{Tail: []byte("oops\n")},
		},
	}
	got := formatExec(r)
	if !strings.Contains(got, "partial\n--- stderr ---\noops\n") {
		t.Fatalf("tails = %q", got)
	}
	if !strings.Contains(got, "status=failed exit=3 · sync=pending") {
		t.Fatalf("footer = %q", got)
	}
	if !strings.Contains(got, "(tail only)") {
		t.Fatalf("truncation not reported: %q", got)
	}
}

func TestFormatExecCancelledAndEmpty(t *testing.T) {
	r := runtime.Result{Status: runtime.Cancelled, ExitCode: -1}
	got := formatExec(r)
	if !strings.HasPrefix(got, "[status=cancelled exit=-1") {
		t.Fatalf("empty output should start with the footer: %q", got)
	}

	r.Output.Err = errors.New("store closed")
	got = formatExec(r)
	if !strings.Contains(got, `logError="store closed"`) {
		t.Fatalf("log error not reported: %q", got)
	}
}
