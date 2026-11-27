package rigaudit

import (
	"bytes"
	"errors"
	"testing"
)

// auditConfig returns a config in which every path RequiredLists names carries
// rigs.
func auditConfig(rigs ...string) map[string]any {
	names := make([]any, len(rigs))
	for i, rig := range rigs {
		names[i] = rig
	}
	return map[string]any{"daemon": map[string]any{"patrols": map[string]any{
		"landing_worker": map[string]any{"rigs": names},
		"patrol_scan": map[string]any{
			"rigs":             names,
			"worktree_cleanup": map[string]any{"rigs": names},
		},
	}}}
}

// loadedSettings is a rig's settings with both merge_queue keys set.
func loadedSettings(string) (map[string]any, error) {
	return map[string]any{"merge_queue": map[string]any{"forgejo": "forgejo", "post_land_command": "make test-slow"}}, nil
}

// TestReport checks the audit's printed lines and exit code: a clean town, a
// required problem, an info-only gap, and the inputs Report refuses before any
// check.
func TestReport(t *testing.T) {
	t.Parallel()

	missingLanding := auditConfig("a", "b")
	missingLanding["daemon"].(map[string]any)["patrols"].(map[string]any)["landing_worker"] = map[string]any{"rigs": []any{"a"}}

	withInfo := auditConfig("a", "b")
	withInfo["daemon"].(map[string]any)["digest"] = map[string]any{"rigs": []any{"a"}}

	noLoad := func(string) (map[string]any, error) {
		return nil, errors.New("a refused input must not load settings")
	}

	tests := []struct {
		name     string
		in       Input
		wantOut  string
		wantCode int
	}{
		{"clean", Input{Unparked: []string{"a", "b"}, Config: auditConfig("a", "b"), LoadSettings: loadedSettings},
			"RIG AUDIT: OK (2 unparked, 0 parked: none; 0 info)\n", 0},
		{"required problem", Input{Unparked: []string{"a", "b"}, Config: missingLanding, LoadSettings: loadedSettings},
			"required  daemon.patrols.landing_worker.rigs lacks b\nRIG AUDIT: 1 problem(s) (2 unparked, 0 parked: none; 0 info)\n", 1},
		{"info only", Input{Unparked: []string{"a", "b"}, Config: withInfo, LoadSettings: loadedSettings},
			"info      daemon.digest.rigs lacks b\nRIG AUDIT: OK (2 unparked, 0 parked: none; 1 info)\n", 0},
		{"no rigs", Input{Config: auditConfig("a"), LoadSettings: noLoad},
			"RIG AUDIT: FAILED: no rigs in the registry\n", 1},
		{"nil config", Input{Unparked: []string{"a"}, LoadSettings: noLoad},
			"RIG AUDIT: FAILED: the town config is empty or unreadable\n", 1},
		{"no rigs and nil config", Input{},
			"RIG AUDIT: FAILED: no rigs in the registry\n", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			if code := Report(&out, tc.in); code != tc.wantCode || out.String() != tc.wantOut {
				t.Errorf("Report() = %d, %q; want %d, %q", code, out.String(), tc.wantCode, tc.wantOut)
			}
		})
	}
}
