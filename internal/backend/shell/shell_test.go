package shell

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/execout"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/runtime"
)

// run executes source in dir and waits. The backend needs nothing from the
// Host, so tests pass none.
func run(ctx context.Context, t *testing.T, dir, source string, env map[string]string) (runtime.Result, error) {
	t.Helper()
	return runWith(ctx, t, New(), dir, source, env)
}

// runWith is run with a tuned backend, so a test can shorten the grace period.
//
// The backend only writes into the sink; the tail comes from Finalize, which
// Runtime calls in production and this helper calls here.
func runWith(ctx context.Context, t *testing.T, b *Backend, dir, source string, env map[string]string) (runtime.Result, error) {
	t.Helper()
	h, err := b.Connect(ctx, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	out := execout.NewSink(ctx, nil, execout.DefaultLimits, "", "")
	ex, err := h.Exec(ctx, runtime.Request{Source: source, Dir: dir, Env: env, Output: out})
	if err != nil {
		t.Fatalf("exec %q: %v", source, err)
	}
	res, err := ex.Result(ctx)
	o, ferr := out.Finalize(context.WithoutCancel(ctx))
	if ferr != nil {
		t.Fatalf("finalize: %v", ferr)
	}
	res.Output = o
	return res, err
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
	if string(res.Output.Stdout.Tail) != "inner" {
		t.Fatalf("stdout %q, want inner", res.Output.Stdout.Tail)
	}
}

func TestEnvKeepsProcessEnvironment(t *testing.T) {
	t.Setenv("AGENT_DESIGN_KEEP", "kept")
	res, err := run(context.Background(), t, t.TempDir(), `printf %s "$AGENT_DESIGN_KEEP"`, nil)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if string(res.Output.Stdout.Tail) != "kept" {
		t.Fatalf("stdout %q, want kept", res.Output.Stdout.Tail)
	}
}

func TestStdoutAndStderrStaySeparate(t *testing.T) {
	res, err := run(context.Background(), t, t.TempDir(), "printf out; printf err >&2", nil)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if string(res.Output.Stdout.Tail) != "out" || string(res.Output.Stderr.Tail) != "err" {
		t.Fatalf("stdout %q stderr %q, want out / err", res.Output.Stdout.Tail, res.Output.Stderr.Tail)
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
	if string(res.Output.Stdout.Tail) != "1" {
		t.Fatalf("stdout %q, want 1 (read failed on EOF)", res.Output.Stdout.Tail)
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
	if res.ExitCode != -1 {
		t.Fatalf("exit %d, want -1 for a signalled command", res.ExitCode)
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
