package tiertrigger

import "testing"

func TestRuns(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		triggers []string
		changed  []string
		daily    bool
		want     bool
	}{
		{name: "a change in the package directory", triggers: []string{"internal/beads"}, changed: []string{"internal/beads/beads.go"}, want: true},
		{name: "a change below it", triggers: []string{"internal/beads"}, changed: []string{"internal/beads/store/schema.go"}, want: true},
		{name: "an exact file trigger", triggers: []string{"go.mod"}, changed: []string{"go.mod"}, want: true},
		{name: "a sibling whose name starts with the trigger", triggers: []string{"internal/beads"}, changed: []string{"internal/beadsx/beads.go"}, want: false},
		{name: "a file beside the triggered directory", triggers: []string{"internal/beads"}, changed: []string{"internal/beads.go"}, want: false},
		{name: "a change elsewhere", triggers: []string{"internal/beads"}, changed: []string{"internal/cmd/root.go"}, want: false},
		{name: "the same suffix in another tree", triggers: []string{"internal/beads"}, changed: []string{"tracker/internal/beads/beads.go"}, want: false},
		{name: "one trigger of several hit", triggers: []string{"internal/beads", "internal/testutil"}, changed: []string{"internal/testutil/dolt.go"}, want: true},
		{name: "no change list", triggers: []string{"internal/beads"}, changed: nil, want: false},
		{name: "no change list on a daily run", triggers: []string{"internal/beads"}, changed: nil, daily: true, want: true},
		{name: "a daily run whatever changed", triggers: []string{"internal/beads"}, changed: []string{"internal/cmd/root.go"}, daily: true, want: true},
		{name: "no triggers", changed: []string{"internal/beads/beads.go"}, want: false},
		{name: "./ spelling on the trigger", triggers: []string{"./internal/beads"}, changed: []string{"internal/beads/beads.go"}, want: true},
		{name: "./ spelling on the change", triggers: []string{"internal/beads"}, changed: []string{"./internal/beads/beads.go"}, want: true},
		{name: "trailing slash on the trigger", triggers: []string{"internal/beads/"}, changed: []string{"internal/beads/beads.go"}, want: true},
		{name: "a doubled trailing slash", triggers: []string{"internal/beads//"}, changed: []string{"internal/beads/beads.go"}, want: true},
		{name: "blank changed entries match nothing", triggers: []string{"internal/beads"}, changed: []string{"", "internal/cmd/root.go"}, want: false},
		{name: "a blank trigger runs the package", triggers: []string{"", "internal/beads/"}, changed: []string{"internal/cmd/root.go"}, want: true},
		{name: "an absolute trigger runs the package", triggers: []string{"/internal/beads"}, changed: []string{"internal/cmd/root.go"}, want: true},
		{name: "a trigger above the root runs the package", triggers: []string{"../internal/beads"}, changed: []string{"internal/cmd/root.go"}, want: true},
		{name: "a trigger that climbs above the root runs the package", triggers: []string{"internal/../.."}, changed: []string{"internal/cmd/root.go"}, want: true},
		{name: "an interior ./ on the trigger", triggers: []string{"internal/./beads"}, changed: []string{"internal/beads/beads.go"}, want: true},
		{name: "an interior .. on the trigger", triggers: []string{"internal/testutil/../beads"}, changed: []string{"internal/beads/beads.go"}, want: true},
		{name: "an interior ./ on the change", triggers: []string{"internal/beads"}, changed: []string{"internal/./beads/beads.go"}, want: true},
		{name: "an interior .. on the change", triggers: []string{"internal/beads"}, changed: []string{"internal/cmd/../beads/beads.go"}, want: true},
		{name: "an interior .. that leaves the trigger", triggers: []string{"internal/beads"}, changed: []string{"internal/testutil/../beadsx/beads.go"}, want: false},
		{name: "the repo root trigger", triggers: []string{"."}, changed: []string{"internal/cmd/root.go"}, want: true},
		{name: "the repo root as ./", triggers: []string{"./"}, changed: []string{"internal/cmd/root.go"}, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := Runs(c.triggers, c.changed, c.daily); got != c.want {
				t.Errorf("Runs(%q, %q, %v) = %v, want %v", c.triggers, c.changed, c.daily, got, c.want)
			}
		})
	}
}
