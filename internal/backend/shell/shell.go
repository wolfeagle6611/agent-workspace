// Package shell runs shell source as a local subprocess.
//
// The backend keeps no storage of its own: the subprocess works inside the
// mount point, so every file it writes reaches the authoritative store
// through FUSE (ADR 0006).
package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/runtime"
)

// Backend runs /bin/sh. It needs no transport, so connecting cannot fail.
type Backend struct{}

// New builds the backend.
func New() *Backend { return &Backend{} }

// ID is the routing name.
func (b *Backend) ID() string { return "shell" }

// Kind is for diagnostics only.
func (b *Backend) Kind() string { return "subprocess" }

// Connect returns a handle. A subprocess backend needs no handshake.
func (b *Backend) Connect(ctx context.Context, host runtime.Host) (runtime.Handle, error) {
	return &handle{}, nil
}

type handle struct{}

var (
	_ runtime.Backend = (*Backend)(nil)
	_ runtime.Handle  = (*handle)(nil)
)

// Sync is none: writes go straight into the authoritative store.
func (h *handle) Sync() runtime.SyncMode { return runtime.SyncNone }

// Close is a no-op. The handle owns nothing, and Result reaps the process.
func (h *handle) Close() error { return nil }

// Exec starts /bin/sh -c source in req.Dir.
//
// The environment is the process environment plus req.Env, stdin is the null
// device so a reading command gets EOF instead of a hang.
func (h *handle) Exec(ctx context.Context, req runtime.Request) (*runtime.Exec, error) {
	cmd := exec.Command("/bin/sh", "-c", req.Source)
	cmd.Dir = req.Dir
	cmd.Env = environ(req.Env)
	cmd.Stdin = nil

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %q in %s: %w", req.Source, req.Dir, err)
	}
	return &runtime.Exec{Result: func(ctx context.Context) (runtime.Result, error) {
		return wait(ctx, cmd, &stdout, &stderr)
	}}, nil
}

// wait reaps the process and classifies the end.
//
// A cancelled context kills the direct child. The process group and the
// grace period are step 4 (go-mapping §5), so a grandchild may outlive this.
func wait(ctx context.Context, cmd *exec.Cmd, stdout, stderr *bytes.Buffer) (runtime.Result, error) {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return classify(err, stdout, stderr)
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		res := runtime.Result{
			Status: runtime.Cancelled,
			Stdout: stdout.Bytes(),
			Stderr: stderr.Bytes(),
		}
		return res, ctx.Err()
	}
}

// classify turns the wait error into a Result. A non-zero exit is a normal
// outcome of the command, so it is not an error of the call.
func classify(err error, stdout, stderr *bytes.Buffer) (runtime.Result, error) {
	res := runtime.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err == nil {
		res.Status = runtime.Completed
		return res, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		res.Status = runtime.Failed
		res.ExitCode = exit.ExitCode()
		return res, nil
	}
	return runtime.Result{}, err
}

// environ merges the process environment with overrides. Later values win,
// and the result is sorted so the child sees one entry per name.
func environ(overrides map[string]string) []string {
	env := make(map[string]string)
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	for k, v := range overrides {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
