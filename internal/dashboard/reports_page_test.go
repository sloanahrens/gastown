package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The Report panel is read where the operator already looks for a report the
// town produces: in the left column, directly under Alerts, which is what the
// layout test pins. What it draws is the state the reader decided — the note
// names a directory or a report it could not read, and the body is the report
// itself.
func TestReportPanelDrawsTheStateTheReaderDecided(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderReports")
	for _, want := range []string{
		`const r = s.reports;`,
		`if (!r) { box.append(el("div", "empty", "reading…")); return; }`,
		`note.className = r.overdue ? "r warnc" : "r";`,
		`if (r.state === "missing" || r.state === "error") {`,
		`box.append(el("div", r.state === "error" ? "badc" : "empty", r.note || r.state));`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderReports has no %q", want)
		}
	}

	// The report's age is the pane's header note, worded and placed like the
	// other panes' readings ("read 25s ago"), and an overdue one carries that
	// mark in the same amber class the pane already used for it.
	for _, want := range []string{
		`note.textContent = "written " + age(r.written) + " ago" + (r.overdue ? " · overdue" : "");`,
		`note.title = new Date(r.written).toLocaleString();`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderReports has no %q", want)
		}
	}
	// The body is then only the report: the age line that used to sit above it
	// is gone, and nothing else still spells the old wording.
	if strings.Contains(draw, "chead") {
		t.Error("renderReports still draws a body header above the report")
	}
	if strings.Contains(draw, "report overdue") {
		t.Error("renderReports still writes the old overdue wording")
	}

	// The report is another tool's text, so every cell goes through el(), which
	// writes textContent: a tag in the report shows as that text and never as
	// markup, and the report's own line breaks are kept by the pre-wrap the box
	// carries rather than by an element that parses anything. The text is split
	// into nodes so a bead id in it can be a button (gt-7005n), and every node
	// is still built the way the pane always built them.
	if strings.Contains(draw, "innerHTML") {
		t.Error("renderReports sets markup from the report's text")
	}
	if !strings.Contains(draw, `rep.append(...reportNodes(r.text, prefixes));`) {
		t.Errorf("renderReports has no %q", `rep.append(...reportNodes(r.text, prefixes));`)
	}
	// A full report shows without scrolling: the box is capped at the height
	// the Feed pane uses, not the 280px that clipped a long report.
	if !strings.Contains(string(indexHTML), `.reptext{margin:0;white-space:pre-wrap;`) {
		t.Error("index.html has no pre-wrap box for the report's text")
	}
	if !strings.Contains(string(indexHTML), `max-height:68vh;overflow:auto}`) {
		t.Error("index.html does not cap the report box at the Feed pane's 68vh")
	}
	if strings.Contains(string(indexHTML), `max-height:280px`) {
		t.Error("index.html still clips a box at 280px")
	}
}

// A state with no write to date — no report yet, or one that could not be read —
// keeps its body text and leaves the header note empty: the pane dates a
// reading, and there is none.
func TestReportPanelLeavesTheHeaderNoteEmptyWhenThereIsNoWrite(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderReports")
	reset := strings.Index(draw, `note.textContent = "";`)
	state := strings.Index(draw, `if (r.state === "missing" || r.state === "error") {`)
	dated := strings.Index(draw, `note.textContent = "written " + age(r.written) + " ago"`)
	if reset < 0 || state < 0 || dated < 0 {
		t.Fatalf("renderReports does not reset the note, name the states, and date the report: %s", draw)
	}
	if reset > state {
		t.Error("renderReports clears the header note only after the nameless states are decided")
	}
	if dated < state {
		t.Error("renderReports dates the report before the state without a write is decided")
	}
}

// The pane is one report tall however many the town keeps: the writes before
// the latest are listed behind a disclosure, closed until the operator opens
// it, and the timestamps are the page's own rendering of the reader's list.
func TestReportPanelListsTheEarlierWritesBehindADisclosure(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderReports")
	for _, want := range []string{
		`const times = (r.times || []).slice(1);`,
		`if (times.length) {`,
		`const d = document.createElement("details");`,
		`d.append(el("summary", "title", times.length + (times.length === 1 ? " earlier report" : " earlier reports")));`,
		`const ul = el("ul", "reptimes");`,
		`for (const t of times) ul.append(el("li", "", new Date(t).toLocaleString()));`,
		`d.append(ul);`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderReports has no %q", want)
		}
	}
	// A disclosure is closed until it is opened: nothing here opens it.
	if strings.Contains(draw, "d.open") {
		t.Error("renderReports opens the list of earlier reports on its own")
	}

	// The panel is drawn with every other pane, and only exists while the
	// reader has reported: the state's field is absent until then.
	all := pageFunc(t, "renderAll")
	if !strings.Contains(all, "renderReports(state)") {
		t.Error("renderAll does not draw the Report panel")
	}
	if !strings.Contains(string(indexHTML), `<section><h2>Report <span class="r" id="reportnote"></span></h2><div class="body" id="report"></div></section>`) {
		t.Error("the Report section is not the markup its neighbours are")
	}
}

// A bead id written in the report opens that bead under the report, in the same
// inline detail the Work queue's rows draw (gt-7005n). The report is another
// tool's text, so the ids are found by shape — never by parsing markup — and
// only a shape whose prefix names a store is a link: a word like "re-run" has
// the shape of an id but no store claims it, so it stays the text it was.
func TestReportLinksTheBeadIdsWhosePrefixNamesAStore(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	// The shape the report's ids are recognised by, and the characters a run of
	// the text has to hold to be looked at as one: everything between those runs
	// is a text node, which is where a tag in the report stops being markup.
	for _, want := range []string{
		`const BEAD_TOKEN = /^[a-z][a-z0-9]*-[a-z0-9][a-z0-9.]*$/;`,
		`const BEAD_RUN = /[a-z0-9][a-z0-9.-]*/g;`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html has no %q", want)
		}
	}

	nodes := pageFunc(t, "reportNodes")
	// In "gt-pufsj and dv-7y3" each run that is an id and whose prefix names a
	// store becomes a button carrying the id as its text; the run between them
	// stays text. The scanner's own position is reset, so a cached report redrawn
	// finds the same ids.
	for _, want := range []string{
		`BEAD_RUN.lastIndex = 0;`,
		`while ((m = BEAD_RUN.exec(s))) {`,
		`if (!rig || !BEAD_TOKEN.test(part)) { plain += part; continue; }`,
		`const b = el("button", "rbead", part);`,
		`b.onclick = () => openReportBead(rig, part);`,
	} {
		if !strings.Contains(nodes, want) {
			t.Errorf("reportNodes has no %q", want)
		}
	}
	// The id is carried by el(), which writes textContent, and the text around it
	// by createTextNode: the report's markup is never parsed, so a tag in it is
	// shown as the text it is. Nothing here writes innerHTML.
	if strings.Contains(nodes, "innerHTML") {
		t.Error("reportNodes parses the report's text as markup")
	}
	// A prefix off Object.prototype — "constructor", say — is not a store, so the
	// lookup may only answer for the keys the payload actually carries.
	if !strings.Contains(nodes, "hasOwnProperty.call(map, p)") {
		t.Error("reportNodes trusts an inherited property as a store name")
	}

	// The button opens the bead through the same fetch and the same detail block
	// the Work queue's rows use, and a second click on the id closes it again: a
	// failed read draws the failure detailBlock already names.
	open := pageFunc(t, "openReportBead")
	for _, want := range []string{
		`if (rOpen.has(key)) { rOpen.delete(key); renderReports(state); return; }`,
		`beadDetailFor(rig, id, rDetail, () => renderReports(state));`,
	} {
		if !strings.Contains(open, want) {
			t.Errorf("openReportBead has no %q", want)
		}
	}
	draw := pageFunc(t, "renderReports")
	for _, want := range []string{
		`rep.append(...reportNodes(r.text, prefixes));`,
		`bd.append(detailBlock(rDetail[key] || {loading: true}));`,
		`const prefixes = (s.queue && s.queue.prefixes) || {};`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderReports has no %q", want)
		}
	}
	// The click reads the bead from the one route with the rig its prefix mapped
	// to, draws what came back, and remembers a failed read as the failure
	// detailBlock names — never as an empty detail.
	read := pageFunc(t, "beadDetailFor")
	for _, want := range []string{
		`if (store[key]) return;`,
		`fetch("/api/bead?rig=" + encodeURIComponent(rig) + "&id=" + encodeURIComponent(id))`,
		`.then(d => { store[key] = d; done(); })`,
		`.catch(() => { store[key] = {error: true}; done(); });`,
	} {
		if !strings.Contains(read, want) {
			t.Errorf("beadDetailFor has no %q", want)
		}
	}
	if !strings.Contains(page, `box.append(el("div", "badc", "could not read this bead right now"));`) {
		t.Error("the detail a report id opens has no failed-read state")
	}
	// The queue's rows open through the same helper, so the two panes cannot
	// drift into two different readers of one route.
	if q := pageFunc(t, "openBead"); !strings.Contains(q, `beadDetailFor(b.rig, b.id, qDetail, () => renderQueue(state, true));`) {
		t.Error("the Work queue's rows no longer open a bead through the shared reader")
	}
}

// The state the page reads carries the panel, and the page is still read-only:
// a GET of /api/state serves the reports the hub polled, and the method that is
// not a read is refused as it always was.
func TestReportsReachTheAPIAndTheGuardStillRefusesWrites(t *testing.T) {
	t.Parallel()

	written := reportsNow.Add(-time.Minute)
	h := NewHub(Config{
		Now: func() time.Time { return reportsNow },
		Reports: func() *Reports {
			return &Reports{At: reportsNow, State: ReportsFresh, Text: "the report", Written: written}
		},
	})
	h.pollReports()

	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/state = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"reports"`) || !strings.Contains(body, `"the report"`) {
		t.Errorf("/api/state does not carry the report the hub holds: %s", body)
	}

	for _, method := range []string{"POST", "PUT", "DELETE"} {
		req := httptest.NewRequest(method, "/api/state", nil)
		req.Host = "127.0.0.1:8787"
		rec := httptest.NewRecorder()
		h.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/state = %d, want the page to refuse it", method, rec.Code)
		}
	}
}
