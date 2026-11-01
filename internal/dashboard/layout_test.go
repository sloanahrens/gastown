package dashboard

import (
	"strings"
	"testing"
)

// The Work panel wants to sit under the tier sweeps, the landing history
// beside om review, the feed under the Work panel at the foot of the left
// column, and the tier sweeps beside the rigs they sweep, so the panes are
// placed by column in index.html rather than by data. Reading the embedded
// page is what keeps a section that drifts back to the wrong column from
// passing (gt-9bf2m, gt-fn9e6.48).
func TestPaneColumns(t *testing.T) {
	t.Parallel()

	cols := strings.Split(string(indexHTML), `<div class="col">`)
	if len(cols) != 3 {
		t.Fatalf("index.html has %d columns, want 2", len(cols)-1)
	}
	left, right := cols[1], cols[2]

	for _, tc := range []struct {
		pane, col, before, after string
	}{
		{"attentionnote", "left", "", "costnote"},
		{"costnote", "left", "attentionnote", "rigsnote"},
		{"rigsnote", "left", "costnote", "tiersweepnote"},
		{"tiersweepnote", "left", "rigsnote", ""},
		{"machnote", "right", "", "cloudnote"},
		{"cloudnote", "right", "machnote", "forgejonote"},
		{"forgejonote", "right", "cloudnote", "omnote"},
		{"omnote", "right", "forgejonote", "jevnote"},
		{"jevnote", "right", "omnote", "mangonote"},
		{"mangonote", "right", "jevnote", ""},
	} {
		got := left
		other := right
		if tc.col == "right" {
			got, other = right, left
		}
		at := strings.Index(got, `id="`+tc.pane+`"`)
		if at < 0 {
			t.Errorf("%s is not in the %s column", tc.pane, tc.col)
			continue
		}
		if strings.Contains(other, `id="`+tc.pane+`"`) {
			t.Errorf("%s is also in the other column", tc.pane)
		}
		if tc.before != "" && strings.Index(got, `id="`+tc.before+`"`) > at {
			t.Errorf("%s does not follow %s in the %s column", tc.pane, tc.before, tc.col)
		}
		if tc.after != "" && at > strings.Index(got, `id="`+tc.after+`"`) {
			t.Errorf("%s does not precede %s in the %s column", tc.pane, tc.after, tc.col)
		}
	}

	if !strings.Contains(string(indexHTML), `<section id="trendsec" class="wide" data-fold="landings" hidden>`) {
		t.Error("the Landings section lost its hidden attribute")
	}
}

// The activity Feed is gone from the page: no section, no ids, no script that
// draws it, and no feed-only styling (gt-069f5). The hub's frames for it are
// still sent and still count as a sign the stream is alive.
func TestNoFeedPanel(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	if got := strings.Count(page, `data-fold="feed"`); got != 0 {
		t.Errorf("the page has %d data-fold=\"feed\" sections, want 0", got)
	}
	for _, gone := range []string{`id="feed"`, `id="chips"`, `$("feed")`, `$("chips")`, "function pushEntry", "function renderChips", "function rowFor", "function applyFilter", `"gt-chips"`, "#feed{"} {
		if strings.Contains(page, gone) {
			t.Errorf("the page still holds %q", gone)
		}
	}
	for _, sp := range pageSections(t) {
		if sp.Key == "feed" {
			t.Error("the page still folds a section under the key \"feed\"")
		}
	}

	// The backlog and entry frames are still liveness: they refresh the stream's
	// last-seen time and draw nothing.
	if !strings.Contains(page, `["backlog", "entry"]`) {
		t.Fatal("the page no longer listens for the backlog and entry frames, so a stream that only carries them reads as silent")
	}
}

// A reader's saved folds may still name sections the page has dropped (feed,
// overseer): they are ignored, and the sections that remain keep their saved
// choice and their defaults (gt-069f5).
func TestSavedFoldsNamingRemovedSectionsAreIgnored(t *testing.T) {
	t.Parallel()

	dom := newPageDOMStoring(t, map[string]string{"gt-fold": `{"feed":false,"overseer":false,"landings":true,"birds":false}`})
	wantFold(t, dom.fold(t, "landings"), "landings", true)
	wantFold(t, dom.fold(t, "birds"), "birds", false)
	wantFold(t, dom.fold(t, "rigs"), "rigs", true)
}

// Jev and Mango sit in the right-hand column directly under om review, Jev first
// and Mango after it, and keep their hidden and collapsed-by-default behaviour
// (gt-069f5).
func TestJevAndMangoSitUnderOMReview(t *testing.T) {
	t.Parallel()

	cols := strings.Split(string(indexHTML), `<div class="col">`)
	if len(cols) != 3 {
		t.Fatalf("index.html has %d columns, want 2", len(cols)-1)
	}
	right := cols[2]
	at := func(needle string) int { return strings.Index(right, needle) }
	om, jev, mango := at(`data-fold="om"`), at(`id="jevsec"`), at(`id="mangosec"`)
	if om < 0 || jev < 0 || mango < 0 {
		t.Fatalf("the right column holds om review at %d, Jev at %d and Mango at %d, want all three", om, jev, mango)
	}
	if !(om < jev && jev < mango) {
		t.Errorf("the right column orders om review (%d), Jev (%d), Mango (%d), want om then Jev then Mango", om, jev, mango)
	}
	if strings.Contains(cols[1], `id="jevsec"`) || strings.Contains(cols[1], `id="mangosec"`) {
		t.Error("Jev or Mango is still in the left column too")
	}
	for _, id := range []string{"jevsec", "mangosec"} {
		tag := right[at(`id="`+id+`"`):]
		tag = tag[:strings.Index(tag, ">")]
		if !strings.Contains(tag, "data-default-collapsed") || !strings.Contains(tag, " hidden") {
			t.Errorf("%s lost its collapsed-by-default or hidden attribute: %s", id, tag)
		}
	}
}
