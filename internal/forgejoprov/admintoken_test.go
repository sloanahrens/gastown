package forgejoprov

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejotest"
)

// fixtureToken stands in for a real token in every test. It is a placeholder,
// and failure messages never print it, so a leak is reported without its value.
const fixtureToken = forgejotest.AdminToken

// adminFile is the token file path the resolution tests name.
const adminFile = "/etc/gt/admin.env"

// holder carries a Secret the two ways a struct can: in an exported field, and
// in an unexported one that fmt prints by reflection.
type holder struct {
	Exported Secret
	hidden   Secret
}

// fakeFiles is the readFile seam. It serves the files it holds, reports
// fs.ErrNotExist for any other path, returns failure for every path when it is
// set, and records every path it is asked for.
type fakeFiles struct {
	files   map[string][]byte
	failure error
	asked   []string
}

func (f *fakeFiles) read(path string) ([]byte, error) {
	f.asked = append(f.asked, path)
	if f.failure != nil {
		return nil, f.failure
	}
	data, ok := f.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return data, nil
}

// TestSecretNeverPrints prints a Secret every way a caller can, and asserts the
// token appears in none of the output.
func TestSecretNeverPrints(t *testing.T) {
	t.Parallel()
	s := newSecret(fixtureToken)
	h := holder{Exported: s, hidden: s}
	encodedHolder, err := json.Marshal(h)
	if err != nil {
		t.Fatal("json.Marshal of a holder failed")
	}
	encodedSecret, err := json.Marshal(s)
	if err != nil {
		t.Fatal("json.Marshal of a Secret failed")
	}
	outputs := []struct {
		name string
		out  string
	}{
		{"%v of a Secret", fmt.Sprintf("%v", s)},
		{"%+v of a Secret", fmt.Sprintf("%+v", s)},
		{"%#v of a Secret", fmt.Sprintf("%#v", s)},
		{"%s of a Secret", fmt.Sprintf("%s", s)},
		{"%q of a Secret", fmt.Sprintf("%q", s)},
		{"%v of a pointer to a Secret", fmt.Sprintf("%v", &s)},
		{"%v of a holder", fmt.Sprintf("%v", h)},
		{"%+v of a holder", fmt.Sprintf("%+v", h)},
		{"%#v of a holder", fmt.Sprintf("%#v", h)},
		{"json.Marshal of a Secret", string(encodedSecret)},
		{"json.Marshal of a holder", string(encodedHolder)},
		{"String", s.String()},
		{"GoString", s.GoString()},
	}
	for _, o := range outputs {
		if strings.Contains(o.out, fixtureToken) {
			t.Errorf("%s prints the token", o.name)
		}
	}
	if got := fmt.Sprintf("%v", Secret{}); got != redacted {
		t.Errorf("the zero Secret prints %q, want %q", got, redacted)
	}
}

// TestResolveAdminTokenOrder pins the source order: the broker's variable beats
// the stand-in variable, which beats the file. Each case sets every source it
// names, so the first one the order picks is the one resolved.
func TestResolveAdminTokenOrder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		vars map[string]string
		want string
	}{
		{
			name: "the broker's variable beats FORGEJO_ADMIN_TOKEN and the file",
			vars: map[string]string{brokerTokenEnv: "broker-tok", adminTokenEnv: "admin-tok"},
			want: "broker-tok",
		},
		{
			name: "FORGEJO_ADMIN_TOKEN beats the file",
			vars: map[string]string{adminTokenEnv: "admin-tok"},
			want: "admin-tok",
		},
		{
			name: "the file is the last source",
			want: "from-file",
		},
	}
	for _, c := range cases {
		ff := &fakeFiles{files: map[string][]byte{adminFile: []byte("FORGEJO_ADMIN_TOKEN=from-file\n")}}
		got, err := ResolveAdminToken(env(c.vars), ff.read, adminFile)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if got.Reveal() != c.want {
			t.Errorf("%s: resolved a different source", c.name)
		}
	}
}

// TestResolveAdminTokenFile covers the file syntax: a bare key, an export with
// single quotes, double quotes, an indented export, and a key given twice where
// the last line wins.
func TestResolveAdminTokenFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "bare key", body: "FORGEJO_ADMIN_TOKEN=x\n", want: "x"},
		{name: "export with single quotes", body: "export FORGEJO_ADMIN_TOKEN='x'\n", want: "x"},
		{name: "double quotes", body: "FORGEJO_ADMIN_TOKEN=\"x\"\n", want: "x"},
		{name: "export with double quotes", body: "export FORGEJO_ADMIN_TOKEN=\"x\"\n", want: "x"},
		{name: "indented export", body: "  export FORGEJO_ADMIN_TOKEN=x\n", want: "x"},
		{
			name: "key given twice, the last line wins",
			body: "FORGEJO_ADMIN_TOKEN=first\nOTHER=1\nFORGEJO_ADMIN_TOKEN=second\n",
			want: "second",
		},
	}
	for _, c := range cases {
		ff := &fakeFiles{files: map[string][]byte{adminFile: []byte(c.body)}}
		got, err := ResolveAdminToken(env(nil), ff.read, adminFile)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if got.Reveal() != c.want {
			t.Errorf("%s: resolved the wrong value", c.name)
		}
	}
}

// TestResolveAdminTokenRefusals drives each refusal: no source at all, a token
// with whitespace, a quote or a backslash, an unreadable file, and a token in
// $HOME/forgejo/.env, which must never be read. It asserts the error names the
// rule and leaks no token.
func TestResolveAdminTokenRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		vars      map[string]string
		tokenFile string
		failure   error
		files     map[string][]byte
		wantErr   string
		leak      string
	}{
		{
			name:    "no source at all points at the auth broker",
			wantErr: "auth broker",
		},
		{
			name:      "a missing token file points at the auth broker and names the file",
			tokenFile: adminFile,
			wantErr:   "auth broker",
		},
		{
			name:      "an empty FORGEJO_ADMIN_TOKEN= line is no token",
			tokenFile: adminFile,
			files:     map[string][]byte{adminFile: []byte("FORGEJO_ADMIN_TOKEN=\n")},
			wantErr:   "auth broker",
		},
		{
			name:      "an unreadable token file is an error, not an absent file",
			tokenFile: adminFile,
			failure:   errors.New("permission denied"),
			wantErr:   "read admin token file",
		},
		{
			name:    "a token with a space is refused without echoing it",
			vars:    map[string]string{adminTokenEnv: "sec ret"},
			wantErr: "refusing to use it",
			leak:    "sec ret",
		},
		{
			name:    "a token with a double quote is refused without echoing it",
			vars:    map[string]string{adminTokenEnv: `sec"ret`},
			wantErr: "refusing to use it",
			leak:    `sec"ret`,
		},
		{
			name:    "a token with a single quote is refused without echoing it",
			vars:    map[string]string{brokerTokenEnv: "sec'ret"},
			wantErr: "refusing to use it",
			leak:    "sec'ret",
		},
		{
			name:    "a token with a backslash is refused without echoing it",
			vars:    map[string]string{adminTokenEnv: `sec\ret`},
			wantErr: "refusing to use it",
			leak:    `sec\ret`,
		},
		{
			name:    "a token with a newline is refused without echoing it",
			vars:    map[string]string{adminTokenEnv: "sec\nret"},
			wantErr: "refusing to use it",
			leak:    "sec\nret",
		},
		{
			name:      "a file value with a quote inside is refused without echoing it",
			tokenFile: adminFile,
			files:     map[string][]byte{adminFile: []byte("FORGEJO_ADMIN_TOKEN=\"sec\"ret\"\n")},
			wantErr:   "refusing to use it",
			leak:      "sec\"ret",
		},
		{
			name:      "mismatched quotes around a file value are refused without echoing it",
			tokenFile: adminFile,
			files:     map[string][]byte{adminFile: []byte("FORGEJO_ADMIN_TOKEN='secret\"\n")},
			wantErr:   "refusing to use it",
			leak:      "secret",
		},
		{
			name:      "a trailing space after a file value is refused, not trimmed",
			tokenFile: adminFile,
			files:     map[string][]byte{adminFile: []byte("FORGEJO_ADMIN_TOKEN=secret \n")},
			wantErr:   "refusing to use it",
			leak:      "secret",
		},
		{
			name:    "$HOME/forgejo/.env is never read",
			vars:    map[string]string{"HOME": "/home/gt"},
			files:   map[string][]byte{"/home/gt/forgejo/.env": []byte("FORGEJO_ADMIN_TOKEN=" + fixtureToken + "\n")},
			wantErr: "auth broker",
			leak:    fixtureToken,
		},
	}
	for _, c := range cases {
		ff := &fakeFiles{files: c.files, failure: c.failure}
		got, err := ResolveAdminToken(env(c.vars), ff.read, c.tokenFile)
		if err == nil {
			t.Errorf("%s: resolved a token, want a refusal", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: the error does not name the rule (want %q)", c.name, c.wantErr)
		}
		if c.leak != "" && strings.Contains(err.Error(), c.leak) {
			t.Errorf("%s: the error echoes the token", c.name)
		}
		if got.Reveal() != "" {
			t.Errorf("%s: a refused resolution returned a value", c.name)
		}
		for _, path := range ff.asked {
			if path != c.tokenFile {
				t.Errorf("%s: read a path other than the token file", c.name)
			}
		}
	}
}

// TestNoAdminTokenIsTheSentinel pins the no-source error: it wraps
// ErrNoAdminToken with its provisioner text unchanged, and a token that was
// found and refused does not wrap it.
func TestNoAdminTokenIsTheSentinel(t *testing.T) {
	t.Parallel()
	t.Run("no source wraps the sentinel and keeps the provisioner text", func(t *testing.T) {
		t.Parallel()
		_, err := ResolveAdminToken(env(nil), (&fakeFiles{}).read, "")
		if !errors.Is(err, ErrNoAdminToken) {
			t.Fatalf("no-source error does not wrap ErrNoAdminToken: %v", err)
		}
		const want = "no admin token: run this command through the auth broker (sudo -u gt-forge gt-approve run forgejo-provision -- <its arguments>), or set FORGEJO_ADMIN_TOKEN for a stand-in"
		if err.Error() != want {
			t.Fatalf("no-source error text changed:\n got %q\nwant %q", err.Error(), want)
		}
	})
	t.Run("an unsafe token does not wrap the sentinel", func(t *testing.T) {
		t.Parallel()
		_, err := ResolveAdminToken(env(map[string]string{adminTokenEnv: "sec ret"}), nil, "")
		if err == nil {
			t.Fatal("unsafe token resolved, want a refusal")
		}
		if errors.Is(err, ErrNoAdminToken) {
			t.Fatalf("refused token wraps ErrNoAdminToken: %v", err)
		}
	})
}

// TestSecretScrub covers what Scrub masks and what it leaves alone.
func TestSecretScrub(t *testing.T) {
	t.Parallel()
	tok := newSecret(fixtureToken)
	cases := []struct {
		name   string
		secret Secret
		text   string
		want   string
	}{
		{name: "value present twice", secret: tok, text: fixtureToken + " then " + fixtureToken, want: redacted + " then " + redacted},
		{name: "value absent", secret: tok, text: "nothing to hide", want: "nothing to hide"},
		{name: "empty secret still masks the authorization line", secret: newSecret(""), text: "Authorization: token abc", want: "Authorization: " + redacted},
		{name: "zero secret still masks bearer text", secret: Secret{}, text: "Bearer abc", want: "Bearer " + redacted},
		{name: "zero secret leaves other text alone", secret: Secret{}, text: "nothing to hide", want: "nothing to hide"},
		{name: "authorization line", secret: tok, text: "a\nAuthorization: token " + fixtureToken + "\nb", want: "a\nAuthorization: " + redacted + "\nb"},
		{name: "authorization line, lower case", secret: tok, text: "authorization: other-scheme xyz", want: "authorization: " + redacted},
		{name: "bearer text", secret: tok, text: "got Bearer abc.DEF-123 back", want: "got Bearer " + redacted + " back"},
		{name: "bearer text, upper case", secret: tok, text: "BEARER abc123", want: "BEARER " + redacted},
		{name: "value is a prefix of other text", secret: newSecret("abc"), text: "abcdef", want: redacted + "def"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.secret.Scrub(tc.text); got != tc.want {
				t.Errorf("Scrub(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}
