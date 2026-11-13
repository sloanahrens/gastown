package bdgateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestExecutorPathMapping: rig and cwd select the directory bd runs in and the
// BEADS_DIR it reads, and a cwd that leaves the workspace or a rig that is not
// one path element is refused before any process starts.
func TestExecutorPathMapping(t *testing.T) {
	t.Parallel()
	root := testWorkspace(t)
	cases := []struct {
		name      string
		rig       string
		cwd       string
		wantDir   string
		wantBeads string
		wantErr   error
	}{
		{
			name: "rig and a crew directory", rig: "gastown", cwd: "gastown/crew/dipper",
			wantDir:   filepath.Join(root, "gastown", "crew", "dipper"),
			wantBeads: filepath.Join(root, "gastown", ".beads"),
		},
		{
			name: "rig with no cwd", rig: "gastown",
			wantDir:   root,
			wantBeads: filepath.Join(root, "gastown", ".beads"),
		},
		{
			name:      "town call",
			wantDir:   root,
			wantBeads: filepath.Join(root, ".beads"),
		},
		{name: "cwd is the parent", rig: "gastown", cwd: "..", wantErr: ErrCwdOutsideWorkspace},
		{name: "cwd escapes upward", rig: "gastown", cwd: "gastown/../../elsewhere", wantErr: ErrCwdOutsideWorkspace},
		{name: "cwd is absolute", rig: "gastown", cwd: "/gastown", wantErr: ErrCwdOutsideWorkspace},
		{name: "rig names two elements", rig: "gastown/crew", wantErr: ErrBadRig},
		{name: "rig is the parent", rig: "..", wantErr: ErrBadRig},
		{name: "rig is the current directory", rig: ".", wantErr: ErrBadRig},
		{name: "rig has no workspace", rig: "elsewhere", wantErr: ErrNoWorkspace},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			e := newTestExecutor(t, root, runner)
			var out bytes.Buffer
			err := e.Exec(context.Background(), Request{Argv: []string{"show", "gt-abc"}, Rig: tc.rig, Cwd: tc.cwd}, nil, &out)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Exec = %v, want %v", err, tc.wantErr)
				}
				if got := len(runner.records()); got != 0 {
					t.Errorf("runner started %d calls, want 0", got)
				}
				if out.Len() != 0 {
					t.Errorf("a refusal before the process wrote frames: %q", out.Bytes())
				}
				return
			}
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			call := runner.only(t)
			if call.dir != tc.wantDir {
				t.Errorf("bd's directory = %q, want %q", call.dir, tc.wantDir)
			}
			if got := envMap(call.env)["BEADS_DIR"]; got != tc.wantBeads {
				t.Errorf("BEADS_DIR = %q, want %q", got, tc.wantBeads)
			}
			if fi, err := os.Stat(call.dir); err != nil || !fi.IsDir() {
				t.Errorf("bd's directory %q is not a directory: %v", call.dir, err)
			}
		})
	}
}

// TestExecutorExit: bd's stdout and stderr come back as their frames, in order
// within each stream, and the exit status the process ended with is the last
// frame's.
func TestExecutorExit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		stdout string
		stderr string
	}{
		{name: "zero", status: 0},
		{name: "status three", status: 3, stdout: "out\n", stderr: "err\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{queue: []Process{newExitedFake(tc.status, tc.stdout, tc.stderr)}}
			e := newTestExecutor(t, testWorkspace(t), runner)
			var out bytes.Buffer
			if err := e.Exec(context.Background(), Request{Argv: []string{"show", "gt-abc"}, Rig: "gastown"}, nil, &out); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			frames := readFrames(t, out.Bytes())
			if len(frames) == 0 {
				t.Fatal("no frames")
			}
			last := frames[len(frames)-1]
			if last.Kind != FrameExit {
				t.Fatalf("last frame kind = %d, want the exit frame", last.Kind)
			}
			if got, err := last.ExitStatus(); err != nil || got != tc.status {
				t.Errorf("exit status = %d (%v), want %d", got, err, tc.status)
			}
			if got := streamOf(frames, FrameStdout); got != tc.stdout {
				t.Errorf("stdout frames = %q, want %q", got, tc.stdout)
			}
			if got := streamOf(frames, FrameStderr); got != tc.stderr {
				t.Errorf("stderr frames = %q, want %q", got, tc.stderr)
			}
		})
	}
}

// TestExecutorTimeout: a call that outlives its timeout kills the process
// group, names the timeout in an error frame and ends with the design's 124,
// and the refusal does not repeat the caller's argv, which can hold free text.
func TestExecutorTimeout(t *testing.T) {
	t.Parallel()
	proc := newBlockingFake("")
	runner := &fakeRunner{queue: []Process{proc}}
	e := newTestExecutor(t, testWorkspace(t), runner)
	var out bytes.Buffer
	err := e.Exec(context.Background(), Request{
		Argv:      []string{"create", "a private bead title"},
		Rig:       "gastown",
		TimeoutMS: 20,
	}, nil, &out)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Exec = %v, want ErrTimeout", err)
	}
	if !proc.wasKilled() {
		t.Error("the process group was not killed")
	}
	frames := readFrames(t, out.Bytes())
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want the error frame and the exit frame", len(frames))
	}
	refusal, err := frames[0].GatewayError()
	if err != nil {
		t.Fatalf("error frame: %v", err)
	}
	if refusal.Code != CodeTimeout {
		t.Errorf("refusal code = %q, want %q", refusal.Code, CodeTimeout)
	}
	if strings.Contains(refusal.Message, "private bead title") {
		t.Errorf("the refusal repeats the caller's text: %q", refusal.Message)
	}
	if status, err := frames[1].ExitStatus(); err != nil || status != 124 {
		t.Errorf("exit status = %d (%v), want 124", status, err)
	}
}

// TestExecutorDisconnect: a client that cancels the request, or one whose
// stream breaks mid-call, still gets bd's process group killed, and the
// executor names the client as the reason.
func TestExecutorDisconnect(t *testing.T) {
	t.Parallel()
	t.Run("canceled context", func(t *testing.T) {
		proc := newBlockingFake("")
		e := newTestExecutor(t, testWorkspace(t), &fakeRunner{queue: []Process{proc}})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var out bytes.Buffer
		err := e.Exec(ctx, Request{Argv: []string{"list"}, Rig: "gastown"}, nil, &out)
		if !errors.Is(err, ErrClientGone) {
			t.Fatalf("Exec = %v, want ErrClientGone", err)
		}
		if !proc.wasKilled() {
			t.Error("the process group was not killed")
		}
		frames := readFrames(t, out.Bytes())
		if len(frames) != 2 {
			t.Fatalf("frames = %d, want the error frame and the exit frame", len(frames))
		}
		refusal, err := frames[0].GatewayError()
		if err != nil || refusal.Code != CodeCanceled {
			t.Errorf("error frame = %+v (%v), want code %q", refusal, err, CodeCanceled)
		}
		if status, err := frames[1].ExitStatus(); err != nil || status != 124 {
			t.Errorf("exit status = %d (%v), want 124", status, err)
		}
	})
	t.Run("broken stream", func(t *testing.T) {
		proc := newBlockingFake("out that cannot be written\n")
		e := newTestExecutor(t, testWorkspace(t), &fakeRunner{queue: []Process{proc}})
		err := e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown"}, nil, failingWriter{})
		if !errors.Is(err, ErrClientGone) {
			t.Fatalf("Exec = %v, want ErrClientGone", err)
		}
		if !proc.wasKilled() {
			t.Error("the process group was not killed")
		}
	})
}

// TestExecutorEndings: a start that fails, a wait that fails and a process
// that ended on a signal each write the gateway's error frame and the design's
// 125, and each names its cause in the returned error.
func TestExecutorEndings(t *testing.T) {
	t.Parallel()
	boom := errors.New("the machine gave up")
	cases := []struct {
		name     string
		runner   *fakeRunner
		wantCode string
		wantErr  error
	}{
		{
			name:     "start fails",
			runner:   &fakeRunner{err: boom},
			wantCode: CodeStartFailed,
			wantErr:  boom,
		},
		{
			name:     "wait fails",
			runner:   &fakeRunner{queue: []Process{newFakeProcess(0, "", "", boom)}},
			wantCode: CodeWaitFailed,
			wantErr:  boom,
		},
		{
			name:     "ended on a signal",
			runner:   &fakeRunner{queue: []Process{newFakeProcess(-1, "", "", nil)}},
			wantCode: CodeSignaled,
			wantErr:  ErrSignaled,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestExecutor(t, testWorkspace(t), tc.runner)
			var out bytes.Buffer
			err := e.Exec(context.Background(), Request{Argv: []string{"show", "gt-abc"}, Rig: "gastown"}, nil, &out)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Exec = %v, want %v", err, tc.wantErr)
			}
			frames := readFrames(t, out.Bytes())
			if len(frames) != 2 {
				t.Fatalf("frames = %d, want the error frame and the exit frame", len(frames))
			}
			refusal, err := frames[0].GatewayError()
			if err != nil || refusal.Code != tc.wantCode {
				t.Errorf("error frame = %+v (%v), want code %q", refusal, err, tc.wantCode)
			}
			if status, err := frames[1].ExitStatus(); err != nil || status != 125 {
				t.Errorf("exit status = %d (%v), want 125", status, err)
			}
		})
	}
}

// TestExecutorCap: with every slot taken the next call is refused by name
// before a process starts, and the slot a call held is released when it ends.
func TestExecutorCap(t *testing.T) {
	t.Parallel()
	proc := newBlockingFake("")
	runner := &fakeRunner{queue: []Process{proc}, started: make(chan struct{})}
	e, err := New(Options{
		Binary:        "/opt/bd/bd",
		WorkspaceRoot: testWorkspace(t),
		Home:          "/home/bdgw",
		Path:          "/usr/bin:/bin",
		DoltHost:      "gt-dolt",
		DoltPort:      "3307",
		MaxConcurrent: 1,
		runner:        runner,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var first bytes.Buffer
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown"}, nil, &first)
	}()
	<-runner.started

	var second bytes.Buffer
	err = e.Exec(context.Background(), Request{Argv: []string{"list"}, Rig: "gastown"}, nil, &second)
	if !errors.Is(err, ErrTooManyCalls) {
		t.Fatalf("the call over the cap = %v, want ErrTooManyCalls", err)
	}
	if got := len(runner.records()); got != 1 {
		t.Errorf("runner started %d calls, want 1", got)
	}
	if second.Len() != 0 {
		t.Errorf("the refused call wrote frames: %q", second.Bytes())
	}

	proc.finish(0)
	if err := <-firstDone; err != nil {
		t.Fatalf("the call holding the slot: %v", err)
	}
}

// TestExecutorStdinCap: a body over the cap is refused before the process
// starts, a body at the cap reaches bd byte for byte, and a broken body is
// reported rather than started.
func TestExecutorStdinCap(t *testing.T) {
	t.Parallel()
	boom := errors.New("the body broke")
	cases := []struct {
		name     string
		stdin    io.Reader
		wantErr  error
		wantBody string
	}{
		{name: "exactly at the cap", stdin: bytes.NewReader(bytes.Repeat([]byte("x"), MaxStdinBytes)), wantBody: strings.Repeat("x", MaxStdinBytes)},
		{name: "over the cap", stdin: bytes.NewReader(bytes.Repeat([]byte("x"), MaxStdinBytes+1)), wantErr: ErrStdinTooLarge},
		{name: "broken body", stdin: errorReader{err: boom}, wantErr: boom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proc := newExitedFake(0, "", "")
			runner := &fakeRunner{queue: []Process{proc}}
			e := newTestExecutor(t, testWorkspace(t), runner)
			var out bytes.Buffer
			err := e.Exec(context.Background(), Request{Argv: []string{"create", "-"}, Rig: "gastown"}, tc.stdin, &out)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Exec = %v, want %v", err, tc.wantErr)
				}
				if got := len(runner.records()); got != 0 {
					t.Errorf("runner started %d calls, want 0", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if got, closed := proc.stdin.contents(); got != tc.wantBody || !closed {
				t.Errorf("bd's stdin = %d bytes (closed %v), want %d bytes closed", len(got), closed, len(tc.wantBody))
			}
		})
	}
}

// TestExecutorEnv: bd's environment is the computed half plus the caller's
// allowlisted keys, and every other key is refused rather than dropped, so a
// caller cannot aim bd at another database, home or token.
func TestExecutorEnv(t *testing.T) {
	t.Parallel()
	root := testWorkspace(t)
	cases := []struct {
		name    string
		env     map[string]string
		wantErr error
	}{
		{name: "no caller env"},
		{name: "one actor", env: map[string]string{"BD_ACTOR": "town/crew/dipper"}},
		{
			name: "the whole allowlist",
			env: map[string]string{
				"BD_ACTOR":            "town/crew/dipper",
				"BEADS_ACTOR":         "town/crew/dipper",
				"BD_MACHINE":          "daemon",
				"BD_JSON_ENVELOPE":    "1",
				"BD_READONLY":         "true",
				"BD_DOLT_AUTO_COMMIT": "1",
			},
		},
		{name: "an unknown key", env: map[string]string{"GT_ROOT": "/Users/sloan/gt"}, wantErr: ErrEnvNotAllowed},
		{name: "BEADS_DIR", env: map[string]string{"BEADS_DIR": "/etc"}, wantErr: ErrEnvNotAllowed},
		{name: "HOME", env: map[string]string{"HOME": "/root"}, wantErr: ErrEnvNotAllowed},
		{name: "PATH", env: map[string]string{"PATH": "/bin"}, wantErr: ErrEnvNotAllowed},
		{name: "BD_ALLOW_NEW_DATABASE", env: map[string]string{"BD_ALLOW_NEW_DATABASE": "1"}, wantErr: ErrEnvNotAllowed},
		{name: "a NUL value", env: map[string]string{"BD_ACTOR": "a\x00b"}, wantErr: ErrEnvNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			e := newTestExecutor(t, root, runner)
			var out bytes.Buffer
			err := e.Exec(context.Background(), Request{Argv: []string{"show", "gt-abc"}, Rig: "gastown", Env: tc.env}, nil, &out)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Exec = %v, want %v", err, tc.wantErr)
				}
				if got := len(runner.records()); got != 0 {
					t.Errorf("runner started %d calls, want 0", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			env := envMap(runner.only(t).env)
			for k, want := range tc.env {
				if got := env[k]; got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
			for k, want := range map[string]string{
				"HOME":                   "/home/bdgw",
				"PATH":                   "/usr/local/bin:/usr/bin:/bin",
				"BEADS_DIR":              filepath.Join(root, "gastown", ".beads"),
				"BEADS_DOLT_SERVER_HOST": "gt-dolt",
				"BEADS_DOLT_SERVER_PORT": "3307",
				"TRACKER_URL":            "http://127.0.0.1:7433",
				"TRACKER_TOKEN_FILE":     "/run/secrets/tracker.token",
			} {
				if got := env[k]; got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
			if got, want := len(env), 7+len(tc.env); got != want {
				t.Errorf("environment holds %d entries, want %d: %v", got, want, env)
			}
		})
	}
}

// TestExecutorTimeoutValue: the head's timeout_ms becomes the call's bound; a
// negative value and one that does not fit a Duration are refused, and zero
// takes the executor's default.
func TestExecutorTimeoutValue(t *testing.T) {
	t.Parallel()
	root := testWorkspace(t)
	cases := []struct {
		name string
		ms   int
		want time.Duration
	}{
		{name: "the head's value", ms: 1500, want: 1500 * time.Millisecond},
		{name: "zero takes the default", ms: 0, want: DefaultTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestExecutor(t, root, &fakeRunner{})
			if got, err := e.timeout(tc.ms); err != nil || got != tc.want {
				t.Errorf("timeout(%d) = %s (%v), want %s", tc.ms, got, err, tc.want)
			}
		})
	}
	// The largest timeout_ms whose milliseconds fit a Duration, plus one.
	largestMS := int64(math.MaxInt64) / int64(time.Millisecond)
	for _, ms := range []int{-1, int(largestMS) + 1} {
		e := newTestExecutor(t, root, &fakeRunner{})
		if _, err := e.timeout(ms); !errors.Is(err, ErrBadTimeout) {
			t.Errorf("timeout(%d) = %v, want ErrBadTimeout", ms, err)
		}
	}
}

// TestExecutorUnwired: an executor built without New refuses rather than
// running with a nil semaphore that would block or default.
func TestExecutorUnwired(t *testing.T) {
	t.Parallel()
	for name, e := range map[string]*Executor{"nil": nil, "zero value": {}} {
		t.Run(name, func(t *testing.T) {
			if err := e.Exec(context.Background(), Request{Argv: []string{"list"}}, nil, io.Discard); !errors.Is(err, ErrUnconfigured) {
				t.Errorf("Exec = %v, want ErrUnconfigured", err)
			}
		})
	}
	t.Run("no frame writer", func(t *testing.T) {
		e := newTestExecutor(t, testWorkspace(t), &fakeRunner{})
		if err := e.Exec(context.Background(), Request{Argv: []string{"list"}}, nil, nil); !errors.Is(err, ErrUnconfigured) {
			t.Errorf("Exec = %v, want ErrUnconfigured", err)
		}
	})
}

// TestExecutorNew: every field bd cannot run without is refused by name, the
// two limits reject negatives, and zero takes the design's defaults.
func TestExecutorNew(t *testing.T) {
	t.Parallel()
	base := Options{
		Binary:        "/opt/bd/bd",
		WorkspaceRoot: "/ws",
		Home:          "/home/bdgw",
		Path:          "/usr/bin:/bin",
		DoltHost:      "gt-dolt",
		DoltPort:      "3307",
	}
	mutations := map[string]func(*Options){
		"no binary":            func(o *Options) { o.Binary = "" },
		"no workspace root":    func(o *Options) { o.WorkspaceRoot = "" },
		"relative workspace":   func(o *Options) { o.WorkspaceRoot = "ws" },
		"no HOME":              func(o *Options) { o.Home = "" },
		"no PATH":              func(o *Options) { o.Path = "" },
		"no Dolt host":         func(o *Options) { o.DoltHost = "" },
		"no Dolt port":         func(o *Options) { o.DoltPort = "" },
		"negative concurrency": func(o *Options) { o.MaxConcurrent = -1 },
		"negative timeout":     func(o *Options) { o.DefaultTimeout = -time.Second },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			opts := base
			mutate(&opts)
			if _, err := New(opts); !errors.Is(err, ErrUnconfigured) {
				t.Errorf("New = %v, want ErrUnconfigured", err)
			}
		})
	}

	e, err := New(base)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := cap(e.slots); got != DefaultMaxConcurrent {
		t.Errorf("default concurrency cap = %d, want %d", got, DefaultMaxConcurrent)
	}
	if e.defaultTimeout != DefaultTimeout {
		t.Errorf("default timeout = %s, want %s", e.defaultTimeout, DefaultTimeout)
	}

	opts := base
	opts.MaxConcurrent = 4
	opts.DefaultTimeout = 5 * time.Second
	e, err = New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := cap(e.slots); got != 4 {
		t.Errorf("concurrency cap = %d, want 4", got)
	}
	if e.defaultTimeout != 5*time.Second {
		t.Errorf("timeout = %s, want 5s", e.defaultTimeout)
	}
}

// fakeProcess is a Process a test drives; nothing is started.
type fakeProcess struct {
	stdout  io.Reader
	stderr  io.Reader
	stdin   *recordingStdin
	exited  chan struct{}
	killed  chan struct{}
	once    sync.Once
	status  int
	waitErr error
}

func newExitedFake(status int, stdout, stderr string) *fakeProcess {
	return newFakeProcess(status, stdout, stderr, nil)
}

func newFakeProcess(status int, stdout, stderr string, waitErr error) *fakeProcess {
	p := &fakeProcess{
		stdout:  strings.NewReader(stdout),
		stderr:  strings.NewReader(stderr),
		stdin:   &recordingStdin{},
		exited:  make(chan struct{}),
		killed:  make(chan struct{}),
		status:  status,
		waitErr: waitErr,
	}
	close(p.exited)
	return p
}

// newBlockingFake is a process that exits only when a test finishes it or the
// executor kills it, with stdout repeating output so a broken frame stream has
// something to fail on.
func newBlockingFake(stdout string) *fakeProcess {
	return &fakeProcess{
		stdout: strings.NewReader(stdout),
		stderr: strings.NewReader(""),
		stdin:  &recordingStdin{},
		exited: make(chan struct{}),
		killed: make(chan struct{}),
	}
}

func (p *fakeProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *fakeProcess) Stdout() io.Reader     { return p.stdout }
func (p *fakeProcess) Stderr() io.Reader     { return p.stderr }

func (p *fakeProcess) Wait() (int, error) {
	<-p.exited
	return p.status, p.waitErr
}

func (p *fakeProcess) Kill() error {
	select {
	case <-p.killed:
	default:
		close(p.killed)
	}
	p.finish(-1)
	return nil
}

func (p *fakeProcess) finish(status int) {
	p.once.Do(func() {
		p.status = status
		close(p.exited)
	})
}

func (p *fakeProcess) wasKilled() bool {
	select {
	case <-p.killed:
		return true
	default:
		return false
	}
}

// recordingStdin keeps what the executor fed bd and whether it closed it.
type recordingStdin struct {
	mu     sync.Mutex
	body   bytes.Buffer
	closed bool
}

func (s *recordingStdin) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.Write(b)
}

func (s *recordingStdin) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *recordingStdin) contents() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.String(), s.closed
}

// failingWriter is a client stream that broke.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("the client's stream is gone") }

// errorReader is a request body that broke while it was read.
type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

// startRecord is one call the fake runner was handed.
type startRecord struct {
	binary string
	argv   []string
	env    []string
	dir    string
}

// fakeRunner stands in for the process runner. Its queue is consumed one
// process per Start, then an immediately successful process is returned.
type fakeRunner struct {
	mu      sync.Mutex
	queue   []Process
	calls   []startRecord
	started chan struct{}
	once    sync.Once
	// err makes Start fail, as a missing or unexecutable binary would.
	err error
}

func (r *fakeRunner) Start(binary string, argv, env []string, dir string) (Process, error) {
	r.mu.Lock()
	r.calls = append(r.calls, startRecord{binary: binary, argv: slices.Clone(argv), env: slices.Clone(env), dir: dir})
	var proc Process = newExitedFake(0, "", "")
	if len(r.queue) > 0 {
		proc, r.queue = r.queue[0], r.queue[1:]
	}
	err := r.err
	r.mu.Unlock()
	if r.started != nil {
		r.once.Do(func() { close(r.started) })
	}
	if err != nil {
		return nil, err
	}
	return proc, nil
}

func (r *fakeRunner) records() []startRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

func (r *fakeRunner) only(t *testing.T) startRecord {
	t.Helper()
	records := r.records()
	if len(records) != 1 {
		t.Fatalf("runner started %d calls, want 1", len(records))
	}
	return records[0]
}

// testWorkspace builds the workspace tree the design pushes: the town's .beads
// and one rig's.
func testWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range []string{".beads", filepath.Join("gastown", ".beads")} {
		if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func newTestExecutor(t *testing.T, root string, runner processRunner) *Executor {
	t.Helper()
	e, err := New(Options{
		Binary:           "/opt/bd/bd",
		WorkspaceRoot:    root,
		Home:             "/home/bdgw",
		Path:             "/usr/local/bin:/usr/bin:/bin",
		DoltHost:         "gt-dolt",
		DoltPort:         "3307",
		TrackerURL:       "http://127.0.0.1:7433",
		TrackerTokenFile: "/run/secrets/tracker.token",
		runner:           runner,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func readFrames(t *testing.T, data []byte) []Frame {
	t.Helper()
	frames := make([]Frame, 0, 3)
	for r := bytes.NewReader(data); ; {
		f, err := ReadFrame(r)
		if errors.Is(err, io.EOF) {
			return frames
		}
		if err != nil {
			t.Fatalf("ReadFrame after %d frames: %v", len(frames), err)
		}
		frames = append(frames, f)
	}
}

func streamOf(frames []Frame, kind FrameKind) string {
	var b strings.Builder
	for _, f := range frames {
		if f.Kind == kind {
			b.Write(f.Payload)
		}
	}
	return b.String()
}

func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}
