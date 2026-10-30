package forgejoprobe

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/forgejoprov"
)

// redacted is what every printing path of a Token shows in place of the value.
const redacted = "[redacted]"

// Token is the Forgejo site-admin token the probe acts as. Every way of printing
// it shows [redacted]: Format covers every verb, including the ones a plain
// string type would print raw (%d, %x), and String and GoString cover the
// callers that reach for them directly.
type Token string

// String returns [redacted], so %v and %s cannot print the value.
func (t Token) String() string { return redacted }

// GoString returns [redacted], so %#v cannot print the value.
func (t Token) GoString() string { return redacted }

// Format writes [redacted] for every verb, so no verb prints the value.
func (t Token) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, redacted)
}

// ResolveToken finds the admin token the probe runs as. It reuses the token
// resolution of forgejoprov, so the probe and the provisioner take the same
// token from the same places in the same order: $GT_AUTH_FORGEJO_TOKEN (the
// broker's), then $FORGEJO_ADMIN_TOKEN, then the last FORGEJO_ADMIN_TOKEN= line
// of tokenFile, which is read only when it is named. The value comes back as a
// Token, so it stays redacted from here on.
func ResolveToken(getenv func(string) string, readFile func(string) ([]byte, error), tokenFile string) (Token, error) {
	secret, err := forgejoprov.ResolveAdminToken(getenv, readFile, tokenFile)
	if errors.Is(err, forgejoprov.ErrNoAdminToken) {
		return "", noTokenError{tokenFile: tokenFile, cause: err}
	}
	if err != nil {
		return "", err
	}
	return Token(secret.Reveal()), nil
}

// noTokenError is the probe's no-token refusal. Its text names forgejo-probe and
// the probe's broker command, not the provisioner's. It unwraps to the
// forgejoprov error, so errors.Is still recognizes the case.
type noTokenError struct {
	tokenFile string
	cause     error
}

// Error returns the operator-facing text. The file is named by its placeholder,
// so no path is printed.
func (e noTokenError) Error() string {
	standIn := "FORGEJO_ADMIN_TOKEN"
	if e.tokenFile != "" {
		standIn = "FORGEJO_ADMIN_TOKEN (or FORGEJO_ADMIN_TOKEN= in <file>)"
	}
	return "no admin token: run forgejo-probe through the auth broker (sudo -u gt-forge gt-approve run forgejo-probe -- <its arguments>), or set " + standIn + " for a stand-in"
}

// Unwrap returns the forgejoprov error the refusal replaces.
func (e noTokenError) Unwrap() error { return e.cause }

// NewClient builds the Forgejo client the probe calls through, authenticated as
// tok. The token travels only in the Authorization header: it is never put in
// the URL, a query, a body or an error. An empty token is refused before any
// request is made, so the client never falls back to reading a token file.
func NewClient(apiURL string, tok Token, httpClient *http.Client) (*forgejo.Client, error) {
	if tok == "" {
		return nil, errors.New("forgejo-probe: no admin token to build the client with")
	}
	opts := []forgejo.Option{
		forgejo.WithBaseURL(apiURL),
		forgejo.WithToken(string(tok)),
	}
	if httpClient != nil {
		opts = append(opts, forgejo.WithHTTPClient(httpClient))
	}
	return forgejo.NewClient("", opts...)
}
