package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// frameRunner answers a call with the frames it was given, as the executor
// would after bd ran.
type frameRunner struct {
	req     bdgateway.Request
	frames  []bdgateway.Frame
	err     error
	tailErr error
	calls   int
}

func (r *frameRunner) Exec(ctx context.Context, req bdgateway.Request, stdin io.Reader, frames io.Writer) error {
	r.calls++
	r.req = req
	if r.err != nil {
		return r.err
	}
	for _, f := range r.frames {
		if err := bdgateway.WriteFrame(frames, f); err != nil {
			return err
		}
	}
	return r.tailErr
}

// newExecFixture builds the policy, call log and config execCall needs.
func newExecFixture(t *testing.T) (*bdgateway.Policy, *bdgateway.CallLog, *execConfig) {
	t.Helper()
	verbsPath := filepath.Join(t.TempDir(), "verbs.json")
	writeFile(t, verbsPath, cmdPolicy, 0o644)
	policy, err := bdgateway.LoadPolicyFile(verbsPath)
	if err != nil {
		t.Fatalf("LoadPolicyFile: %v", err)
	}
	logDir := t.TempDir()
	ec := newExecConfig()
	ec.verbsPath = verbsPath
	ec.logDir = logDir
	log, err := ec.callLog(policy)
	if err != nil {
		t.Fatalf("callLog: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return policy, log, ec
}

// TestBdgwExec: one call from argv and stdin becomes bd's streams and exit
// status, for a caller already inside the container, through the same policy,
// executor and call log the HTTP gateway uses.
func TestBdgwExec(t *testing.T) {
	t.Parallel()
	policy, log, ec := newExecFixture(t)
	ec.rig = "gastown"
	ec.cwd = "gastown/crew/dipper"
	ec.timeoutMS = 120000
	ec.caller = "mac.sloan-shell"

	runner := &frameRunner{frames: []bdgateway.Frame{
		{Kind: bdgateway.FrameStdout, Payload: []byte("hello\n")},
		{Kind: bdgateway.FrameStderr, Payload: []byte("warn\n")},
		{Kind: bdgateway.FrameExit, Payload: []byte("4")},
	}}
	var stdout, stderr bytes.Buffer
	env := map[string]string{"BD_ACTOR": "town/crew/dipper", "HOME": "/root", "SECRET": "x"}
	status := execCall(context.Background(), policy, log, runner, ec, []string{"show", "gt-abc"}, env,
		strings.NewReader("body"), &stdout, &stderr)

	if status != 4 {
		t.Errorf("status = %d, want bd's 4", status)
	}
	if stdout.String() != "hello\n" || stderr.String() != "warn\n" {
		t.Errorf("streams = %q / %q, want bd's own", stdout.String(), stderr.String())
	}
	if runner.calls != 1 {
		t.Errorf("executor calls = %d, want 1", runner.calls)
	}
	if got := runner.req.Rig; got != "gastown" {
		t.Errorf("rig = %q, want gastown", got)
	}
	if got := runner.req.Cwd; got != "gastown/crew/dipper" {
		t.Errorf("cwd = %q, want the town-relative directory", got)
	}
	if got := runner.req.TimeoutMS; got != 30000 {
		t.Errorf("timeout_ms = %d, want the read cap 30000", got)
	}
	if got, ok := runner.req.Env["BD_ACTOR"]; !ok || got != "town/crew/dipper" {
		t.Errorf("env = %v, want BD_ACTOR kept", runner.req.Env)
	}
	for _, name := range []string{"HOME", "SECRET"} {
		if _, ok := runner.req.Env[name]; ok {
			t.Errorf("env carries %s, which the gateway never takes from a caller", name)
		}
	}

	if err := log.Close(); err != nil {
		t.Fatalf("log.Close: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(ec.logDir, bdgateway.DefaultCallLogName))
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	line := strings.TrimSpace(string(data))
	if !strings.Contains(line, `"class":"operator"`) || !strings.Contains(line, `"caller":"mac.sloan-shell"`) ||
		!strings.Contains(line, `"exit_code":4`) || !strings.Contains(line, `"verb":"show"`) {
		t.Errorf("call log line = %s, want class operator, caller mac.sloan-shell, verb show, exit 4", line)
	}
}

// TestBdgwExecRefusals: a verb the policy refuses, and a call the executor
// refuses before bd starts, both exit 125 with the reason on stderr; a gateway
// error frame is shown and the exit frame's status still passes through.
func TestBdgwExecRefusals(t *testing.T) {
	t.Parallel()
	policy, log, ec := newExecFixture(t)
	cases := []struct {
		name       string
		argv       []string
		runner     *frameRunner
		wantStatus int
		wantErr    string
		wantCalls  int
	}{
		{
			name:       "unlisted verb",
			argv:       []string{"sql"},
			runner:     &frameRunner{},
			wantStatus: exitRefused,
			wantErr:    "verb not allowlisted",
		},
		{
			name:       "executor refusal",
			argv:       []string{"show"},
			runner:     &frameRunner{err: bdgateway.ErrNoWorkspace},
			wantStatus: exitRefused,
			wantErr:    "workspace is not present",
			wantCalls:  1,
		},
		{
			name: "gateway error frame",
			argv: []string{"show"},
			runner: &frameRunner{frames: []bdgateway.Frame{
				{Kind: bdgateway.FrameError, Payload: []byte(`{"code":"timeout","message":"bd did not finish"}`)},
				{Kind: bdgateway.FrameExit, Payload: []byte("124")},
			}},
			wantStatus: 124,
			wantErr:    "timeout: bd did not finish",
			wantCalls:  1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			status := execCall(context.Background(), policy, log, tc.runner, ec, tc.argv, nil,
				strings.NewReader(""), &stdout, &stderr)
			if status != tc.wantStatus {
				t.Errorf("status = %d, want %d", status, tc.wantStatus)
			}
			if !strings.Contains(stderr.String(), tc.wantErr) {
				t.Errorf("stderr = %q, want it to name %q", stderr.String(), tc.wantErr)
			}
			if tc.runner.calls != tc.wantCalls {
				t.Errorf("executor calls = %d, want %d", tc.runner.calls, tc.wantCalls)
			}
		})
	}
}

// rawRunner writes bytes that are not a frame, standing in for a runner that
// broke the frame contract.
type rawRunner struct{ payload []byte }

func (r rawRunner) Exec(ctx context.Context, req bdgateway.Request, stdin io.Reader, frames io.Writer) error {
	_, err := frames.Write(r.payload)
	return err
}

// failingWriter is a stream of the caller's that is gone: every write fails.
type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) { return 0, errors.New("stream closed") }

// TestBdgwExecStreamFailures: a frame the sink cannot turn into bd's streams,
// or that is not a frame at all, ends the call as a refusal through the same
// execCall the command runs, never as a success.
func TestBdgwExecStreamFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		runner bdgateway.CallRunner
		stdout io.Writer
		stderr io.Writer
	}{
		{
			name:   "unreadable frame",
			runner: rawRunner{payload: []byte("not a frame")},
		},
		{
			name:   "stdout gone",
			runner: &frameRunner{frames: []bdgateway.Frame{{Kind: bdgateway.FrameStdout, Payload: []byte("out\n")}}},
			stdout: failingWriter{},
		},
		{
			name:   "stderr gone",
			runner: &frameRunner{frames: []bdgateway.Frame{{Kind: bdgateway.FrameStderr, Payload: []byte("err\n")}}},
			stderr: failingWriter{},
		},
		{
			name:   "exit frame without a status",
			runner: &frameRunner{frames: []bdgateway.Frame{{Kind: bdgateway.FrameExit, Payload: []byte("x")}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy, log, ec := newExecFixture(t)
			var stdout, stderr bytes.Buffer
			out := tc.stdout
			if out == nil {
				out = &stdout
			}
			errOut := tc.stderr
			if errOut == nil {
				errOut = &stderr
			}
			status := execCall(context.Background(), policy, log, tc.runner, ec, []string{"show"}, nil,
				strings.NewReader(""), out, errOut)
			if status != exitRefused {
				t.Errorf("status = %d, want the refusal %d", status, exitRefused)
			}
		})
	}
}

// TestBdgwExecLostExitFrame: a call whose stream began but whose exit frame
// never arrived is not a success. exec exits with the refusal status and the
// call log records it, as the HTTP handler records 125 for the same ending.
func TestBdgwExecLostExitFrame(t *testing.T) {
	t.Parallel()
	policy, log, ec := newExecFixture(t)
	runner := &frameRunner{
		frames:  []bdgateway.Frame{{Kind: bdgateway.FrameStdout, Payload: []byte("out\n")}},
		tailErr: bdgateway.ErrClientGone,
	}
	var stdout, stderr bytes.Buffer
	status := execCall(context.Background(), policy, log, runner, ec, []string{"show"}, nil,
		strings.NewReader(""), &stdout, &stderr)
	if status != exitRefused {
		t.Errorf("status = %d, want %d: a call with no exit frame is not a success", status, exitRefused)
	}
	if !strings.Contains(stderr.String(), "without an exit frame") {
		t.Errorf("stderr = %q, want it to name the lost exit frame", stderr.String())
	}
	if err := log.Close(); err != nil {
		t.Fatalf("log.Close: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(ec.logDir, bdgateway.DefaultCallLogName))
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	if !strings.Contains(string(data), `"exit_code":125`) {
		t.Errorf("call log = %s, want exit 125", strings.TrimSpace(string(data)))
	}
}

// TestBdgwExecEnv: the process environment becomes the request's env map, with
// an entry that names no value dropped rather than half-read.
func TestBdgwExecEnv(t *testing.T) {
	t.Parallel()
	got := callerEnv([]string{"BD_ACTOR=a", "PATH=/bin", "no-value", "=nameless"})
	want := map[string]string{"BD_ACTOR": "a", "PATH": "/bin"}
	if len(got) != len(want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env[%q] = %q, want %q", k, got[k], v)
		}
	}
}
