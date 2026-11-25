package cmd

import (
	"strings"
	"testing"
	"time"
)

// omTestReusedLiteral is the stage line a reused landing logs. The writer's
// side is pinned by internal/land's own test (TestVerdictReuseSuffixForm), which
// asserts the same suffix text.
const omTestReusedLiteral = "stages: ci 1m32s, om reused (2m31s, 14m0s old)"

func TestReviewPoolStageReused(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		stages string
		cost   time.Duration
		age    time.Duration
		ok     bool
	}{
		{"reused form parses cost and age", "ci 1m32s, om reused (2m31s, 14m0s old)", 2*time.Minute + 31*time.Second, 14 * time.Minute, true},
		{"zero age", "ci 1s, om reused (2m31s, 0s old)", 2*time.Minute + 31*time.Second, 0, true},
		{"age over an hour", "ci 1s, om reused (2m31s, 1h2m3s old)", 2*time.Minute + 31*time.Second, time.Hour + 2*time.Minute + 3*time.Second, true},
		{"plain om line", "lint 3s, gate 1m, om 20s", 0, 0, false},
		{"waiver form", "ci 1m32s, om skipped (overseer-reviewed)", 0, 0, false},
		{"skipped gate with om time", "ci skipped (om rejected), om 12s", 0, 0, false},
		{"suffix with a bad age duration", "ci 1m32s, om reused (2m31s, soon old)", 0, 0, false},
		{"suffix with a bad cost duration", "ci 1m32s, om reused (later, 14m0s old)", 0, 0, false},
		{"negative age", "ci 1s, om reused (2m31s, -5m0s old)", 0, 0, false},
		{"negative cost", "ci 1s, om reused (-2m31s, 14m0s old)", 0, 0, false},
		{"age without the old word", "ci 1s, om reused (2m31s, 14m0s)", 0, 0, false},
		{"cost missing", "ci 1s, om reused (14m0s old)", 0, 0, false},
		{"text after the closing paren", "ci 1s, om reused (2m31s, 14m0s old) extra", 0, 0, false},
		{"unclosed paren", "ci 1s, om reused (2m31s, 14m0s old", 0, 0, false},
		{"suffix with no stage before it", "om reused (2m31s, 14m0s old)", 0, 0, false},
		{"empty stages", "", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cost, age, ok := omStageReused(tc.stages)
			if ok != tc.ok || cost != tc.cost || age != tc.age {
				t.Fatalf("omStageReused(%q) = (%v, %v, %v), want (%v, %v, %v)",
					tc.stages, cost, age, ok, tc.cost, tc.age, tc.ok)
			}
		})
	}
}

func TestReviewPoolStageReusedLiteral(t *testing.T) {
	t.Parallel()
	_, stages, found := strings.Cut(omTestReusedLiteral, "stages: ")
	if !found {
		t.Fatalf("literal %q has no stages prefix", omTestReusedLiteral)
	}
	cost, age, ok := omStageReused(stages)
	if !ok {
		t.Fatalf("omStageReused(%q) not ok", stages)
	}
	if want := 2*time.Minute + 31*time.Second; cost != want {
		t.Errorf("cost = %v, want %v", cost, want)
	}
	if want := 14 * time.Minute; age != want {
		t.Errorf("age = %v, want %v", age, want)
	}
}
