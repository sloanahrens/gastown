package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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
		Caps:       fixedCaps{},
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

// fixedCaps serves a cap snapshot a test sets; the fixture runs calls only for
// their HTTP answers.
type fixedCaps struct{ snap bdgateway.CapSnapshot }

func (f fixedCaps) CapStats() bdgateway.CapSnapshot { return f.snap }

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
// loads, the verbs file loads, a workspace tree is present and Dolt answers.
func TestBdgwReadyCheck(t *testing.T) {
	t.Parallel()
	f := newFixture(t, exitRunner{})
	passes := func(context.Context) error { return nil }
	fails := func(context.Context) error { return errors.New("connection refused") }
	if err := checkReady(f.tokensDir, f.verbsPath, f.workspaceRoot, passes); err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("checkReady with no workspace = %v, want a workspace refusal", err)
	}
	if err := os.MkdirAll(filepath.Join(f.workspaceRoot, "gastown", ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.workspaceRoot, "gastown", ".beads", "metadata.json"), "{}", 0o644)
	if err := checkReady(f.tokensDir, f.verbsPath, f.workspaceRoot, passes); err != nil {
		t.Fatalf("checkReady = %v, want ready", err)
	}
	if err := checkReady(f.tokensDir, f.verbsPath, f.workspaceRoot, fails); err == nil || !strings.Contains(err.Error(), "dolt: connection refused") {
		t.Errorf("checkReady with a failing Dolt probe = %v, want a dolt refusal", err)
	}
	if err := checkReady(f.tokensDir, f.verbsPath, f.workspaceRoot, nil); err == nil || !strings.Contains(err.Error(), "dolt: no probe is wired") {
		t.Errorf("checkReady with no Dolt probe = %v, want a refusal naming the missing probe", err)
	}
	if err := checkReady(filepath.Join(f.tokensDir, "missing"), f.verbsPath, f.workspaceRoot, passes); err == nil || !strings.Contains(err.Error(), "tokens") {
		t.Errorf("checkReady with no token directory = %v, want a tokens refusal", err)
	}
	if err := checkReady(f.tokensDir, filepath.Join(f.verbsPath, "missing"), f.workspaceRoot, passes); err == nil || !strings.Contains(err.Error(), "verbs") {
		t.Errorf("checkReady with no verbs file = %v, want a verbs refusal", err)
	}
	notATree := filepath.Join(t.TempDir(), "file")
	writeFile(t, notATree, "x", 0o644)
	if err := checkReady(f.tokensDir, f.verbsPath, notATree, passes); err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Errorf("checkReady with a workspace root that cannot be read = %v, want a workspace refusal", err)
	}
}

// newReadyTestConfig is a readyConfig whose files load and whose workspace holds
// one file, so only the Dolt answer decides the check.
func newReadyTestConfig(t *testing.T) *readyConfig {
	t.Helper()
	rc := newReadyConfig()
	rc.tokensDir = t.TempDir()
	writeFile(t, filepath.Join(rc.tokensDir, "read.dash.token"), testReadToken, 0o600)
	rc.verbsPath = filepath.Join(t.TempDir(), "verbs.json")
	writeFile(t, rc.verbsPath, cmdPolicy, 0o644)
	rc.workspaceRoot = t.TempDir()
	if err := os.MkdirAll(filepath.Join(rc.workspaceRoot, "gastown", ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(rc.workspaceRoot, "gastown", ".beads", "metadata.json"), "{}", 0o644)
	return rc
}

// pipeConn answers a dial with an in-memory connection, so the success case
// needs no socket.
func pipeConn(context.Context, string, string) (net.Conn, error) {
	client, server := net.Pipe()
	if err := server.Close(); err != nil {
		return nil, err
	}
	return client, nil
}

// TestReadyDoltExit: bdgw ready exits 0 when the files load and the Dolt dial
// passes, and exits 1 naming dolt when the dial fails or never answers.
func TestReadyDoltExit(t *testing.T) {
	t.Parallel()
	// blocked holds a dial that ignores its context open until the test ends, so
	// the deadline, not the dial, is what makes ready answer.
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	cases := []struct {
		name string
		dial func(context.Context, string, string) (net.Conn, error)
		// probeTimeout is the production bound for the rows that answer at
		// once, because only the last row tests the deadline: a short bound
		// on the others races their answer against a loaded runner's
		// scheduling and measures the host, not the gateway. The last row
		// overrides it so the test does not wait the production bound.
		probeTimeout time.Duration
		wantStatus   int
		wantStderr   string
	}{
		{
			name:         "all pass",
			dial:         pipeConn,
			probeTimeout: doltProbeTimeout,
			wantStatus:   exitOK,
		},
		{
			name:         "dolt unreachable",
			dial:         func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("connection refused") },
			probeTimeout: doltProbeTimeout,
			wantStatus:   exitFailed,
			wantStderr:   "not ready: dolt: connection refused",
		},
		{
			name: "dial never answers",
			dial: func(context.Context, string, string) (net.Conn, error) {
				<-blocked
				return nil, errors.New("released")
			},
			probeTimeout: 20 * time.Millisecond,
			wantStatus:   exitFailed,
			wantStderr:   "not ready: dolt: context deadline exceeded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rc := newReadyTestConfig(t)
			rc.probeTimeout = tc.probeTimeout
			var mu sync.Mutex
			var dialed []string
			rc.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
				mu.Lock()
				dialed = append(dialed, addr)
				mu.Unlock()
				return tc.dial(ctx, network, addr)
			}
			var stderr bytes.Buffer
			got := runReadyWith(nil, &stderr, rc)
			if got != tc.wantStatus {
				t.Fatalf("bdgw ready = %d, want %d; stderr %s", got, tc.wantStatus, stderr.String())
			}
			if got != exitOK {
				if !strings.Contains(stderr.String(), tc.wantStderr) {
					t.Errorf("stderr = %q, want it to carry %q", stderr.String(), tc.wantStderr)
				}
				return
			}
			mu.Lock()
			defer mu.Unlock()
			want := net.JoinHostPort(rc.doltHost, rc.doltPort)
			if len(dialed) != 1 || dialed[0] != want {
				t.Errorf("the Dolt probe dialed %v, want [%s]", dialed, want)
			}
		})
	}
}

// TestReadyDoltServe: the serving gateway answers /readyz 503 with the Dolt
// message when the injected dial fails, and 200 when it passes, having asked
// the endpoint the flags name.
func TestReadyDoltServe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		dial func(context.Context, string, string) (net.Conn, error)
		want int
	}{
		{
			name: "dial fails",
			dial: func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("connection refused") },
			want: http.StatusServiceUnavailable,
		},
		{name: "dial passes", dial: pipeConn, want: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bound, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			sc := newServeTestConfig(t)
			var mu sync.Mutex
			var dialed []string
			sc.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
				mu.Lock()
				dialed = append(dialed, addr)
				mu.Unlock()
				return tc.dial(ctx, network, addr)
			}
			mainPl, pushPl := newPipeListener(), newPipeListener()
			sc.listen = listenByAddr(sc.addr, mainPl, sc.pushAddr, pushPl)
			mainClient, pushClient := dialClient(t, mainPl), dialClient(t, pushPl)

			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			served := make(chan error, 1)
			go func() { served <- serve(ctx, sc) }()
			waitFor(t, bound, "both listeners accept", func() bool {
				return acceptsNow(t, bound, mainPl) && acceptsNow(t, bound, pushPl)
			})
			pushed := send(bound, pushClient, http.MethodPut, "/v1/workspace", testDaemonToken, strings.NewReader(pushBody(t)))
			if pushed.err != nil || pushed.status != http.StatusOK {
				t.Fatalf("push = %d (%v), want 200; body %s", pushed.status, pushed.err, pushed.body)
			}

			ready := send(bound, mainClient, http.MethodGet, "/readyz", testReadToken, nil)
			if ready.err != nil {
				t.Fatalf("/readyz: %v", ready.err)
			}
			if ready.status != tc.want {
				t.Fatalf("/readyz = %d, want %d; body %s", ready.status, tc.want, ready.body)
			}
			if tc.want != http.StatusOK && !strings.Contains(string(ready.body), "dolt is not reachable") {
				t.Errorf("/readyz body = %s, want the Dolt message", ready.body)
			}
			if tc.want == http.StatusOK && !strings.Contains(string(ready.body), `"ready"`) {
				t.Errorf("/readyz body = %s, want the ready status", ready.body)
			}
			mu.Lock()
			gotDialed := append([]string(nil), dialed...)
			mu.Unlock()
			wantAddr := net.JoinHostPort(sc.doltHost, sc.doltPort)
			if len(gotDialed) != 1 || gotDialed[0] != wantAddr {
				t.Errorf("the Dolt probe dialed %v, want [%s]", gotDialed, wantAddr)
			}

			stop()
			select {
			case err := <-served:
				if err != nil {
					t.Errorf("serve after the drain = %v, want nil", err)
				}
			case <-bound.Done():
				t.Fatal("serve did not return after the drain")
			}
		})
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

// errPipeClosed is what a dial gets once the in-memory listener has closed.
var errPipeClosed = errors.New("the in-memory listener is closed")

// pipeAddr is the address of the in-memory listener.
type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// pipeListener is an in-memory net.Listener. Each dial is a net.Pipe whose far
// end the server accepts, so serve runs its real listener and drain with no
// socket.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

// dial returns the client end of a new connection, or errPipeClosed once the
// listener has closed.
func (l *pipeListener) dial(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.done:
		_ = client.Close()
		_ = server.Close()
		return nil, errPipeClosed
	case <-ctx.Done():
		_ = client.Close()
		_ = server.Close()
		return nil, ctx.Err()
	}
}

// waitFor yields until cond holds, and fails the test when bound ends first.
func waitFor(t *testing.T, bound context.Context, what string, cond func() bool) {
	t.Helper()
	for !cond() {
		if err := bound.Err(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		runtime.Gosched()
	}
}

// TestBdgwServeDrainsInFlightCall: serve runs its accept loop and drain on an
// in-memory listener. Once shutdown begins, the listener closes at once, yet a
// call already in flight on its connection finishes with its status, and serve
// returns nil only after that. The executor is a blocking runner, injected
// through the gateway seam, so the call stays in flight until the test releases
// it.
func TestBdgwServeDrainsInFlightCall(t *testing.T) {
	t.Parallel()
	bound, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	sc := newServeConfig()
	sc.tokensDir = t.TempDir()
	writeFile(t, filepath.Join(sc.tokensDir, "read.dash.token"), testReadToken, 0o600)
	sc.verbsPath = filepath.Join(t.TempDir(), "verbs.json")
	writeFile(t, sc.verbsPath, cmdPolicy, 0o644)
	sc.logDir = t.TempDir()
	sc.workspaceRoot = t.TempDir()
	runner := newBlockingRunner()
	sc.newGateway = func(opts bdgateway.GatewayOptions) (*bdgateway.Gateway, error) {
		opts.Runner = runner
		return bdgateway.NewGateway(opts)
	}
	pl := newPipeListener()
	pushPl := newPipeListener()
	sc.listen = listenByAddr(sc.addr, pl, sc.pushAddr, pushPl)
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return pl.dial(ctx) },
	}}
	t.Cleanup(client.CloseIdleConnections)
	// accepts reports whether the listener takes a new connection right now.
	accepts := func() bool {
		c, err := pl.dial(bound)
		if err != nil {
			return false
		}
		if err := c.Close(); err != nil {
			t.Errorf("close the probe connection: %v", err)
		}
		return true
	}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, sc) }()
	var serveErr error
	finished := false
	returned := func() bool {
		if !finished {
			select {
			case serveErr = <-served:
				finished = true
			default:
			}
		}
		return finished
	}
	waitFor(t, bound, "the server listens", func() bool {
		return returned() || accepts()
	})
	if returned() {
		t.Fatalf("serve returned before any shutdown: %v", serveErr)
	}

	head, err := json.Marshal(bdgateway.Request{Argv: []string{"show"}})
	if err != nil {
		t.Fatalf("marshal head: %v", err)
	}
	type reply struct {
		status int
		body   []byte
		err    error
	}
	inFlight := make(chan reply, 1)
	go func() {
		req, err := http.NewRequestWithContext(bound, http.MethodPost, "http://bdgw.test/v1/exec", strings.NewReader(string(head)+"\n"))
		if err != nil {
			inFlight <- reply{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+testReadToken)
		resp, err := client.Do(req)
		if err != nil {
			inFlight <- reply{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		inFlight <- reply{status: resp.StatusCode, body: body, err: err}
	}()
	select {
	case <-runner.started:
	case <-bound.Done():
		t.Fatal("the call did not reach the executor within the test's bound")
	}

	stop()
	// The listener closes as the drain begins, while the call is still held.
	waitFor(t, bound, "the drain closes the listener", func() bool {
		return !accepts()
	})
	if returned() {
		t.Fatal("serve returned while a call was still in flight")
	}

	close(runner.release)
	var got reply
	select {
	case got = <-inFlight:
	case <-bound.Done():
		t.Fatal("the in-flight call did not finish within the test's bound")
	}
	if got.err != nil {
		t.Fatalf("the in-flight call failed in the drain: %v", got.err)
	}
	if got.status != http.StatusOK {
		t.Fatalf("the in-flight call = %d, want 200", got.status)
	}
	frame, err := bdgateway.ReadFrame(bytes.NewReader(got.body))
	if err != nil {
		t.Fatalf("read the exit frame: %v", err)
	}
	if status, err := frame.ExitStatus(); err != nil || status != 0 {
		t.Errorf("exit status = %d (%v), want 0", status, err)
	}
	waitFor(t, bound, "serve returns after the drain", returned)
	if serveErr != nil {
		t.Errorf("serve after a drain = %v, want nil", serveErr)
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

// listenByAddr is a serve listen seam that hands each of the two addresses its
// own in-memory listener and refuses any other, so a test sees every listener
// serve opens.
func listenByAddr(mainAddr string, mainPl *pipeListener, pushAddr string, pushPl *pipeListener) func(string) (net.Listener, error) {
	return func(addr string) (net.Listener, error) {
		switch addr {
		case mainAddr:
			return mainPl, nil
		case pushAddr:
			return pushPl, nil
		default:
			return nil, fmt.Errorf("bdgw serve listened on %q, want %q or %q", addr, mainAddr, pushAddr)
		}
	}
}

// newServeTestConfig is a serveConfig whose pieces load from temp directories,
// with an exit runner behind the gateway.
func newServeTestConfig(t *testing.T) *serveConfig {
	t.Helper()
	sc := newServeConfig()
	sc.tokensDir = t.TempDir()
	writeFile(t, filepath.Join(sc.tokensDir, "read.dash.token"), testReadToken, 0o600)
	writeFile(t, filepath.Join(sc.tokensDir, "daemon.dipper.token"), testDaemonToken, 0o600)
	sc.verbsPath = filepath.Join(t.TempDir(), "verbs.json")
	writeFile(t, sc.verbsPath, cmdPolicy, 0o644)
	sc.logDir = t.TempDir()
	sc.workspaceRoot = t.TempDir()
	sc.newGateway = func(opts bdgateway.GatewayOptions) (*bdgateway.Gateway, error) {
		opts.Runner = exitRunner{}
		return bdgateway.NewGateway(opts)
	}
	return sc
}

// dialClient returns a client whose connections reach the given in-memory
// listener.
func dialClient(t *testing.T, pl *pipeListener) *http.Client {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return pl.dial(ctx) },
	}}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

// acceptsNow reports whether the listener takes a new connection right now.
func acceptsNow(t *testing.T, bound context.Context, pl *pipeListener) bool {
	t.Helper()
	c, err := pl.dial(bound)
	if err != nil {
		return false
	}
	if err := c.Close(); err != nil {
		t.Errorf("close the probe connection: %v", err)
	}
	return true
}

// execHead is a valid POST /v1/exec head for the read verb show.
func execHead(t *testing.T) string {
	t.Helper()
	head, err := json.Marshal(bdgateway.Request{Argv: []string{"show"}})
	if err != nil {
		t.Fatalf("marshal head: %v", err)
	}
	return string(head) + "\n"
}

// httpReply is one request's status, body or transport error.
type httpReply struct {
	status int
	body   []byte
	err    error
}

// send makes one request against the gateway through an in-memory listener.
func send(bound context.Context, client *http.Client, method, path, token string, body io.Reader) httpReply {
	req, err := http.NewRequestWithContext(bound, method, "http://bdgw.test"+path, body)
	if err != nil {
		return httpReply{err: err}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return httpReply{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	return httpReply{status: resp.StatusCode, body: got, err: err}
}

// brokenListener is a net.Listener whose Accept fails at once, so a server on it
// ends serving with the error.
type brokenListener struct{ err error }

func (l brokenListener) Accept() (net.Conn, error) { return nil, l.err }
func (l brokenListener) Close() error              { return nil }
func (l brokenListener) Addr() net.Addr            { return pipeAddr{} }

// closeErrListener closes the pipe listener it wraps and reports err with the
// result, standing in for a socket whose Close fails.
type closeErrListener struct {
	*pipeListener
	err error
}

func (l closeErrListener) Close() error {
	return errors.Join(l.pipeListener.Close(), l.err)
}

// TestServePushListenerRoutes: with the default flags the push listener serves
// PUT /v1/workspace and answers /v1/exec 404, while the main listener still runs
// a call. The gateway is the production one; only the listener is in memory.
func TestServePushListenerRoutes(t *testing.T) {
	t.Parallel()
	bound, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	sc := newServeTestConfig(t)
	mainPl, pushPl := newPipeListener(), newPipeListener()
	sc.listen = listenByAddr(sc.addr, mainPl, sc.pushAddr, pushPl)
	mainClient, pushClient := dialClient(t, mainPl), dialClient(t, pushPl)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, sc) }()
	waitFor(t, bound, "both listeners accept", func() bool {
		return acceptsNow(t, bound, mainPl) && acceptsNow(t, bound, pushPl)
	})

	pushed := send(bound, pushClient, http.MethodPut, "/v1/workspace", testDaemonToken, strings.NewReader(pushBody(t)))
	if pushed.err != nil || pushed.status != http.StatusOK {
		t.Fatalf("push listener PUT /v1/workspace = %d (%v), want 200; body %s", pushed.status, pushed.err, pushed.body)
	}
	pushExec := send(bound, pushClient, http.MethodPost, "/v1/exec", testReadToken, strings.NewReader(execHead(t)))
	if pushExec.err != nil || pushExec.status != http.StatusNotFound {
		t.Fatalf("push listener POST /v1/exec = %d (%v), want 404; body %s", pushExec.status, pushExec.err, pushExec.body)
	}
	mainExec := send(bound, mainClient, http.MethodPost, "/v1/exec", testReadToken, strings.NewReader(execHead(t)))
	if mainExec.err != nil || mainExec.status != http.StatusOK {
		t.Fatalf("main listener POST /v1/exec = %d (%v), want 200; body %s", mainExec.status, mainExec.err, mainExec.body)
	}

	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serve after the drain = %v, want nil", err)
		}
	case <-bound.Done():
		t.Fatal("serve did not return after the drain")
	}
}

// TestServePushListenerOff: --push-addr "" binds the main listener only.
func TestServePushListenerOff(t *testing.T) {
	t.Parallel()
	bound, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	sc := newServeTestConfig(t)
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	bindServe(fs, sc)
	if err := fs.Parse([]string{"--push-addr", ""}); err != nil {
		t.Fatalf(`parse --push-addr "": %v`, err)
	}
	if sc.pushAddr != "" {
		t.Fatalf(`--push-addr "" left pushAddr = %q, want empty`, sc.pushAddr)
	}

	mainPl := newPipeListener()
	var mu sync.Mutex
	var listened []string
	sc.listen = func(addr string) (net.Listener, error) {
		mu.Lock()
		listened = append(listened, addr)
		mu.Unlock()
		if addr != sc.addr {
			return nil, fmt.Errorf("bdgw serve listened on %q, want only %q", addr, sc.addr)
		}
		return mainPl, nil
	}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, sc) }()
	waitFor(t, bound, "the main listener accepts", func() bool {
		return acceptsNow(t, bound, mainPl)
	})

	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serve with the push listener off = %v, want nil", err)
		}
	case <-bound.Done():
		t.Fatal("serve did not return after the drain")
	}
	mu.Lock()
	got := append([]string(nil), listened...)
	mu.Unlock()
	if len(got) != 1 || got[0] != sc.addr {
		t.Errorf("listeners opened = %v, want only %q", got, sc.addr)
	}
}

// TestServePushListenerDrain: cancelling the context drains both listeners
// within the grace. A call already running finishes, and serve returns nil only
// after it does.
func TestServePushListenerDrain(t *testing.T) {
	t.Parallel()
	bound, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	sc := newServeTestConfig(t)
	runner := newBlockingRunner()
	sc.newGateway = func(opts bdgateway.GatewayOptions) (*bdgateway.Gateway, error) {
		opts.Runner = runner
		return bdgateway.NewGateway(opts)
	}
	mainPl, pushPl := newPipeListener(), newPipeListener()
	sc.listen = listenByAddr(sc.addr, mainPl, sc.pushAddr, pushPl)
	mainClient := dialClient(t, mainPl)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, sc) }()
	waitFor(t, bound, "both listeners accept", func() bool {
		return acceptsNow(t, bound, mainPl) && acceptsNow(t, bound, pushPl)
	})

	inFlight := make(chan httpReply, 1)
	go func() {
		inFlight <- send(bound, mainClient, http.MethodPost, "/v1/exec", testReadToken, strings.NewReader(execHead(t)))
	}()
	select {
	case <-runner.started:
	case <-bound.Done():
		t.Fatal("the call did not reach the executor within the test's bound")
	}

	stop()
	waitFor(t, bound, "the drain closes both listeners", func() bool {
		return !acceptsNow(t, bound, mainPl) && !acceptsNow(t, bound, pushPl)
	})
	select {
	case err := <-served:
		t.Fatalf("serve returned while a call was still in flight: %v", err)
	default:
	}

	close(runner.release)
	var got httpReply
	select {
	case got = <-inFlight:
	case <-bound.Done():
		t.Fatal("the in-flight call did not finish within the test's bound")
	}
	if got.err != nil || got.status != http.StatusOK {
		t.Fatalf("the in-flight call = %d (%v), want 200", got.status, got.err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serve after the drain = %v, want nil", err)
		}
	case <-bound.Done():
		t.Fatal("serve did not return after the drain")
	}
}

// TestServePushListenerDrainTimesOut: a call still running when the grace
// expires makes the drain fail, and serve returns that error rather than
// reading the shutdown as clean.
func TestServePushListenerDrainTimesOut(t *testing.T) {
	t.Parallel()
	bound, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	sc := newServeTestConfig(t)
	sc.shutdownGrace = time.Millisecond
	runner := newBlockingRunner()
	sc.newGateway = func(opts bdgateway.GatewayOptions) (*bdgateway.Gateway, error) {
		opts.Runner = runner
		return bdgateway.NewGateway(opts)
	}
	mainPl, pushPl := newPipeListener(), newPipeListener()
	sc.listen = listenByAddr(sc.addr, mainPl, sc.pushAddr, pushPl)
	mainClient := dialClient(t, mainPl)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, sc) }()
	waitFor(t, bound, "both listeners accept", func() bool {
		return acceptsNow(t, bound, mainPl) && acceptsNow(t, bound, pushPl)
	})

	inFlight := make(chan httpReply, 1)
	go func() {
		inFlight <- send(bound, mainClient, http.MethodPost, "/v1/exec", testReadToken, strings.NewReader(execHead(t)))
	}()
	select {
	case <-runner.started:
	case <-bound.Done():
		t.Fatal("the call did not reach the executor within the test's bound")
	}

	stop()
	select {
	case err := <-served:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("serve with a grace the call outlasts = %v, want %v", err, context.DeadlineExceeded)
		}
	case <-bound.Done():
		t.Fatal("serve did not return when the grace expired")
	}
	// Release the call so the handler and its connection finish before the
	// test ends.
	close(runner.release)
	select {
	case <-inFlight:
	case <-bound.Done():
		t.Fatal("the in-flight call did not finish within the test's bound")
	}
}

// TestServePushListenerFailure: a listener that cannot bind or fails while
// serving ends serve with that error, and the other listener does not keep
// accepting.
func TestServePushListenerFailure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// listen hands each address the listener the case wants for it.
		listen func(sc *serveConfig, mainPl, pushPl *pipeListener) func(string) (net.Listener, error)
		// probe is the listener that must be closed once serve returns.
		probe func(mainPl, pushPl *pipeListener) *pipeListener
		want  string
	}{
		{
			name: "push cannot bind",
			listen: func(sc *serveConfig, mainPl, _ *pipeListener) func(string) (net.Listener, error) {
				return func(addr string) (net.Listener, error) {
					if addr == sc.pushAddr {
						return nil, errors.New("the push address is taken")
					}
					return mainPl, nil
				}
			},
			probe: func(mainPl, _ *pipeListener) *pipeListener { return mainPl },
			want:  "the push address is taken",
		},
		{
			name: "push cannot bind and the main listener does not close",
			listen: func(sc *serveConfig, mainPl, _ *pipeListener) func(string) (net.Listener, error) {
				return func(addr string) (net.Listener, error) {
					if addr == sc.pushAddr {
						return nil, errors.New("the push address is taken")
					}
					return closeErrListener{pipeListener: mainPl, err: errors.New("the main listener did not close")}, nil
				}
			},
			probe: func(mainPl, _ *pipeListener) *pipeListener { return mainPl },
			want:  "the main listener did not close",
		},
		{
			name: "push fails while serving",
			listen: func(sc *serveConfig, mainPl, _ *pipeListener) func(string) (net.Listener, error) {
				return func(addr string) (net.Listener, error) {
					if addr == sc.pushAddr {
						return brokenListener{err: errors.New("the push listener broke")}, nil
					}
					return mainPl, nil
				}
			},
			probe: func(mainPl, _ *pipeListener) *pipeListener { return mainPl },
			want:  "the push listener broke",
		},
		{
			name: "main fails while serving",
			listen: func(sc *serveConfig, _, pushPl *pipeListener) func(string) (net.Listener, error) {
				return func(addr string) (net.Listener, error) {
					if addr == sc.addr {
						return brokenListener{err: errors.New("the main listener broke")}, nil
					}
					return pushPl, nil
				}
			},
			probe: func(_, pushPl *pipeListener) *pipeListener { return pushPl },
			want:  "the main listener broke",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bound, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)
			sc := newServeTestConfig(t)
			mainPl, pushPl := newPipeListener(), newPipeListener()
			sc.listen = tc.listen(sc, mainPl, pushPl)

			served := make(chan error, 1)
			go func() { served <- serve(context.Background(), sc) }()
			var serveErr error
			select {
			case serveErr = <-served:
			case <-bound.Done():
				t.Fatal("serve did not end when a listener failed")
			}
			if serveErr == nil || !strings.Contains(serveErr.Error(), tc.want) {
				t.Fatalf("serve = %v, want a refusal naming %q", serveErr, tc.want)
			}
			if _, err := tc.probe(mainPl, pushPl).dial(bound); err == nil {
				t.Error("a listener stayed open after serve returned")
			}
		})
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

// logPerPodServeConfig is a serve config whose pieces are in temp directories
// and whose listeners are in-memory, so a call to serve returns without a socket.
func logPerPodServeConfig(t *testing.T) *serveConfig {
	t.Helper()
	sc := newServeConfig()
	sc.tokensDir = t.TempDir()
	writeFile(t, filepath.Join(sc.tokensDir, "read.dash.token"), testReadToken, 0o600)
	sc.verbsPath = filepath.Join(t.TempDir(), "verbs.json")
	writeFile(t, sc.verbsPath, cmdPolicy, 0o644)
	sc.logDir = t.TempDir()
	sc.workspaceRoot = t.TempDir()
	sc.listen = listenByAddr(sc.addr, newPipeListener(), sc.pushAddr, newPipeListener())
	return sc
}

// parseServeFlags parses args with the same flag binding runServe uses, so a
// test that names --log-per-pod drives the flag the command reads.
func parseServeFlags(t *testing.T, sc *serveConfig, args ...string) {
	t.Helper()
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	bindServe(fs, sc)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
}

// TestServeLogPerPodPath: with --log-per-pod serve writes the call log under
// <log-dir>/<hostname>, and without it directly under <log-dir>. The context is
// already done, so serve builds the call log and drains at once.
func TestServeLogPerPodPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		perPod  bool
		want    string
		notWant string
	}{
		{name: "per pod", perPod: true, want: filepath.Join("gw-0", "call.log"), notWant: "call.log"},
		{name: "shared", perPod: false, want: "call.log", notWant: filepath.Join("gw-0", "call.log")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sc := logPerPodServeConfig(t)
			var args []string
			if tc.perPod {
				args = []string{"--log-per-pod"}
			}
			parseServeFlags(t, sc, args...)
			sc.hostname = func() (string, error) { return "gw-0", nil }
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := serve(ctx, sc); err != nil {
				t.Fatalf("serve = %v", err)
			}
			if _, err := os.Stat(filepath.Join(sc.logDir, tc.want)); err != nil {
				t.Errorf("call log %s: %v", tc.want, err)
			}
			if _, err := os.Stat(filepath.Join(sc.logDir, tc.notWant)); !os.IsNotExist(err) {
				t.Errorf("call log written at %s too: stat = %v", tc.notWant, err)
			}
		})
	}
}

// writePodCallLog writes one call log line under root's directory for pod, the
// layout --log-per-pod gives each pod. The caller names the line; the stamp is
// fixed.
func writePodCallLog(t *testing.T, root, pod, caller string) {
	t.Helper()
	dir := filepath.Join(root, pod)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("make pod directory %s: %v", dir, err)
	}
	line, err := json.Marshal(bdgateway.CallEntry{
		TS:         "2026-11-18T10:00:00Z",
		Class:      "read",
		Caller:     caller,
		Verb:       "show",
		Flags:      []string{},
		Beads:      []string{},
		DurationMS: 5,
	})
	if err != nil {
		t.Fatalf("marshal call entry: %v", err)
	}
	writeFile(t, filepath.Join(dir, bdgateway.DefaultCallLogName), string(line)+"\n", 0o600)
}

// TestServeCallsPodsWiring: under --log-per-pod a live serve answers GET
// /v1/calls with every pod's calls, not only its own. Serve's own log is pod
// gw-0's directory, so the line from gw-1 reaches the answer only when serve
// passes the shared root as CallsDir. Deleting that line in serve makes this
// test fail.
func TestServeCallsPodsWiring(t *testing.T) {
	t.Parallel()
	bound, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	sc := logPerPodServeConfig(t)
	parseServeFlags(t, sc, "--log-per-pod")
	sc.hostname = func() (string, error) { return "gw-0", nil }
	writePodCallLog(t, sc.logDir, "gw-0", "from-gw-0")
	writePodCallLog(t, sc.logDir, "gw-1", "from-gw-1")
	pl, pushPl := newPipeListener(), newPipeListener()
	sc.listen = listenByAddr(sc.addr, pl, sc.pushAddr, pushPl)
	client := dialClient(t, pl)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, sc) }()
	waitFor(t, bound, "the listener accepts", func() bool {
		return acceptsNow(t, bound, pl)
	})

	calls := send(bound, client, http.MethodGet, "/v1/calls", testReadToken, nil)
	if calls.err != nil {
		t.Fatalf("GET /v1/calls: %v", calls.err)
	}
	if calls.status != http.StatusOK {
		t.Fatalf("GET /v1/calls = %d, want 200; body %s", calls.status, calls.body)
	}
	var got struct {
		Calls []bdgateway.CallEntry `json:"calls"`
	}
	if err := json.Unmarshal(calls.body, &got); err != nil {
		t.Fatalf("decode GET /v1/calls: %v; body %s", err, calls.body)
	}
	seen := make(map[string]bool)
	for _, c := range got.Calls {
		seen[c.Caller] = true
	}
	for _, want := range []string{"from-gw-0", "from-gw-1"} {
		if !seen[want] {
			t.Errorf("GET /v1/calls lacks the call from %s; callers %v", want, seen)
		}
	}

	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serve after the drain = %v, want nil", err)
		}
	case <-bound.Done():
		t.Fatal("serve did not return after the drain")
	}
}

// TestServeLogPerPodRefuses: a hostname that is empty, holds a slash or an
// upper-case letter, or cannot be read stops serve, and the error names the
// flag. No call log is written in the shared directory instead.
func TestServeLogPerPodRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		hostname func() (string, error)
	}{
		{name: "empty hostname", hostname: func() (string, error) { return "", nil }},
		{name: "slash in hostname", hostname: func() (string, error) { return "gw/0", nil }},
		{name: "upper-case hostname", hostname: func() (string, error) { return "GW-0", nil }},
		{name: "parent directory hostname", hostname: func() (string, error) { return "..", nil }},
		{name: "hostname read error", hostname: func() (string, error) { return "", errors.New("no hostname") }},
		{name: "no hostname source", hostname: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sc := logPerPodServeConfig(t)
			parseServeFlags(t, sc, "--log-per-pod")
			sc.hostname = tc.hostname
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := serve(ctx, sc)
			if err == nil || !strings.Contains(err.Error(), "--log-per-pod") {
				t.Fatalf("serve = %v, want a refusal naming --log-per-pod", err)
			}
			if _, statErr := os.Stat(filepath.Join(sc.logDir, "call.log")); !os.IsNotExist(statErr) {
				t.Errorf("a call log was written in the shared directory: stat = %v", statErr)
			}
		})
	}
}
