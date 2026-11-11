package forgejotest

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// roundTrip sends one request to route (the path below the API root) through the fake and
// returns the response and its body.
func roundTrip(t *testing.T, f *RepoFake, method, route, auth, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, f.API()+route, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := f.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip %s %s: %v", method, route, err)
	}
	defer func() { _ = resp.Body.Close() }()
	reply, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	return resp, string(reply)
}

// TestRepoFakeAnswersTheThreeRoutes: each route answers the healthy status and body, and a
// route the run should not call is a 404.
func TestRepoFakeAnswersTheThreeRoutes(t *testing.T) {
	t.Parallel()
	f := NewRepoFake("sloan", "notes")
	cases := []struct {
		name       string
		method     string
		route      string
		wantStatus int
		wantBody   string
	}{
		{"repository read", http.MethodGet, "/repos/sloan/notes", http.StatusNotFound, ""},
		{"admin create", http.MethodPost, "/admin/users/sloan/repos", http.StatusCreated, `{"name":"notes"}`},
		{"contents commit", http.MethodPost, "/repos/sloan/notes/contents", http.StatusCreated, `{"commit":{"sha":"abc123"}}`},
		{"unknown route", http.MethodGet, "/version", http.StatusNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := roundTrip(t, f, tc.method, tc.route, "", "")
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if body != tc.wantBody {
				t.Fatalf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}
}

// TestRepoFakeOverridesEachRoute: a status and the seed reply a test sets are the ones the
// route answers.
func TestRepoFakeOverridesEachRoute(t *testing.T) {
	t.Parallel()
	f := NewRepoFake("sloan", "notes")
	f.RepoStatus = http.StatusOK
	f.CreateStatus = http.StatusForbidden
	f.SeedStatus = http.StatusUnprocessableEntity
	f.SeedReply = `{"message":"refused"}`

	if resp, _ := roundTrip(t, f, http.MethodGet, "/repos/sloan/notes", "", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("repository read = %d, want the override %d", resp.StatusCode, http.StatusOK)
	}
	if resp, _ := roundTrip(t, f, http.MethodPost, "/admin/users/sloan/repos", "", ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("admin create = %d, want the override %d", resp.StatusCode, http.StatusForbidden)
	}
	resp, body := roundTrip(t, f, http.MethodPost, "/repos/sloan/notes/contents", "", "")
	if resp.StatusCode != http.StatusUnprocessableEntity || body != f.SeedReply {
		t.Errorf("contents commit = %d %q, want the overrides %d %q", resp.StatusCode, body, http.StatusUnprocessableEntity, f.SeedReply)
	}
}

// TestRepoFakeTransportFailures: Down fails every request, WritesDown fails only a write,
// and a read still answers while WritesDown is set.
func TestRepoFakeTransportFailures(t *testing.T) {
	t.Parallel()
	down := NewRepoFake("sloan", "notes")
	down.Down = true
	req, err := http.NewRequest(http.MethodGet, down.API()+"/repos/sloan/notes", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if _, err := down.RoundTrip(req); err == nil {
		t.Error("a request to a Down fake answered; want a transport error")
	}

	writesDown := NewRepoFake("sloan", "notes")
	writesDown.WritesDown = true
	if resp, _ := roundTrip(t, writesDown, http.MethodGet, "/repos/sloan/notes", "", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("read of a WritesDown fake = %d, want it answered normally", resp.StatusCode)
	}
	write, err := http.NewRequest(http.MethodPost, writesDown.API()+"/admin/users/sloan/repos", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if _, err := writesDown.RoundTrip(write); err == nil {
		t.Error("a write to a WritesDown fake answered; want a transport error")
	}
}

// TestRepoFakeRecordsMethodPathAuthAndBody: the call a run makes is recorded whole, and
// Writes lists only the non-GET calls.
func TestRepoFakeRecordsMethodPathAuthAndBody(t *testing.T) {
	t.Parallel()
	f := NewRepoFake("sloan", "notes")
	roundTrip(t, f, http.MethodGet, "/repos/sloan/notes", "token read-secret", "")
	roundTrip(t, f, http.MethodPost, "/admin/users/sloan/repos", "token write-secret", `{"name":"notes"}`)

	reqs := f.Requests()
	if len(reqs) != 2 {
		t.Fatalf("recorded %d requests, want 2", len(reqs))
	}
	got := reqs[1]
	wantPath := APIPath + "/admin/users/sloan/repos"
	if got.Method != http.MethodPost || got.Path != wantPath || got.Auth != "token write-secret" || string(got.Body) != `{"name":"notes"}` {
		t.Errorf("recorded = %+v, want the POST with its path, auth and body", got)
	}
	if w := f.Writes(); len(w) != 1 || w[0].Method != http.MethodPost {
		t.Errorf("Writes = %+v, want the one POST", w)
	}
}
