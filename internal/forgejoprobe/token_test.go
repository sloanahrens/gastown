package forgejoprobe

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"testing"
)

// fixtureToken stands in for the admin token in every test. It is a placeholder,
// and failure messages never print it, so a leak is reported without its value.
const fixtureToken = "fixture-admin-7f3a9c"

// probeTokenFile is the token file path the resolution tests name.
const probeTokenFile = "/etc/gt/probe-admin.env"

// The two variables the resolution reads, spelled here so a test states the
// order it means rather than reading the production constants.
const (
	brokerVar = "GT_AUTH_FORGEJO_TOKEN"
	adminVar  = "FORGEJO_ADMIN_TOKEN"
)

// fakeFiles is the readFile seam. It serves the files it holds, reports
// fs.ErrNotExist for any other path, and records every path it is asked for, so
// a test can assert that a file was never read.
type fakeFiles struct {
	files map[string][]byte
	asked []string
}

func (f *fakeFiles) read(path string) ([]byte, error) {
	f.asked = append(f.asked, path)
	data, ok := f.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return data, nil
}

// envOf is the getenv seam: a variable the map does not name reads empty, so no
// test sees the host's environment.
func envOf(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

// TestResolveTokenOrder pins the precedence: the broker's variable beats the
// stand-in variable, which beats the file. With every source unset, the error
// names the broker command.
func TestResolveTokenOrder(t *testing.T) {
	t.Parallel()
	const broker, admin, file = "broker-token-1", "admin-token-2", "file-token-3"
	fileWith := func() *fakeFiles {
		return &fakeFiles{files: map[string][]byte{probeTokenFile: []byte("FORGEJO_ADMIN_TOKEN=" + file + "\n")}}
	}
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"broker beats admin and file", map[string]string{brokerVar: broker, adminVar: admin}, broker},
		{"admin beats file", map[string]string{adminVar: admin}, admin},
		{"file when no variable is set", nil, file},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveToken(envOf(tc.env), fileWith().read, probeTokenFile)
			if err != nil {
				t.Fatalf("ResolveToken: unexpected error %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("ResolveToken picked the wrong source: got %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("all three unset names the broker command", func(t *testing.T) {
		t.Parallel()
		files := &fakeFiles{}
		_, err := ResolveToken(envOf(nil), files.read, probeTokenFile)
		if err == nil {
			t.Fatal("ResolveToken with no source: want an error, got none")
		}
		if !strings.Contains(err.Error(), "gt-approve run") {
			t.Fatalf("no-token error does not name the auth broker command: %q", err)
		}
	})
}

// TestResolveTokenFileParsing covers the file's line forms. The file is read
// only when it is named: a decoy at the default path is never asked for.
func TestResolveTokenFileParsing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		content string
		want    string
		wantErr bool
	}{
		{"plain line", "FORGEJO_ADMIN_TOKEN=tok-plain\n", "tok-plain", false},
		{"export prefix", "export FORGEJO_ADMIN_TOKEN=tok-export\n", "tok-export", false},
		{"leading spaces", "   FORGEJO_ADMIN_TOKEN=tok-indent\n", "tok-indent", false},
		{"single quotes", "FORGEJO_ADMIN_TOKEN='tok-single'\n", "tok-single", false},
		{"double quotes", "FORGEJO_ADMIN_TOKEN=\"tok-double\"\n", "tok-double", false},
		{"two lines, last wins", "FORGEJO_ADMIN_TOKEN=tok-first\nFORGEJO_ADMIN_TOKEN=tok-second\n", "tok-second", false},
		{"comment line ignored", "# FORGEJO_ADMIN_TOKEN=tok-commented\nFORGEJO_ADMIN_TOKEN=tok-live\n", "tok-live", false},
		{"no matching line", "OTHER_KEY=tok-other\n", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := &fakeFiles{files: map[string][]byte{probeTokenFile: []byte(tc.content)}}
			got, err := ResolveToken(envOf(nil), files.read, probeTokenFile)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveToken: want an error for a file with no matching line, got token %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveToken: unexpected error %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("ResolveToken: got %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("a decoy at the default path is never read", func(t *testing.T) {
		t.Parallel()
		files := &fakeFiles{files: map[string][]byte{
			"/home/op/forgejo/.env": []byte("FORGEJO_ADMIN_TOKEN=tok-decoy\n"),
		}}
		_, err := ResolveToken(envOf(nil), files.read, "")
		if err == nil {
			t.Fatal("ResolveToken with no file named: want an error, got a token from a decoy")
		}
		if len(files.asked) != 0 {
			t.Fatalf("ResolveToken read a file that was not named: %v", files.asked)
		}
	})
}

// TestResolveTokenRefusesUnsafe refuses a token holding whitespace, a quote or a
// backslash, and the refusal does not echo the token.
func TestResolveTokenRefusesUnsafe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		token string
	}{
		{"newline", "tok-line\nbreak-9f2"},
		{"double quote", `tok"quote-9f2`},
		{"single quote", "tok'quote-9f2"},
		{"backslash", `tok\slash-9f2`},
		{"space", "tok space-9f2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ResolveToken(envOf(map[string]string{adminVar: tc.token}), nil, "")
			if err == nil {
				t.Fatalf("ResolveToken with a %s in the token: want a refusal, got none", tc.name)
			}
			if strings.Contains(err.Error(), tc.token) {
				t.Fatalf("refusal echoes the token: %q", err)
			}
		})
	}
}

// TestTokenIsRedacted prints a Token every way a caller can, and asserts the
// value appears in none of the output.
func TestTokenIsRedacted(t *testing.T) {
	t.Parallel()
	tok := Token(fixtureToken)
	outputs := map[string]string{
		"%s":     fmt.Sprintf("%s", tok),
		"%v":     fmt.Sprintf("%v", tok),
		"%+v":    fmt.Sprintf("%+v", tok),
		"%#v":    fmt.Sprintf("%#v", tok),
		"%q":     fmt.Sprintf("%q", tok),
		"%x":     fmt.Sprintf("%x", tok),
		"%d":     fmt.Sprintf("%d", tok),
		"err":    fmt.Errorf("wrapped: %v", tok).Error(),
		"Sprint": fmt.Sprint(tok),
	}
	for verb, out := range outputs {
		if strings.Contains(out, fixtureToken) {
			t.Errorf("%s printed the token value", verb)
		}
		if !strings.Contains(out, redacted) {
			t.Errorf("%s did not show the redaction marker: %q", verb, out)
		}
	}
}

// fakeTransport is the RoundTripper seam: it records each request and answers
// every one with a fixed version body, so no socket is opened.
type fakeTransport struct {
	requests []*http.Request
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.requests = append(f.requests, req)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"version":"0.0.0-test"}`)),
		Request:    req,
	}, nil
}

// TestNewClientSendsTheToken makes one call through the client and asserts the
// token rides only in the Authorization header. An empty token is refused before
// any request.
func TestNewClientSendsTheToken(t *testing.T) {
	t.Parallel()
	t.Run("token is sent in the header only", func(t *testing.T) {
		t.Parallel()
		transport := &fakeTransport{}
		client, err := NewClient("https://forgejo.example/api/v1", Token(fixtureToken), &http.Client{Transport: transport})
		if err != nil {
			t.Fatalf("NewClient: unexpected error %v", err)
		}
		if _, err := client.Version(context.Background()); err != nil {
			t.Fatalf("Version: unexpected error %v", err)
		}
		if len(transport.requests) != 1 {
			t.Fatalf("want one request, got %d", len(transport.requests))
		}
		req := transport.requests[0]
		if got := req.Header.Get("Authorization"); got != "token "+fixtureToken {
			t.Fatalf("Authorization header: got %q, want the admin token", got)
		}
		if strings.Contains(req.URL.String(), fixtureToken) {
			t.Fatalf("token appears in the request URL: %s", req.URL)
		}
		if req.URL.RawQuery != "" && strings.Contains(req.URL.RawQuery, fixtureToken) {
			t.Fatalf("token appears in the query: %s", req.URL.RawQuery)
		}
		if req.Body != nil && req.Body != http.NoBody {
			body, _ := io.ReadAll(req.Body)
			if strings.Contains(string(body), fixtureToken) {
				t.Fatal("token appears in the request body")
			}
		}
	})

	t.Run("empty token fails before any request", func(t *testing.T) {
		t.Parallel()
		transport := &fakeTransport{}
		_, err := NewClient("https://forgejo.example/api/v1", "", &http.Client{Transport: transport})
		if err == nil {
			t.Fatal("NewClient with an empty token: want an error, got none")
		}
		if !strings.Contains(err.Error(), "no admin token") {
			t.Fatalf("NewClient with an empty token refused for another reason: %q", err)
		}
		if len(transport.requests) != 0 {
			t.Fatalf("NewClient with an empty token made %d requests", len(transport.requests))
		}
	})
}
