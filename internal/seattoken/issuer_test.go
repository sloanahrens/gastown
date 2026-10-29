package seattoken

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
)

// adminToken is the fake admin credential a test hands the client; the tracker
// validates it and it is never compared to a real one.
const adminToken = "trk_admin-token-value"

// fakeTransport answers the client's requests from a function, so a test needs
// no socket and no waiting.
type fakeTransport func(*http.Request) (*http.Response, error)

func (f fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// testClient returns a tracker client whose requests the test answers.
func testClient(t *testing.T, rt fakeTransport) *client.Client {
	t.Helper()
	tc, err := client.New(client.Config{
		BaseURL: "http://127.0.0.1:7433",
		Token:   adminToken,
		HTTP:    &http.Client{Transport: rt},
	})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return tc
}

func answer(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestTrackerIssuerIssue(t *testing.T) {
	t.Parallel()
	var (
		method, path, auth string
		asked              api.IssueTokenRequest
	)
	tc := testClient(t, func(r *http.Request) (*http.Response, error) {
		method, path, auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&asked); err != nil {
			t.Errorf("request body: %v", err)
		}
		return answer(http.StatusOK, `{"actor":"gastown/polecats/cheedo","scope":"seat","token":"`+seatToken+`"}`), nil
	})

	got, err := NewTrackerIssuer(tc).Issue(context.Background(), "gastown/polecats/cheedo")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if got != seatToken {
		t.Errorf("Issue = %q, want the token the tracker returned", got)
	}
	if method != http.MethodPost || path != "/v1/tokens" {
		t.Errorf("Issue asked %s %s, want POST /v1/tokens", method, path)
	}
	if auth != "Bearer "+adminToken {
		t.Errorf("Issue sent Authorization %q, want the admin credential", auth)
	}
	if asked.Actor != "gastown/polecats/cheedo" || asked.ScopeOrSeat() != api.ScopeSeat {
		t.Errorf("Issue asked for %+v, want a seat token for gastown/polecats/cheedo", asked)
	}
}

func TestTrackerIssuerIssueRefused(t *testing.T) {
	t.Parallel()
	tc := testClient(t, func(*http.Request) (*http.Response, error) {
		return answer(http.StatusForbidden, `{"kind":"forbidden","message":"the token is not admin"}`), nil
	})

	got, err := NewTrackerIssuer(tc).Issue(context.Background(), "gastown/polecats/cheedo")
	if err == nil {
		t.Fatal("Issue succeeded against a refusal; a caller would write an empty token file")
	}
	if got != "" {
		t.Errorf("Issue = %q with an error, want no token", got)
	}
	if !strings.Contains(err.Error(), "issued no seat token") {
		t.Errorf("Issue error = %v, want it to name what failed", err)
	}
}

// TestHostClientConfig checks the config hostClientConfig builds for client.New:
// the town's endpoint and the credential daemon.env holds.
func TestHostClientConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		town string
		want client.Config
	}{
		{
			name: "the endpoint and the credential",
			town: `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z","tracker":{"port":7600}}`,
			want: client.Config{BaseURL: "http://127.0.0.1:7600", Token: adminToken},
		},
		{
			name: "a town that names no tracker reaches the default",
			town: `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z"}`,
			want: client.Config{BaseURL: "http://127.0.0.1:7433", Token: adminToken},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			writeFile(t, filepath.Join(townRoot, "mayor", "town.json"), tc.town)
			writeFile(t, filepath.Join(townRoot, "settings", "daemon.env"), "TRACKER_ADMIN_TOKEN="+adminToken+"\n")

			got, err := hostClientConfig(townRoot)
			if err != nil {
				t.Fatalf("hostClientConfig: %v", err)
			}
			if got.BaseURL != tc.want.BaseURL || got.Token != tc.want.Token || got.TokenFile != "" {
				t.Errorf("hostClientConfig = {BaseURL: %q, Token: %q, TokenFile: %q}, want {%q, %q, \"\"}",
					got.BaseURL, got.Token, got.TokenFile, tc.want.BaseURL, tc.want.Token)
			}
		})
	}
}

// A credential the loader did not produce yields no client: the zero value
// stands for one, and the config must refuse it rather than carry an empty
// secret into the client.
func TestAdminClientConfigRefusesAnUnloadedToken(t *testing.T) {
	t.Parallel()
	got, err := adminClientConfig("http://127.0.0.1:7433", config.TrackerAdminToken{})
	if err == nil {
		t.Fatalf("adminClientConfig = %+v, want an error for a credential that was not loaded", got)
	}
	if got.Token != "" {
		t.Errorf("adminClientConfig returned token %q with an error", got.Token)
	}
}

func TestNewHostIssuer(t *testing.T) {
	t.Parallel()
	const townBase = `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z"`
	cases := []struct {
		name    string
		town    string
		daemon  string
		wantErr string
	}{
		{
			name:   "a town with an endpoint and a credential",
			town:   townBase + `,"tracker":{"port":7600}}`,
			daemon: "TRACKER_ADMIN_TOKEN=" + adminToken + "\n",
		},
		{
			name:    "no admin credential",
			town:    townBase + `}`,
			wantErr: "TRACKER_ADMIN_TOKEN",
		},
		{
			name:    "a port that is not a number",
			town:    townBase + `,"tracker":{"port":"seven"}}`,
			daemon:  "TRACKER_ADMIN_TOKEN=" + adminToken + "\n",
			wantErr: "town.json",
		},
		{
			// The tracker client carries a token in the clear only to a
			// loopback address, so a remote plain-http endpoint is refused
			// here rather than at the first issue.
			name:    "a remote tracker over plain http",
			town:    townBase + `,"tracker":{"host":"tracker.internal","port":8000}}`,
			daemon:  "TRACKER_ADMIN_TOKEN=" + adminToken + "\n",
			wantErr: "plain http",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			writeFile(t, filepath.Join(townRoot, "mayor", "town.json"), tc.town)
			if tc.daemon != "" {
				writeFile(t, filepath.Join(townRoot, "settings", "daemon.env"), tc.daemon)
			}

			issuer, err := NewHostIssuer(townRoot)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("NewHostIssuer error = %v, want it to name %q", err, tc.wantErr)
				}
				if issuer != nil {
					t.Error("NewHostIssuer returned an issuer with an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("NewHostIssuer: %v", err)
			}
			if issuer == nil || issuer.tc == nil {
				t.Fatal("NewHostIssuer returned no client to issue with")
			}
		})
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
