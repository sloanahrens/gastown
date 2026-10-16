package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Digest block is read where the operator already looks for the overseer's
// side of the town: in the Overseer panel, under the heartbeat. What it draws
// is the state the reader decided — the heading note names a directory or a
// report it could not read, and the body is the digest's own sections.
func TestReportPanelDrawsTheStateTheReaderDecided(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "drawDigest")
	for _, want := range []string{
		`const r = s.reports;`,
		`if (!r) { box.append(el("div", "empty", "reading…")); return; }`,
		`note.className = !rPick && r.overdue ? "r warnc" : "r";`,
		`if (r.state === "missing" || r.state === "error") {`,
		`box.append(el("div", r.state === "error" ? "badc" : "empty", r.note || r.state));`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("drawDigest has no %q", want)
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
			t.Errorf("drawDigest has no %q", want)
		}
	}
	// The body is then only the report: the age line that used to sit above it
	// is gone, and nothing else still spells the old wording.
	if strings.Contains(draw, "chead") {
		t.Error("drawDigest still draws a body header above the report")
	}
	if strings.Contains(draw, "report overdue") {
		t.Error("drawDigest still writes the old overdue wording")
	}

	// The digest is another tool's text, so every cell goes through el(), which
	// writes textContent: a tag in the digest shows as that text and never as
	// markup, and a section's own line breaks are kept by the pre-wrap the box
	// carries rather than by an element that parses anything. The text is split
	// into nodes so a bead id in it can be a button (gt-7005n), and every node
	// is still built the way the block always built them.
	if strings.Contains(draw, "innerHTML") {
		t.Error("drawDigest sets markup from the report's text")
	}
	if !strings.Contains(draw, `drawDigestSections(box, rPick ? read.text : r.text, prefixes);`) {
		t.Errorf("drawDigest has no %q", `drawDigestSections(box, rPick ? read.text : r.text, prefixes);`)
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
// keeps its body text and leaves the heading note empty: the block dates a
// reading, and there is none. The note is the block's own, drawn fresh with it,
// so a state that returns before the dating leaves it bare.
func TestReportPanelLeavesTheHeadingNoteEmptyWhenThereIsNoWrite(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	for name, tc := range map[string]map[string]any{
		"missing": {"state": "missing", "note": "the overseer has written no report yet"},
		"error":   {"state": "error", "note": "the reports directory could not be read"},
	} {
		dom.call(t, "renderOverseer", map[string]any{"reports": tc})
		block := blockNamed(t, overseerBlocks(t, dom), "Digest")
		if block.Note != "" {
			t.Errorf("%s: the block's note reads %q, want it empty", name, block.Note)
		}
		if !strings.Contains(block.Text, tc["note"].(string)) {
			t.Errorf("%s: the block reads %q, want the reader's own words", name, block.Text)
		}
	}
}

// The pane is one report tall however many the town keeps: the writes before
// the latest are listed behind a disclosure, closed until the operator opens
// it, and the timestamps are the page's own rendering of the reader's list.
// Each is a button — its text written through el(), which writes textContent —
// that reads that report in place of the latest (gt-4e065).
func TestReportPanelListsTheEarlierWritesBehindADisclosure(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "drawDigest")
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
			t.Errorf("drawDigest has no %q", want)
		}
	}
	// A disclosure is closed until it is opened: nothing here opens it.
	if strings.Contains(draw, "d.open") {
		t.Error("drawDigest opens the list of earlier reports on its own")
	}

	// The block is drawn inside the Overseer panel, with the blocks beside it,
	// and the panel is what renderAll draws: the block has no renderer of its
	// own to call (gt-uv08f).
	dom := newPageDOM(t)
	dom.call(t, "renderOverseer", map[string]any{"reports": map[string]any{
		"state": "fresh", "text": "overseer report\n", "written": "2026-10-15T20:00:00Z",
		"times": []any{"2026-10-15T20:00:00Z", "2026-10-15T19:45:00Z"},
	}})
	if block := blockNamed(t, overseerBlocks(t, dom), "Digest"); !strings.Contains(block.Text, "1 earlier report") {
		t.Errorf("the digest block reads %q, want the earlier writes behind their disclosure", block.Text)
	}
	if all := pageFunc(t, "renderAll"); !strings.Contains(all, "renderOverseer(state)") {
		t.Error("renderAll does not draw the Overseer panel")
	}
}

// archiveDigest is a digest in the shape the daemon's report patrol writes it:
// a header line, then the named sections, each row indented under its heading.
const archiveDigest = `overseer report 2026-10-15T20:00:00Z (covers 2026-10-15T19:45:00Z to 2026-10-15T20:00:00Z)

DONE
  gt-a1  landed a thing  gastown
  dv-b2  shipped another  dv

IN PROGRESS
  gastown/nux  gt-c3  working on it  23m

LEFT
  next  gt-d4  gastown  two seats free
  held  gt-e5  gastown  waiting on gt-a1

OPEN QUESTIONS
  gt-q1  2h  held: gt-a1

health: GREEN
`

// The block's body is the digest's own sections: the three that say what the
// town did and what it still has to do are drawn, with the beads in them
// openable, and the questions section is left out — those questions are the
// attention list's rows, which the panel draws in full, so a digest's
// fifteen-minute-old copy of them would be one fact twice (gt-uv08f).
func TestDigestBlockDrawsTheDigestsOwnSections(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderOverseer", map[string]any{"reports": map[string]any{
		"state": "fresh", "text": archiveDigest, "written": "2026-10-15T20:00:00Z",
		"times": []any{"2026-10-15T20:00:00Z"},
	}})

	block := blockNamed(t, overseerBlocks(t, dom), "Digest")
	for _, want := range []string{
		"DONE", "gt-a1  landed a thing  gastown", "dv-b2  shipped another  dv",
		"IN PROGRESS", "gastown/nux  gt-c3  working on it  23m",
		"LEFT", "next  gt-d4  gastown  two seats free", "held  gt-e5  gastown  waiting on gt-a1",
	} {
		if !strings.Contains(block.Text, want) {
			t.Errorf("the digest block reads %q, want %q in it", block.Text, want)
		}
	}
	// The header line, the questions themselves and the health line are not the
	// three sections the block draws. The questions' heading is named as a
	// heading the block does not draw — a section is never dropped in silence —
	// but none of its rows are drawn here: they are the attention list's.
	for _, gone := range []string{"overseer report 2026", "gt-q1", "health: GREEN"} {
		if strings.Contains(block.Text, gone) {
			t.Errorf("the digest block draws %q: %q", gone, block.Text)
		}
	}
	if !strings.Contains(block.Text, "not drawn: OPEN QUESTIONS") {
		t.Errorf("the digest block drops the questions section in silence: %q", block.Text)
	}
	// The digest's own reading of a section with nothing in it is the reader's
	// word, not the block's: "(none)" is what the writer writes.
	dom = newPageDOM(t)
	dom.call(t, "renderOverseer", map[string]any{"reports": map[string]any{
		"state": "fresh", "text": "overseer report x\n\nDONE\n  (none)\n\nIN PROGRESS\n  (none)\n\nLEFT\n  (none)\n",
		"written": "2026-10-15T20:00:00Z", "times": []any{"2026-10-15T20:00:00Z"},
	}})
	if got := blockNamed(t, overseerBlocks(t, dom), "Digest"); strings.Count(got.Text, "(none)") != 3 {
		t.Errorf("an empty digest reads %q, want the writer's three empty sections", got.Text)
	}
}

// A heading the block does not draw — the questions one, and any a writer adds
// beside the three — is named under them, in the digest's own order. A section
// the page silently dropped would read as a digest that never had it (gt-uv08f).
func TestDigestBlockNamesTheSectionsItDoesNotDraw(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderOverseer", map[string]any{"reports": map[string]any{
		"state": "fresh", "written": "2026-10-15T20:00:00Z", "times": []any{"2026-10-15T20:00:00Z"},
		"text": "overseer report x\n\nDONE\n  gt-a1  landed a thing\n\nBACKLOG\n  gt-b2  waiting\n\nOPEN QUESTIONS\n  gt-q1  2h\n",
	}})
	block := blockNamed(t, overseerBlocks(t, dom), "Digest")
	if want := "not drawn: BACKLOG, OPEN QUESTIONS"; !strings.Contains(block.Text, want) {
		t.Errorf("the digest block reads %q, want %q in it", block.Text, want)
	}
	// Naming a heading is not drawing its rows.
	if strings.Contains(block.Text, "gt-b2") || strings.Contains(block.Text, "gt-q1") {
		t.Errorf("the digest block draws an undrawn section's rows: %q", block.Text)
	}
}

// A digest whose shape the page does not recognise — an older daemon's, or
// another tool's report in the same directory — is drawn whole rather than as
// nothing: the sections are how the block sorts a digest, not what it takes for
// one.
func TestDigestBlockDrawsADigestOfAnUnknownShapeWhole(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderOverseer", map[string]any{"reports": map[string]any{
		"state": "fresh", "text": "a report with no sections at all\ngt-a1 is mentioned\n",
		"written": "2026-10-15T20:00:00Z", "times": []any{"2026-10-15T20:00:00Z"},
	}})
	block := blockNamed(t, overseerBlocks(t, dom), "Digest")
	if !strings.Contains(block.Text, "a report with no sections at all") || !strings.Contains(block.Text, "gt-a1 is mentioned") {
		t.Errorf("the block reads %q, want the whole report", block.Text)
	}
}

// The digest is parsed by its own shape — a heading is a line of capitals at
// the left margin, where every row under one is indented — and the page holds
// that shape in one place, so a section the writer changes is a section the
// block follows.
func TestDigestSectionsReadTheDigestShape(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	v, err := dom.vm.RunString(`JSON.stringify(Array.from(digestSections(` + strconv.Quote(archiveDigest) + `).entries()).map(([k, rows]) => [k, digestLines(rows)]))`)
	if err != nil {
		t.Fatalf("reading the digest's sections: %v", err)
	}
	var got [][2]string
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("reading the digest's sections: %v", err)
	}
	want := [][2]string{
		{"DONE", "  gt-a1  landed a thing  gastown\n  dv-b2  shipped another  dv"},
		{"IN PROGRESS", "  gastown/nux  gt-c3  working on it  23m"},
		{"LEFT", "  next  gt-d4  gastown  two seats free\n  held  gt-e5  gastown  waiting on gt-a1"},
		{"OPEN QUESTIONS", "  gt-q1  2h  held: gt-a1"},
	}
	if len(got) != len(want) {
		t.Fatalf("the digest read as %v, want its four sections", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("section %d read as %q, want %q", i, got[i], want[i])
		}
	}
	// The header line above the first heading belongs to no section, and the
	// blank lines the writer puts around a section are not part of it: a row
	// that is not indented and not capitals is not a heading either.
	for _, text := range []string{
		"",
		"a report with no sections",
		"overseer report x\n\nlower case heading\n  a row\n",
	} {
		if v, err := dom.vm.RunString(`String(digestSections(` + strconv.Quote(text) + `).size)`); err != nil || v.String() != "0" {
			t.Errorf("digestSections(%.20q) read %v sections (%v), want none", text, v, err)
		}
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
		`if (rOpen.has(key)) { rOpen.delete(key); renderOverseer(state); return; }`,
		`beadDetailFor(rig, id, rDetail, () => renderOverseer(state));`,
	} {
		if !strings.Contains(open, want) {
			t.Errorf("openReportBead has no %q", want)
		}
	}
	draw := pageFunc(t, "drawDigest")
	for _, want := range []string{
		`drawDigestSections(box, rPick ? read.text : r.text, prefixes);`,
		`bd.append(detailBlock(rDetail[id] || {loading: true}));`,
		`const prefixes = (s.queue && s.queue.prefixes) || {};`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("drawDigest has no %q", want)
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
	// The Work panel's rows open through the same helper, so the two panes
	// cannot drift into two different readers of one route.
	if q := pageFunc(t, "openBead"); !strings.Contains(q, `beadDetailFor(b.rig, b.id, qDetail, () => renderWork(state, true));`) {
		t.Error("the Work panel's rows no longer open a bead through the shared reader")
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
		`function showLatest() { rPick = ""; renderOverseer(state); }`,
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
		`.then(d => { rRead[name] = {text: d.text || ""}; renderOverseer(state); })`,
		`.catch(() => { rRead[name] = {error: true}; renderOverseer(state); });`,
	} {
		if !strings.Contains(open, want) {
			t.Errorf("openReport has no %q", want)
		}
	}

	// The block dispatches a timestamp's click to that reader, returns through
	// its own control, and names a failed read in the body.
	draw := pageFunc(t, "drawDigest")
	for _, want := range []string{
		`b.onclick = () => openReport(name);`,
		`const b = el("button", "rbead", "← back to latest");`,
		`b.onclick = showLatest;`,
		`box.append(el("div", read && read.error ? "badc" : "empty", read && read.error ? "could not read this report" : "reading…"));`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("drawDigest has no %q", want)
		}
	}
}
