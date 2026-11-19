package taskmap

import (
	"strings"
	"testing"
)

// TestTownRunnerRollUsePointsAtDryRun: the roll's steps live in the command's
// --dry-run, so its Use names the situation and points there instead of
// restating them (gt-lt2wmc).
func TestTownRunnerRollUsePointsAtDryRun(t *testing.T) {
	t.Parallel()
	e, ok := Lookup("town runner roll")
	if !ok {
		t.Fatal("town runner roll has no task map entry")
	}
	if !strings.HasPrefix(e.Use, "Use this when") {
		t.Errorf("Use = %q; want a line starting \"Use this when\"", e.Use)
	}
	if !strings.Contains(e.Use, "--dry-run") {
		t.Errorf("Use = %q; want it to point at --dry-run for the steps", e.Use)
	}
	for _, step := range []string{"hold", "wait", "apply"} {
		if strings.Contains(e.Use, step) {
			t.Errorf("Use = %q; it restates the %q step, but the steps belong to --dry-run", e.Use, step)
		}
	}
}
