package config

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
)

// trackerAdminFakeSecret is a fake admin credential. Its marker is the one
// every fake credential in this package starts with, so a leak is found by
// the same substring search.
const trackerAdminFakeSecret = fakeCredentialMarker + "tracker-admin-secret"

func TestLoadTrackerAdminToken_ReadsKeyFromDaemonEnv(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeDaemonEnv(t, townRoot, "OTHER=x\nTRACKER_ADMIN_TOKEN="+trackerAdminFakeSecret+"\n")

	tok, err := LoadTrackerAdminToken(townRoot)
	if err != nil {
		t.Fatalf("LoadTrackerAdminToken() error = %v", err)
	}
	if got := tok.Reveal(); got != trackerAdminFakeSecret {
		t.Errorf("Reveal() = %q, want the daemon.env value", got)
	}
}

// A missing key, an empty key and a missing file are all errors that name
// the key and print none of the file's other values.
func TestLoadTrackerAdminToken_MissingKeyErrorNamesKeyNotValues(t *testing.T) {
	t.Parallel()
	const sibling = fakeCredentialMarker + "sibling-value"
	cases := []struct {
		name string
		body *string
	}{
		{name: "no daemon.env", body: nil},
		{name: "key absent", body: strPtr("OTHER=" + sibling + "\n")},
		{name: "key empty", body: strPtr("TRACKER_ADMIN_TOKEN=\nOTHER=" + sibling + "\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			if tc.body != nil {
				writeDaemonEnv(t, townRoot, *tc.body)
			}
			_, err := LoadTrackerAdminToken(townRoot)
			if err == nil {
				t.Fatal("LoadTrackerAdminToken() error = nil, want an error for a missing key")
			}
			if !strings.Contains(err.Error(), trackerAdminTokenKey) {
				t.Errorf("error = %q, want it to name %s", err, trackerAdminTokenKey)
			}
			if strings.Contains(err.Error(), sibling) {
				t.Errorf("error = %q, want no value from daemon.env", err)
			}
		})
	}
}

// A malformed daemon.env is an error that names the file and line. The line
// may be the token itself, so the error must not quote it.
func TestLoadTrackerAdminToken_MalformedLineErrorDoesNotEchoLine(t *testing.T) {
	t.Parallel()
	const leak = fakeCredentialMarker + "malformed-line"
	townRoot := t.TempDir()
	writeDaemonEnv(t, townRoot, "TRACKER_ADMIN_TOKEN "+leak+"\n")

	_, err := LoadTrackerAdminToken(townRoot)
	if err == nil {
		t.Fatal("LoadTrackerAdminToken() error = nil, want an error for a line with no '='")
	}
	if !strings.Contains(err.Error(), "line 1") {
		t.Errorf("error = %q, want it to name line 1", err)
	}
	if strings.Contains(err.Error(), leak) {
		t.Errorf("error = %q, want the malformed line's text left out", err)
	}
}

// Every fmt verb and the log package print the redaction marker, never the
// credential, whatever the verb or flag.
func TestTrackerAdminToken_RedactsInEveryFormat(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeDaemonEnv(t, townRoot, "TRACKER_ADMIN_TOKEN="+trackerAdminFakeSecret+"\n")
	tok, err := LoadTrackerAdminToken(townRoot)
	if err != nil {
		t.Fatalf("LoadTrackerAdminToken() error = %v", err)
	}

	forms := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"}
	for _, form := range forms {
		t.Run(form, func(t *testing.T) {
			t.Parallel()
			got := fmt.Sprintf(form, tok)
			if strings.Contains(got, trackerAdminFakeSecret) || strings.Contains(got, "tracker-admin-secret") {
				t.Errorf("fmt.Sprintf(%q) = %q, want the credential redacted", form, got)
			}
			if !strings.Contains(got, trackerAdminRedacted) {
				t.Errorf("fmt.Sprintf(%q) = %q, want the redaction marker", form, got)
			}
		})
	}

	t.Run("String", func(t *testing.T) {
		t.Parallel()
		if got := tok.String(); strings.Contains(got, "tracker-admin-secret") {
			t.Errorf("String() = %q, want the credential redacted", got)
		}
	})

	t.Run("log", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		l := log.New(&buf, "", 0)
		l.Print(tok)
		l.Printf("token=%v", tok)
		if got := buf.String(); strings.Contains(got, "tracker-admin-secret") {
			t.Errorf("log output = %q, want the credential redacted", got)
		}
	})
}

// No seat environment carries the admin credential, whether it is in
// daemon.env or exported in the spawning process. AgentEnv is the seat
// environment builder (env.go).
func TestSeatEnvNeverCarriesTrackerAdminToken(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := SaveTownSettings(TownSettingsPath(townRoot), NewTownSettings()); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
	writeDaemonEnv(t, townRoot, "TRACKER_ADMIN_TOKEN="+trackerAdminFakeSecret+"\n")
	parent := parentEnvWithFakeCredentials()
	parent[trackerAdminTokenKey] = trackerAdminFakeSecret
	h := agentHost(parent)

	roles := []AgentEnvConfig{
		{Role: "deacon", TownRoot: townRoot},
		{Role: "witness", Rig: "gastown", TownRoot: townRoot},
		{Role: "refinery", Rig: "gastown", TownRoot: townRoot},
		{Role: constants.RolePolecat, Rig: "gastown", AgentName: "nux", TownRoot: townRoot},
		{Role: constants.RoleCrew, Rig: "gastown", AgentName: "holden", TownRoot: townRoot},
	}
	for _, cfg := range roles {
		t.Run(cfg.Role, func(t *testing.T) {
			t.Parallel()
			cfg.Getenv = h.getenv
			env := AgentEnv(cfg)
			if _, ok := env[trackerAdminTokenKey]; ok {
				t.Errorf("AgentEnv carries %s", trackerAdminTokenKey)
			}
			for k, v := range env {
				if strings.Contains(v, trackerAdminFakeSecret) {
					t.Errorf("AgentEnv[%s] carries the tracker admin credential", k)
				}
			}
		})
	}
}

// A daemon.env that exists but cannot be read is an error naming the file. It
// must not read as a missing key, which would let the caller go on as if no
// token were set.
func TestLoadTrackerAdminToken_UnreadableFileIsErrorNotMissingKey(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	// A directory where the file should be: the read fails, and not with "not exist".
	if err := os.MkdirAll(DaemonEnvPath(townRoot), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := LoadTrackerAdminToken(townRoot)
	if err == nil {
		t.Fatal("LoadTrackerAdminToken() error = nil, want an error for an unreadable daemon.env")
	}
	if !strings.Contains(err.Error(), "daemon.env") {
		t.Errorf("error = %q, want it to name the file", err)
	}
	if strings.Contains(err.Error(), "is not set") {
		t.Errorf("error = %q, want the read failure, not a missing key", err)
	}
}

func strPtr(s string) *string { return &s }
