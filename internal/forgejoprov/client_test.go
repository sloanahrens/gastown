package forgejoprov

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// recordingTransport passes every request to the fake and keeps the
// Authorization value and URL each one carried, so a test can read what left
// the client on the wire.
type recordingTransport struct {
	next  http.RoundTripper
	auths []string
	urls  []string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.auths = append(r.auths, req.Header.Get("Authorization"))
	r.urls = append(r.urls, req.URL.String())
	return r.next.RoundTrip(req)
}

// failingTransport is a transport that never reaches a server.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial tcp: connection refused")
}

// clientFor returns a client over the fake, with the admin token and the log
// lines it is given captured.
func clientFor(f *fakeForgejo, dryRun bool, logs *[]string) *Client {
	return &Client{
		API:    fakeAPIRoot + fakeAPIPath,
		HTTP:   &http.Client{Transport: f},
		Token:  newSecret(fixtureToken),
		DryRun: dryRun,
		Log:    func(line string) { *logs = append(*logs, line) },
	}
}

// TestClientDryRunSendsNoWrite runs every write method through a dry-run client
// against a fake holding a user, and checks the fake sees none of them while a GET
// still reaches it.
func TestClientDryRunSendsNoWrite(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	f.call(t, http.MethodPost, "/admin/users", jsonBody(userMap("bot-polecat")))
	f.reset()
	var logs []string
	c := clientFor(f, true, &logs)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		status, body, err := c.Do(method, "/users/bot-polecat", map[string]any{"login": "bot-polecat"})
		if err != nil {
			t.Fatalf("dry-run %s: unexpected error: %v", method, err)
		}
		if status != 0 || len(body) != 0 {
			t.Errorf("dry-run %s = status %d, body %q; want status 0 and no body", method, status, body)
		}
	}
	if writes := f.writes(); len(writes) != 0 {
		t.Errorf("dry run reached the fake with %d write(s): %+v", len(writes), writes)
	}

	status, _, err := c.Do(http.MethodGet, "/users/bot-polecat", nil)
	if err != nil {
		t.Fatalf("dry-run GET: unexpected error: %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("dry-run GET status = %d, want %d from the fake", status, http.StatusNotFound)
	}
	if got := len(f.callLog()); got != 1 {
		t.Errorf("fake answered %d request(s), want only the one GET", got)
	}
}

// TestClientSendsTheTokenOnlyInTheHeader checks that every request carries the
// token in its Authorization header, that no URL carries it, and that the
// reply and the log lines do not either.
func TestClientSendsTheTokenOnlyInTheHeader(t *testing.T) {
	t.Parallel()
	f := newFakeForgejo()
	rec := &recordingTransport{next: f}
	var logs []string
	c := clientFor(f, false, &logs)
	c.HTTP = &http.Client{Transport: rec}

	if _, _, err := c.Do(http.MethodPost, "/admin/users", map[string]any{"username": "bot-polecat"}); err != nil {
		t.Fatalf("POST: unexpected error: %v", err)
	}
	if _, _, err := c.Do(http.MethodGet, "/users/bot-polecat", nil); err != nil {
		t.Fatalf("GET: unexpected error: %v", err)
	}
	if len(rec.auths) != 2 {
		t.Fatalf("recorded %d request(s), want 2", len(rec.auths))
	}
	for i, auth := range rec.auths {
		if auth != "token "+fixtureToken {
			t.Errorf("request %d carried Authorization %q, want the token header", i, redactToken(auth))
		}
	}
	for i, url := range rec.urls {
		if strings.Contains(url, fixtureToken) {
			t.Errorf("request %d URL carries the token: %s", i, strings.ReplaceAll(url, fixtureToken, redacted))
		}
	}
	if n := len(f.violations()); n != 0 {
		t.Errorf("fake refused %d request(s) that should have carried the token", n)
	}
	for _, line := range logs {
		if strings.Contains(line, fixtureToken) {
			t.Errorf("log line carries the token: %s", strings.ReplaceAll(line, fixtureToken, redacted))
		}
	}
}

// TestClientTransportErrorHasNoToken checks that a transport failure is reported as
// METHOD URL: request failed, with no token in the text.
func TestClientTransportErrorHasNoToken(t *testing.T) {
	t.Parallel()
	var logs []string
	c := &Client{
		API:   fakeAPIRoot + fakeAPIPath,
		HTTP:  &http.Client{Transport: failingTransport{}},
		Token: newSecret(fixtureToken),
		Log:   func(line string) { logs = append(logs, line) },
	}
	_, _, err := c.Do(http.MethodGet, "/users/bot-polecat", nil)
	if err == nil {
		t.Fatal("transport failure returned no error")
	}
	want := "GET " + fakeAPIRoot + fakeAPIPath + "/users/bot-polecat: request failed"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	if strings.Contains(err.Error(), fixtureToken) {
		t.Errorf("transport error carries the token")
	}
}

// redactToken hides the fixture token in a failure message.
func redactToken(s string) string {
	return strings.ReplaceAll(s, fixtureToken, redacted)
}

// TestBodyTail covers the 300-byte cut, the newline to space rule and the empty body.
func TestBodyTail(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 400)
	cases := []struct {
		name string
		resp []byte
		want string
	}{
		{name: "empty body", resp: nil, want: ""},
		{name: "short body", resp: []byte("nope"), want: " — nope"},
		{name: "newlines become spaces", resp: []byte("a\nb\nc"), want: " — a b c"},
		{name: "cut at 300 bytes", resp: []byte(long), want: " — " + strings.Repeat("a", 300)},
		{name: "exactly 300 bytes kept", resp: []byte(strings.Repeat("b", 300)), want: " — " + strings.Repeat("b", 300)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := BodyTail(tc.resp); got != tc.want {
				t.Errorf("BodyTail = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUnexpectedFormat pins the exact text of an unexpected-status error.
func TestUnexpectedFormat(t *testing.T) {
	t.Parallel()
	err := Unexpected("GET", "/users/bot-polecat", 500, []byte("boom\nline"))
	want := "GET /users/bot-polecat: unexpected HTTP 500 — boom line"
	if err.Error() != want {
		t.Errorf("Unexpected = %q, want %q", err.Error(), want)
	}
	bare := Unexpected("DELETE", "/x", 404, nil)
	if want := "DELETE /x: unexpected HTTP 404"; bare.Error() != want {
		t.Errorf("Unexpected with no body = %q, want %q", bare.Error(), want)
	}
}

// TestFindObject covers a match, a miss, a non-array and malformed JSON.
func TestFindObject(t *testing.T) {
	t.Parallel()
	list := []byte(`[{"username":"alice","id":1},{"username":"bob","id":2}]`)
	cases := []struct {
		name  string
		list  []byte
		key   string
		value string
		want  bool
		id    any
	}{
		{name: "finds by key and value", list: list, key: "username", value: "bob", want: true, id: float64(2)},
		{name: "no match", list: list, key: "username", value: "carol", want: false},
		{name: "unknown key", list: list, key: "login", value: "bob", want: false},
		{name: "not an array", list: []byte(`{"username":"bob"}`), key: "username", value: "bob", want: false},
		{name: "malformed JSON", list: []byte(`[{"username":`), key: "username", value: "bob", want: false},
		{name: "empty input", list: nil, key: "username", value: "bob", want: false},
		{name: "skips non-object items", list: []byte(`[1,null,{"username":"bob","id":2}]`), key: "username", value: "bob", want: true, id: float64(2)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			obj, ok := FindObject(tc.list, tc.key, tc.value)
			if ok != tc.want {
				t.Fatalf("FindObject found = %v, want %v", ok, tc.want)
			}
			if tc.want && obj["id"] != tc.id {
				t.Errorf("FindObject id = %v, want %v", obj["id"], tc.id)
			}
		})
	}
}
