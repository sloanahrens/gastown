package forgejoprov

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejotest"
)

// revokeBot and the tokens the revoke tests give it.
const (
	revokeBot   = "bot-registry"
	revokeName  = "town"
	revokeOther = "gt-provision"
)

// revokeProvisioner returns a Provisioner over transport with the run's log lines
// captured in logs.
func revokeProvisioner(transport http.RoundTripper, logs *[]string) *Provisioner {
	return &Provisioner{Client: &Client{
		API:   forgejotest.APIRoot + forgejotest.APIPath,
		HTTP:  &http.Client{Transport: transport},
		Token: newSecret(fixtureToken),
		Log:   func(line string) { *logs = append(*logs, line) },
	}}
}

// seedRevokeBot stores the registry bot holding tokens, then forgets the seeding
// calls so a test counts only the calls it makes next.
func seedRevokeBot(t *testing.T, f *forgejotest.Fake, tokens ...string) {
	t.Helper()
	f.SeedUser(revokeBot, "registry@bots.invalid")
	for _, name := range tokens {
		status, body := f.Call(t, http.MethodPost, "/admin/users/"+revokeBot+"/tokens",
			forgejotest.JSONBody(map[string]any{"name": name, "scopes": []string{"write:repository"}}))
		if status != http.StatusCreated {
			t.Fatalf("mint %s: HTTP %d %s", name, status, body)
		}
	}
	f.ClearCalls()
}

// deleteCalls returns the DELETE calls the fake answered.
func deleteCalls(f *forgejotest.Fake) []forgejotest.LoggedCall {
	var out []forgejotest.LoggedCall
	for _, call := range f.CallLog() {
		if call.Method == http.MethodDelete {
			out = append(out, call)
		}
	}
	return out
}

// TestRevokeNamedToken revokes one of the registry bot's two tokens: the run sends
// exactly one DELETE for that name and logs the bot and the token name, never a value.
func TestRevokeNamedToken(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	seedRevokeBot(t, f, revokeName, revokeOther)
	values := f.MintedValues()
	var logs []string
	p := revokeProvisioner(f, &logs)

	if err := p.revokeNamedToken(revokeBot, revokeName); err != nil {
		t.Fatalf("revokeNamedToken: %v", err)
	}
	deletes := deleteCalls(f)
	if len(deletes) != 1 {
		t.Fatalf("the revoke sent %d DELETE(s), want exactly 1: %v", len(deletes), f.CallLog())
	}
	if want := forgejotest.APIPath + "/admin/users/" + revokeBot + "/tokens/" + revokeName; deletes[0].Path != want {
		t.Fatalf("revoked %s, want %s", deletes[0].Path, want)
	}
	if _, ok := f.TokenScopes(revokeBot, revokeName); ok {
		t.Fatalf("the %s token survived the revoke", revokeName)
	}
	if _, ok := f.TokenScopes(revokeBot, revokeOther); !ok {
		t.Fatalf("revoking %s removed %s", revokeName, revokeOther)
	}
	named := false
	for _, line := range logs {
		if strings.Contains(line, "revoked") && strings.Contains(line, revokeName) && strings.Contains(line, revokeBot) {
			named = true
		}
	}
	if !named {
		t.Fatalf("logs %q do not name the bot and the revoked token", logs)
	}
	joined := strings.Join(logs, "\n")
	for _, value := range values {
		if strings.Contains(joined, value) {
			t.Fatal("the logs carry a token value")
		}
	}
}

// TestRevokeNamedTokenRefuses drives every refusal: the bot's only token, a name it
// does not hold, an unreadable list and a failed DELETE. Each returns an error and
// none sends a DELETE or logs a success line.
func TestRevokeNamedTokenRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		tokens         []string
		revoke         string
		transport      func(*forgejotest.Fake) http.RoundTripper
		wantText       string
		wantErrNoToken bool
	}{
		{
			name:     "the bot's only token",
			tokens:   []string{revokeName},
			revoke:   revokeName,
			wantText: "leave the bot with no token",
		},
		{
			name:           "a name the bot does not hold",
			tokens:         []string{revokeName, revokeOther},
			revoke:         "absent",
			wantText:       "absent",
			wantErrNoToken: true,
		},
		{
			name:      "an unreadable token list",
			tokens:    []string{revokeName, revokeOther},
			revoke:    revokeName,
			transport: func(*forgejotest.Fake) http.RoundTripper { return failingTransport{} },
			wantText:  "request failed",
		},
		{
			name:      "a failed DELETE",
			tokens:    []string{revokeName, revokeOther},
			revoke:    revokeName,
			transport: func(f *forgejotest.Fake) http.RoundTripper { return deleteFailsTransport{next: f} },
			wantText:  "HTTP 500",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := forgejotest.New()
			seedRevokeBot(t, f, tc.tokens...)
			transport := http.RoundTripper(f)
			if tc.transport != nil {
				transport = tc.transport(f)
			}
			var logs []string
			p := revokeProvisioner(transport, &logs)

			err := p.revokeNamedToken(revokeBot, tc.revoke)
			if err == nil {
				t.Fatal("revokeNamedToken succeeded, want a refusal")
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("error %q lacks %q", err, tc.wantText)
			}
			if tc.wantErrNoToken && !errors.Is(err, ErrNoToken) {
				t.Fatalf("error %q does not wrap ErrNoToken", err)
			}
			if deletes := deleteCalls(f); len(deletes) != 0 {
				t.Fatalf("a refused revoke sent %d DELETE(s): %v", len(deletes), deletes)
			}
			for _, line := range logs {
				if strings.Contains(line, "revoked") {
					t.Fatalf("a refused revoke logged a success line: %q", line)
				}
			}
		})
	}
}

// deleteFailsTransport answers a DELETE with a 500 and passes every other request
// to next, so a revoke whose list reads fails at the delete.
type deleteFailsTransport struct{ next http.RoundTripper }

func (d deleteFailsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodDelete {
		return respond(req, http.StatusInternalServerError, `{"message":"refused"}`)
	}
	return d.next.RoundTrip(req)
}
