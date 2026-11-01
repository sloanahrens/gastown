package backend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// writeTownDaemonEnv writes the town's settings/daemon.env, where the admin
// credential is kept.
func writeTownDaemonEnv(t *testing.T, townRoot, body string) {
	t.Helper()
	dir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "daemon.env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The town-side config carries the admin credential from settings/daemon.env,
// and the endpoint from TRACKER_URL. The seat token in the same environment is
// never the token of the town-side client.
func TestTownConfigCarriesTheAdminCredentialNotTheSeatToken(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeTownDaemonEnv(t, townRoot, "TRACKER_ADMIN_TOKEN=admin-secret\n")
	env := map[string]string{
		"TRACKER_URL":        "https://tracker.example.com",
		"TRACKER_TOKEN":      "seat-secret",
		"TRACKER_TOKEN_FILE": "/sandbox/.tracker/token",
	}

	cfg, err := TownConfig(townRoot, func(key string) string { return env[key] })
	if err != nil {
		t.Fatalf("TownConfig: %v", err)
	}
	if cfg.Tracker.Token != "admin-secret" {
		t.Errorf("token = %q, want the admin credential", cfg.Tracker.Token)
	}
	if cfg.Tracker.TokenFile != "" {
		t.Errorf("token file = %q, want none: the seat's file is never read", cfg.Tracker.TokenFile)
	}
	if cfg.Tracker.BaseURL != "https://tracker.example.com" {
		t.Errorf("endpoint = %q, want TRACKER_URL", cfg.Tracker.BaseURL)
	}
}

// Every missing or unusable input is refused by name, so the caller reports the
// setting and no token is used.
func TestTownConfigRefusesWhatItCannotReach(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		noTown    bool   // pass no town root
		daemonEnv string // "" writes no file
		env       map[string]string
		want      string
	}{
		{
			name:      "no endpoint",
			daemonEnv: "TRACKER_ADMIN_TOKEN=admin-secret\n",
			env:       map[string]string{},
			want:      "TRACKER_URL is not set",
		},
		{
			name:   "no town root",
			noTown: true,
			env:    map[string]string{"TRACKER_URL": "https://tracker.example.com"},
			want:   "no town root",
		},
		{
			name: "no admin credential in settings",
			env:  map[string]string{"TRACKER_URL": "https://tracker.example.com"},
			want: "TRACKER_ADMIN_TOKEN is not set",
		},
		{
			name: "a seat token alone is not the town's credential",
			env: map[string]string{
				"TRACKER_URL":   "https://tracker.example.com",
				"TRACKER_TOKEN": "seat-secret",
			},
			want: "TRACKER_ADMIN_TOKEN is not set",
		},
		{
			name:      "an admin key that names an unset variable",
			daemonEnv: "TRACKER_ADMIN_TOKEN=${TRACKER_ABSENT_FOR_TEST}\n",
			env:       map[string]string{"TRACKER_URL": "https://tracker.example.com"},
			want:      "is not set",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := ""
			if !tt.noTown {
				townRoot = t.TempDir()
				if tt.daemonEnv != "" {
					writeTownDaemonEnv(t, townRoot, tt.daemonEnv)
				}
			}
			cfg, err := TownConfig(townRoot, func(key string) string { return tt.env[key] })
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("TownConfig endpoint %q, error %v; want a refusal naming %q", cfg.Tracker.BaseURL, err, tt.want)
			}
			if cfg.Tracker.Token != "" {
				t.Error("a refused TownConfig still carried a token")
			}
		})
	}
}

// A loader that yields no credential and no error is refused, not used: the
// client is never built around an empty secret.
func TestTownConfigRefusesALoaderThatYieldsNoCredential(t *testing.T) {
	t.Parallel()
	noCredential := func(string) (config.TrackerAdminToken, error) { return config.TrackerAdminToken{}, nil }
	env := map[string]string{"TRACKER_URL": "https://tracker.example.com"}

	cfg, err := townConfig("/town", func(key string) string { return env[key] }, noCredential)
	if err == nil || !strings.Contains(err.Error(), "not valid") {
		t.Fatalf("townConfig endpoint %q, error %v; want a refusal of the credential", cfg.Tracker.BaseURL, err)
	}
}
