package bdgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// gatewayPolicy is the policy the handler tests drive: a read verb, the two
// reads with a large-rig cap, a write verb and an admin verb.
const gatewayPolicy = `{
	"refused_flags": ["db"],
	"verbs": [
		{"path": "show", "class": "read"},
		{"path": "list", "class": "read"},
		{"path": "ready", "class": "read"},
		{"path": "create", "class": "write"},
		{"path": "config set", "class": "admin"}
	]
}`

const (
	testReadToken     = "read-token-0123456789abcdef0123456789"
	testDaemonToken   = "daemon-token-0123456789abcdef01234567"
	testOperatorToken = "operator-token-0123456789abcdef012345"
)

// fixedCaps is a CapCounters a test reads directly: a gateway test that does not
// exercise the cap serves this snapshot.
type fixedCaps struct{ snap CapSnapshot }

func (f fixedCaps) CapStats() CapSnapshot { return f.snap }

// newTestGateway builds a Gateway from real pieces: a token directory holding
// one token per class, the gateway policy, the given store, a call log in its
// own directory and runner, and cap counters at zero.
func newTestGateway(t *testing.T, store *WorkspaceStore, log *CallLog, runner CallRunner) *Gateway {
	t.Helper()
	return newTestGatewayWithCaps(t, store, log, runner, fixedCaps{})
}

func newTestGatewayWithCaps(t *testing.T, store *WorkspaceStore, log *CallLog, runner CallRunner, caps CapCounters) *Gateway {
	t.Helper()
	g, err := NewGateway(GatewayOptions{
		Auth:       testAuthenticator(t),
		Policy:     mustPolicy(t),
		Workspaces: store,
		Log:        log,
		Runner:     runner,
		Caps:       caps,
	})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	return g
}

func testAuthenticator(t *testing.T) *Authenticator {
	t.Helper()
	dir := t.TempDir()
	for name, token := range map[string]string{
		"read.dash.token":     testReadToken,
		"daemon.dipper.token": testDaemonToken,
		"operator.mac.token":  testOperatorToken,
	} {
		writeToken(t, dir, name, token)
	}
	auth, err := NewAuthenticator(dir)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	return auth
}

func writeToken(t *testing.T, dir, name, token string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(token), 0o600); err != nil {
		t.Fatalf("write token %s: %v", name, err)
	}
}

func mustPolicy(t *testing.T) *Policy {
	t.Helper()
	p, err := ParsePolicy(strings.NewReader(gatewayPolicy))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	return p
}

func newTestCallLogWithDir(t *testing.T, dir string) *CallLog {
	t.Helper()
	log, err := NewCallLog(CallLogOptions{
		Dir:          dir,
		Policy:       mustPolicy(t),
		BeadPrefixes: []string{"gt", "hq"},
	})
	if err != nil {
		t.Fatalf("NewCallLog: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log
}

func newTestCallLog(t *testing.T) *CallLog {
	t.Helper()
	return newTestCallLogWithDir(t, t.TempDir())
}

// mustStore is an empty, loadable workspace store: a fresh volume before the
// first push.
func mustStore(t *testing.T) *WorkspaceStore {
	t.Helper()
	store, err := OpenWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenWorkspaceStore: %v", err)
	}
	return store
}

// execRequest builds POST /v1/exec with a head line and the stdin that follows.
func execRequest(t *testing.T, token string, head Request, stdin string) *http.Request {
	t.Helper()
	body, err := json.Marshal(head)
	if err != nil {
		t.Fatalf("marshal head: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, execPath, strings.NewReader(string(body)+"\n"+stdin))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Gt-Caller", "daemon.spec-tick")
	return req
}

// TestBdgwServeEndToEnd drives one request through the whole stack: the HTTP
// handler, the authenticator, the policy, the executor and its fake bd, and the
// call log. The fake bd is injected at the executor's runner, so no process
// starts and the test is in-process.
func TestBdgwServeEndToEnd(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := OpenWorkspaceStore(root)
	if err != nil {
		t.Fatalf("OpenWorkspaceStore: %v", err)
	}
	doc := WorkspaceDocument{Files: map[string]string{
		"gastown/.beads/metadata.json": "{}",
		".beads/routes.jsonl":          `{"prefix":"gt","path":"gastown/.beads"}`,
	}}
	doc.ManifestSHA = WorkspaceManifest(doc.Files)
	if _, err := store.Put(doc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	proc := newExitedFake(3, "out\n", "err\n")
	runner := &fakeRunner{queue: []Process{proc}, bound: waitCtx(t)}
	exec := newTestExecutor(t, root, runner)
	logDir := t.TempDir()
	log := newTestCallLogWithDir(t, logDir)
	g := newTestGateway(t, store, log, exec)

	req := execRequest(t, testReadToken, Request{
		Argv: []string{"show", "gt-abc"},
		Env:  map[string]string{"BD_ACTOR": "town/crew/dipper"},
		Rig:  "gastown",
	}, "the body")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(WorkspaceHashHeader); got != doc.ManifestSHA {
		t.Errorf("%s = %q, want %q", WorkspaceHashHeader, got, doc.ManifestSHA)
	}
	frames := readFrames(t, rec.Body.Bytes())
	if got := streamOf(frames, FrameStdout); got != "out\n" {
		t.Errorf("stdout frames = %q, want %q", got, "out\n")
	}
	if got := streamOf(frames, FrameStderr); got != "err\n" {
		t.Errorf("stderr frames = %q, want %q", got, "err\n")
	}
	if got, err := frames[len(frames)-1].ExitStatus(); err != nil || got != 3 {
		t.Errorf("exit status = %d (%v), want 3", got, err)
	}

	// The call reached the executor as bd's argv, in the rig's directory and
	// with BEADS_DIR under the pushed workspace.
	if len(runner.calls) != 1 {
		t.Fatalf("starts = %d, want 1", len(runner.calls))
	}
	call := runner.calls[0]
	if got := strings.Join(call.argv, " "); got != "show gt-abc" {
		t.Errorf("bd argv = %q, want %q", got, "show gt-abc")
	}
	if want := filepath.Join(root, "gastown", ".beads"); envMap(call.env)["BEADS_DIR"] != want {
		t.Errorf("BEADS_DIR = %q, want %q", envMap(call.env)["BEADS_DIR"], want)
	}
	if got := envMap(call.env)["BD_ACTOR"]; got != "town/crew/dipper" {
		t.Errorf("BD_ACTOR = %q, want the caller's", got)
	}
	if got, closed := proc.stdin.contents(); got != "the body" || !closed {
		t.Errorf("bd stdin = %q closed=%v, want the request body closed", got, closed)
	}

	if err := log.Close(); err != nil {
		t.Fatalf("log.Close: %v", err)
	}
	lines := readLines(t, filepath.Join(logDir, DefaultCallLogName))
	if len(lines) != 1 {
		t.Fatalf("call log lines = %d, want 1", len(lines))
	}
	var entry struct {
		Class    string `json:"class"`
		Caller   string `json:"caller"`
		Verb     string `json:"verb"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("decode call log line: %v", err)
	}
	if entry.Class != "read" || entry.Caller != "daemon.spec-tick" || entry.Verb != "show" || entry.ExitCode != 3 {
		t.Errorf("call log entry = %+v, want class read, caller daemon.spec-tick, verb show, exit 3", entry)
	}

	// The same policy refuses a verb it does not list, and the token decides
	// which class the caller is.
	if code := postExec(t, g, testReadToken, Request{Argv: []string{"sql"}}); code != http.StatusForbidden {
		t.Errorf("unlisted verb status = %d, want 403", code)
	}
	if code := postExec(t, g, testReadToken, Request{Argv: []string{"create", "--title=x"}}); code != http.StatusForbidden {
		t.Errorf("write verb with a read token status = %d, want 403", code)
	}
	if code := postExec(t, g, "not-a-token", Request{Argv: []string{"show"}}); code != http.StatusUnauthorized {
		t.Errorf("bad token status = %d, want 401", code)
	}
	if code := postExec(t, g, "", Request{Argv: []string{"show"}}); code != http.StatusUnauthorized {
		t.Errorf("no token status = %d, want 401", code)
	}
}

// postExec runs one exec request and returns its status.
func postExec(t *testing.T, g *Gateway, token string, head Request) int {
	t.Helper()
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, execRequest(t, token, head, ""))
	return rec.Code
}

// stubRunner is the CallRunner seam: it records the request it was given and
// answers with err, writing an exit frame when err is nil.
type stubRunner struct {
	err  error
	req  Request
	ran  bool
	exit int
}

func (s *stubRunner) Exec(ctx context.Context, req Request, stdin io.Reader, frames io.Writer) error {
	s.ran = true
	s.req = req
	if s.err != nil {
		return s.err
	}
	return WriteFrame(frames, ExitFrame(s.exit))
}

// silentRunner returns without writing a frame: a runner that broke the frame
// contract without reporting an error.
type silentRunner struct{}

func (silentRunner) Exec(ctx context.Context, req Request, stdin io.Reader, frames io.Writer) error {
	return nil
}

// TestBdgwServerRefusals drives every refusal the handler answers before a
// stream begins, and every status it maps an executor refusal to. Each row
// fails if its branch is removed.
func TestBdgwServerRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		runner  error
		noFrame bool
		token   string
		head    *Request
		method  string
		path    string
		want    int
	}{
		{name: "no token", token: "", head: &Request{Argv: []string{"show"}}, want: http.StatusUnauthorized},
		{name: "bad token", token: "wrong", head: &Request{Argv: []string{"show"}}, want: http.StatusUnauthorized},
		{name: "unlisted verb", token: testReadToken, head: &Request{Argv: []string{"sql"}}, want: http.StatusForbidden},
		{name: "write verb on a read token", token: testReadToken, head: &Request{Argv: []string{"create"}}, want: http.StatusForbidden},
		{name: "refused flag", token: testReadToken, head: &Request{Argv: []string{"show", "--db", "/x"}}, want: http.StatusForbidden},
		{name: "stdin over the cap", token: testReadToken, runner: ErrStdinTooLarge, head: &Request{Argv: []string{"show"}}, want: http.StatusRequestEntityTooLarge},
		{name: "concurrency cap", token: testReadToken, runner: ErrTooManyCalls, head: &Request{Argv: []string{"show"}}, want: http.StatusTooManyRequests},
		{name: "bad rig", token: testReadToken, runner: ErrBadRig, head: &Request{Argv: []string{"show"}}, want: http.StatusBadRequest},
		{name: "cwd outside", token: testReadToken, runner: ErrCwdOutsideWorkspace, head: &Request{Argv: []string{"show"}}, want: http.StatusBadRequest},
		{name: "no workspace", token: testReadToken, runner: ErrNoWorkspace, head: &Request{Argv: []string{"show"}}, want: http.StatusBadRequest},
		{name: "bad timeout", token: testReadToken, runner: ErrBadTimeout, head: &Request{Argv: []string{"show"}}, want: http.StatusBadRequest},
		{name: "env not allowed", token: testReadToken, runner: ErrEnvNotAllowed, head: &Request{Argv: []string{"show"}}, want: http.StatusBadRequest},
		{name: "unconfigured", token: testReadToken, runner: ErrUnconfigured, head: &Request{Argv: []string{"show"}}, want: http.StatusInternalServerError},
		{name: "runner returned no frame", token: testReadToken, noFrame: true, head: &Request{Argv: []string{"show"}}, want: http.StatusInternalServerError},
		{name: "client gone", token: testReadToken, runner: ErrClientGone, head: &Request{Argv: []string{"show"}}, want: statusClientClosed},
		{name: "unknown failure", token: testReadToken, runner: errors.New("disk on fire"), head: &Request{Argv: []string{"show"}}, want: http.StatusInternalServerError},
		{name: "wrong method", token: testReadToken, method: http.MethodGet, path: execPath, want: http.StatusMethodNotAllowed},
		{name: "unknown path", token: testReadToken, method: http.MethodGet, path: "/v2/exec", want: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var runner CallRunner = &stubRunner{err: tc.runner}
			if tc.noFrame {
				runner = silentRunner{}
			}
			g := newTestGateway(t, mustStore(t), newTestCallLog(t), runner)
			rec := httptest.NewRecorder()
			if tc.method != "" {
				req := httptest.NewRequest(tc.method, tc.path, nil)
				if tc.token != "" {
					req.Header.Set("Authorization", "Bearer "+tc.token)
				}
				g.ServeHTTP(rec, req)
			} else {
				g.ServeHTTP(rec, execRequest(t, tc.token, *tc.head, ""))
			}
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want >= 400 {
				var payload ErrorPayload
				if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || payload.Code == "" {
					t.Errorf("refusal body = %q (%v), want a code and message", rec.Body.String(), err)
				}
			}
		})
	}
}

// TestBdgwInternalFailureHidesDetail: a failure inside the gateway is a 500 whose
// body names the failure in fixed words, never the error text the executor or the
// host wrote, so a caller cannot read host paths or the bd binary from a refusal.
func TestBdgwInternalFailureHidesDetail(t *testing.T) {
	t.Parallel()
	const detail = "no bd binary at /opt/secret/bd"
	cases := []struct {
		name   string
		runner error
	}{
		{name: "unknown failure", runner: errors.New("disk on fire: " + detail)},
		{name: "unconfigured", runner: fmt.Errorf("%w: %s", ErrUnconfigured, detail)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			log, calls := newCapturingCallLog(t)
			g := newTestGateway(t, mustStore(t), log, &stubRunner{err: tc.runner})
			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, execRequest(t, testReadToken, Request{Argv: []string{"show"}}, ""))
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), detail) {
				t.Errorf("body %q echoes the internal error detail", rec.Body.String())
			}
			var payload ErrorPayload
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("body %q is not an error payload: %v", rec.Body.String(), err)
			}
			if payload.Code != codeInternal || payload.Message != "the gateway failed the call" {
				t.Errorf("payload = %+v, want code %q and the fixed message", payload, codeInternal)
			}
			// The operator still gets the cause: the call log keeps the full error.
			entries := calls()
			if len(entries) != 1 || !strings.Contains(entries[0].Error, detail) || entries[0].ExitCode != ExitRefused {
				t.Errorf("call log = %+v, want one refused call carrying the detail", entries)
			}
		})
	}
}

// TestBdgwNoFrameIsLogged: a runner that returns without a frame or an error is a
// 500 for the caller, and the call log records that cause for the operator.
func TestBdgwNoFrameIsLogged(t *testing.T) {
	t.Parallel()
	log, calls := newCapturingCallLog(t)
	g := newTestGateway(t, mustStore(t), log, silentRunner{})
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, execRequest(t, testReadToken, Request{Argv: []string{"show"}}, ""))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	entries := calls()
	if len(entries) != 1 || entries[0].Error != noFrameMessage || entries[0].ExitCode != ExitRefused {
		t.Errorf("call log = %+v, want one refused call with the no-frame cause", entries)
	}
}

// newCapturingCallLog is a call log that keeps its lines in memory instead of on
// disk. The returned func closes the log and returns the entries it stored, so
// it reads what the writer wrote, never a line still queued.
func newCapturingCallLog(t *testing.T) (*CallLog, func() []entry) {
	t.Helper()
	var lines [][]byte
	hook := func(line []byte) error {
		lines = append(lines, append([]byte(nil), line...))
		return nil
	}
	l, err := newCallLog(testOptions(t), hook)
	if err != nil {
		t.Fatalf("newCallLog: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, func() []entry {
		if err := l.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		entries := make([]entry, 0, len(lines))
		for _, line := range lines {
			var e entry
			if err := json.Unmarshal(line, &e); err != nil {
				t.Fatalf("call log line %q: %v", line, err)
			}
			entries = append(entries, e)
		}
		return entries
	}
}

// newRawCallLog is a call log that keeps each line as written, so a test can
// check what reached the log's file rather than what an entry decodes to. The
// returned func closes the log before it reads.
func newRawCallLog(t *testing.T) (*CallLog, func() []string) {
	t.Helper()
	var lines []string
	hook := func(line []byte) error {
		lines = append(lines, strings.TrimSpace(string(line)))
		return nil
	}
	l, err := newCallLog(testOptions(t), hook)
	if err != nil {
		t.Fatalf("newCallLog: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, func() []string {
		if err := l.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		return lines
	}
}

// TestBdgwCapRefusalIsLogged: a call the executor refuses at the concurrency cap
// is answered 429 with code too_many_calls, and the call log keeps one refused
// line for it with the class, caller and verb the 500 refusal's line carries.
func TestBdgwCapRefusalIsLogged(t *testing.T) {
	t.Parallel()
	log, calls := newCapturingCallLog(t)
	g := newTestGateway(t, mustStore(t), log, &stubRunner{err: ErrTooManyCalls})
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, execRequest(t, testReadToken, Request{Argv: []string{"show"}}, ""))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body %s", rec.Code, rec.Body.String())
	}
	var payload ErrorPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body %q is not an error payload: %v", rec.Body.String(), err)
	}
	if payload.Code != codeTooManyCalls {
		t.Errorf("code = %q, want %q", payload.Code, codeTooManyCalls)
	}
	entries := calls()
	if len(entries) != 1 {
		t.Fatalf("call log = %+v, want one line for the refusal", entries)
	}
	if e := entries[0]; e.ExitCode != ExitRefused || e.Class != "read" || e.Caller != "daemon.spec-tick" || e.Verb != "show" {
		t.Errorf("call log line = %+v, want ExitRefused with the class, caller and verb", e)
	}
}

// TestBdgwCapRefusalLeavesCallStats: the line a cap refusal writes moves none of
// the counters GET /v1/stats serves, since the gateway served no call; the
// executor's too_many_calls is where a refusal is counted.
func TestBdgwCapRefusalLeavesCallStats(t *testing.T) {
	t.Parallel()
	log, calls := newCapturingCallLog(t)
	g := newTestGateway(t, mustStore(t), log, &stubRunner{err: ErrTooManyCalls})
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, execRequest(t, testReadToken, Request{Argv: []string{"show"}}, ""))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body %s", rec.Code, rec.Body.String())
	}
	if got := log.Stats(); got.Calls != 0 || got.Errors != 0 || got.Dropped != 0 || len(got.Verbs) != 0 {
		t.Errorf("Stats = %+v, want every counter at zero", got)
	}
	if entries := calls(); len(entries) != 1 {
		t.Errorf("call log = %+v, want the refused line still written", entries)
	}
}

// TestBdgwCapRefusalLogsNoFlagValueOrStdin: the cap refusal's line keeps the
// same fields as the 500 refusal's and carries neither a flag's value nor any
// stdin, whatever the caller sent.
func TestBdgwCapRefusalLogsNoFlagValueOrStdin(t *testing.T) {
	t.Parallel()
	const secretDir = "/secret/workspace-dir"
	const secretStdin = "sk-live-abcdef123456"

	capLog, capLines := newRawCallLog(t)
	g := newTestGateway(t, mustStore(t), capLog, &stubRunner{err: ErrTooManyCalls})
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, execRequest(t, testReadToken, Request{Argv: []string{"show", "-C", secretDir}}, secretStdin))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body %s", rec.Code, rec.Body.String())
	}

	failLog, failLines := newRawCallLog(t)
	fail := newTestGateway(t, mustStore(t), failLog, &stubRunner{err: errors.New("disk on fire")})
	fail.ServeHTTP(httptest.NewRecorder(), execRequest(t, testReadToken, Request{Argv: []string{"show"}}, ""))

	capLine := capLines()
	failLine := failLines()
	if len(capLine) != 1 || len(failLine) != 1 {
		t.Fatalf("call log = %d and %d lines, want one each", len(capLine), len(failLine))
	}
	for _, secret := range []string{secretDir, secretStdin} {
		if strings.Contains(capLine[0], secret) {
			t.Errorf("cap refusal line %q carries %q", capLine[0], secret)
		}
	}
	// The flag's name is logged, its value is not.
	var line entry
	if err := json.Unmarshal([]byte(capLine[0]), &line); err != nil {
		t.Fatalf("cap refusal line %q: %v", capLine[0], err)
	}
	if len(line.Flags) != 1 || line.Flags[0] != "-C" {
		t.Errorf("flags = %v, want only -C", line.Flags)
	}
	// Same fields as the 500 refusal's line: nothing rides along unseen.
	if got, want := keys(decodeLine(t, capLine[0])), keys(decodeLine(t, failLine[0])); !reflect.DeepEqual(got, want) {
		t.Errorf("cap line fields = %v, want the 500 line's %v", got, want)
	}
}

// TestBdgwTimeoutCap: the head's timeout_ms is narrowed to the class maximum
// before it reaches the executor, and a zero or negative one is left for the
// executor's default and its refusal.
func TestBdgwTimeoutCap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		argv []string
		ms   int
		want int
	}{
		{name: "read under the cap", argv: []string{"show"}, ms: 1000, want: 1000},
		{name: "read over the cap", argv: []string{"show"}, ms: 120000, want: 30000},
		{name: "list keeps its large cap", argv: []string{"list"}, ms: 120000, want: 120000},
		{name: "list over its large cap", argv: []string{"list"}, ms: 600000, want: 120000},
		{name: "ready keeps its large cap", argv: []string{"ready"}, ms: 120000, want: 120000},
		{name: "write over the cap", argv: []string{"create"}, ms: 120000, want: 60000},
		{name: "zero is the executor's default", argv: []string{"show"}, ms: 0, want: 0},
		{name: "negative is the executor's refusal", argv: []string{"show"}, ms: -5, want: -5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &stubRunner{exit: 0}
			g := newTestGateway(t, mustStore(t), newTestCallLog(t), stub)
			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, execRequest(t, testDaemonToken, Request{Argv: tc.argv, TimeoutMS: tc.ms}, ""))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
			}
			if !stub.ran {
				t.Fatal("the runner did not run")
			}
			if stub.req.TimeoutMS != tc.want {
				t.Errorf("timeout_ms = %d, want %d", stub.req.TimeoutMS, tc.want)
			}
		})
	}
}

// TestBdgwReadyReasons: Ready names each dependency it can see missing, so a
// gateway built without one refuses rather than serving with the check off.
func TestBdgwReadyReasons(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		g    *Gateway
		want string
	}{
		{name: "no authenticator", g: &Gateway{policy: mustPolicy(t), workspaces: mustStore(t)}, want: "no token loader"},
		{name: "no token table", g: &Gateway{auth: &Authenticator{}, policy: mustPolicy(t), workspaces: mustStore(t)}, want: "no token table is loaded"},
		{name: "no verbs file", g: &Gateway{auth: testAuthenticator(t), workspaces: mustStore(t)}, want: "no verbs file"},
		{name: "no workspace store", g: &Gateway{auth: testAuthenticator(t), policy: mustPolicy(t)}, want: "no workspace is loaded"},
		{name: "no workspace loaded", g: &Gateway{auth: testAuthenticator(t), policy: mustPolicy(t), workspaces: mustStore(t)}, want: "no workspace is loaded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.g.Ready()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Ready = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// loadedGatewayWithProbe builds a gateway whose pieces are all loaded, with the
// given Dolt probe wired, so Ready answers nil and only the probe can refuse.
func loadedGatewayWithProbe(t *testing.T, probe func(context.Context) error) *Gateway {
	t.Helper()
	store := mustStore(t)
	pushWorkspace(t, store, map[string]string{"gastown/.beads/metadata.json": "{}"})
	g, err := NewGateway(GatewayOptions{
		Auth:       testAuthenticator(t),
		Policy:     mustPolicy(t),
		Workspaces: store,
		Log:        newTestCallLog(t),
		Runner:     &stubRunner{exit: 0},
		Caps:       fixedCaps{},
		DoltProbe:  probe,
	})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	return g
}

// getReady drives GET /readyz with a read token and returns the recorder.
func getReady(t *testing.T, g *Gateway) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, readyPath, nil)
	req.Header.Set("Authorization", "Bearer "+testReadToken)
	g.ServeHTTP(rec, req)
	return rec
}

// TestGatewayReadyDoltServe: /readyz is 200 when the Dolt probe answers and 503
// naming Dolt, in a fixed message, when it fails. A call and Ready itself never
// touch the probe, so only readiness reports the Dolt reason.
func TestGatewayReadyDoltServe(t *testing.T) {
	t.Parallel()
	probeErr := errors.New("dial tcp 127.0.0.1:3307: connect: connection refused")
	cases := []struct {
		name    string
		probe   func(context.Context) error
		want    int
		message string
		// notInBody is text the probe's error carries that the body must not:
		// the readiness body names no endpoint.
		notInBody string
	}{
		{name: "dolt answers", probe: func(context.Context) error { return nil }, want: http.StatusOK},
		{name: "dolt unreachable", probe: func(context.Context) error { return probeErr }, want: http.StatusServiceUnavailable,
			message: doltUnreachableMessage, notInBody: "127.0.0.1:3307"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := getReady(t, loadedGatewayWithProbe(t, tc.probe))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.message == "" {
				return
			}
			var payload ErrorPayload
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("body %q is not an error payload: %v", rec.Body.String(), err)
			}
			if payload.Message != tc.message {
				t.Errorf("message = %q, want %q", payload.Message, tc.message)
			}
			if tc.notInBody != "" && strings.Contains(rec.Body.String(), tc.notInBody) {
				t.Errorf("body %q carries %q, want it left out", rec.Body.String(), tc.notInBody)
			}
		})
	}

	// The probe's budget belongs to the handler, not the dialer: the request's
	// context has already ended here, so a handler that waits for the probe
	// hangs instead of refusing.
	t.Run("a probe that ignores its context is bounded", func(t *testing.T) {
		t.Parallel()
		blocked := make(chan struct{})
		t.Cleanup(func() { close(blocked) })
		g := loadedGatewayWithProbe(t, func(context.Context) error {
			<-blocked
			return nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, readyPath, nil).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+testReadToken)
		g.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body.String())
		}
		var payload ErrorPayload
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("body %q is not an error payload: %v", rec.Body.String(), err)
		}
		if payload.Message != doltUnreachableMessage {
			t.Errorf("message = %q, want %q", payload.Message, doltUnreachableMessage)
		}
	})

	// The Dolt reason belongs to /readyz alone: a call and the Ready check must
	// not run the probe, or a slow dial would land in every push path.
	t.Run("only readiness probes", func(t *testing.T) {
		t.Parallel()
		var calls int
		g := loadedGatewayWithProbe(t, func(context.Context) error {
			calls++
			return probeErr
		})
		if err := g.Ready(); err != nil {
			t.Fatalf("Ready = %v, want nil with every piece loaded", err)
		}
		if code := postExec(t, g, testReadToken, Request{Argv: []string{"show"}}); code != http.StatusOK {
			t.Fatalf("call status = %d, want 200", code)
		}
		if calls != 0 {
			t.Errorf("the probe ran %d times outside /readyz, want 0", calls)
		}
	})
}

// TestGatewayReadyDoltNil: with no probe wired, readiness is exactly what it was
// before the probe existed: 200 when the pieces are loaded, and the workspace
// reason still first when they are not.
func TestGatewayReadyDoltNil(t *testing.T) {
	t.Parallel()
	if rec := getReady(t, loadedGatewayWithProbe(t, nil)); rec.Code != http.StatusOK {
		t.Fatalf("loaded gateway status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	g := newTestGateway(t, mustStore(t), newTestCallLog(t), &stubRunner{exit: 0})
	rec := getReady(t, g)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unloaded workspace status = %d, want 503; body %s", rec.Code, rec.Body.String())
	}
	var payload ErrorPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body %q is not an error payload: %v", rec.Body.String(), err)
	}
	if !strings.Contains(payload.Message, "no workspace is loaded") {
		t.Errorf("message = %q, want the workspace reason", payload.Message)
	}
}

// TestBdgwStatsAndWorkspace: the counters need a token, and the workspace takes
// a push only from a class that may write.
func TestBdgwStatsAndWorkspace(t *testing.T) {
	t.Parallel()
	store := mustStore(t)
	g := newTestGateway(t, store, newTestCallLog(t), &stubRunner{exit: 0})

	doc := WorkspaceDocument{Files: map[string]string{"gastown/.beads/metadata.json": "{}"}}
	doc.ManifestSHA = WorkspaceManifest(doc.Files)
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}

	cases := []struct {
		name  string
		token string
		want  int
	}{
		{name: "push as read", token: testReadToken, want: http.StatusForbidden},
		{name: "push as daemon", token: testDaemonToken, want: http.StatusOK},
		{name: "push as operator", token: testOperatorToken, want: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, workspacePath, strings.NewReader(string(body)))
			req.Header.Set("Authorization", "Bearer "+tc.token)
			g.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.want, rec.Body.String())
			}
			if got := rec.Header().Get(WorkspaceHashHeader); got == "" {
				t.Error("the response carries no workspace hash")
			}
		})
	}
	if !store.Loaded() {
		t.Error("the store did not load the pushed workspace")
	}
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, workspacePath, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET workspace = %d, want 405", rec.Code)
	}

	stats := httptest.NewRecorder()
	g.ServeHTTP(stats, httptest.NewRequest(http.MethodGet, statsPath, nil))
	if stats.Code != http.StatusUnauthorized {
		t.Errorf("stats without a token = %d, want 401", stats.Code)
	}
	stats = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, statsPath, nil)
	req.Header.Set("Authorization", "Bearer "+testReadToken)
	g.ServeHTTP(stats, req)
	if stats.Code != http.StatusOK {
		t.Fatalf("stats = %d, want 200; body %s", stats.Code, stats.Body.String())
	}
	var got Stats
	if err := json.Unmarshal(stats.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
}

// exitByVerb answers each call with the exit status the test gave its verb.
type exitByVerb map[string]int

func (m exitByVerb) Exec(ctx context.Context, req Request, stdin io.Reader, frames io.Writer) error {
	return WriteFrame(frames, ExitFrame(m[req.Argv[0]]))
}

// TestBdgwStatsCapFields: GET /v1/stats carries the call log's four counters with
// their values, and the executor's three cap counters beside them.
func TestBdgwStatsCapFields(t *testing.T) {
	t.Parallel()
	caps := fixedCaps{snap: CapSnapshot{TooManyCalls: 7, InFlight: 2, PeakInFlight: 9}}
	g := newTestGatewayWithCaps(t, mustStore(t), newTestCallLog(t), exitByVerb{"show": 0, "list": 1}, caps)
	for _, head := range []Request{{Argv: []string{"show"}}, {Argv: []string{"list"}}} {
		if code := postExec(t, g, testReadToken, head); code != http.StatusOK {
			t.Fatalf("a served call = %d, want 200", code)
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, statsPath, nil)
	req.Header.Set("Authorization", "Bearer "+testReadToken)
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stats = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	// The counters are pointers, so a field the body dropped is nil rather than
	// the zero a reader could not tell from a real value.
	var got struct {
		Calls        *uint64           `json:"calls"`
		Errors       *uint64           `json:"errors"`
		Dropped      *uint64           `json:"dropped"`
		Verbs        map[string]uint64 `json:"verbs"`
		TooManyCalls *uint64           `json:"too_many_calls"`
		InFlight     *uint64           `json:"in_flight"`
		PeakInFlight *uint64           `json:"peak_in_flight"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	for _, f := range []struct {
		name string
		got  *uint64
		want uint64
	}{
		{"calls", got.Calls, 2},
		{"errors", got.Errors, 1},
		{"dropped", got.Dropped, 0},
		{"too_many_calls", got.TooManyCalls, 7},
		{"in_flight", got.InFlight, 2},
		{"peak_in_flight", got.PeakInFlight, 9},
	} {
		switch {
		case f.got == nil:
			t.Errorf("stats carries no %s", f.name)
		case *f.got != f.want:
			t.Errorf("%s = %d, want %d", f.name, *f.got, f.want)
		}
	}
	if got.Verbs["show"] != 1 || got.Verbs["list"] != 1 {
		t.Errorf("verbs = %v, want show and list counted once each", got.Verbs)
	}
}

// brokenResponse is a client that stops reading after the first frame.
type brokenResponse struct {
	header  http.Header
	started bool
	status  int
}

func (b *brokenResponse) Header() http.Header {
	if b.header == nil {
		b.header = http.Header{}
	}
	return b.header
}

func (b *brokenResponse) WriteHeader(status int) { b.status = status }

func (b *brokenResponse) Write(p []byte) (int, error) {
	if b.started {
		return 0, errors.New("the client is gone")
	}
	b.started = true
	return len(p), nil
}

func (b *brokenResponse) Flush() {}

// stdoutThenExit writes output and an exit frame, as the executor does for a
// call that produced output.
type stdoutThenExit struct{}

func (stdoutThenExit) Exec(ctx context.Context, req Request, stdin io.Reader, frames io.Writer) error {
	if err := WriteFrame(frames, Frame{Kind: FrameStdout, Payload: []byte("out\n")}); err != nil {
		return err
	}
	return WriteFrame(frames, ExitFrame(0))
}

// TestBdgwBrokenClientStream: when the caller stops reading after output began,
// the call is recorded as the gateway's failed call (125) rather than as a
// status bd chose, and the stream's status stays the 200 it already sent.
func TestBdgwBrokenClientStream(t *testing.T) {
	t.Parallel()
	logDir := t.TempDir()
	log := newTestCallLogWithDir(t, logDir)
	g := newTestGateway(t, mustStore(t), log, stdoutThenExit{})
	rec := &brokenResponse{}
	g.ServeHTTP(rec, execRequest(t, testReadToken, Request{Argv: []string{"show"}}, ""))

	if rec.status != http.StatusOK {
		t.Errorf("status = %d, want the 200 the stream began with", rec.status)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("log.Close: %v", err)
	}
	lines := readLines(t, filepath.Join(logDir, DefaultCallLogName))
	if len(lines) != 1 {
		t.Fatalf("call log lines = %d, want 1", len(lines))
	}
	var entry struct {
		ExitCode int `json:"exit_code"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("decode call log line: %v", err)
	}
	if entry.ExitCode != 125 {
		t.Errorf("exit_code = %d, want 125: no exit frame reached the caller", entry.ExitCode)
	}
}

// garbageRunner writes bytes that are not a frame, standing in for a runner
// that broke the frame contract.
type garbageRunner struct{}

func (garbageRunner) Exec(ctx context.Context, req Request, stdin io.Reader, frames io.Writer) error {
	_, err := frames.Write([]byte("not a frame"))
	return err
}

// TestBdgwMalformedFrame: a runner's unwritable frame is refused with a 500, not
// read as a call that ran.
func TestBdgwMalformedFrame(t *testing.T) {
	t.Parallel()
	g := newTestGateway(t, mustStore(t), newTestCallLog(t), garbageRunner{})
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, execRequest(t, testReadToken, Request{Argv: []string{"show"}}, ""))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	var payload ErrorPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || payload.Code != codeInternal {
		t.Errorf("body = %q (%v), want code %q", rec.Body.String(), err, codeInternal)
	}
}

// stderrFrame is one frame a test feeds httpFrames.Write.
type stderrFrame struct {
	kind    FrameKind
	payload string
}

// frameBytes returns the wire bytes WriteFrame produces for one frame, which is
// what httpFrames.Write reads in a single call.
func frameBytes(t *testing.T, kind FrameKind, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Frame{Kind: kind, Payload: payload}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	return buf.Bytes()
}

// oneByteStderrFrames splits s into one stderr frame per byte, the shortest
// frames a phrase can be delivered in.
func oneByteStderrFrames(s string) []stderrFrame {
	frames := make([]stderrFrame, 0, len(s))
	for i := 0; i < len(s); i++ {
		frames = append(frames, stderrFrame{FrameStderr, s[i : i+1]})
	}
	return frames
}

// TestCallConflictFrames: httpFrames reports a conflict for the two phrases bd
// writes when Dolt rolled the transaction back, in any case, also when the
// phrase is split across two or more stderr frames of any size, and only for
// stderr.
func TestCallConflictFrames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		write []stderrFrame
		want  bool
	}{
		{
			name:  "Error 1213 in one stderr frame",
			write: []stderrFrame{{FrameStderr, "bd: Error 1213 (40001): this transaction conflicts"}},
			want:  true,
		},
		{
			name:  "Error 1213 in upper case",
			write: []stderrFrame{{FrameStderr, "BD: ERROR 1213 (40001): this transaction conflicts"}},
			want:  true,
		},
		{
			name:  "serialization failure in upper case",
			write: []stderrFrame{{FrameStderr, "bd: SERIALIZATION FAILURE: this transaction conflicts"}},
			want:  true,
		},
		{
			name:  "serialization failure in mixed case",
			write: []stderrFrame{{FrameStderr, "bd: Serialization Failure: this transaction conflicts"}},
			want:  true,
		},
		{
			name: "Error 1213 split across two stderr frames",
			write: []stderrFrame{
				{FrameStderr, "bd: Error 12"},
				{FrameStderr, "13 (40001): this transaction conflicts"},
			},
			want: true,
		},
		{
			name: "serialization failure split across two stderr frames",
			write: []stderrFrame{
				{FrameStderr, "bd: serializ"},
				{FrameStderr, "ation failure: this transaction conflicts"},
			},
			want: true,
		},
		{
			name: "Error 1213 split across three stderr frames",
			write: []stderrFrame{
				{FrameStderr, "bd: Error 1"},
				{FrameStderr, "2"},
				{FrameStderr, "13 (40001): this transaction conflicts"},
			},
			want: true,
		},
		{
			name:  "serialization failure delivered one byte per stderr frame",
			write: oneByteStderrFrames("bd: serialization failure: this transaction conflicts"),
			want:  true,
		},
		{
			name: "serialization failure split around a frame shorter than the carry",
			write: []stderrFrame{
				{FrameStderr, "bd: serialization f"},
				{FrameStderr, "ail"},
				{FrameStderr, "ure: this transaction conflicts"},
			},
			want: true,
		},
		{
			name:  "other stderr",
			write: []stderrFrame{{FrameStderr, "bd: no such issue: gt-nope"}},
			want:  false,
		},
		{
			name:  "stdout carrying the phrase",
			write: []stderrFrame{{FrameStdout, "Error 1213 serialization failure"}},
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &httpFrames{w: httptest.NewRecorder()}
			for _, w := range tt.write {
				if _, err := f.Write(frameBytes(t, w.kind, []byte(w.payload))); err != nil {
					t.Fatalf("Write: %v", err)
				}
			}
			if got := f.Conflict(); got != tt.want {
				t.Errorf("Conflict() = %v, want %v", got, tt.want)
			}
		})
	}
}

// assertFramesForwarded checks the recorder body decodes to exactly the frames a
// test wrote, unchanged, which is what the caller reads.
func assertFramesForwarded(t *testing.T, rec *httptest.ResponseRecorder, sent [][]byte) {
	t.Helper()
	frames := readFrames(t, rec.Body.Bytes())
	if len(frames) != len(sent) {
		t.Fatalf("forwarded frames = %d, want %d", len(frames), len(sent))
	}
	for i, p := range sent {
		if frames[i].Kind != FrameStderr || !bytes.Equal(frames[i].Payload, p) {
			t.Errorf("forwarded frame %d = kind %d, %d bytes, want stderr and the %d bytes written", i, frames[i].Kind, len(frames[i].Payload), len(p))
		}
	}
}

// TestCallConflictFramesBound: the scan stops after the first 64 KiB of stderr,
// and every frame still reaches the caller unchanged.
func TestCallConflictFramesBound(t *testing.T) {
	t.Parallel()

	t.Run("marker starting at the bound is not scanned", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		f := &httpFrames{w: rec}
		sent := [][]byte{
			bytes.Repeat([]byte{'a'}, conflictScanLimit),
			[]byte("bd: Error 1213 (40001): this transaction conflicts"),
		}
		for _, p := range sent {
			if _, err := f.Write(frameBytes(t, FrameStderr, p)); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
		if f.Conflict() {
			t.Error("Conflict() = true for a marker starting at the 64 KiB scan bound")
		}
		assertFramesForwarded(t, rec, sent)
	})

	t.Run("marker past the bound in the same frame is not scanned", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		f := &httpFrames{w: rec}
		oneFrame := append(bytes.Repeat([]byte{'a'}, conflictScanLimit), "bd: Error 1213 (40001)"...)
		if _, err := f.Write(frameBytes(t, FrameStderr, oneFrame)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if f.Conflict() {
			t.Error("Conflict() = true for a marker past the 64 KiB scan bound")
		}
		assertFramesForwarded(t, rec, [][]byte{oneFrame})
	})

	t.Run("marker ending at the bound is found", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		f := &httpFrames{w: rec}
		atEdge := append(bytes.Repeat([]byte{'a'}, conflictScanLimit-len("Error 1213")), "Error 1213"...)
		if _, err := f.Write(frameBytes(t, FrameStderr, atEdge)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if !f.Conflict() {
			t.Error("Conflict() = false for a marker ending at the 64 KiB scan bound")
		}
		assertFramesForwarded(t, rec, [][]byte{atEdge})
	})
}

// TestBdgwNewGatewayRefusesUnwired refuses a gateway missing any piece by name.
func TestBdgwNewGatewayRefusesUnwired(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts GatewayOptions
		want string
	}{
		{name: "no auth", want: "authenticator"},
		{name: "no policy", want: "policy"},
		{name: "no workspace", want: "workspace store"},
		{name: "no log", want: "call log"},
		{name: "no runner", want: "call runner"},
		{name: "no cap counters", want: "cap counters"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := GatewayOptions{
				Auth:       testAuthenticator(t),
				Policy:     mustPolicy(t),
				Workspaces: mustStore(t),
				Log:        newTestCallLog(t),
				Runner:     &stubRunner{},
				Caps:       fixedCaps{},
			}
			switch i {
			case 0:
				opts.Auth = nil
			case 1:
				opts.Policy = nil
			case 2:
				opts.Workspaces = nil
			case 3:
				opts.Log = nil
			case 4:
				opts.Runner = nil
			case 5:
				opts.Caps = nil
			}
			_, err := NewGateway(opts)
			if !errors.Is(err, ErrUnconfigured) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewGateway = %v, want ErrUnconfigured naming %q", err, tc.want)
			}
		})
	}
}

// TestBdgwHealthAlwaysOK: /healthz answers 200 with no token and with a token
// the gateway does not know, since it only says the process serves.
func TestBdgwHealthAlwaysOK(t *testing.T) {
	t.Parallel()
	g := newTestGateway(t, mustStore(t), newTestCallLog(t), &stubRunner{})
	for _, token := range []string{"", "not-a-token"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, healthPath, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		g.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("healthz with token %q = %d, want 200", token, rec.Code)
		}
	}
}
