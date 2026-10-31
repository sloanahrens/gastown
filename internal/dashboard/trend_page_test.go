package dashboard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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

// A crew member's claim is "town/crew/name" or "rig/crew/name" — claude-agent
// sets the actor that way — and the By column shows the bare name, as it does
// for a polecat; any other assignee is shown as it was written. The page draws
// the rows here rather than being read as source, so the cell the By column
// ends up holding is what decides (gt-zc45r, gt-t76l2).
func TestAQueuedRowNamesACrewAssigneeByBareName(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderTrend", map[string]any{
		"queue": map[string]any{"landing": []any{
			map[string]any{"id": "gt-crew", "assignee": "town/crew/heron"},
			map[string]any{"id": "gt-rig", "assignee": "gastown/crew/jay"},
			map[string]any{"id": "gt-polecat", "assignee": "gastown/polecats/dag"},
			map[string]any{"id": "gt-other", "assignee": "sloan"},
		}},
	})

	// #trend holds the landings wrap alone with nothing landed in the window:
	// its caption, then the table, whose first row is the header.
	for i, want := range []struct{ bead, by, title string }{
		{"gt-crew", "heron", "heron"},
		{"gt-rig", "jay", "jay"},
		{"gt-polecat", "dag", "gastown/dag"},
		{"gt-other", "sloan", "sloan"},
	} {
		row := i + 1
		if main := dom.descend(t, "trend", 0, 1, 0, row, 1).Text; !strings.Contains(main, want.bead) {
			t.Fatalf("row %d is the bead %q, want %s", row, main, want.bead)
		}
		by := dom.descend(t, "trend", 0, 1, 0, row, 2)
		if by.Text != want.by || by.Title != want.title {
			t.Errorf("%s reads %q (%q) in the By column, want %q (%q)", want.bead, by.Text, by.Title, want.by, want.title)
		}
	}
}

// The By column is still the column that says who built a landing, heading and
// hover alike, so the name a rejected or failing row now carries reads as that
// landing's submitter rather than as a new kind of column (gt-gclus).
func TestTheLandingsByColumnStillSaysWhoBuiltIt(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 26, 20, 30, 0, 0, time.Local)
	dom := newPageDOM(t)
	dom.call(t, "renderTrend", map[string]any{"trend": map[string]any{
		"hours": []map[string]any{{"hour": now.Truncate(time.Hour).Format(time.RFC3339), "landed": 1, "rejected": 1}},
		"recent": []map[string]any{{
			"at": now.Format(time.RFC3339), "bead": "gt-rejected", "rig": "gastown",
			"outcome": "rejected", "kind": "review", "polecat": "agate",
		}},
	}})

	// The table is found by its class rather than by its index: the day's
	// figures sit above it in the same box, and a column that moves must not
	// move the expectation with it.
	v, err := dom.vm.RunString(`JSON.stringify((() => {
  let tbl = null;
  (function walk(n) {
    if (tbl) return;
    if (n.className === "ltable") { tbl = n; return; }
    for (const c of (n.children || [])) walk(c);
  })(document.getElementById("trend"));
  if (!tbl) return null;
  const head = tbl.children[0].children[2], cell = tbl.children[1].children[2];
  return {head: head.textContent, tip: head.title, cell: cell.textContent};
})())`)
	if err != nil {
		t.Fatalf("reading the drawn landings table: %v", err)
	}
	var got struct {
		Head string `json:"head"`
		Tip  string `json:"tip"`
		Cell string `json:"cell"`
	}
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("decoding the drawn table: %v", err)
	}
	if got.Head != "By" || !strings.Contains(got.Tip, "who built it") {
		t.Errorf("the By heading reads %q (%q), want the column that says who built it", got.Head, got.Tip)
	}
	if got.Cell != "agate" {
		t.Errorf("the rejected row's By cell reads %q, want the submitter the server named", got.Cell)
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

// The Ship cell says what the number means per rig, and says something honest
// when there is no number: a staging deploy that failed reads as its own
// warning, a landing waiting on staging says so rather than waiting on a
// restart that never comes, and a rig with no ship definition shows a dash
// (gt-lqqjj).
func TestLandingsShipCellNamesWhatShipsEachRig(t *testing.T) {
	t.Parallel()

	tbl := pageFunc(t, "landingTable")
	for _, want := range []string{
		`const appRig = r.ship_via === "staging";`,
		`ship.title = appRig ? "dispatched to staging deployed" : "dispatched to deployed";`,
		`else if (r.ship_failed) { ship.textContent = "staging failed"; ship.title = "the staging run that covers this landing failed: " + (r.ship_run_state || "failed"); }`,
		`else if (r.ship_pending) { ship.textContent = "pending"; ship.title = appRig ? "landed; waiting for the staging deploy" : "landed; waiting for the restart that installs it"; }`,
		`else ship.textContent = "–";`,
	} {
		if !strings.Contains(tbl, want) {
			t.Errorf("landingTable has no %q", want)
		}
	}

	// The failed cell is drawn before the plain pending one, so a failed
	// staging run is never overwritten by "pending".
	if failed, pending := strings.Index(tbl, "r.ship_failed"), strings.Index(tbl, `ship.textContent = "pending"`); failed < 0 || pending < 0 || failed > pending {
		t.Error("the staging-failed cell is not drawn before the pending one")
	}
	if strings.Contains(tbl, "innerHTML") {
		t.Error("the landings table builds markup instead of text")
	}
}

// The Polecat column reads a landing's polecat time in seconds, minutes, or
// hours and minutes. A whole hour drops its empty minutes ("26h", not
// "26h00m"), a total that may understate the time carries a +, a positive
// reading under a second is "<1s" rather than a zero, and a landing with no
// reading — a crew landing, or one landed before the record kept it — is a
// dash (gt-90wi1).
func TestThePolecatTimeColumnFormatsItsReadings(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	for _, c := range []struct {
		secs  string // a JS expression, so the unknown reading can be null
		lower bool
		upper bool
		want  string
	}{
		{`null`, false, false, "–"},
		{`undefined`, false, false, "–"},
		{`0`, false, false, "–"},
		{`0`, true, false, "–"},
		{`0`, false, true, "–"},
		{`0.4`, false, false, "<1s"},
		{`0.6`, false, false, "<1s"},
		{`0.4`, true, false, "<1s+"},
		{`1`, false, false, "1s"},
		{`59`, false, false, "59s"},
		{`60`, false, false, "60s"},
		{`75`, false, false, "75s"},
		{`89`, false, false, "89s"},
		{`90`, false, false, "2m"},
		{`1380`, false, false, "23m"},
		{`3569.6`, false, false, "1h"},
		{`3570`, false, false, "1h"},
		{`3600`, false, false, "1h"},
		{`4320`, false, false, "1h12m"},
		{`5400`, false, false, "1h30m"},
		{`93600`, false, false, "26h"},
		{`1380`, true, false, "23m+"},
		// A reading from the bead's dispatch may overstate the seat's time, so
		// it is marked an upper bound rather than a floor (gt-v7ptp).
		{`1380`, false, true, "≤23m"},
		{`0.4`, false, true, "≤1s"},
	} {
		v, err := dom.vm.RunString(fmt.Sprintf("polecatDur(%s, %t, %t)", c.secs, c.lower, c.upper))
		if err != nil {
			t.Fatalf("polecatDur(%s, %t, %t): %v", c.secs, c.lower, c.upper, err)
		}
		if got := v.String(); got != c.want {
			t.Errorf("polecatDur(%s, %t, %t) = %q, want %q", c.secs, c.lower, c.upper, got, c.want)
		}
	}
}

// polecatCell is one row of the Landings table as the page drew it: the bead,
// and the Polecat cell's text and hover.
type polecatCell struct {
	Bead  string `json:"bead"`
	Text  string `json:"text"`
	Title string `json:"title"`
}

// drawnPolecatColumn renders the Landings table and reads its Polecat column
// back cell by cell, finding both columns by their heading so a column that
// moves does not move the expectation with it.
func drawnPolecatColumn(t *testing.T, dom *pageDOM) []polecatCell {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((() => {
  let tbl = null;
  (function walk(n) {
    if (tbl) return;
    if (n.className === "ltable") { tbl = n; return; }
    for (const c of (n.children || [])) walk(c);
  })(document.getElementById("trend"));
  if (!tbl) return [];
  const col = name => tbl.children[0].children.findIndex(th => th.textContent === name);
  const beadCol = col("Landing"), polecatCol = col("Polecat");
  if (beadCol < 0 || polecatCol < 0) throw new Error("the table has no Landing and Polecat headings");
  return tbl.children.slice(1).map(tr => ({
    bead: tr.children[beadCol].textContent,
    text: tr.children[polecatCol].textContent,
    title: tr.children[polecatCol].title,
  }));
})())`)
	if err != nil {
		t.Fatalf("reading the drawn landings table: %v", err)
	}
	var cells []polecatCell
	if err := json.Unmarshal([]byte(v.String()), &cells); err != nil {
		t.Fatalf("decoding the drawn table: %v", err)
	}
	return cells
}

// The Landings pane draws the polecat time in a column of its own, and lists
// the attempts that make it up on the cell's hover. A landing whose record
// carries no reading draws a dash, as a crew landing does; an attempt whose
// span never closed reads "open"; and an attempt that is itself a floor keeps
// its + in the hover, as the total does. The golden is the column written by
// hand from these fixtures, not from a run of the page (gt-90wi1).
func TestTheLandingsPaneDrawsThePolecatTimeColumn(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 26, 14, 0, 0, 0, time.Local)
	dom := newPageDOM(t)
	dom.call(t, "renderTrend", map[string]any{"trend": map[string]any{
		"hours": []map[string]any{{"hour": now.Format(time.RFC3339), "landed": 5}},
		"recent": []map[string]any{
			{
				"at": now.Add(-time.Minute).Format(time.RFC3339), "bead": "gt-attempts", "outcome": "landed",
				"polecat_secs": 1380,
				"polecat_attempts": []map[string]any{
					{"seat": "gastown/opal", "secs": 840},
					{"seat": "gastown/jade", "secs": 540},
				},
			},
			{
				"at": now.Add(-2 * time.Minute).Format(time.RFC3339), "bead": "gt-lower-attempts", "outcome": "landed",
				"polecat_secs": 1380, "polecat_lower_bound": true,
				"polecat_attempts": []map[string]any{
					{"seat": "gastown/opal", "secs": 840, "lower_bound": true},
					{"seat": "gastown/jade", "secs": 540},
				},
			},
			{
				// gt-p85pw.92's shape: no span start, so the record carries the
				// dispatch interval, marked an upper bound (gt-v7ptp).
				"at": now.Add(-2*time.Minute + 30*time.Second).Format(time.RFC3339), "bead": "gt-upper", "outcome": "landed",
				"polecat_secs": 1380, "polecat_upper_bound": true,
				"polecat_attempts": []map[string]any{
					{"seat": "gastown/morsov", "secs": 1380, "upper_bound": true},
				},
			},
			{"at": now.Add(-3 * time.Minute).Format(time.RFC3339), "bead": "gt-crew", "outcome": "landed"},
			{
				"at": now.Add(-4 * time.Minute).Format(time.RFC3339), "bead": "gt-open", "outcome": "landed",
				"polecat_lower_bound": true,
				"polecat_attempts":    []map[string]any{{"seat": "gastown/onyx", "secs": 0, "lower_bound": true}},
			},
			{
				"at": now.Add(-5 * time.Minute).Format(time.RFC3339), "bead": "gt-subsecond", "outcome": "landed",
				"polecat_secs": 0.4,
			},
		},
	}})

	got := drawnPolecatColumn(t, dom)
	raw, err := os.ReadFile(filepath.Join("testdata", "landings_polecat_column.golden"))
	if err != nil {
		t.Fatalf("reading the golden: %v", err)
	}
	var want []polecatCell
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("decoding the golden: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the drawn Polecat column:\n got %+v\nwant %+v", got, want)
	}
}

// A landing built on a bird or crew branch has no polecat, so its Polecat cell
// reads bird rather than a dash, which would read as missing data, and its
// tooltip says there is no polecat time (gt-kmp7c, gt-rxs7w). A polecat's
// landing with no reading keeps the dash.
func TestTheLandingsPaneReadsBirdInThePolecatCellOfACrewBranchRow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 29, 14, 0, 0, 0, time.Local)
	dom := newPageDOM(t)
	dom.call(t, "renderTrend", map[string]any{"trend": map[string]any{
		"hours": []map[string]any{{"hour": now.Format(time.RFC3339), "landed": 2}},
		"recent": []map[string]any{
			{"at": now.Add(-time.Minute).Format(time.RFC3339), "bead": "gt-crew", "outcome": "landed", "crew": true},
			{"at": now.Add(-2 * time.Minute).Format(time.RFC3339), "bead": "gt-polecat", "outcome": "landed"},
		},
	}})

	got := map[string]string{}
	titles := map[string]string{}
	for _, c := range drawnPolecatColumn(t, dom) {
		got[c.Bead] = c.Text
		titles[c.Bead] = c.Title
	}
	if got["gt-crew"] != "bird" {
		t.Errorf("crew branch row's Polecat cell reads %q, want bird", got["gt-crew"])
	}
	if titles["gt-crew"] != "bird or crew branch: no polecat time exists" {
		t.Errorf("crew branch row's Polecat cell tooltip reads %q, want the no-polecat-time reason", titles["gt-crew"])
	}
	if got["gt-polecat"] != "–" {
		t.Errorf("polecat row with no reading reads %q, want the dash", got["gt-polecat"])
	}
}

// A reading of a minute or more is still drawn in seconds while it stays under
// a minute and a half, then rounds to whole minutes; the band is fmtDur's own,
// so the Polecat column's format does not move it. Every value is asserted as
// drawn, so a change to the rounding fails on the value that changed
// (gt-eswib).
func TestThePolecatColumnDrawsTheSecondsBand(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 26, 14, 0, 0, 0, time.Local)
	recent := make([]map[string]any, 0, 5)
	for i, secs := range []float64{59, 60, 75, 89, 90} {
		recent = append(recent, map[string]any{
			"at":           now.Add(-time.Duration(i+1) * time.Minute).Format(time.RFC3339),
			"bead":         fmt.Sprintf("gt-%ds", int(secs)),
			"outcome":      "landed",
			"polecat_secs": secs,
		})
	}
	dom := newPageDOM(t)
	dom.call(t, "renderTrend", map[string]any{"trend": map[string]any{
		"hours":  []map[string]any{{"hour": now.Format(time.RFC3339), "landed": len(recent)}},
		"recent": recent,
	}})

	got := drawnPolecatColumn(t, dom)
	want := []polecatCell{
		{"gt-59s", "59s", ""},
		{"gt-60s", "60s", ""},
		{"gt-75s", "75s", ""},
		{"gt-89s", "89s", ""},
		{"gt-90s", "2m", ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the drawn seconds band:\n got %+v\nwant %+v", got, want)
	}
}
