package runtime_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitlab.webullbroker.com/webull-go/agent-design/internal/backend/shell"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/execout"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/runtime"
	"gitlab.webullbroker.com/webull-go/agent-design/internal/vfs"
)

// host is a workspace without a store: it resolves under one directory.
type host struct {
	point string
	err   error
	asked []string
}

func (h *host) Resolve(ctx context.Context, path string) (string, error) {
	h.asked = append(h.asked, path)
	if h.err != nil {
		return "", h.err
	}
	return filepath.Join(h.point, path), nil
}

// outputs has no store: these tests care about the execution contract, not
// about where the text lands.
type outputs struct{ seen []string }

func (o *outputs) For(ctx context.Context, backendID, execID string) runtime.OutputSink {
	o.seen = append(o.seen, backendID+"."+execID)
	return execout.NewSink(ctx, nil, execout.DefaultLimits, "", "")
}

// handle is a backend that answers with whatever the test says.
type handle struct {
	mode   runtime.SyncMode
	result runtime.Result
	stdout string
	calls  int
	closed int
	seen   []runtime.Request
}

func (h *handle) Sync() runtime.SyncMode { return h.mode }

// newHandle builds a fake handle. It declares SyncNone, the phase-one mode.
func newHandle(results ...runtime.Result) *handle {
	h := &handle{mode: runtime.SyncNone}
	if len(results) > 0 {
		h.result = results[0]
	}
	return h
}

func (h *handle) Exec(ctx context.Context, req runtime.Request) (*runtime.Exec, error) {
	h.seen = append(h.seen, req)
	return &runtime.Exec{Result: func(context.Context) (runtime.Result, error) {
		h.calls++
		if h.stdout != "" {
			_, _ = io.WriteString(req.Output.Stdout(), h.stdout)
		}
		return h.result, nil
	}}, nil
}

func (h *handle) Close() error {
	h.closed++
	return nil
}

type backend struct {
	id  string
	hdl *handle
}

func (b *backend) ID() string   { return b.id }
func (b *backend) Kind() string { return "fake" }

func (b *backend) Connect(ctx context.Context, h runtime.Host) (runtime.Handle, error) {
	return b.hdl, nil
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var e *vfs.Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not a *vfs.Error", err)
	}
	return e.Code
}

func TestUnknownBackendIsNoBackend(t *testing.T) {
	rt := runtime.New(&host{point: t.TempDir()}, &outputs{}, &backend{id: "fake", hdl: newHandle()})

	_, err := rt.Exec(context.Background(), "true", runtime.ExecOptions{Backend: "nope"})
	if !errors.Is(err, vfs.ErrNoBackend) {
		t.Fatalf("err %v, want ErrNoBackend", err)
	}
	if got := codeOf(t, err); got != vfs.CodeNoBackend {
		t.Fatalf("code %q, want %q", got, vfs.CodeNoBackend)
	}
}

func TestNoRegisteredBackendIsNoBackend(t *testing.T) {
	rt := runtime.New(&host{point: t.TempDir()}, &outputs{})

	if _, err := rt.Exec(context.Background(), "true", runtime.ExecOptions{}); !errors.Is(err, vfs.ErrNoBackend) {
		t.Fatalf("err %v, want ErrNoBackend", err)
	}
}

func TestEmptyBackendWithTwoRegisteredIsInvalid(t *testing.T) {
	rt := runtime.New(&host{point: t.TempDir()}, &outputs{},
		&backend{id: "a", hdl: newHandle()},
		&backend{id: "b", hdl: newHandle()},
	)

	_, err := rt.Exec(context.Background(), "true", runtime.ExecOptions{})
	if !errors.Is(err, vfs.ErrInvalid) {
		t.Fatalf("err %v, want ErrInvalid", err)
	}
	if got := codeOf(t, err); got != vfs.CodeInvalid {
		t.Fatalf("code %q, want %q", got, vfs.CodeInvalid)
	}
}

func TestSingleBackendNeedsNoName(t *testing.T) {
	dir := t.TempDir()
	h := &host{point: dir}
	hdl := newHandle(runtime.Result{Status: runtime.Completed})
	rt := runtime.New(h, &outputs{}, &backend{id: "fake", hdl: hdl})

	ex, err := rt.Exec(context.Background(), "true", runtime.ExecOptions{})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if ex.ID == "" {
		t.Fatal("no execution id minted")
	}
	if ex.Backend != "fake" {
		t.Fatalf("backend %q, want fake", ex.Backend)
	}
	if len(h.asked) != 1 || h.asked[0] != "/" {
		t.Fatalf("resolved %v, want [/] for an empty cwd", h.asked)
	}
	if want := filepath.Join(dir); len(hdl.seen) != 1 || hdl.seen[0].Dir != want {
		t.Fatalf("dir %v, want %s", hdl.seen, want)
	}
}

func TestCallerSuppliedIDWins(t *testing.T) {
	rt := runtime.New(&host{point: t.TempDir()}, &outputs{}, &backend{id: "fake", hdl: newHandle()})

	ex, err := rt.Exec(context.Background(), "true", runtime.ExecOptions{ID: "mine"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if ex.ID != "mine" {
		t.Fatalf("id %q, want mine", ex.ID)
	}
}

func TestRemoteSyncBackendIsRefused(t *testing.T) {
	hdl := &handle{mode: runtime.SyncRemote}
	rt := runtime.New(&host{point: t.TempDir()}, &outputs{}, &backend{id: "fake", hdl: hdl})

	_, err := rt.Exec(context.Background(), "true", runtime.ExecOptions{})
	if !errors.Is(err, vfs.ErrInvalid) {
		t.Fatalf("err %v, want ErrInvalid", err)
	}
	if hdl.closed != 1 {
		t.Fatalf("handle closed %d times, want 1", hdl.closed)
	}
}

func TestResolveFailureClosesTheHandle(t *testing.T) {
	hdl := newHandle()
	rt := runtime.New(&host{err: vfs.NewError(vfs.CodeNotFound, "/nope")}, &outputs{},
		&backend{id: "fake", hdl: hdl})

	_, err := rt.Exec(context.Background(), "true", runtime.ExecOptions{Cwd: "/nope"})
	if !errors.Is(err, vfs.ErrNotFound) {
		t.Fatalf("err %v, want ErrNotFound", err)
	}
	if hdl.closed != 1 || len(hdl.seen) != 0 {
		t.Fatalf("closed %d, execs %d: want 1 and 0", hdl.closed, len(hdl.seen))
	}
}

func TestResultIsRepeatable(t *testing.T) {
	hdl := newHandle(runtime.Result{Status: runtime.Completed})
	hdl.stdout = "once"
	rt := runtime.New(&host{point: t.TempDir()}, &outputs{}, &backend{id: "fake", hdl: hdl})

	ex, err := rt.Exec(context.Background(), "true", runtime.ExecOptions{})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	first, err := ex.Result(context.Background())
	if err != nil {
		t.Fatalf("first result: %v", err)
	}
	second, err := ex.Result(context.Background())
	if err != nil {
		t.Fatalf("second result: %v", err)
	}
	if string(first.Output.Stdout.Tail) != "once" || string(second.Output.Stdout.Tail) != "once" {
		t.Fatalf("results %q / %q, want once twice", first.Output.Stdout.Tail, second.Output.Stdout.Tail)
	}
	if hdl.calls != 1 {
		t.Fatalf("backend ran %d times, want 1", hdl.calls)
	}
	if hdl.closed != 1 {
		t.Fatalf("handle closed %d times, want 1", hdl.closed)
	}
}

// entry is the whole execution surface a caller uses.
type entry interface {
	Exec(ctx context.Context, source string, opts runtime.ExecOptions) (*runtime.Exec, error)
}

// runOne is a caller. It names no backend, which is the point: swapping the
// backend must not change this function.
func runOne(ctx context.Context, e entry) (runtime.Result, error) {
	ex, err := e.Exec(ctx, "printf hi > f", runtime.ExecOptions{})
	if err != nil {
		return runtime.Result{}, err
	}
	return ex.Result(ctx)
}

func TestCallerUnchangedAcrossBackends(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	hdl := newHandle(runtime.Result{Status: runtime.Completed})
	hdl.stdout = "faked"
	fake := &backend{id: "fake", hdl: hdl}
	scripted := runtime.New(&host{point: dir}, &outputs{}, fake)
	real := runtime.New(&host{point: dir}, &outputs{}, shell.New())

	for _, tc := range []struct {
		name string
		rt   entry
		want string
	}{
		{"fake", scripted, "faked"},
		{"shell", real, ""},
	} {
		res, err := runOne(ctx, tc.rt)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if res.Status != runtime.Completed {
			t.Fatalf("%s: status %v, want Completed", tc.name, res.Status)
		}
		if string(res.Output.Stdout.Tail) != tc.want {
			t.Fatalf("%s: stdout %q, want %q", tc.name, res.Output.Stdout.Tail, tc.want)
		}
	}

	// The fake recorded the same request shape the shell backend acted on.
	if len(fake.hdl.seen) != 1 || fake.hdl.seen[0].Source != "printf hi > f" {
		t.Fatalf("fake saw %+v", fake.hdl.seen)
	}
	got, err := os.ReadFile(filepath.Join(dir, "f"))
	if err != nil {
		t.Fatalf("read f: %v", err)
	}
	if string(got) != "hi" {
		t.Fatalf("f = %q, want hi", got)
	}
}

func TestTimeoutCancelsTheCommand(t *testing.T) {
	rt := runtime.New(&host{point: t.TempDir()}, &outputs{}, shell.New())

	start := time.Now()
	ex, err := rt.Exec(context.Background(), "sleep 10",
		runtime.ExecOptions{Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	res, err := ex.Result(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want DeadlineExceeded", err)
	}
	if res.Status != runtime.Cancelled {
		t.Fatalf("status %v, want Cancelled", res.Status)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("timeout took %s: the command kept running", took)
	}
}

func TestOutputIsNamedForTheExecution(t *testing.T) {
	h := &host{point: t.TempDir()}
	o := &outputs{}
	hdl := newHandle()
	rt := runtime.New(h, o, &backend{id: "fake", hdl: hdl})

	ex, err := rt.Exec(context.Background(), "true", runtime.ExecOptions{ID: "exec-7"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if _, err := ex.Result(context.Background()); err != nil {
		t.Fatalf("result: %v", err)
	}
	if len(o.seen) != 1 || o.seen[0] != "fake.exec-7" {
		t.Fatalf("output targets %v, want [fake.exec-7]", o.seen)
	}
}

// storeHost is a host with a real store: it is the workspace wiring without
// the FUSE mount, so the whole output path runs on any platform.
type storeOutputs struct{ store *vfs.Store }

func (o *storeOutputs) For(ctx context.Context, backendID, execID string) runtime.OutputSink {
	path := "/.computer/output/" + backendID + "." + execID
	return execout.NewSink(ctx, o.store, execout.DefaultLimits, path+".stdout.log", path+".stderr.log")
}

func TestShellOutputLandsInTheStore(t *testing.T) {
	ctx := context.Background()
	store, err := vfs.Open(filepath.Join(t.TempDir(), "ws.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if err := store.MkdirAll(ctx, "/.computer/output"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rt := runtime.New(&host{point: t.TempDir()}, &storeOutputs{store: store}, shell.New())
	ex, err := rt.Exec(ctx, "seq 1 100000", runtime.ExecOptions{})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	res, err := ex.Result(ctx)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	out := res.Output.Stdout
	if res.Status != runtime.Completed || out.Bytes == 0 {
		t.Fatalf("status %v bytes %d", res.Status, out.Bytes)
	}
	if !out.Truncated || len(out.Tail) > 64<<10 {
		t.Fatalf("truncated %v tail %d bytes", out.Truncated, len(out.Tail))
	}
	full, err := store.ReadFile(ctx, out.Path)
	if err != nil {
		t.Fatalf("read back %s: %v", out.Path, err)
	}
	if int64(len(full.Data)) != out.Bytes {
		t.Fatalf("stored %d bytes, counted %d", len(full.Data), out.Bytes)
	}
}
