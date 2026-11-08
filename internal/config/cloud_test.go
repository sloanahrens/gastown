package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The cloud patrol writes outside the town, so the operator names its directory
// per machine: the environment when it is set, and the patrol's own default
// otherwise. The environment is injected here rather than set, which the unit
// tier forbids.
func TestCloudReportsDirFollowsTheEnvironment(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, env, want string
	}{
		{"no override", "", CloudReportsDefault},
		{"an empty override", "   ", CloudReportsDefault},
		{"an override", "/tmp/patrol", "/tmp/patrol"},
		{"an override with space around it", "  /tmp/patrol\n", "/tmp/patrol"},
	} {
		got := cloudReportsDir(func(string) string { return tc.env })
		if got != tc.want {
			t.Errorf("%s: directory %q, want %q", tc.name, got, tc.want)
		}
	}

	// The seam is asked for the variable the panel documents, not some other.
	var asked string
	cloudReportsDir(func(name string) string { asked = name; return "" })
	if asked != CloudReportsEnv {
		t.Errorf("the resolver reads %q, want %q", asked, CloudReportsEnv)
	}
}

// The app power line reads the directories the wrapper and the release bot
// already name through their own variables, so the resolvers ask for exactly
// those and fall back to the shared defaults.
func TestCloudAppPowerDirsFollowTheBotsVariables(t *testing.T) {
	t.Parallel()

	if CloudOffEnv != "GT_OFF_DIR" || CloudReleasesEnv != "GT_CLOUD_RELEASES_DIR" {
		t.Errorf("the variables are %q and %q, not the ones the wrapper and the bot read", CloudOffEnv, CloudReleasesEnv)
	}
	for _, tc := range []struct{ name, env, def string }{
		{"off", CloudOffEnv, CloudOffDefault},
		{"releases", CloudReleasesEnv, CloudReleasesDefault},
	} {
		if got := envDir(func(string) string { return "  " }, tc.env, tc.def); got != tc.def {
			t.Errorf("%s: a blank override gives %q, want %q", tc.name, got, tc.def)
		}
		if got := envDir(func(string) string { return " /tmp/x\n" }, tc.env, tc.def); got != "/tmp/x" {
			t.Errorf("%s: an override gives %q, want /tmp/x", tc.name, got)
		}
	}
	if CloudOffDefault != "/Users/Shared/gt-cloud/off" || CloudReleasesDefault != "/Users/Shared/gt-cloud/releases" {
		t.Error("a default moved off the shared gt-cloud directories")
	}
}

// cloudSettingsFixture loads one settings document from a fresh file.
func cloudSettingsFixture(t *testing.T, doc string) (*TownSettings, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadTownSettings(path)
}

func TestCloudSettings(t *testing.T) {
	t.Parallel()

	t.Run("a block with source and ledger_dsn_file loads", func(t *testing.T) {
		t.Parallel()
		ts, err := cloudSettingsFixture(t, `{"type":"town-settings","version":1,"cloud":{"source":"/srv/cloud","ledger_dsn_file":"/run/ledger.dsn"}}`)
		if err != nil {
			t.Fatalf("LoadTownSettings: %v", err)
		}
		if ts.Cloud == nil || ts.Cloud.Source != "/srv/cloud" || ts.Cloud.LedgerDSNFile != "/run/ledger.dsn" {
			t.Fatalf("cloud block = %+v", ts.Cloud)
		}
		// ledger_dsn_file wins: the block says Postgres even though it also
		// names a directory.
		got := ts.ResolveCloud(func(string) string { return "" })
		if !got.Postgres || got.DSNFile != "/run/ledger.dsn" {
			t.Errorf("a DSN file resolves to %+v, want the Postgres source", got)
		}
	})

	t.Run("an unknown key is refused", func(t *testing.T) {
		t.Parallel()
		_, err := cloudSettingsFixture(t, `{"type":"town-settings","cloud":{"source":"/srv/cloud","ledgr_dsn_file":"/run/x"}}`)
		if err == nil {
			t.Fatal("a misspelled key loaded")
		}
		if !strings.Contains(err.Error(), "cloud") || !strings.Contains(err.Error(), "ledgr_dsn_file") {
			t.Errorf("err = %v, want the cloud key named", err)
		}
	})

	t.Run("an empty block is off", func(t *testing.T) {
		t.Parallel()
		ts, err := cloudSettingsFixture(t, `{"type":"town-settings","cloud":{}}`)
		if err != nil {
			t.Fatalf("LoadTownSettings: %v", err)
		}
		got := ts.ResolveCloud(func(string) string { return "/env/reports" })
		if !got.Off {
			t.Errorf("an empty block resolves to %+v, want off", got)
		}
		if got.ReportsDir != "" || got.ReleasesDir != "" {
			t.Errorf("an off block still names directories: %+v", got)
		}
		if CloudNotConfigured != "cloud step not configured" {
			t.Errorf("the off note is %q", CloudNotConfigured)
		}
	})

	t.Run("an absent block keeps today's directories", func(t *testing.T) {
		t.Parallel()
		ts, err := cloudSettingsFixture(t, `{"type":"town-settings","version":1}`)
		if err != nil {
			t.Fatalf("LoadTownSettings: %v", err)
		}
		env := func(name string) string {
			switch name {
			case CloudReportsEnv:
				return "/env/reports"
			case CloudReleasesEnv:
				return "/env/releases"
			case CloudOffEnv:
				return "/env/off"
			}
			return ""
		}
		got := ts.ResolveCloud(env)
		if got.Off || got.Postgres {
			t.Errorf("an absent block resolves to %+v, want the file directories", got)
		}
		if got.ReportsDir != "/env/reports" || got.ReleasesDir != "/env/releases" || got.OffDir != "/env/off" {
			t.Errorf("an absent block resolves to %+v, want the environment directories", got)
		}
	})

	t.Run("a source replaces both ledger directories and not the markers", func(t *testing.T) {
		t.Parallel()
		ts, err := cloudSettingsFixture(t, `{"type":"town-settings","cloud":{"source":"/srv/cloud"}}`)
		if err != nil {
			t.Fatalf("LoadTownSettings: %v", err)
		}
		got := ts.ResolveCloud(func(string) string { return "" })
		if got.Off || got.Postgres || got.ReportsDir != "/srv/cloud" || got.ReleasesDir != "/srv/cloud" {
			t.Errorf("source resolves to %+v, want both ledger directories", got)
		}
		if got.OffDir != CloudOffDefault {
			t.Errorf("off dir = %q, want the wrapper's own %q: the markers stay on the machine", got.OffDir, CloudOffDefault)
		}
	})
}
