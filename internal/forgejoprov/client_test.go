package forgejoprov

import (
	"errors"
	"io"
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

// brokenBodyTransport answers every request with a 200 whose body fails on read, so
// the client's reply read has an error to report.
type brokenBodyTransport struct{}

func (brokenBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(brokenReader{}),
		Request:    req,
	}, nil
}

// brokenReader is a reply body whose read fails part way through.
type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) {
	return 0, errors.New("connection reset by peer")
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
	// Do logs nothing of its own, so any line here would be one it should not
	// have written; a line that carries the token is the leak this test hunts.
	if len(logs) != 0 {
		t.Errorf("Do wrote %d log line(s); it should log nothing", len(logs))
	}
	for _, line := range logs {
		if strings.Contains(line, fixtureToken) {
			t.Errorf("log line carries the token: %s", strings.ReplaceAll(line, fixtureToken, redacted))
		}
	}
}

// TestClientDefaultHTTPIsBounded checks that a client with no HTTP set sends through
// a client that times out, and that a caller's own client is used as given.
func TestClientDefaultHTTPIsBounded(t *testing.T) {
	t.Parallel()
	if got := (&Client{}).httpClient().Timeout; got <= 0 {
		t.Errorf("default client Timeout = %v, want a bound so a stalled server cannot hold the run", got)
	}
	own := &http.Client{}
	if got := (&Client{HTTP: own}).httpClient(); got != own {
		t.Errorf("caller's client was replaced")
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

// TestClientRefusesAnUnencodableBody checks that a body JSON cannot encode is refused
// before any request is sent.
func TestClientRefusesAnUnencodableBody(t *testing.T) {
	t.Parallel()
	rec := &recordingTransport{next: newFakeForgejo()}
	c := &Client{
		API:   fakeAPIRoot + fakeAPIPath,
		HTTP:  &http.Client{Transport: rec},
		Token: newSecret(fixtureToken),
	}

	status, body, err := c.Do(http.MethodPost, "/admin/users", make(chan int))
	if err == nil {
		t.Fatal("an unencodable body returned no error")
	}
	if !strings.Contains(err.Error(), "POST "+fakeAPIRoot+fakeAPIPath+"/admin/users: encode body") {
		t.Errorf("error = %q, want the method, URL and encode-body rule", err.Error())
	}
	if status != 0 || body != nil {
		t.Errorf("Do returned status %d, body %q with the error; want 0 and no body", status, body)
	}
	if len(rec.urls) != 0 {
		t.Errorf("an unencodable body still sent %d request(s)", len(rec.urls))
	}
}

// TestClientRefusesABadURL checks that a request the client cannot build is
// reported, with no token in the text, and that nothing is sent.
func TestClientRefusesABadURL(t *testing.T) {
	t.Parallel()
	rec := &recordingTransport{next: newFakeForgejo()}
	c := &Client{
		API:   "http://[::1",
		HTTP:  &http.Client{Transport: rec},
		Token: newSecret(fixtureToken),
	}
	_, _, err := c.Do(http.MethodGet, "/users/bot-polecat", nil)
	if err == nil {
		t.Fatal("a malformed API root returned no error")
	}
	if !strings.Contains(err.Error(), "GET http://[::1/users/bot-polecat:") {
		t.Errorf("error = %q, want the method and URL", err.Error())
	}
	if strings.Contains(err.Error(), fixtureToken) {
		t.Errorf("request-build error carries the token")
	}
	if len(rec.urls) != 0 {
		t.Errorf("a request that could not be built still reached the transport")
	}
}

// TestClientReportsAReplyReadError checks that a reply body that fails on read is an
// error, not an empty reply that looks like success.
func TestClientReportsAReplyReadError(t *testing.T) {
	t.Parallel()
	c := &Client{
		API:   fakeAPIRoot + fakeAPIPath,
		HTTP:  &http.Client{Transport: brokenBodyTransport{}},
		Token: newSecret(fixtureToken),
	}
	status, body, err := c.Do(http.MethodGet, "/users/bot-polecat", nil)
	if err == nil {
		t.Fatal("a reply that fails on read returned no error")
	}
	if !strings.Contains(err.Error(), "GET "+fakeAPIRoot+fakeAPIPath+"/users/bot-polecat: read reply") {
		t.Errorf("error = %q, want the method, URL and read-reply rule", err.Error())
	}
	if status != 0 || body != nil {
		t.Errorf("Do returned status %d, body %q with the error; want 0 and no body", status, body)
	}
	if strings.Contains(err.Error(), fixtureToken) {
		t.Errorf("read error carries the token")
	}
}

// redactToken hides the fixture token in a failure message.
func redactToken(s string) string {
	return strings.ReplaceAll(s, fixtureToken, redacted)
}

// TestBodyTail covers the 300-byte cut, the newline to space rule, the JSON
// credential mask and the empty body, on the formatter that knows no token.
func TestBodyTail(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 400)
	cases := []struct {
		name string
		resp []byte
		want string
	}{
		{name: "empty body", resp: nil, want: ""},
		{name: "short body", resp: []byte("nope"), want: " - nope"},
		{name: "newlines become spaces", resp: []byte("a\nb\nc"), want: " - a b c"},
		{name: "cut at 300 bytes", resp: []byte(long), want: " - " + strings.Repeat("a", 300)},
		{name: "exactly 300 bytes kept", resp: []byte(strings.Repeat("b", 300)), want: " - " + strings.Repeat("b", 300)},
		{name: "cut backs off a split UTF-8 sequence", resp: []byte(strings.Repeat("a", 299) + "é" + "b"), want: " - " + strings.Repeat("a", 299)},
		{name: "minted token is masked", resp: []byte(`{"name":"bot","sha1":"` + fixtureToken + `"}`), want: ` - {"name":"bot","sha1":"[redacted]"}`},
		{name: "password is masked", resp: []byte(`{"password": "hunter2"}`), want: ` - {"password": "[redacted]"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := bodyTail(tc.resp); got != tc.want {
				t.Errorf("bodyTail = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUnexpectedFormat pins the exact text of an unexpected-status error.
func TestUnexpectedFormat(t *testing.T) {
	t.Parallel()
	c := &Client{Token: newSecret(fixtureToken)}
	err := c.Unexpected("GET", "/users/bot-polecat", 500, []byte("boom\nline"))
	want := "GET /users/bot-polecat: unexpected HTTP 500 - boom line"
	if err.Error() != want {
		t.Errorf("Unexpected = %q, want %q", err.Error(), want)
	}
	bare := c.Unexpected("DELETE", "/x", 404, nil)
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

// TestClientBodyTailScrubsTheEcho feeds the client replies that echo its own
// token in each shape a server or proxy might, and checks none reaches the tail.
func TestClientBodyTailScrubsTheEcho(t *testing.T) {
	t.Parallel()
	c := &Client{Token: newSecret(fixtureToken)}
	cases := []struct {
		name string
		resp string
	}{
		{name: "plain text", resp: "invalid credential " + fixtureToken + " for user"},
		{name: "authorization line", resp: "request headers:\nAuthorization: token " + fixtureToken + "\nAccept: application/json"},
		{name: "authorization line, other case", resp: "authorization:token " + fixtureToken},
		{name: "bearer text", resp: "rejected Bearer " + fixtureToken + " as expired"},
		{name: "bearer text, other case", resp: "rejected BEARER " + fixtureToken},
		{name: "twice", resp: fixtureToken + " and again " + fixtureToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tail := c.BodyTail([]byte(tc.resp))
			if strings.Contains(tail, fixtureToken) {
				t.Errorf("BodyTail carries the token: %q", redactToken(tail))
			}
			if !strings.Contains(tail, redacted) {
				t.Errorf("BodyTail = %q, want the masked marker %q", tail, redacted)
			}
			err := c.Unexpected("GET", "/x", 500, []byte(tc.resp))
			if strings.Contains(err.Error(), fixtureToken) {
				t.Errorf("Unexpected carries the token: %q", redactToken(err.Error()))
			}
		})
	}
}

// TestClientBodyTailScrubsBeforeTheCut puts the token across the 300-byte cut:
// scrubbing the cut text instead of the whole reply would leave its first part
// in the tail.
func TestClientBodyTailScrubsBeforeTheCut(t *testing.T) {
	t.Parallel()
	c := &Client{Token: newSecret(fixtureToken)}
	resp := strings.Repeat("x", 295) + fixtureToken + strings.Repeat("y", 50)
	tail := c.BodyTail([]byte(resp))
	for n := 4; n <= len(fixtureToken); n++ {
		if strings.Contains(tail, fixtureToken[:n]) {
			t.Fatalf("BodyTail keeps the first %d characters of the token across the cut: %q", n, tail[len(tail)-20:])
		}
	}
	if got := len(tail) - len(" - "); got > bodyTailLimit {
		t.Errorf("tail is %d bytes, want at most %d", got, bodyTailLimit)
	}
}

// TestClientBodyTailEmptyAndAnonymous covers the empty reply and a client that
// has no token to hide.
func TestClientBodyTailEmptyAndAnonymous(t *testing.T) {
	t.Parallel()
	if got := (&Client{Token: newSecret(fixtureToken)}).BodyTail(nil); got != "" {
		t.Errorf("BodyTail(nil) = %q, want empty", got)
	}
	anon := &Client{}
	if got, want := anon.BodyTail([]byte("Authorization: token x")), " - Authorization: token x"; got != want {
		t.Errorf("a client with no token changed the reply: %q, want %q", got, want)
	}
}
