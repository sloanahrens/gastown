package config

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
)

// trackerAdminFakeSecret is a fake admin credential. Its marker is the one
// every fake credential in this package starts with, so a leak is found by
// the same substring search.
const trackerAdminFakeSecret = fakeCredentialMarker + "tracker-admin-secret"

// The credential is a ${VAR} reference in settings/daemon.env, resolved the
// way the secret mechanism resolves one: a name daemon.env defines is read
// from it, any other from the process environment.
func TestLoadTrackerAdminToken_ResolvesValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		body   string
		parent map[string]string
	}{
		{
			name: "literal value",
			body: "OTHER=x\n" + trackerAdminTokenKey + "=" + trackerAdminFakeSecret + "\n",
		},
		{
			name: "reference to a daemon.env name",
			body: trackerAdminTokenKey + "=${GT_TRACKER_ADMIN_CRED}\nGT_TRACKER_ADMIN_CRED=" + trackerAdminFakeSecret + "\n",
		},
		{
			name:   "reference to a process name",
			body:   trackerAdminTokenKey + "=${FROM_PARENT}\n",
			parent: map[string]string{"FROM_PARENT": trackerAdminFakeSecret},
		},
		{
			name:   "value naming only itself takes the process value",
			body:   trackerAdminTokenKey + "=${" + trackerAdminTokenKey + "}\n",
			parent: map[string]string{trackerAdminTokenKey: trackerAdminFakeSecret},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			writeDaemonEnv(t, townRoot, tc.body)
			getenv := func(name string) string { return tc.parent[name] }

			tok, err := loadTrackerAdminToken(townRoot, getenv)
			if err != nil {
				t.Fatalf("loadTrackerAdminToken() error = %v", err)
			}
			got, ok := tok.Reveal()
			if !ok {
				t.Fatal("Reveal() ok = false, want a loaded credential")
			}
			if got != trackerAdminFakeSecret {
				t.Errorf("Reveal() = %q, want the referenced value", got)
			}
		})
	}
}

// Every way the key can fail to resolve is an error that names the key or the
// referenced name and prints no value. The returned token is not a credential,
// so a caller that ignored the error cannot use it.
func TestLoadTrackerAdminToken_ErrorNamesKeyNotValues(t *testing.T) {
	t.Parallel()
	const sibling = fakeCredentialMarker + "sibling-value"
	cases := []struct {
		name string
		body *string
	}{
		{name: "no daemon.env", body: nil},
		{name: "key absent", body: strPtr("OTHER=" + sibling + "\n")},
		{name: "key empty", body: strPtr(trackerAdminTokenKey + "=\nOTHER=" + sibling + "\n")},
		{name: "key whitespace", body: strPtr(trackerAdminTokenKey + "= \t \nOTHER=" + sibling + "\n")},
		{name: "reference to an unset name", body: strPtr(trackerAdminTokenKey + "=${GT_TRACKER_ADMIN_UNSET}\n")},
		{
			name: "reference cycle",
			body: strPtr(trackerAdminTokenKey + "=${GT_TRACKER_ADMIN_LOOP}\nGT_TRACKER_ADMIN_LOOP=${" + trackerAdminTokenKey + "}\n"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			if tc.body != nil {
				writeDaemonEnv(t, townRoot, *tc.body)
			}
			tok, err := loadTrackerAdminToken(townRoot, func(string) string { return "" })
			if err == nil {
				t.Fatal("loadTrackerAdminToken() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), trackerAdminTokenKey) {
				t.Errorf("error = %q, want it to name %s", err, trackerAdminTokenKey)
			}
			if strings.Contains(err.Error(), sibling) || strings.Contains(err.Error(), "tracker-admin-secret") {
				t.Errorf("error = %q, want no value from daemon.env", err)
			}
			if _, ok := tok.Reveal(); ok {
				t.Error("Reveal() ok = true after an error, want a token that reports it is unset")
			}
			if got := tok.String(); got != trackerAdminRedacted {
				t.Errorf("String() = %q, want the redaction marker", got)
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
	writeDaemonEnv(t, townRoot, trackerAdminTokenKey+" "+leak+"\n")

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

// A daemon.env that exists but cannot be read is an error naming the file and
// the key. It must not read as a missing key, which would let the caller go on
// as if no token were set.
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
	if !strings.Contains(err.Error(), trackerAdminTokenKey) {
		t.Errorf("error = %q, want it to name the key it could not read", err)
	}
	if strings.Contains(err.Error(), "is not set") {
		t.Errorf("error = %q, want the read failure, not a missing key", err)
	}
}

// Every fmt verb and the log package print the redaction marker, never the
// credential, whatever the verb or flag.
func TestTrackerAdminToken_RedactsInEveryFormat(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeDaemonEnv(t, townRoot, trackerAdminTokenKey+"="+trackerAdminFakeSecret+"\n")
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

	t.Run("zero value", func(t *testing.T) {
		t.Parallel()
		if got := fmt.Sprintf("%v %s", TrackerAdminToken{}, TrackerAdminToken{}); strings.Contains(got, "tracker-admin-secret") {
			t.Errorf("zero value formats as %q, want the redaction marker", got)
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
// daemon.env or exported in the spawning process: not AgentEnv (tmux
// new-session -e) and not the startup command the seat runs (argv).
func TestSeatEnvNeverCarriesTrackerAdminToken(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "gastown")
	if err := SaveTownSettings(TownSettingsPath(townRoot), NewTownSettings()); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
	if err := SaveRigSettings(RigSettingsPath(rigPath), NewRigSettings()); err != nil {
		t.Fatalf("SaveRigSettings: %v", err)
	}
	writeDaemonEnv(t, townRoot, trackerAdminTokenKey+"=${GT_TRACKER_ADMIN_CRED}\nGT_TRACKER_ADMIN_CRED="+trackerAdminFakeSecret+"\n")
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

			rp := rigPath
			if cfg.Rig == "" {
				rp = ""
			}
			cmd, err := buildStartupCommandFromConfig(h, cfg, rp, "beacon", "")
			if err != nil {
				t.Fatalf("buildStartupCommandFromConfig: %v", err)
			}
			if strings.Contains(cmd, trackerAdminFakeSecret) {
				t.Errorf("startup command carries the tracker admin credential: %s", cmd)
			}
			if strings.Contains(cmd, trackerAdminTokenKey) {
				t.Errorf("startup command names %s: %s", trackerAdminTokenKey, cmd)
			}
		})
	}
}

func strPtr(s string) *string { return &s }
