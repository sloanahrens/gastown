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
	// carries rather than by an element that parses anything.
	if strings.Contains(draw, "innerHTML") {
		t.Error("renderReports sets markup from the report's text")
	}
	if !strings.Contains(draw, `box.append(el("div", "reptext", r.text || ""));`) {
		t.Errorf("renderReports has no %q", `box.append(el("div", "reptext", r.text || ""));`)
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
