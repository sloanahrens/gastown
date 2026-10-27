package land

import "testing"

// A submission is the human's only when its actor is the whole identity the
// town records for them: an agent whose name merely contains it is an agent
// (gt-2ge3z).
func TestIsHumanActor(t *testing.T) {
	t.Parallel()
	const human = "Sloan Ahrens"
	for _, tc := range []struct {
		name, actor, human string
		want               bool
	}{
		{"the human's own identity", "Sloan Ahrens", human, true},
		{"the identity with surrounding space", "  Sloan Ahrens\t", human, true},
		{"an agent whose name contains the identity", "Sloan Ahrens (agent)", human, false},
		{"an agent whose name contains half of it", "sloan-runner", human, false},
		{"the identity in another case", "sloan ahrens", human, false},
		{"another person", "Ada Lovelace", human, false},
		{"no actor", "", human, false},
		{"no human identity configured", "Sloan Ahrens", "", false},
		{"no human identity and no actor", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsHumanActor(tc.actor, tc.human); got != tc.want {
				t.Errorf("IsHumanActor(%q, %q) = %v, want %v", tc.actor, tc.human, got, tc.want)
			}
		})
	}
}
