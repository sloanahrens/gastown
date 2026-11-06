package forgejoprov

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejotest"
)

// TestListRepoSecretNamesReturnsNamesOnly stores a secret whose value holds a quote, a
// backslash and non-ASCII runes, then lists the repository's secrets. The list carries the
// name and neither the value nor its JSON-escaped form.
func TestListRepoSecretNamesReturnsNamesOnly(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	f.AddRepo(secretRepoPath)
	f.ClearCalls()
	c := clientFor(f, false, new([]string))
	if err := c.SetRepoSecret(t.Context(), secretOwner, secretRepoName, secretName, leakyValue); err != nil {
		t.Fatalf("store the secret: %v", err)
	}

	names, err := c.ListRepoSecretNames(secretOwner, secretRepoName)
	if err != nil {
		t.Fatalf("ListRepoSecretNames: %v", err)
	}
	if !slices.Equal(names, []string{secretName}) {
		t.Fatalf("names = %q, want [%s]", names, secretName)
	}
	escaped, err := json.Marshal(leakyValue)
	if err != nil {
		t.Fatalf("encode the fixture value: %v", err)
	}
	for _, form := range []string{leakyValue, string(escaped[1 : len(escaped)-1])} {
		if strings.Contains(strings.Join(names, "\n"), form) {
			t.Fatalf("the listed names carry the secret value")
		}
	}
}

// TestListRepoSecretNamesRefusesAPathPart checks an owner or repository that is not one
// plain name is refused before any request is sent.
func TestListRepoSecretNamesRefusesAPathPart(t *testing.T) {
	t.Parallel()
	cases := map[string][2]string{
		"owner holds a slash": {"sloan/x", secretRepoName},
		"repo holds a slash":  {secretOwner, "gastown/x"},
		"owner is empty":      {"", secretRepoName},
		"repo is a traversal": {secretOwner, ".."},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := forgejotest.New()
			f.AddRepo(secretRepoPath)
			f.ClearCalls()
			c := clientFor(f, false, new([]string))
			if _, err := c.ListRepoSecretNames(tc[0], tc[1]); err == nil {
				t.Fatalf("ListRepoSecretNames(%q, %q) succeeded, want a refusal", tc[0], tc[1])
			}
			if n := len(f.CallLog()); n != 0 {
				t.Fatalf("a refused repository made %d request(s)", n)
			}
		})
	}
}

// TestListTokenNamesReturnsTheNamesTheUserHolds mints two tokens for the viewer bot and
// lists them: the names come back in the fake's order, the values do not.
func TestListTokenNamesReturnsTheNamesTheUserHolds(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	f.SeedUser(DefaultBotPrefix+"viewer", "viewer@"+DefaultBotEmailDomain)
	for _, name := range []string{"sources-token-20261106-203500", "sources-token-20261106-210000"} {
		if status, body := f.Call(t, http.MethodPost, "/admin/users/"+DefaultBotPrefix+"viewer/tokens",
			forgejotest.JSONBody(map[string]any{"name": name, "scopes": []string{"read:repository"}})); status != http.StatusCreated {
			t.Fatalf("mint %s: HTTP %d %s", name, status, body)
		}
	}
	c := clientFor(f, false, new([]string))

	names, err := c.ListTokenNames(DefaultBotPrefix + "viewer")
	if err != nil {
		t.Fatalf("ListTokenNames: %v", err)
	}
	want := []string{"sources-token-20261106-203500", "sources-token-20261106-210000"}
	if !slices.Equal(names, want) {
		t.Fatalf("names = %q, want %q", names, want)
	}
}

// TestListNamesReadsEveryPage serves a list of 120 items in pages of 50: the read follows
// the pages to the short last one and returns all 120, one request per page.
func TestListNamesReadsEveryPage(t *testing.T) {
	t.Parallel()
	pt := &pagedTokens{pages: []int{tokenPageSize, tokenPageSize, 20}}
	c := &Client{API: forgejotest.APIRoot + forgejotest.APIPath, HTTP: &http.Client{Transport: pt}, Token: newSecret(fixtureToken)}

	names, err := c.ListTokenNames("bot-viewer")
	if err != nil {
		t.Fatalf("ListTokenNames: %v", err)
	}
	if len(names) != 120 {
		t.Fatalf("got %d names, want all 120 of the three pages", len(names))
	}
	if len(pt.requested) != 3 {
		t.Fatalf("requested %d page(s), want 3: %v", len(pt.requested), pt.requested)
	}
}

// TestListNamesRefusesAPartialOrUnreadableList checks each reading failure is an error,
// so the caller never judges a name from a list it could not read to the end. Each error
// names the path, and none carries the reply body, which holds the escaped leaky value.
func TestListNamesRefusesAPartialOrUnreadableList(t *testing.T) {
	t.Parallel()
	escaped, err := json.Marshal(leakyValue)
	if err != nil {
		t.Fatalf("encode the fixture value: %v", err)
	}
	forms := []string{leakyValue, string(escaped[1 : len(escaped)-1])}
	cases := []struct {
		name      string
		transport http.RoundTripper
		wantText  string
	}{
		{
			name:      "a status other than 200",
			transport: stubReply(http.StatusInternalServerError, `{"message":"`+string(escaped[1:len(escaped)-1])+`"}`),
			wantText:  "HTTP 500",
		},
		{
			name:      "a reply that is not a JSON array",
			transport: stubReply(http.StatusOK, `{"name":"`+string(escaped[1:len(escaped)-1])+`"}`),
			wantText:  "not a JSON array",
		},
		{
			name:      "an item that carries no name",
			transport: stubReply(http.StatusOK, `[{"id":7}]`),
			wantText:  "carries no name",
		},
		{
			name:      "no reply at all",
			transport: failingTransport{},
			wantText:  "request failed",
		},
		{
			name:      "a reply body that fails on read",
			transport: brokenBodyTransport{},
			wantText:  "read reply",
		},
		{
			name:      "every page full past the bound",
			transport: &pagedTokens{pages: fullPages(listMaxPages + 1)},
			wantText:  "is not read to the end",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &Client{API: forgejotest.APIRoot + forgejotest.APIPath, HTTP: &http.Client{Transport: tc.transport}, Token: newSecret(fixtureToken)}
			_, err := c.ListTokenNames("bot-viewer")
			if err == nil {
				t.Fatal("ListTokenNames succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("error %q lacks %q", err, tc.wantText)
			}
			for _, form := range forms {
				if strings.Contains(err.Error(), form) {
					t.Fatalf("error %q carries the reply body", err)
				}
			}
		})
	}
}

// fullPages is n pages, each one full, so a read that follows them never reaches a short
// page.
func fullPages(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = tokenPageSize
	}
	return out
}

// stubReply is a transport that answers every request with status and body.
func stubReply(status int, body string) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return respond(req, status, body)
	})
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
