package bdgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// newTestGateway builds a Gateway from real pieces: a token directory holding
// one token per class, the gateway policy, the given store, a call log in its
// own directory and runner.
func newTestGateway(t *testing.T, store *WorkspaceStore, log *CallLog, runner CallRunner) *Gateway {
	t.Helper()
	g, err := NewGateway(GatewayOptions{
		Auth:       testAuthenticator(t),
		Policy:     mustPolicy(t),
		Workspaces: store,
		Log:        log,
		Runner:     runner,
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
		"read.dash.token":          testReadToken,
		"daemon.dipper.token":      testDaemonToken,
		"operator.mac-sloan.token": testOperatorToken,
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
			g := newTestGateway(t, mustStore(t), newTestCallLog(t), &stubRunner{err: tc.runner})
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
		})
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
