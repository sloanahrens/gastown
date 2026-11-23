package dashboard

import (
	"testing"
)

// A partial roll — install and restart done, a later step not passed — is its
// own amber state, never the failed red, and its title names the step it did
// not pass, so six rolls that all swapped and restarted but failed verify do not
// read like a roll that never installed (gt-c8yqh9). These tests drive the
// pane's own render function and read back the chip it drew.

// rollChipAt is the roll chip's place in one row: the sha, the five stage chips
// before it (post-land, shell, integration, race, images), the pair chip, then
// the roll.
const rollChipAt = 7

// A partial roll is amber and names the step it missed, a failed roll keeps its
// red ✗ and plain title whatever step its record names, and a partial roll whose
// record names no step says the outcome word alone rather than a blank title.
func TestSystemUpdatesPageDrawsAPartialRollApartFromAFailedOne(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		roll  map[string]any
		class string
		text  string
		title string
	}{
		{"partial names the step it missed", map[string]any{"outcome": "partial", "step": "verify"}, "tag warnc", "roll ✗", "roll: partial at verify"},
		{"failed is unchanged", map[string]any{"outcome": "failed", "step": "install"}, "tag badc", "roll ✗", "roll: failed"},
		{"partial with no step says the outcome alone", map[string]any{"outcome": "partial"}, "tag warnc", "roll ✗", "roll: partial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dom := newPageDOM(t)
			dom.call(t, "renderSystemUpdates", systemUpdatesState(systemUpdatesSources{}, map[string]any{"sha": "abcdef123456", "roll": tc.roll}))
			chip := dom.descend(t, "systemupdates", 0, rollChipAt)
			if chip.Class != tc.class {
				t.Errorf("%s: roll chip wears %q, want %q", tc.name, chip.Class, tc.class)
			}
			if chip.Text != tc.text {
				t.Errorf("%s: roll chip reads %q, want %q", tc.name, chip.Text, tc.text)
			}
			if chip.Title != tc.title {
				t.Errorf("%s: roll chip is titled %q, want %q", tc.name, chip.Title, tc.title)
			}
		})
	}
}

// The state the pane folds a partial roll to is its own, distinct from the one
// an outcome it cannot place folds to, so the chip's colour is driven by the
// outcome rather than by a title that happens to say partial.
func TestSystemUpdatesPartialRollIsItsOwnStageState(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	for _, tc := range []struct {
		outcome, want string
	}{{"partial", "partial"}, {"failed", "failed"}, {"mystery", "failed"}} {
		expr := `systemUpdateStageState({roll: {outcome: ` + quoteJS(tc.outcome) + `}}, "roll")`
		if got := pageExpr(t, dom, expr); got != tc.want {
			t.Errorf("a %q roll folds to stage state %q, want %q", tc.outcome, got, tc.want)
		}
	}
}
