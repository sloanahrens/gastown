package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

const (
	testReadToken   = "read-token-0123456789abcdef0123456789"
	testDaemonToken = "daemon-token-0123456789abcdef01234567"
)

// cmdPolicy is the verbs file the command tests write to disk: a read verb, a
// write verb and a verb the file does not list.
const cmdPolicy = `{
	"refused_flags": ["db"],
	"verbs": [
		{"path": "show", "class": "read"},
		{"path": "create", "class": "write"}
	]
}`

// fixture is a built gateway and the values a request needs to reach it.
type fixture struct {
	gateway       *bdgateway.Gateway
	store         *bdgateway.WorkspaceStore
	readToken     string
	daemonToken   string
	tokensDir     string
	verbsPath     string
	logDir        string
	workspaceRoot string
}

// newFixture builds the gateway bdgw serve builds, in temp directories, with
// runner standing in for the executor.
func newFixture(t *testing.T, runner bdgateway.CallRunner) *fixture {
	t.Helper()
	f := &fixture{
		readToken:     testReadToken,
		daemonToken:   testDaemonToken,
		tokensDir:     t.TempDir(),
		verbsPath:     filepath.Join(t.TempDir(), "verbs.json"),
		logDir:        t.TempDir(),
		workspaceRoot: t.TempDir(),
	}
	writeFile(t, filepath.Join(f.tokensDir, "read.dash.token"), f.readToken, 0o600)
	writeFile(t, filepath.Join(f.tokensDir, "daemon.dipper.token"), f.daemonToken, 0o600)
	writeFile(t, f.verbsPath, cmdPolicy, 0o644)

	auth, err := bdgateway.NewAuthenticator(f.tokensDir)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	policy, err := bdgateway.LoadPolicyFile(f.verbsPath)
	if err != nil {
		t.Fatalf("LoadPolicyFile: %v", err)
	}
	store, err := bdgateway.OpenWorkspaceStore(f.workspaceRoot)
	if err != nil {
		t.Fatalf("OpenWorkspaceStore: %v", err)
	}
	log, err := bdgateway.NewCallLog(bdgateway.CallLogOptions{
		Dir:          f.logDir,
		Policy:       policy,
		BeadPrefixes: []string{"gt"},
	})
	if err != nil {
		t.Fatalf("NewCallLog: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	gateway, err := bdgateway.NewGateway(bdgateway.GatewayOptions{
		Auth:       auth,
		Policy:     policy,
		Workspaces: store,
		Log:        log,
		Runner:     runner,
	})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	f.gateway, f.store = gateway, store
	return f
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// request builds one request against an endpoint with an optional bearer token.
func request(t *testing.T, method, path, token string, body io.Reader) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// pushBody is a valid workspace document with one file.
func pushBody(t *testing.T) string {
	t.Helper()
	doc := bdgateway.WorkspaceDocument{Files: map[string]string{
		"gastown/.beads/metadata.json": "{}",
	}}
	doc.ManifestSHA = bdgateway.WorkspaceManifest(doc.Files)
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	return string(body)
}

// exitRunner answers every call with one exit frame, so a request that reaches
// the executor finishes without a process.
type exitRunner struct {
	status int
}

func (r exitRunner) Exec(ctx context.Context, req bdgateway.Request, stdin io.Reader, frames io.Writer) error {
	return bdgateway.WriteFrame(frames, bdgateway.ExitFrame(r.status))
}

// TestBdgwReadyz: /healthz answers always, /readyz answers only once tokens,
// verbs and a workspace are loaded and a valid token authenticates, and the
// workspace takes a push only from a token that may write.
func TestBdgwReadyz(t *testing.T) {
	t.Parallel()
	f := newFixture(t, exitRunner{})

	health := httptest.NewRecorder()
	f.gateway.ServeHTTP(health, request(t, http.MethodGet, "/healthz", "", nil))
	if health.Code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", health.Code)
	}

	cases := []struct {
		name  string
		token string
		want  int
	}{
		{name: "no token", token: "", want: http.StatusUnauthorized},
		{name: "unknown token", token: "not-a-token", want: http.StatusUnauthorized},
		{name: "no workspace yet", token: f.readToken, want: http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			f.gateway.ServeHTTP(rec, request(t, http.MethodGet, "/readyz", tc.token, nil))
			if rec.Code != tc.want {
				t.Fatalf("/readyz = %d, want %d; body %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	refused := httptest.NewRecorder()
	f.gateway.ServeHTTP(refused, request(t, http.MethodPut, "/v1/workspace", f.readToken, strings.NewReader(pushBody(t))))
	if refused.Code != http.StatusForbidden {
		t.Errorf("push with a read token = %d, want 403", refused.Code)
	}
	accepted := httptest.NewRecorder()
	f.gateway.ServeHTTP(accepted, request(t, http.MethodPut, "/v1/workspace", f.daemonToken, strings.NewReader(pushBody(t))))
	if accepted.Code != http.StatusOK {
		t.Fatalf("push with a daemon token = %d, want 200; body %s", accepted.Code, accepted.Body.String())
	}

	ready := httptest.NewRecorder()
	f.gateway.ServeHTTP(ready, request(t, http.MethodGet, "/readyz", f.readToken, nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("/readyz after the push = %d, want 200; body %s", ready.Code, ready.Body.String())
	}

	f.gateway.BeginShutdown()
	draining := httptest.NewRecorder()
	f.gateway.ServeHTTP(draining, request(t, http.MethodGet, "/readyz", f.readToken, nil))
	if draining.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz while shutting down = %d, want 503", draining.Code)
	}
}

// TestBdgwReadyCheck covers the questions bdgw ready asks: the token directory
// loads, the verbs file loads and a workspace tree is present.
func TestBdgwReadyCheck(t *testing.T) {
	t.Parallel()
	f := newFixture(t, exitRunner{})
	if err := checkReady(f.tokensDir, f.verbsPath, f.workspaceRoot); err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("checkReady with no workspace = %v, want a workspace refusal", err)
	}
	if err := os.MkdirAll(filepath.Join(f.workspaceRoot, "gastown", ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.workspaceRoot, "gastown", ".beads", "metadata.json"), "{}", 0o644)
	if err := checkReady(f.tokensDir, f.verbsPath, f.workspaceRoot); err != nil {
		t.Fatalf("checkReady = %v, want ready", err)
	}
	if err := checkReady(filepath.Join(f.tokensDir, "missing"), f.verbsPath, f.workspaceRoot); err == nil || !strings.Contains(err.Error(), "tokens") {
		t.Errorf("checkReady with no token directory = %v, want a tokens refusal", err)
	}
	if err := checkReady(f.tokensDir, filepath.Join(f.verbsPath, "missing"), f.workspaceRoot); err == nil || !strings.Contains(err.Error(), "verbs") {
		t.Errorf("checkReady with no verbs file = %v, want a verbs refusal", err)
	}
	notATree := filepath.Join(t.TempDir(), "file")
	writeFile(t, notATree, "x", 0o644)
	if err := checkReady(f.tokensDir, f.verbsPath, notATree); err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Errorf("checkReady with a workspace root that cannot be read = %v, want a workspace refusal", err)
	}
}

// blockingRunner holds one call in flight until the test releases it, and
// counts the calls that reached it.
type blockingRunner struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func newBlockingRunner() *blockingRunner {
	return &blockingRunner{started: make(chan struct{}, 1), release: make(chan struct{})}
}

func (b *blockingRunner) Exec(ctx context.Context, req bdgateway.Request, stdin io.Reader, frames io.Writer) error {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	select {
	case b.started <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return bdgateway.WriteFrame(frames, bdgateway.ExitFrame(0))
}

func (b *blockingRunner) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// TestBdgwGracefulShutdown: a call already running when shutdown begins still
// finishes and reports its status, and a call that arrives after it is refused
// without reaching the executor.
func TestBdgwGracefulShutdown(t *testing.T) {
	t.Parallel()
	bound, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	runner := newBlockingRunner()
	f := newFixture(t, runner)

	head, err := json.Marshal(bdgateway.Request{Argv: []string{"show"}})
	if err != nil {
		t.Fatalf("marshal head: %v", err)
	}
	body := string(head) + "\n"
	first := request(t, http.MethodPost, "/v1/exec", f.readToken, strings.NewReader(body)).WithContext(bound)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.gateway.ServeHTTP(rec, first)
	}()
	select {
	case <-runner.started:
	case <-bound.Done():
		t.Fatal("the call did not reach the executor within the test's bound")
	}

	f.gateway.BeginShutdown()

	// The bound on this request is the test's: a gateway that let the call
	// through would block it on the runner, and the deadline ends it instead of
	// hanging the tier.
	second := httptest.NewRecorder()
	f.gateway.ServeHTTP(second, request(t, http.MethodPost, "/v1/exec", f.readToken, strings.NewReader(body)).WithContext(bound))
	if second.Code != http.StatusServiceUnavailable {
		t.Errorf("a call during shutdown = %d, want 503", second.Code)
	}
	if got := runner.callCount(); got != 1 {
		t.Errorf("executor calls = %d, want 1: the refused call must not reach it", got)
	}

	close(runner.release)
	select {
	case <-done:
	case <-bound.Done():
		t.Fatal("the in-flight call did not finish within the test's bound")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("the in-flight call = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	frame, err := bdgateway.ReadFrame(rec.Body)
	if err != nil {
		t.Fatalf("read the exit frame: %v", err)
	}
	if status, err := frame.ExitStatus(); err != nil || status != 0 {
		t.Errorf("exit status = %d (%v), want 0", status, err)
	}
}

// TestBdgwServeStartupRefusals: serve stops before it listens when a piece it
// must load is missing or invalid, and names the piece. The address is one the
// listener refuses, so a regression that reached the listen fails on this
// assertion instead of serving.
func TestBdgwServeStartupRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		change func(t *testing.T, sc *serveConfig)
		want   string
	}{
		{name: "token directory", change: func(t *testing.T, sc *serveConfig) {
			sc.tokensDir = filepath.Join(sc.tokensDir, "missing")
		}, want: "token directory"},
		{name: "verbs file", change: func(t *testing.T, sc *serveConfig) {
			writeFile(t, sc.verbsPath, "{", 0o644)
		}, want: "bad verbs file"},
		{name: "call log", change: func(t *testing.T, sc *serveConfig) {
			file := filepath.Join(t.TempDir(), "file")
			writeFile(t, file, "x", 0o644)
			sc.logDir = filepath.Join(file, "logs")
		}, want: "call log directory"},
		{name: "executor", change: func(t *testing.T, sc *serveConfig) {
			sc.doltHost = ""
		}, want: "Dolt host"},
		{name: "workspace store", change: func(t *testing.T, sc *serveConfig) {
			root := filepath.Join(t.TempDir(), "file")
			writeFile(t, root, "x", 0o644)
			sc.workspaceRoot = root
		}, want: "bad workspace root"},
		{name: "gateway", change: func(t *testing.T, sc *serveConfig) {
			sc.newGateway = func(bdgateway.GatewayOptions) (*bdgateway.Gateway, error) {
				return nil, errors.New("an unwired gateway")
			}
		}, want: "an unwired gateway"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sc := newServeConfig()
			sc.addr = "127.0.0.1:99999999"
			sc.tokensDir = t.TempDir()
			writeFile(t, filepath.Join(sc.tokensDir, "read.dash.token"), testReadToken, 0o600)
			sc.verbsPath = filepath.Join(t.TempDir(), "verbs.json")
			writeFile(t, sc.verbsPath, cmdPolicy, 0o644)
			sc.logDir = t.TempDir()
			sc.workspaceRoot = t.TempDir()
			tc.change(t, sc)
			err := serve(context.Background(), sc)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("serve = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// TestBdgwServeListenFailure: a build whose listener cannot bind is a serve
// failure, so the process exits non-zero rather than reading as a clean stop.
func TestBdgwServeListenFailure(t *testing.T) {
	t.Parallel()
	sc := newServeConfig()
	sc.addr = "127.0.0.1:99999999"
	sc.tokensDir = t.TempDir()
	writeFile(t, filepath.Join(sc.tokensDir, "read.dash.token"), testReadToken, 0o600)
	sc.verbsPath = filepath.Join(t.TempDir(), "verbs.json")
	writeFile(t, sc.verbsPath, cmdPolicy, 0o644)
	sc.logDir = t.TempDir()
	sc.workspaceRoot = t.TempDir()
	if err := serve(context.Background(), sc); err == nil {
		t.Fatal("serve with a listener that cannot bind = nil, want its error")
	}
}

// TestBdgwRunDispatch: the command names its modes, and an unknown one is a
// usage error rather than a silent success.
func TestBdgwRunDispatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want int
	}{
		{name: "no mode", args: nil, want: exitUsage},
		{name: "unknown mode", args: []string{"serve-please"}, want: exitUsage},
		{name: "help", args: []string{"help"}, want: exitOK},
		{name: "exec without arguments", args: []string{"exec"}, want: exitUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if got := run(tc.args, &out, &errOut); got != tc.want {
				t.Errorf("run(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}
