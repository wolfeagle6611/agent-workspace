package shell

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/runtime"
)

// run executes source in dir and waits. The backend needs nothing from the
// Host, so tests pass none.
func run(ctx context.Context, t *testing.T, dir, source string, env map[string]string) (runtime.Result, error) {
	t.Helper()
	h, err := New().Connect(ctx, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ex, err := h.Exec(ctx, runtime.Request{Source: source, Dir: dir, Env: env})
	if err != nil {
		t.Fatalf("exec %q: %v", source, err)
	}
	return ex.Result(ctx)
}

func TestWritesIntoDir(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	res, err := run(ctx, t, dir, "printf hi > f", nil)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Status != runtime.Completed || res.ExitCode != 0 {
		t.Fatalf("status %v exit %d, want Completed 0", res.Status, res.ExitCode)
	}
	got, err := os.ReadFile(filepath.Join(dir, "f"))
	if err != nil {
		t.Fatalf("read f: %v", err)
	}
	if string(got) != "hi" {
		t.Fatalf("f = %q, want hi", got)
	}
}

func TestNonZeroExitIsAResultNotAnError(t *testing.T) {
	res, err := run(context.Background(), t, t.TempDir(), "exit 3", nil)
	if err != nil {
		t.Fatalf("exit 3 gave error %v, want nil", err)
	}
	if res.Status != runtime.Failed || res.ExitCode != 3 {
		t.Fatalf("status %v exit %d, want Failed 3", res.Status, res.ExitCode)
	}
}

func TestEnvOverridesProcessEnvironment(t *testing.T) {
	t.Setenv("AGENT_DESIGN_TEST", "outer")
	res, err := run(context.Background(), t, t.TempDir(),
		`printf %s "$AGENT_DESIGN_TEST"`, map[string]string{"AGENT_DESIGN_TEST": "inner"})
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if string(res.Stdout) != "inner" {
		t.Fatalf("stdout %q, want inner", res.Stdout)
	}
}

func TestEnvKeepsProcessEnvironment(t *testing.T) {
	t.Setenv("AGENT_DESIGN_KEEP", "kept")
	res, err := run(context.Background(), t, t.TempDir(), `printf %s "$AGENT_DESIGN_KEEP"`, nil)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if string(res.Stdout) != "kept" {
		t.Fatalf("stdout %q, want kept", res.Stdout)
	}
}

func TestStdoutAndStderrStaySeparate(t *testing.T) {
	res, err := run(context.Background(), t, t.TempDir(), "printf out; printf err >&2", nil)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if string(res.Stdout) != "out" || string(res.Stderr) != "err" {
		t.Fatalf("stdout %q stderr %q, want out / err", res.Stdout, res.Stderr)
	}
}

func TestStdinIsNullDevice(t *testing.T) {
	// A command that reads must get EOF, not block until the test times out.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := run(ctx, t, t.TempDir(), "read line; printf %s $?", nil)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if string(res.Stdout) != "1" {
		t.Fatalf("stdout %q, want 1 (read failed on EOF)", res.Stdout)
	}
}

func TestCancelledContextKillsTheCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := run(ctx, t, t.TempDir(), "sleep 5", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want DeadlineExceeded", err)
	}
	if res.Status != runtime.Cancelled {
		t.Fatalf("status %v, want Cancelled", res.Status)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("cancel took %s: the child kept running", took)
	}
}

func TestStartFailureIsAnError(t *testing.T) {
	h, err := New().Connect(context.Background(), nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := h.Exec(context.Background(),
		runtime.Request{Source: "true", Dir: missing}); err == nil {
		t.Fatal("exec in a missing directory succeeded, want an error")
	}
}
