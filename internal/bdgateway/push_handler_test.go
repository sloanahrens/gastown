package bdgateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pushBody is a valid push document: the files every push-handler test sends.
func pushBody(t *testing.T) string {
	t.Helper()
	files := testWorkspaceFiles("push-handler")
	doc := WorkspaceDocument{ManifestSHA: WorkspaceManifest(files), Files: files}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	return string(body)
}

// TestPushHandlerServesWhileUnready: a pod whose workspace volume is fresh fails
// Ready, and the push handler still takes the repair push and stores the tree.
func TestPushHandlerServesWhileUnready(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := OpenWorkspaceStore(root)
	if err != nil {
		t.Fatalf("OpenWorkspaceStore: %v", err)
	}
	g := newTestGateway(t, store, newTestCallLog(t), &stubRunner{exit: 0})
	if err := g.Ready(); err == nil {
		t.Fatal("the gateway reports ready with no workspace loaded; the test needs it unready")
	}

	rec := httptest.NewRecorder()
	NewPushHandler(g).ServeHTTP(rec, pushRequestFor(http.MethodPut, workspacePath, testDaemonToken, pushBody(t)))

	if rec.Code != http.StatusOK {
		t.Fatalf("push while unready = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if !store.Loaded() {
		t.Error("the store did not load the pushed workspace")
	}
	if err := g.Ready(); err != nil {
		t.Errorf("Ready after the push = %v, want nil", err)
	}
	if got := rec.Header().Get(WorkspaceHashHeader); got != store.Manifest() {
		t.Errorf("hash header = %q, want the stored manifest %q", got, store.Manifest())
	}
	for name, want := range testWorkspaceFiles("push-handler") {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Errorf("reading the stored %s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s on disk = %q, want %q", name, got, want)
		}
	}
}

// TestPushHandlerAuth: the push handler makes the same token and class decision
// ServeHTTP makes, for every class on both routes.
func TestPushHandlerAuth(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		method string
		path   string
		token  string
		want   int
	}{
		{name: "push as read", method: http.MethodPut, path: workspacePath, token: testReadToken, want: http.StatusForbidden},
		{name: "push as daemon", method: http.MethodPut, path: workspacePath, token: testDaemonToken, want: http.StatusOK},
		{name: "push as operator", method: http.MethodPut, path: workspacePath, token: testOperatorToken, want: http.StatusOK},
		{name: "push with no token", method: http.MethodPut, path: workspacePath, want: http.StatusUnauthorized},
		{name: "push with an unknown token", method: http.MethodPut, path: workspacePath, token: "no-such-token", want: http.StatusUnauthorized},
		{name: "stats as read", method: http.MethodGet, path: statsPath, token: testReadToken, want: http.StatusOK},
		{name: "stats as daemon", method: http.MethodGet, path: statsPath, token: testDaemonToken, want: http.StatusOK},
		{name: "stats as operator", method: http.MethodGet, path: statsPath, token: testOperatorToken, want: http.StatusOK},
		{name: "stats with no token", method: http.MethodGet, path: statsPath, want: http.StatusUnauthorized},
		{name: "stats with an unknown token", method: http.MethodGet, path: statsPath, token: "no-such-token", want: http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := pushHandlerResponse(t, tc.method, tc.path, tc.token, pushBody(t))
			if rec.Code != tc.want {
				t.Fatalf("push handler status = %d, want %d", rec.Code, tc.want)
			}
			if got := rec.Header().Get(WorkspaceHashHeader); got == "" {
				t.Error("the response carries no workspace hash")
			}
			if want := gatewayStatus(t, tc.method, tc.path, tc.token, pushBody(t)); rec.Code != want {
				t.Fatalf("push handler status = %d, but ServeHTTP answers %d", rec.Code, want)
			}
		})
	}
}

// pushHandlerResponse runs one request through a push handler over its own store.
func pushHandlerResponse(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	g := newTestGateway(t, mustStore(t), newTestCallLog(t), &stubRunner{exit: 0})
	rec := httptest.NewRecorder()
	NewPushHandler(g).ServeHTTP(rec, pushRequestFor(method, path, token, body))
	return rec
}

// gatewayStatus runs the same request through ServeHTTP over its own store.
func gatewayStatus(t *testing.T, method, path, token, body string) int {
	t.Helper()
	g := newTestGateway(t, mustStore(t), newTestCallLog(t), &stubRunner{exit: 0})
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, pushRequestFor(method, path, token, body))
	return rec.Code
}

// pushRequestFor builds a request for either handler.
func pushRequestFor(method, path, token, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Gt-Caller", "daemon.workspace-pusher")
	return req
}

// TestPushHandlerRoutes: every route the push handler does not serve answers 404
// and runs no bd call, even with a valid write token.
func TestPushHandlerRoutes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		method string
		path   string
		token  string
		body   string
	}{
		{name: "exec", method: http.MethodPost, path: execPath, token: testDaemonToken, body: `{"argv":["show"]}` + "\n"},
		{name: "readyz", method: http.MethodGet, path: readyPath, token: testReadToken},
		{name: "healthz", method: http.MethodGet, path: healthPath},
		{name: "an unknown path", method: http.MethodGet, path: "/v1/nope", token: testReadToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := &stubRunner{exit: 0}
			g := newTestGateway(t, mustStore(t), newTestCallLog(t), runner)
			rec := httptest.NewRecorder()
			NewPushHandler(g).ServeHTTP(rec, pushRequestFor(tc.method, tc.path, tc.token, tc.body))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body.String())
			}
			var payload ErrorPayload
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode refusal: %v; body %s", err, rec.Body.String())
			}
			if payload.Code != codeNotFound || payload.Message != "no such endpoint" {
				t.Errorf("refusal = %+v, want code %q and message %q", payload, codeNotFound, "no such endpoint")
			}
			if runner.ran {
				t.Errorf("the push handler ran a bd call for %s", tc.path)
			}
			if got := rec.Header().Get(WorkspaceHashHeader); got == "" {
				t.Error("the refusal carries no workspace hash")
			}
		})
	}
}

// TestPushHandlerWithoutGateway: a push handler built with no gateway refuses
// every request with a 500 rather than panicking.
func TestPushHandlerWithoutGateway(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	NewPushHandler(nil).ServeHTTP(rec, pushRequestFor(http.MethodGet, statsPath, testReadToken, ""))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	var payload ErrorPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || payload.Code != codeInternal {
		t.Errorf("body = %q (%v), want code %q", rec.Body.String(), err, codeInternal)
	}
}
