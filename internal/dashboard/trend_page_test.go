package dashboard

import (
	"strings"
	"testing"
)

// The Landings pane shows the beads waiting for the landing worker: a bead
// submitted with gt done used to appear only in the Queue pane's "waiting to
// land" tab, so a deep queue — several submitted at once — was invisible in the
// pane that watches landings, and someone watching it saw nothing between gt
// done and the worker's first stage (gt-zc45r).
//
// The rows come from the Queue reader's landing list, already in the state, so
// the pane adds no second read of the stores; and a bead the worker has already
// taken has a live row in the trend, so it is dropped here — the live row wins.
func TestLandingsPaneShowsQueuedLandingsAboveTheLiveRows(t *testing.T) {
	t.Parallel()

	queued := pageFunc(t, "queuedLandings")
	for _, want := range []string{
		`const waiting = (s.queue && s.queue.landing) || [];`,
		`const taken = new Set((rows || []).map(r => r.bead));`,
		`const since = b => b.updated_at ? new Date(b.updated_at).getTime() : 0;`,
		`return waiting.filter(b => !taken.has(b.id)).sort((a, b) => since(a) - since(b));`,
	} {
		if !strings.Contains(queued, want) {
			t.Errorf("queuedLandings has no %q", want)
		}
	}

	// The waiting rows are built before the live and finished ones, so they ride
	// at the top of the table.
	tbl := pageFunc(t, "landingTable")
	waiting, live := strings.Index(tbl, "for (const b of shown)"), strings.Index(tbl, "for (const r of rows)")
	if waiting < 0 || live < 0 || waiting > live {
		t.Error("the queued rows are not drawn above the live and finished ones")
	}
	for _, want := range []string{
		"const shown = waiting.slice(0, QUEUED_LANDING_MAX);",
		`const tag = el("span", "tag", "queued");`,
	} {
		if !strings.Contains(tbl, want) {
			t.Errorf("landingTable has no %q", want)
		}
	}
}

// A queued row says how long the bead has waited, aged from its last update:
// the label write gt done makes is normally the bead's last write. The order the
// worker will take the beads in is not known to the page, so a row says nothing
// about position (gt-zc45r).
func TestQueuedLandingRowAgesTheWaitFromTheBeadsLastUpdate(t *testing.T) {
	t.Parallel()

	tbl := pageFunc(t, "landingTable")
	for _, want := range []string{
		`el("td", "title", b.updated_at ? "waiting " + age(b.updated_at) : "waiting")`,
		`when.title = "waiting since the bead's last update, which gt done moves";`,
	} {
		if !strings.Contains(tbl, want) {
			t.Errorf("the queued row has no %q", want)
		}
	}

	// The holder is the bead's assignee: a polecat named by its store is shown
	// the way the By column shows a holder, and any other assignee as written.
	holder := pageFunc(t, "queueHolder")
	if want := `const m = /^([^/]+)\/polecats\/([^/]+)$/.exec(b.assignee || "");`; !strings.Contains(holder, want) {
		t.Errorf("queueHolder has no %q", want)
	}
	if want := `by.textContent = names.label(h.rig, h.name);`; !strings.Contains(tbl, want) {
		t.Errorf("a queued row's holder is not named through the shared helper: %s", want)
	}
}

// The queued rows are capped at ten — dequeued beads are the point of the pane,
// not a wall of them — and the rest are counted with a pointer to the tab that
// holds them rather than dropped silently (gt-zc45r).
func TestQueuedLandingsAreCappedWithAPointerToTheQueuePane(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	for _, want := range []string{
		"const QUEUED_LANDING_MAX = 10;",
		"const more = waiting.length - shown.length;",
		`if (more) wrap.append(el("div", "empty", "and " + more + " more waiting to land — see the Queue pane's “Waiting to land” tab"));`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html has no %q", want)
		}
	}
}

// A bead's own text stays text: the id, the title and the store all go through
// el(), which writes textContent, so a title holding markup shows as that
// markup's text and never as markup (gt-zc45r).
func TestQueuedLandingsDrawEveryCellAsText(t *testing.T) {
	t.Parallel()

	tbl := pageFunc(t, "landingTable")
	for _, want := range []string{
		`main.append(el("span", "bead", b.id));`,
		`if (b.title) main.append(document.createElement("br"), el("span", "title", (b.rig ? b.rig + " · " : "") + b.title));`,
	} {
		if !strings.Contains(tbl, want) {
			t.Errorf("a queued row does not draw %q as text", want)
		}
	}
	if strings.Contains(tbl, "innerHTML") {
		t.Error("the landings table builds markup instead of text")
	}
}

// A queue of beads waiting to land is reason enough to show the section: with no
// landings in the window the pane is the queue alone rather than the day's
// figures drawn from a reading nobody has made yet, and with neither the section
// stays hidden as it was (gt-zc45r).
func TestLandingsSectionShowsForAQueueAlone(t *testing.T) {
	t.Parallel()

	trend := pageFunc(t, "renderTrend")
	for _, want := range []string{
		"const hours = (t && t.hours) || [];",
		"const waiting = queuedLandings(s, recent);",
		"if (!hours.length && !waiting.length) { sec.hidden = true; box.replaceChildren(); return; }",
		"if (!hours.length) {",
		"const q = landingTable(s, names, recent, waiting);",
	} {
		if !strings.Contains(trend, want) {
			t.Errorf("renderTrend has no %q", want)
		}
	}
}
