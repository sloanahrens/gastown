package forgejoprov

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejotest"
)

// The repository, owner and secret the SetRepoSecret tests write.
const (
	secretOwner    = "owner"
	secretRepoName = "gastown"
	secretRepoPath = secretOwner + "/" + secretRepoName
	secretName     = "SOURCES_TOKEN"
	secretRoute    = "/repos/" + secretOwner + "/" + secretRepoName + "/actions/secrets/" + secretName
)

// leakyValue holds every character class a JSON encoder escapes: a double quote, a
// backslash, the HTML characters, a control character and non-ASCII runes. A server
// that echoes it inside a JSON body sends the escaped form, not this one.
const leakyValue = "q\"uote\\back<tag>&amp\x07noéπ"

// lastStatus returns the status of the reply to the last call the fake answered.
func lastStatus(f *forgejotest.Fake) int {
	calls := f.CallLog()
	if len(calls) == 0 {
		return 0
	}
	return calls[len(calls)-1].Status
}

// TestSetRepoSecretCreatesThenUpdates stores a secret the fake does not hold and then
// replaces it: the first call succeeds on the 201 the fake sends for a create, the
// second on its 204 for an update, and the read-back shows the second value.
func TestSetRepoSecretCreatesThenUpdates(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	f.AddRepo(secretRepoPath)
	var logs []string
	c := clientFor(f, false, &logs)

	if err := c.SetRepoSecret(context.Background(), secretOwner, secretRepoName, secretName, "first-value"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if status := lastStatus(f); status != http.StatusCreated {
		t.Fatalf("first call answered %d, want %d", status, http.StatusCreated)
	}
	if got, ok := f.Secret(secretRepoPath, secretName); !ok || got != "first-value" {
		t.Fatalf("after the first call the fake does not hold the first value")
	}

	if err := c.SetRepoSecret(context.Background(), secretOwner, secretRepoName, secretName, "second-value"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if status := lastStatus(f); status != http.StatusNoContent {
		t.Fatalf("second call answered %d, want %d", status, http.StatusNoContent)
	}
	if got, ok := f.Secret(secretRepoPath, secretName); !ok || got != "second-value" {
		t.Fatalf("after the second call the fake does not hold the second value")
	}
	if n := len(f.Writes()); n != 2 {
		t.Fatalf("the fake saw %d write(s), want 2", n)
	}
}

// TestSetRepoSecretRefusesBadInput covers every input SetRepoSecret refuses before it
// sends anything: an owner or repository that is not a plain name, a secret name
// outside the repository's own convention, and an empty value. No call reaches the
// fake and nothing is logged.
func TestSetRepoSecretRefusesBadInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		owner  string
		repo   string
		secret string
		value  string
	}{
		{name: "empty value", owner: secretOwner, repo: secretRepoName, secret: secretName, value: ""},
		{name: "lower-case name", owner: secretOwner, repo: secretRepoName, secret: "sources_token", value: "fixture-value"},
		{name: "hyphen in name", owner: secretOwner, repo: secretRepoName, secret: "SOURCES-TOKEN", value: "fixture-value"},
		{name: "empty name", owner: secretOwner, repo: secretRepoName, secret: "", value: "fixture-value"},
		{name: "owner walks the path", owner: "..", repo: secretRepoName, secret: secretName, value: "fixture-value"},
		{name: "empty repository", owner: secretOwner, repo: "", secret: secretName, value: "fixture-value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := forgejotest.New()
			f.AddRepo(secretRepoPath)
			var logs []string
			client := clientFor(f, false, &logs)
			err := client.SetRepoSecret(context.Background(), c.owner, c.repo, c.secret, c.value)
			if err == nil {
				t.Fatalf("SetRepoSecret(%q, %q, %q) returned no error, want a refusal", c.owner, c.repo, c.secret)
			}
			if n := len(f.CallLog()); n != 0 {
				t.Fatalf("%d call(s) reached the fake, want the refusal before any request", n)
			}
			if len(logs) != 0 {
				t.Fatalf("the refusal logged %q, want no line", logs)
			}
		})
	}
}

// TestSetRepoSecretDryRunSendsNoWrite checks a dry-run client reports the secret it
// would set and sends nothing, so a dry run never fails on the status it would get.
func TestSetRepoSecretDryRunSendsNoWrite(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	f.AddRepo(secretRepoPath)
	var logs []string
	c := clientFor(f, true, &logs)

	if err := c.SetRepoSecret(context.Background(), secretOwner, secretRepoName, secretName, "fixture-value"); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if n := len(f.CallLog()); n != 0 {
		t.Fatalf("the dry run sent %d request(s), want none", n)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "would set secret "+secretName) {
		t.Fatalf("the dry run logged %d line(s), want the one naming the secret it would set", len(logs))
	}
}

// TestSetRepoSecretReportsATransportFailure checks a request that never reaches the
// server is an error naming the route, and that it carries no value.
func TestSetRepoSecretReportsATransportFailure(t *testing.T) {
	t.Parallel()
	var logs []string
	c := &Client{
		API:   forgejotest.APIRoot + forgejotest.APIPath,
		HTTP:  &http.Client{Transport: failingTransport{}},
		Token: newSecret(fixtureToken),
		Log:   func(line string) { logs = append(logs, line) },
	}
	err := c.SetRepoSecret(context.Background(), secretOwner, secretRepoName, secretName, leakyValue)
	if err == nil {
		t.Fatal("SetRepoSecret returned no error, want the transport failure")
	}
	if !strings.Contains(err.Error(), secretRoute) {
		t.Fatalf("the error does not name the route %s", secretRoute)
	}
	for _, leak := range leakedForms(t, leakyValue) {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("the error carries the value")
		}
	}
}

// TestSetRepoSecretNeverLeaksTheValue refuses the write with 403, 404 and 500, the fake
// echoing the value inside a JSON error body each time, and checks the error and the
// log carry the status, the route and the fixed phrase, and neither the raw value nor
// the escaped form the body holds nor any text from that body.
func TestSetRepoSecretNeverLeaksTheValue(t *testing.T) {
	t.Parallel()
	forms := leakedForms(t, leakyValue)
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := forgejotest.New()
			f.AddRepo(secretRepoPath)
			f.SecretRefusal = status
			// The fake must really echo the value, in the escaped form the encoder
			// gives it, or the refusal below proves nothing.
			_, body := f.Call(t, http.MethodPut, secretRoute, forgejotest.JSONBody(map[string]string{"data": leakyValue}))
			if !strings.Contains(body, forms[1]) {
				t.Fatalf("the fake's refusal does not echo the value in its escaped form")
			}
			var echoed struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal([]byte(body), &echoed); err != nil || echoed.Message == "" {
				t.Fatalf("the fake's refusal carries no message to leak")
			}
			f.ClearCalls()

			var logs []string
			c := clientFor(f, false, &logs)
			err := c.SetRepoSecret(context.Background(), secretOwner, secretRepoName, secretName, leakyValue)
			if err == nil {
				t.Fatal("SetRepoSecret returned no error, want the refusal")
			}
			msg := err.Error()
			leaks := []struct{ form, what string }{
				{forms[0], "the raw value"},
				{forms[1], "the value's escaped form"},
				{echoed.Message, "text of the reply"},
			}
			for _, leak := range leaks {
				if strings.Contains(msg, leak.form) {
					t.Fatalf("the error carries %s", leak.what)
				}
				for _, line := range logs {
					if strings.Contains(line, leak.form) {
						t.Fatalf("a log line carries %s", leak.what)
					}
				}
			}
			if !strings.Contains(msg, fmt.Sprint(status)) {
				t.Fatalf("the error does not carry the status %d", status)
			}
			if !strings.Contains(msg, secretRoute) {
				t.Fatalf("the error does not carry the route %s", secretRoute)
			}
			if !strings.Contains(msg, secretNotSet) {
				t.Fatalf("the error does not carry the fixed phrase %q", secretNotSet)
			}
		})
	}
}

// leakedForms returns the value as the request carries it and as json.Marshal escapes
// it inside a JSON string, and fails when the two agree: a fixture that never escapes
// cannot catch an error built from the reply.
func leakedForms(t *testing.T, value string) []string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode the fixture value: %v", err)
	}
	escaped := string(encoded[1 : len(encoded)-1])
	if escaped == value {
		t.Fatalf("the fixture value has nothing for a JSON encoder to escape")
	}
	return []string{value, escaped}
}
