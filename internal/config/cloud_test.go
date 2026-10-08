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
