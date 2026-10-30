package forgejoprov

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"unicode"
)

// The environment variables and the file key the admin token resolves from,
// in the order the script reads them.
const (
	// brokerTokenEnv is the variable the auth broker's run act sets.
	brokerTokenEnv = "GT_AUTH_FORGEJO_TOKEN" //nolint:gosec // G101: the variable's name, not a credential
	// adminTokenEnv is the stand-in variable set by hand.
	adminTokenEnv = "FORGEJO_ADMIN_TOKEN" //nolint:gosec // G101: the variable's name, not a credential
)

// redacted is what every printing path of a Secret shows in place of the value.
const redacted = "[redacted]"

// adminTokenLine matches a FORGEJO_ADMIN_TOKEN= line of the admin token file:
// an optional export, and the raw value after the equals sign.
var adminTokenLine = regexp.MustCompile(`^[ \t]*(?:export[ \t]+)?` + adminTokenEnv + `=(.*)$`)

// secretValue is the token itself, behind a pointer in Secret so that fmt's
// reflective printing of an unexported Secret field shows an address, never the
// token.
type secretValue struct {
	token string
}

// Secret is a credential: the site-admin token, or a bot token once minted. Every
// way of printing it (String, GoString, Format, and JSON encoding) shows
// [redacted], so a stray %v, %s, %+v, %#v or json.Marshal cannot leak the value.
// Reveal is the one accessor that returns it.
type Secret struct {
	v *secretValue
}

// newSecret wraps a token value in a Secret.
func newSecret(token string) Secret {
	return Secret{v: &secretValue{token: token}}
}

// String stands in for the value under %v and %s, which read it through here.
func (s Secret) String() string { return redacted }

// GoString returns [redacted], so %#v cannot print the value.
func (s Secret) GoString() string { return redacted }

// Format writes [redacted] for every verb, so no verb prints the value.
func (s Secret) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, redacted)
}

// MarshalJSON keeps a struct holding a Secret from encoding the value.
func (s Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"` + redacted + `"`), nil
}

// Reveal returns the raw value. Only the Authorization header may use it.
func (s Secret) Reveal() string {
	if s.v == nil {
		return ""
	}
	return s.v.token
}

// authorizationLine matches an Authorization header echoed into text, from its
// name to the end of the line, whatever scheme the value uses.
var authorizationLine = regexp.MustCompile(`(?i)(authorization:[ \t]*)[^\r\n]*`)

// bearerText matches a Bearer credential echoed into text.
var bearerText = regexp.MustCompile(`(?i)(bearer[ \t]+)[A-Za-z0-9._~+/=-]+`)

// Scrub returns text with the token masked wherever it was echoed: every
// occurrence of the value, the rest of any Authorization line, and the
// credential after "Bearer". The Authorization and Bearer rules match by shape,
// not by value, so they apply even to a zero or empty Secret; only the
// replacement of the value itself needs a value to look for.
func (s Secret) Scrub(text string) string {
	if value := s.Reveal(); value != "" {
		text = strings.ReplaceAll(text, value, redacted)
	}
	text = authorizationLine.ReplaceAllString(text, "${1}"+redacted)
	return bearerText.ReplaceAllString(text, "${1}"+redacted)
}

// ResolveAdminToken finds the site-admin token the way the script does. The
// first of these that is set wins: $GT_AUTH_FORGEJO_TOKEN (the broker's), then
// $FORGEJO_ADMIN_TOKEN, then the FORGEJO_ADMIN_TOKEN= line of tokenFile, where
// the last matching line wins. No other source is read: a token in
// $HOME/forgejo/.env is never looked at. A token holding whitespace, a quote or
// a backslash is refused, and no error text echoes the token.
func ResolveAdminToken(getenv func(string) string, readFile func(string) ([]byte, error), tokenFile string) (Secret, error) {
	token := firstNonEmpty(getenv(brokerTokenEnv), getenv(adminTokenEnv))
	if token == "" && tokenFile != "" {
		fromFile, err := adminTokenFromFile(readFile, tokenFile)
		if err != nil {
			return Secret{}, err
		}
		token = fromFile
	}
	if token == "" {
		return Secret{}, noAdminTokenError(tokenFile)
	}
	if err := refuseUnsafeToken(token); err != nil {
		return Secret{}, err
	}
	return newSecret(token), nil
}

// adminTokenFromFile returns the value of the last FORGEJO_ADMIN_TOKEN= line of
// path, unquoted, or "" when the file is absent. A file that exists but cannot
// be read is an error: it is not the same as an absent file.
func adminTokenFromFile(readFile func(string) ([]byte, error), path string) (string, error) {
	data, err := readFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read admin token file %s: %w", path, err)
	}
	token := ""
	for _, line := range strings.Split(string(data), "\n") {
		if m := adminTokenLine.FindStringSubmatch(line); m != nil {
			token = unquote(m[1])
		}
	}
	return token, nil
}

// unquote drops one pair of matching single or double quotes around value. A
// value whose two ends are different quotes, or that has only one, keeps them,
// and refuseUnsafeToken then refuses it: a mismatched pair is a broken line, not
// a token to guess at. Whitespace after the value is not trimmed either, so a
// trailing space is refused the same way.
func unquote(value string) string {
	if len(value) >= 2 && (value[0] == '\'' || value[0] == '"') && value[len(value)-1] == value[0] {
		return value[1 : len(value)-1]
	}
	return value
}

// refuseUnsafeToken refuses a token whose characters would break the
// Authorization header or the config it is written into. The message names the
// rule and not the token.
func refuseUnsafeToken(token string) error {
	for _, r := range token {
		if unicode.IsSpace(r) || r == '"' || r == '\'' || r == '\\' {
			return errors.New("the admin token holds whitespace, a quote or a backslash; refusing to use it")
		}
	}
	return nil
}

// ErrNoAdminToken is the sentinel every no-source refusal wraps, so a caller can
// tell "no source holds a token" from a token that was found and refused.
var ErrNoAdminToken = errors.New("no admin token")

// noAdminToken is the script's error when no source holds a token. Its text
// points the operator at the auth broker, and it unwraps to ErrNoAdminToken.
type noAdminToken struct {
	text string
}

// Error returns the operator-facing text.
func (e noAdminToken) Error() string { return e.text }

// Unwrap returns ErrNoAdminToken, so errors.Is recognizes the case.
func (e noAdminToken) Unwrap() error { return ErrNoAdminToken }

// noAdminTokenError is the error ResolveAdminToken returns when no source holds
// a token. It points the operator at the auth broker.
func noAdminTokenError(tokenFile string) error {
	standIn := ""
	if tokenFile != "" {
		standIn = fmt.Sprintf(" (or FORGEJO_ADMIN_TOKEN= in %s)", tokenFile)
	}
	return noAdminToken{text: fmt.Sprintf("no admin token: run this command through the auth broker (sudo -u gt-forge gt-approve run forgejo-provision -- <its arguments>), or set FORGEJO_ADMIN_TOKEN%s for a stand-in", standIn)}
}
