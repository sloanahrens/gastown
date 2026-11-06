package forgejoprov

import (
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejotest"
)

// The account, token name and scope the scoped-token tests mint.
const (
	scopedUser  = "bot-viewer"
	scopedName  = "sources-token-test"
	scopedScope = "read:repository"
)

// newScopedFake returns a fake that already holds the viewer account, as a run on an
// instance a viewer run has provisioned does.
func newScopedFake() *forgejotest.Fake {
	f := forgejotest.New()
	f.ViewerExists = true
	return f
}

// scopedClient returns a client whose requests go through transport, holding the admin
// token the fake accepts.
func scopedClient(transport http.RoundTripper) *Client {
	return &Client{
		API:   forgejotest.APIRoot + forgejotest.APIPath,
		HTTP:  &http.Client{Transport: transport},
		Token: newSecret(fixtureToken),
	}
}

// echoTransport answers every request with status and a JSON error body that quotes
// value, as a server that echoes the request or the token back does.
type echoTransport struct {
	status int
	value  string
}

func (e echoTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body := forgejotest.JSONBody(map[string]string{"message": "rejected " + e.value, "sha1": e.value})
	return &http.Response{
		StatusCode: e.status,
		Status:     http.StatusText(e.status),
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

// TestScopedTokenMintsAndRevokes mints a read:repository token for the viewer account
// and checks the fake holds it under the name and scope asked for and that the value
// returned is the one it minted. Revoking it removes it.
func TestScopedTokenMintsAndRevokes(t *testing.T) {
	t.Parallel()
	f := newScopedFake()
	c := scopedClient(f)

	value, err := c.MintToken(scopedUser, scopedName, []string{scopedScope})
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if value == "" {
		t.Fatal("MintToken returned an empty value")
	}
	if got := f.Token(scopedUser, scopedName); got != value {
		t.Fatalf("MintToken returned a value the fake did not mint under the name")
	}
	scopes, ok := f.TokenScopes(scopedUser, scopedName)
	if !ok || !slices.Equal(scopes, []string{scopedScope}) {
		t.Fatalf("the fake holds scopes %v (present %v), want exactly [%s]", scopes, ok, scopedScope)
	}

	if err := c.RevokeToken(scopedUser, scopedName); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, ok := f.TokenScopes(scopedUser, scopedName); ok {
		t.Fatal("the token survived RevokeToken")
	}

	t.Run("refusals", func(t *testing.T) { scopedRefusals(t) })
	t.Run("echoed value", func(t *testing.T) { scopedNeverEchoes(t) })
}

// unreachableDelete is a transport that sends every call to the fake except a DELETE,
// which never reaches a server.
type unreachableDelete struct {
	fake *forgejotest.Fake
}

func (u unreachableDelete) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodDelete {
		return failingTransport{}.RoundTrip(req)
	}
	return u.fake.RoundTrip(req)
}

// TestScopedTokenReportsATransportFailure checks a call that never reaches the server is
// an error naming the fixed phrase, and that a mint whose reply holds no value and whose
// revoke cannot be sent says the token it made is still live.
func TestScopedTokenReportsATransportFailure(t *testing.T) {
	t.Parallel()
	if _, err := scopedClient(failingTransport{}).MintToken(scopedUser, scopedName, []string{scopedScope}); err == nil || !strings.Contains(err.Error(), "did not complete") {
		t.Fatalf("MintToken over a dead transport returned %v, want the did-not-complete error", err)
	}
	if err := scopedClient(failingTransport{}).RevokeToken(scopedUser, scopedName); err == nil || !strings.Contains(err.Error(), "did not complete") {
		t.Fatalf("RevokeToken over a dead transport returned %v, want the did-not-complete error", err)
	}

	f := newScopedFake()
	f.TokenField = "value"
	_, err := scopedClient(unreachableDelete{fake: f}).MintToken(scopedUser, scopedName, []string{scopedScope})
	if err == nil || !strings.Contains(err.Error(), "still live") {
		t.Fatalf("MintToken with no value and no revoke returned %v, want the still-live error", err)
	}
}

// scopedRefusals drives each refusal the mint and the revoke can meet: a 403, a 422 for
// a name already taken, a 500, and a 404 for a token that is not there. Each error names
// the status, is free of the reply's own text, and carries no value.
func scopedRefusals(t *testing.T) {
	cases := []struct {
		name     string
		status   string
		replyTxt string
		call     func(t *testing.T) error
	}{
		{
			name: "mint forbidden", status: "HTTP 403", replyTxt: "the token needs site-administrator",
			call: func(t *testing.T) error {
				f := newScopedFake()
				f.Adminless = true
				_, err := scopedClient(f).MintToken(scopedUser, scopedName, []string{scopedScope})
				return err
			},
		},
		{
			name: "mint name taken", status: "HTTP 422", replyTxt: "used already",
			call: func(t *testing.T) error {
				f := newScopedFake()
				c := scopedClient(f)
				if _, err := c.MintToken(scopedUser, scopedName, []string{scopedScope}); err != nil {
					t.Fatalf("first MintToken: %v", err)
				}
				_, err := c.MintToken(scopedUser, scopedName, []string{scopedScope})
				return err
			},
		},
		{
			name: "mint server error", status: "HTTP 500", replyTxt: "rejected",
			call: func(t *testing.T) error {
				_, err := scopedClient(echoTransport{status: http.StatusInternalServerError, value: "x"}).
					MintToken(scopedUser, scopedName, []string{scopedScope})
				return err
			},
		},
		{
			name: "revoke of an absent token", status: "HTTP 404", replyTxt: "does not exist",
			call: func(t *testing.T) error {
				return scopedClient(newScopedFake()).RevokeToken(scopedUser, scopedName)
			},
		},
		{
			name: "revoke server error", status: "HTTP 500", replyTxt: "rejected",
			call: func(t *testing.T) error {
				return scopedClient(echoTransport{status: http.StatusInternalServerError, value: "x"}).
					RevokeToken(scopedUser, scopedName)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.call(t)
			if err == nil {
				t.Fatal("the call returned no error, want the refusal")
			}
			if !strings.Contains(err.Error(), tc.status) {
				t.Fatalf("the error %q does not name %s", err, tc.status)
			}
			if strings.Contains(err.Error(), tc.replyTxt) {
				t.Fatalf("the error %q carries text from the reply", err)
			}
		})
	}
}

// scopedNeverEchoes mints a value holding a quote, a backslash and a non-ASCII rune,
// then has a server echo it, escaped, in every refusal body. No returned error carries
// the raw value or its JSON-escaped form. The success path returns the value unchanged,
// so the escaping is not lost in the reply.
func scopedNeverEchoes(t *testing.T) {
	forms := leakedForms(t, leakyValue)

	f := newScopedFake()
	f.NewToken = func() string { return leakyValue }
	value, err := scopedClient(f).MintToken(scopedUser, scopedName, []string{scopedScope})
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if value != leakyValue {
		t.Fatalf("MintToken returned a value that differs from the one minted")
	}

	for _, status := range []int{http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError} {
		c := scopedClient(echoTransport{status: status, value: leakyValue})
		_, mintErr := c.MintToken(scopedUser, scopedName, []string{scopedScope})
		errs := map[string]error{"mint": mintErr, "revoke": c.RevokeToken(scopedUser, scopedName)}
		for call, err := range errs {
			if err == nil {
				t.Fatalf("%d on %s: no error, want the refusal", status, call)
			}
			for _, leak := range forms {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("%d on %s: the error carries the value or its escaped form", status, call)
				}
			}
		}
	}
}

// TestScopedTokenRevokesAReplyWithNoValue has the server answer a mint with a reply that
// holds no value under either name. No value can be kept, so the token the mint made is
// revoked, the call fails, and the error holds no value.
func TestScopedTokenRevokesAReplyWithNoValue(t *testing.T) {
	t.Parallel()
	f := newScopedFake()
	f.NewToken = func() string { return leakyValue }
	f.TokenField = "value"
	c := scopedClient(f)

	_, err := c.MintToken(scopedUser, scopedName, []string{scopedScope})
	if err == nil {
		t.Fatal("MintToken returned no error for a reply with no value")
	}
	if _, ok := f.TokenScopes(scopedUser, scopedName); ok {
		t.Fatal("the token whose value was not kept survived the mint")
	}
	for _, leak := range leakedForms(t, leakyValue) {
		if strings.Contains(err.Error(), leak) {
			t.Fatal("the error carries the value")
		}
	}
}
