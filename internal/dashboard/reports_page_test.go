package dashboard

import (
	"net/http"
	"net/http/httptest"
	"regexp"
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
		`note.className = !rPick && r.overdue ? "r warnc" : "r";`,
		`if (r.state === "missing" || r.state === "error") {`,
		`box.append(el("div", r.state === "error" ? "badc" : "empty", r.note || r.state));`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderReports has no %q", want)
		}
	}

	// The report's age is the pane's header note, worded and placed like the
	// other panes' readings ("read 25s ago"), and an overdue one carries that
	// mark in the same amber class the pane already used for it. The note dates
	// whichever report is shown — the latest, or the earlier one a reader chose
	// (gt-4e065) — and only a latest report is ever the overdue one.
	for _, want := range []string{
		`note.textContent = "written " + age(shown) + " ago" + (!rPick && r.overdue ? " · overdue" : "");`,
		`note.title = shown.toLocaleString();`,
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
	if !strings.Contains(draw, `rep.append(...reportNodes(rPick ? read.text : r.text, prefixes));`) {
		t.Errorf("renderReports has no %q", `rep.append(...reportNodes(rPick ? read.text : r.text, prefixes));`)
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
	dated := strings.Index(draw, `note.textContent = "written " + age(shown) + " ago"`)
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
// Each is a button — its text written through el(), which writes textContent —
// that reads that report in place of the latest (gt-4e065).
func TestReportPanelListsTheEarlierWritesBehindADisclosure(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderReports")
	for _, want := range []string{
		`const earlier = times.slice(1);`,
		`if (earlier.length) {`,
		`const d = document.createElement("details");`,
		`d.append(el("summary", "title", earlier.length + (earlier.length === 1 ? " earlier report" : " earlier reports")));`,
		`const ul = el("ul", "reptimes");`,
		`for (const t of earlier) {`,
		`const b = el("button", "rtime" + (name === rPick ? " on" : ""), new Date(t).toLocaleString());`,
		`b.onclick = () => openReport(name);`,
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
		`if (!rig || !BEAD_TOKEN.test(id)) { plain += part; continue; }`,
		`const b = el("button", "rbead", id);`,
		`b.onclick = () => openReportBead(rig, id);`,
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
		`rep.append(...reportNodes(rPick ? read.text : r.text, prefixes));`,
		`bd.append(detailBlock(rDetail[id] || {loading: true}));`,
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

// An id written at the end of a sentence must link as the id, not as the id with
// the sentence's full stop stuck to it: "landed gt-eorhf." used to open
// gt-eorhf. — which /api/bead answers 502, and the failed read is remembered for
// the life of the page (gt-1h5gx). The run the scanner finds is trimmed of the
// dots that trail it before the id is tested and linked, and those dots go back
// as the plain text they are; a dot inside the id (dv-1fel.4) is left alone.
func TestReportLinksAnIdWithoutTheSentenceFullStopBehindIt(t *testing.T) {
	t.Parallel()

	// The run is trimmed to the id before the prefix is read and the token
	// tested, the id is what the button carries and opens, and the dots the trim
	// cut off are handed back to the plain text the button is followed by.
	nodes := pageFunc(t, "reportNodes")
	for _, want := range []string{
		`const id = part.replace(/\.+$/, "");`,
		`const p = dash > 0 ? id.slice(0, dash) : "";`,
		`if (!rig || !BEAD_TOKEN.test(id)) { plain += part; continue; }`,
		`const b = el("button", "rbead", id);`,
		`b.onclick = () => openReportBead(rig, id);`,
		`plain += part.slice(id.length);`,
	} {
		if !strings.Contains(nodes, want) {
			t.Errorf("reportNodes has no %q", want)
		}
	}

	// The page cannot be executed here, so the decision it makes about a run —
	// which characters hold one (BEAD_RUN), what it is trimmed to, and whether
	// that is an id (BEAD_TOKEN) — is read out of the page and driven over the
	// ways a sentence writes an id. A change to any of those patterns fails here.
	page := string(indexHTML)
	lit := func(pat string) string {
		t.Helper()
		m := regexp.MustCompile(pat).FindStringSubmatch(page)
		if m == nil {
			t.Fatalf("index.html has no %s", pat)
		}
		return m[1]
	}
	runRe := regexp.MustCompile(lit(`const BEAD_RUN = /(.+)/g;`))
	tokenRe := regexp.MustCompile(lit(`const BEAD_TOKEN = /(.+)/;`))
	trimRe := regexp.MustCompile(lit(`part\.replace\(/(.+)/, ""\)`))

	for _, c := range []struct {
		text string
		run  string // the run the page's scanner finds and links
		id   string // the id that run is trimmed to
		tail string // the dots the trim leaves to the text after the button
	}{
		{`landed gt-eorhf.`, "gt-eorhf.", "gt-eorhf", "."},
		{`dv-1fel.4 shipped`, "dv-1fel.4", "dv-1fel.4", ""},
		{`landed dv-1fel.4.`, "dv-1fel.4.", "dv-1fel.4", "."},
		{`gt-eorhf, then read it`, "gt-eorhf", "gt-eorhf", ""},
		{`(gt-eorhf)`, "gt-eorhf", "gt-eorhf", ""},
		{`gt-eorhf: read it`, "gt-eorhf", "gt-eorhf", ""},
		{`gt-eorhf; read it`, "gt-eorhf", "gt-eorhf", ""},
	} {
		var got []string
		for _, part := range runRe.FindAllString(c.text, -1) {
			id := trimRe.ReplaceAllString(part, "")
			// A word like "then" has no hyphen and so no id shape; only the id's
			// trimmed run is a candidate here, which is all this bead changed.
			if !tokenRe.MatchString(id) {
				continue
			}
			got = append(got, part+"/"+id+"/"+part[len(id):])
		}
		want := c.run + "/" + c.id + "/" + c.tail
		if len(got) != 1 || got[0] != want {
			t.Errorf("%q links %v, want just %q", c.text, got, want)
		}
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

// An earlier report is read where the operator already looks: a timestamp in
// the list is a button that fetches that report and draws it in place of the
// latest, with a control back to the latest and a header note that dates the
// report shown. A fetch that failed shows that failure — never the latest under
// a header that names another report (gt-4e065).
func TestReportPanelReadsAnEarlierReportFromItsTimestamp(t *testing.T) {
	t.Parallel()

	// The choice lives outside the drawing function, so a poll refresh redraws
	// the report the reader picked rather than falling back to the latest.
	page := string(indexHTML)
	for _, want := range []string{
		`let rPick = "";`,
		`const rRead = {};`,
		`function reportFile(t) {`,
		`function reportTime(name) {`,
		`function showLatest() { rPick = ""; renderReports(state); }`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html has no %q", want)
		}
	}

	// The click reads the report from the one route that names it, remembers a
	// failed read as the failure it was, and does not fetch a report twice.
	open := pageFunc(t, "openReport")
	for _, want := range []string{
		`fetch("/api/report?name=" + encodeURIComponent(name))`,
		`.then(res => res.ok ? res.json() : Promise.reject(new Error(String(res.status))))`,
		`.then(d => { rRead[name] = {text: d.text || ""}; renderReports(state); })`,
		`.catch(() => { rRead[name] = {error: true}; renderReports(state); });`,
	} {
		if !strings.Contains(open, want) {
			t.Errorf("openReport has no %q", want)
		}
	}

	// The pane dispatches a timestamp's click to that reader, returns through
	// its own control, and names a failed read in the body.
	draw := pageFunc(t, "renderReports")
	for _, want := range []string{
		`b.onclick = () => openReport(name);`,
		`const b = el("button", "rbead", "← back to latest");`,
		`b.onclick = showLatest;`,
		`box.append(el("div", read && read.error ? "badc" : "empty", read && read.error ? "could not read this report" : "reading…"));`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderReports has no %q", want)
		}
	}
}
