package dashboard

import (
	"reflect"
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
		{"polecatsnote", "left", "tiersweepnote", ""},
		{"machnote", "right", "", "cloudnote"},
		{"cloudnote", "right", "machnote", "forgejonote"},
		{"forgejonote", "right", "cloudnote", "omnote"},
		{"omnote", "right", "forgejonote", "jevnote"},
		{"jevnote", "right", "omnote", "mangonote"},
		{"mangonote", "right", "jevnote", ""},
		{"beadsnote", "right", "mangonote", ""},
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

// The activity Feed is gone from the page: no feed or chips element and no fold
// under the feed's key (gt-069f5). trendsec is the positive control: a lookup
// that found nothing would pass the absence checks.
func TestNoFeedPanel(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	if got := pageExpr(t, dom, `document.getElementById("trendsec") !== null`); got != "true" {
		t.Fatal("the stub's element lookup finds no #trendsec, so the absence checks below prove nothing")
	}
	for _, id := range []string{"feed", "chips"} {
		if got := pageExpr(t, dom, `document.getElementById(`+quoteJS(id)+`) === null`); got != "true" {
			t.Errorf("the page still has #%s", id)
		}
	}
	for _, key := range dom.foldKeys(t) {
		if key == "feed" {
			t.Error("the page still folds a section under the key \"feed\"")
		}
	}
}

// The entry frame is still a sign the stream is alive: a stream that carries
// only entry frames restarts the watchdog's clock and is never replaced. The
// backlog frame is covered by TestANamedEventRestartsTheWatchdog (gt-069f5).
func TestEntryFramesRestartTheWatchdog(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	advanceTimers(t, dom, 89000)
	deliverStream(t, dom, "entry", "{}")

	// Without the entry frame's reset the stream is replaced at 90 s from load,
	// which falls inside this advance.
	advanceTimers(t, dom, 2000)
	if got := pageStreams(t, dom); got != 1 {
		t.Fatalf("the page had %d streams after an entry frame 89 s in, want the replacement clock reset", got)
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

	// A save that names only the dropped sections reads every section the way a
	// reader with no saved choice does.
	onlyDropped := newPageDOMStoring(t, map[string]string{"gt-fold": `{"feed":false,"overseer":false}`})
	fresh := newPageDOM(t)
	for _, key := range fresh.foldKeys(t) {
		if got, want := onlyDropped.fold(t, key), fresh.fold(t, key); !reflect.DeepEqual(got, want) {
			t.Errorf("the %s section reads %+v after a save naming only dropped sections, want %+v", key, got, want)
		}
	}
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
