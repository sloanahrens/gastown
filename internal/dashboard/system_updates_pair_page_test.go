package dashboard

import (
	"strings"
	"testing"
)

// The pair chip (gt-jug4e0) reads the row's own Pair, the state the hub
// classified from the attention queue and the images runs (gt-clorl0). These
// tests drive the pane's own render function and read back the chip it drew.

// pairChipAt is the pair chip's place in one row: the sha, the five stage chips
// before it (post-land, shell, integration, race, images), then the pair chip,
// then the roll.
const pairChipAt = 6

// pairChipRow is one commit row whose pair state is state, with every stage
// absent so the pair chip is the only one a test reads.
func pairChipRow(pair any) map[string]any {
	row := map[string]any{"sha": "abcdef123456"}
	if pair != nil {
		row["pair"] = pair
	}
	return row
}

// A ready pair waits amber and says so, a building pair wears amber and the
// running mark, a failed, refused or stuck pair is red, and an absent pair is
// grey. A Pair the page does not know, including one the row did not fill, is
// grey with a question mark rather than grey like an absent one: a reading
// nobody made must not read as a commit with no pair.
func TestSystemUpdatesPairChip(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		pair  any
		class string
		text  string
		title string
	}{
		{"ready waits for its roll", "ready", "warnc", "pair waiting for roll", "pair: ready"},
		{"building shows the running mark", "building", "warnc", "pair ◌", "pair: building"},
		{"failed is red", "failed", "badc", "pair ✗", "pair: failed"},
		{"refused is red", "refused", "badc", "pair ✗", "pair: refused"},
		{"stuck is red", "stuck", "badc", "pair ✗", "pair: stuck"},
		{"unknown is grey with a question mark", "unknown", "absent", "pair ?", "pair: unknown"},
		{"absent is grey", "absent", "absent", "pair –", "pair: absent"},
		{"a row that filled no pair is unknown", nil, "absent", "pair ?", "pair: unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dom := newPageDOM(t)
			dom.call(t, "renderSystemUpdates", systemUpdatesState(systemUpdatesSources{}, pairChipRow(tc.pair)))
			chip := dom.descend(t, "systemupdates", 0, pairChipAt)
			if got, want := chip.Class, "tag "+tc.class; got != want {
				t.Errorf("pair %v wears %q, want %q", tc.pair, got, want)
			}
			if got := chip.Text; got != tc.text {
				t.Errorf("pair %v reads %q, want %q", tc.pair, got, tc.text)
			}
			if got := chip.Title; got != tc.title {
				t.Errorf("pair %v is titled %q, want %q", tc.pair, got, tc.title)
			}
			if got := dom.descend(t, "systemupdates", 0, pairChipAt-1).Text; !strings.HasPrefix(got, "images ") {
				t.Errorf("the chip before pair reads %q, want the images chip", got)
			}
			if got := dom.descend(t, "systemupdates", 0, pairChipAt+1).Text; !strings.HasPrefix(got, "roll ") {
				t.Errorf("the chip after pair reads %q, want the roll chip", got)
			}
		})
	}
}

// pairHeaderRow is a commit whose every stage passed, with the pair set to
// pair, so a header test can read the one chip the icon took its colour from.
func pairHeaderRow(pair string) map[string]any {
	row := systemUpdateGoodRow()
	row["pair"] = pair
	return row
}

// A commit that already rolled keeps the pair the hub classified for it: the
// roll's own success does not overwrite a refused pair, because the pair says
// whether the images were ever fit to roll, not whether one did.
func TestSystemUpdatesPairChipSurvivesARoll(t *testing.T) {
	t.Parallel()

	row := systemUpdateGoodRow()
	row["pair"] = "refused"
	dom := newPageDOM(t)
	dom.call(t, "renderSystemUpdates", systemUpdatesState(systemUpdatesSources{}, row))
	if chip := dom.descend(t, "systemupdates", 0, pairChipAt); chip.Class != "tag badc" || chip.Text != "pair ✗" {
		t.Errorf("a rolled commit draws its pair as %q %q, want the refused pair", chip.Class, chip.Text)
	}
}

// The pane header's icon is the worst chip the pane drew, so a red pair chip
// turns it red even when every stage passed, and a ready or building pair turns
// it amber. A pair that is absent leaves it the muted dash rather than a tick:
// grey is nothing read, not something done.
func TestSystemUpdatesPairHeader(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		pair  string
		class string
		glyph string
	}{
		{"refused is red", "refused", "badc", "✗"},
		{"failed is red", "failed", "badc", "✗"},
		{"stuck is red", "stuck", "badc", "✗"},
		{"ready is amber", "ready", "warnc", "◌"},
		{"building is amber", "building", "warnc", "◌"},
		{"absent stays grey", "absent", "", "–"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dom := newPageDOM(t)
			dom.call(t, "renderSystemUpdates", systemUpdatesState(systemUpdatesSources{}, pairHeaderRow(tc.pair)))
			ico := dom.element(t, "systemupdatesico")
			wantClass := "suico"
			if tc.class != "" {
				wantClass += " " + tc.class
			}
			if ico.Class != wantClass {
				t.Errorf("pair %s: header icon wears %q, want %q", tc.pair, ico.Class, wantClass)
			}
			if ico.Text != tc.glyph {
				t.Errorf("pair %s: header icon reads %q, want %q", tc.pair, ico.Text, tc.glyph)
			}
			if tc.class != "" && !strings.Contains(ico.Title, "pair") {
				t.Errorf("pair %s: header icon title %q does not name the pair that set it", tc.pair, ico.Title)
			}
		})
	}
}

// With no rows the header icon is repainted as nothing to read, so a red icon
// left by the last reading is cleared rather than standing for an empty pane.
func TestSystemUpdatesPairHeaderClearsWithNoRows(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderSystemUpdates", systemUpdatesState(systemUpdatesSources{}, pairHeaderRow("refused")))
	dom.call(t, "renderSystemUpdates", systemUpdatesState(systemUpdatesSources{}))
	ico := dom.element(t, "systemupdatesico")
	if ico.Class != "suico" || ico.Text != "–" {
		t.Errorf("an empty pane's header icon wears %q reading %q, want the muted dash", ico.Class, ico.Text)
	}
}
