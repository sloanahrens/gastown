package forgejoprov

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejotest"
)

// countingPassword is the password countingRandom draws first: the alphabet's
// first passwordLength characters.
const countingPassword = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef"

// mintedSecret is a minted bot token's value, as a create-token reply carries it
// in its sha1 field.
var mintedSecret = fmt.Sprintf("%040x", 7)

// echoReply is a refusal that echoes the admin token and a minted token in JSON
// fields. A failing branch that quotes it must not quote either value.
var echoReply = `{"message":"refused","token":"` + fixtureToken + `","sha1":"` + mintedSecret + `"}`

// countingRandom is a random source that yields 0, 1, 2, ... so a test knows the
// password it draws.
type countingRandom struct{ next byte }

func (c *countingRandom) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = c.next
		c.next++
	}
	return len(p), nil
}

// bodyTransport passes every request to next and keeps each request body, so a
// test can read what a create call sent: the password is only ever in a body.
type bodyTransport struct {
	next   http.RoundTripper
	bodies []string
}

func (b *bodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		b.bodies = append(b.bodies, string(data))
		req.Body = io.NopCloser(bytes.NewReader(data))
	}
	return b.next.RoundTrip(req)
}

// staleReadTransport answers every GET 404, as a read that lags behind a create
// does, and passes the writes to next.
type staleReadTransport struct{ next http.RoundTripper }

func (s staleReadTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return s.next.RoundTrip(req)
	}
	return respond(req, http.StatusNotFound, `{"message":"user does not exist"}`)
}

// createDropsTransport passes the reads to next and drops every create without a
// reply, as a connection that breaks on the POST does.
type createDropsTransport struct{ next http.RoundTripper }

func (c createDropsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return nil, errors.New("connection reset by peer")
	}
	return c.next.RoundTrip(req)
}

// refusingTransport answers each method with the status given for it, and every
// reply with echoReply. It never reaches the fake.
type refusingTransport struct {
	getStatus  int
	postStatus int
	methods    []string
}

func (r *refusingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.methods = append(r.methods, req.Method)
	status := r.postStatus
	if req.Method == http.MethodGet {
		status = r.getStatus
	}
	return respond(req, status, echoReply)
}

// respond builds the reply to req with the status and JSON body given.
func respond(req *http.Request, status int, body string) (*http.Response, error) {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

// userProvisioner returns a Provisioner over transport with the default bot prefix
// and email domain, a counting password source, and the log lines captured in logs.
func userProvisioner(transport http.RoundTripper, dryRun bool, logs *[]string) *Provisioner {
	return &Provisioner{
		Client: &Client{
			API:    forgejotest.APIRoot + forgejotest.APIPath,
			HTTP:   &http.Client{Transport: transport},
			Token:  newSecret(fixtureToken),
			DryRun: dryRun,
			Log:    func(line string) { *logs = append(*logs, line) },
		},
		BotPrefix:      DefaultBotPrefix,
		BotEmailDomain: DefaultBotEmailDomain,
		Random:         &countingRandom{},
	}
}

// TestEnsureUserCreatesWhenMissing checks that a missing bot is created by one POST,
// and that a second run finds it and writes nothing. The password travels in the
// create body and in no log line.
func TestEnsureUserCreatesWhenMissing(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	bodies := &bodyTransport{next: f}
	var logs []string
	p := userProvisioner(bodies, false, &logs)

	if err := p.EnsureUser("polecat"); err != nil {
		t.Fatalf("EnsureUser on a missing user: unexpected error: %v", err)
	}
	writes := f.Writes()
	if len(writes) != 1 || writes[0].Method != http.MethodPost || writes[0].Path != forgejotest.APIPath+"/admin/users" || writes[0].Status != http.StatusCreated {
		t.Fatalf("writes = %+v, want one POST /admin/users answered 201", writes)
	}
	if email, ok := f.UserEmail("bot-polecat"); !ok || email != "bot-polecat@"+DefaultBotEmailDomain {
		t.Errorf("fake user email = %q, want bot-polecat with its bot email", email)
	}

	if err := p.EnsureUser("polecat"); err != nil {
		t.Fatalf("EnsureUser on an existing user: unexpected error: %v", err)
	}
	if n := len(f.Writes()); n != 1 {
		t.Errorf("two EnsureUser runs wrote %d time(s), want 1: an existing user takes no write", n)
	}
	if want := []string{"created user bot-polecat", "user bot-polecat exists"}; !slices.Equal(logs, want) {
		t.Errorf("log lines = %q, want %q", logs, want)
	}

	if len(bodies.bodies) != 1 {
		t.Fatalf("captured %d request body(ies), want the one create body", len(bodies.bodies))
	}
	for _, field := range []string{`"password":"` + countingPassword + `"`, `"must_change_password":false`, `"send_notify":false`} {
		if !strings.Contains(bodies.bodies[0], field) {
			t.Errorf("create body lacks %s", field)
		}
	}
	for _, line := range logs {
		if strings.Contains(line, countingPassword) {
			t.Errorf("log line carries the password: %q", line)
		}
	}
}

// TestEnsureUserLeavesAnExistingUser checks that a bot already on the instance takes
// no write and draws no password.
func TestEnsureUserLeavesAnExistingUser(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	f.SeedUser("bot-polecat", "bot-polecat@"+DefaultBotEmailDomain)
	var logs []string
	p := userProvisioner(f, false, &logs)
	p.Random = brokenReader{}

	if err := p.EnsureUser("polecat"); err != nil {
		t.Fatalf("EnsureUser on an existing user: unexpected error: %v", err)
	}
	if writes := f.Writes(); len(writes) != 0 {
		t.Errorf("existing user took %d write(s), want none: %+v", len(writes), writes)
	}
	if want := []string{"user bot-polecat exists"}; !slices.Equal(logs, want) {
		t.Errorf("log lines = %q, want %q", logs, want)
	}
}

// TestEnsureUserTreats422AsExisting checks that a create answered 422 is a bot that a
// concurrent run made first: the run logs it as existing and carries on.
func TestEnsureUserTreats422AsExisting(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	f.SeedUser("bot-polecat", "bot-polecat@"+DefaultBotEmailDomain)
	var logs []string
	bodies := &bodyTransport{next: staleReadTransport{next: f}}
	p := userProvisioner(bodies, false, &logs)

	if err := p.EnsureUser("polecat"); err != nil {
		t.Fatalf("EnsureUser with a 422 create: unexpected error: %v", err)
	}
	writes := f.Writes()
	if len(writes) != 1 || writes[0].Status != http.StatusUnprocessableEntity {
		t.Fatalf("writes = %+v, want one create answered 422", writes)
	}
	if want := []string{"user bot-polecat exists"}; !slices.Equal(logs, want) {
		t.Errorf("log lines = %q, want %q", logs, want)
	}
	// The 422 path draws and sends a password, so the same no-leak rule holds here.
	if len(bodies.bodies) != 1 || !strings.Contains(bodies.bodies[0], countingPassword) {
		t.Fatalf("create body does not carry the drawn password: the no-leak check below would prove nothing")
	}
	for _, line := range logs {
		if strings.Contains(line, countingPassword) {
			t.Errorf("log line carries the password: %q", line)
		}
	}
}

// TestEnsureUserFailsOnOtherStatus checks that a read or a create answered with a
// status other than the expected ones fails the run, naming the request and the
// status, and that the error quotes neither the admin token nor a minted token that
// the reply echoed.
func TestEnsureUserFailsOnOtherStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		getStatus  int
		postStatus int
		wantErr    string
		wantPost   bool
	}{
		{"read answers 500", http.StatusInternalServerError, 0, "GET /users/bot-polecat: unexpected HTTP 500", false},
		{"read answers 403", http.StatusForbidden, 0, "GET /users/bot-polecat: unexpected HTTP 403", false},
		{"create answers 500", http.StatusNotFound, http.StatusInternalServerError, "POST /admin/users: unexpected HTTP 500", true},
		{"create answers 403", http.StatusNotFound, http.StatusForbidden, "POST /admin/users: unexpected HTTP 403", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := &refusingTransport{getStatus: tc.getStatus, postStatus: tc.postStatus}
			var logs []string
			p := userProvisioner(tr, false, &logs)

			err := p.EnsureUser("polecat")
			if err == nil {
				t.Fatalf("EnsureUser answered %d: want an error", tc.getStatus)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to name %q", err.Error(), tc.wantErr)
			}
			if strings.Contains(err.Error(), fixtureToken) || strings.Contains(err.Error(), mintedSecret) {
				t.Errorf("error quotes a credential the reply echoed")
			}
			if strings.Contains(err.Error(), countingPassword) {
				t.Errorf("error carries the password drawn for the create")
			}
			wantMethods := []string{http.MethodGet}
			if tc.wantPost {
				wantMethods = append(wantMethods, http.MethodPost)
			}
			if !slices.Equal(tr.methods, wantMethods) {
				t.Errorf("requests = %q, want %q", tr.methods, wantMethods)
			}
		})
	}
}

// TestEnsureUserStopsWhenTheCreateGetsNoReply checks that a create whose request
// never gets a reply fails the run, and that no progress line claims the bot was
// made.
func TestEnsureUserStopsWhenTheCreateGetsNoReply(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	var logs []string
	p := userProvisioner(createDropsTransport{next: f}, false, &logs)

	err := p.EnsureUser("polecat")
	if err == nil || !strings.Contains(err.Error(), "POST ") || !strings.Contains(err.Error(), "request failed") {
		t.Fatalf("EnsureUser with no create reply: error = %v, want one naming the failed POST", err)
	}
	if strings.Contains(err.Error(), countingPassword) {
		t.Errorf("error carries the password drawn for the create")
	}
	if len(logs) != 0 {
		t.Errorf("log lines = %q, want none", logs)
	}
}

// TestEnsureUserDryRun checks that a dry run reports the create it would make and
// sends no write, and draws no password.
func TestEnsureUserDryRun(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	var logs []string
	p := userProvisioner(f, true, &logs)
	p.Random = brokenReader{}

	if err := p.EnsureUser("polecat"); err != nil {
		t.Fatalf("EnsureUser in a dry run: unexpected error: %v", err)
	}
	if writes := f.Writes(); len(writes) != 0 {
		t.Errorf("dry run reached the fake with %d write(s): %+v", len(writes), writes)
	}
	if _, ok := f.UserEmail("bot-polecat"); ok {
		t.Errorf("dry run created bot-polecat")
	}
	if got := len(f.CallLog()); got != 1 {
		t.Errorf("fake answered %d request(s), want only the read", got)
	}
	if want := []string{"would create user bot-polecat"}; !slices.Equal(logs, want) {
		t.Errorf("log lines = %q, want %q", logs, want)
	}
}

// TestEnsureUserStopsOnRandomFailure checks that a password source that fails stops
// the run before any create is sent.
func TestEnsureUserStopsOnRandomFailure(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	var logs []string
	p := userProvisioner(f, false, &logs)
	p.Random = brokenReader{}

	err := p.EnsureUser("polecat")
	if err == nil || !strings.Contains(err.Error(), "read random password") {
		t.Fatalf("EnsureUser with a failing source: error = %v, want one naming the read", err)
	}
	if writes := f.Writes(); len(writes) != 0 {
		t.Errorf("failed draw still sent %d write(s): %+v", len(writes), writes)
	}
}

// TestEnsureUserStopsOnTransportFailure checks that a request that never gets a reply
// fails the run rather than being read as a missing user.
func TestEnsureUserStopsOnTransportFailure(t *testing.T) {
	t.Parallel()
	var logs []string
	p := userProvisioner(failingTransport{}, false, &logs)

	err := p.EnsureUser("polecat")
	if err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Fatalf("EnsureUser with no reply: error = %v, want one naming the failed request", err)
	}
	if len(logs) != 0 {
		t.Errorf("log lines = %q, want none", logs)
	}
}

// TestRandomPassword checks that a password is 32 characters of [A-Za-z0-9] and that
// two draws from the real source differ.
func TestRandomPassword(t *testing.T) {
	t.Parallel()
	shape := regexp.MustCompile(`^[A-Za-z0-9]{32}$`)
	first, err := randomPassword(rand.Reader)
	if err != nil {
		t.Fatalf("randomPassword with crypto/rand: unexpected error: %v", err)
	}
	second, err := randomPassword(rand.Reader)
	if err != nil {
		t.Fatalf("randomPassword with crypto/rand: unexpected error: %v", err)
	}
	for _, pw := range []string{first, second} {
		if !shape.MatchString(pw) {
			t.Errorf("password is not 32 characters of [A-Za-z0-9]")
		}
	}
	if first == second {
		t.Errorf("two draws from the real source were equal")
	}
}

// TestRandomPasswordRedrawsBytesAboveTheAlphabet checks that a byte the alphabet
// cannot take evenly is drawn again, so the password is the one the next bytes give.
func TestRandomPasswordRedrawsBytesAboveTheAlphabet(t *testing.T) {
	t.Parallel()
	high := bytes.Repeat([]byte{250}, passwordLength)
	got, err := randomPassword(io.MultiReader(bytes.NewReader(high), &countingRandom{}))
	if err != nil {
		t.Fatalf("randomPassword: unexpected error: %v", err)
	}
	if got != countingPassword {
		t.Errorf("password drew the rejected bytes, want the password of the bytes after them")
	}
}
