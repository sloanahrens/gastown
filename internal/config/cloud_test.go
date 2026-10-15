package config

import "testing"

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
