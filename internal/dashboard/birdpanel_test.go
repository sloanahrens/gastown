package dashboard

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// A birds read with an answer sets the state's rows and marks the read: the
// rows when the reader has them, and no rows when the birds really went away.
// A read with no answer at all keeps the rows it last read and flags itself
// (gt-q6h8e).
func TestBirdPanelPollFollowsTheRead(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	rows := []BirdReport{
		{Bird: "dunlin", State: BirdReportFresh, Written: now.Add(-time.Hour), Summary: "the day in one line"},
		{Bird: "heron", State: BirdReportStale, Stale: true, Written: now.Add(-40 * time.Hour)},
	}
	read := rows
	h := NewHub(Config{
		Now:        func() time.Time { return now },
		BirdsEvery: time.Minute,
		Birds:      func() []BirdReport { return read },
	})

	h.pollBirds()
	st := h.State()
	if len(st.Birds) != 2 || st.Birds[0].Bird != "dunlin" || st.Birds[1].Bird != "heron" {
		t.Fatalf("the read's rows are not the state's: %+v", st.Birds)
	}
	if r := st.Reads[SourceBirds]; r.Error || r.At.IsZero() || r.Every <= 0 {
		t.Fatalf("a read that answered is flagged as failed: %+v", r)
	}

	// Every bird's report is gone: the reader answered with no rows, which is a
	// read that worked and found nobody rather than one that failed. The rows
	// the state held must not keep drawing birds that are not there.
	read = []BirdReport{}
	h.pollBirds()
	st = h.State()
	if len(st.Birds) != 0 {
		t.Errorf("a read with no rows kept the rows it held: %+v", st.Birds)
	}
	if r := st.Reads[SourceBirds]; r.Error {
		t.Errorf("a read with no rows is flagged as failed: %+v", r)
	}

	// A reader with no answer at all is a failed read: the rows stand, and the
	// read is flagged with its last good age so the pane ages them (gt-q6h8e).
	read = rows
	h.pollBirds()
	read = nil
	h.pollBirds()
	st = h.State()
	if len(st.Birds) != 2 || st.Birds[0].Bird != "dunlin" || st.Birds[1].Bird != "heron" {
		t.Fatalf("a read with no answer dropped the last rows: %+v", st.Birds)
	}
	if r := st.Reads[SourceBirds]; !r.Error || r.At.IsZero() {
		t.Errorf("a read with no answer is not flagged with its last good age: %+v", r)
	}
}

// A hub with no birds reader starts no poller: the run returns without a read,
// and the poll is a no-op rather than a read of nothing.
func TestBirdPanelWithNoReaderReadsNothing(t *testing.T) {
	t.Parallel()

	h := NewHub(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.Run(ctx)
	h.pollBirds()
	st := h.State()
	if st.Birds != nil {
		t.Errorf("a hub with no birds reader drew rows: %+v", st.Birds)
	}
	if _, ok := st.Reads[SourceBirds]; ok {
		t.Errorf("a hub with no birds reader recorded a read: %+v", st.Reads[SourceBirds])
	}
}

// birdDrawnCell is one element the Birds panel drew: its tag, its text and
// class, the link it carries when it is one, and how many children it holds — a
// leaf written through textContent has none.
type birdDrawnCell struct {
	Tag    string `json:"tag"`
	Text   string `json:"text"`
	Class  string `json:"class"`
	Href   string `json:"href"`
	Target string `json:"target"`
	Rel    string `json:"rel"`
	Title  string `json:"title"`
	Kids   int    `json:"kids"`
}

// birdDrawnRow is one row of the Birds panel: the class it wears, the cells of
// its head in the order the block appended them, and its body line when it drew
// one.
type birdDrawnRow struct {
	Class string          `json:"class"`
	Head  []birdDrawnCell `json:"head"`
	Body  *birdDrawnCell  `json:"body"`
}

// birdRows reads the rows the Birds panel drew into its own body, in the order
// the panel drew them. The "Needs you" list is not a row, so it is left out.
func birdRows(t *testing.T, dom *pageDOM) []birdDrawnRow {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const cell = c => ({tag: c.tagName, text: c.textContent, class: c.className, href: c.href || "", target: c.target || "", rel: c.rel || "", title: c.title || "", kids: c.children.length});
		return __byId["birds"].children.__arr.filter(c => (" " + (c.className || "") + " ").indexOf(" brow ") >= 0).map(row => ({
			class: row.className,
			head: (row.children[0] ? row.children[0].children.__arr : []).map(cell),
			body: row.children[1] ? cell(row.children[1]) : null
		}));
	})())`)
	if err != nil {
		t.Fatalf("reading the Birds panel's rows: %v", err)
	}
	var out []birdDrawnRow
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the Birds panel's rows: %v", err)
	}
	return out
}

// birdLines reads the class of every line the Birds panel drew into its body,
// in the order it drew them: the rows and the "Needs you" list.
func birdLines(t *testing.T, dom *pageDOM) []string {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify(__byId["birds"].children.__arr.map(c => c.className))`)
	if err != nil {
		t.Fatalf("reading the Birds panel's lines: %v", err)
	}
	var out []string
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the Birds panel's lines: %v", err)
	}
	return out
}

// The block draws the row the reader decided: the name, the summary and the age
// of it, the amber mark and STALE chip on a stale row, a row in error named in
// the alert class, the badge a bird asks for the operator with, and the two
// pages it wrote as links that open in a new tab, the name itself linking to
// the report.
// Every cell is the page's own text, so a tag a bird wrote shows as that text
// rather than as markup.
func TestBirdPanelDrawsAgeStaleSummaryAndLinks(t *testing.T) {
	t.Parallel()

	written := time.Now().UTC().Add(-30 * time.Minute).Format(time.RFC3339)
	stamp := time.Now().UTC().Format(time.RFC3339)
	dom := newPageDOM(t)
	dom.call(t, "renderBirds", map[string]any{
		"birds": []any{
			map[string]any{
				"bird": "dunlin", "state": "fresh", "written": written,
				"summary": "the <b>day</b> in one line", "needs_operator": true,
				"has_page": true, "has_prev": true,
			},
			map[string]any{"bird": "heron", "state": "stale", "written": written, "stale": true},
			map[string]any{"bird": "curlew", "state": "error", "note": "daily.json is not valid JSON"},
		},
		"reads": map[string]any{"birds": map[string]any{"at": stamp, "every_sec": 60}},
	})

	rows := birdRows(t, dom)
	if len(rows) != 3 {
		t.Fatalf("the block drew %d rows, want the reader's three: %+v", len(rows), rows)
	}

	// The bird that wrote a page, a previous page and a summary: its name, both
	// links, the badge and the age of the summary, then the summary itself.
	if rows[0].Class != "brow" {
		t.Errorf("a fresh row is drawn as %q, want no mark", rows[0].Class)
	}
	wantHead := []birdDrawnCell{
		{Tag: "A", Text: "dunlin", Class: "bname", Href: "/birds/dunlin/daily.html", Target: "_blank", Rel: "noopener"},
		{Tag: "A", Text: "report", Class: "bpage", Href: "/birds/dunlin/daily.html", Target: "_blank", Rel: "noopener"},
		{Tag: "A", Text: "previous", Class: "bprev", Href: "/birds/dunlin/daily.prev.html", Target: "_blank", Rel: "noopener"},
		{Tag: "SPAN", Text: "needs the operator", Class: "badge warnc"},
		{Tag: "SPAN", Text: "written 30m ago", Class: "bage"},
	}
	if !slices.Equal(rows[0].Head, wantHead) {
		t.Errorf("the row's head reads %+v, want %+v", rows[0].Head, wantHead)
	}
	wantBody := birdDrawnCell{Tag: "DIV", Text: "the <b>day</b> in one line", Class: "bsum"}
	if rows[0].Body == nil || *rows[0].Body != wantBody {
		t.Errorf("the row's summary reads %+v, want %+v", rows[0].Body, wantBody)
	}

	// The bird the reader could not use shows what is wrong in the alert class,
	// and no age, because the reader had no instant to show. It is not marked
	// stale, so it stays above the stale row below.
	if rows[1].Body == nil || rows[1].Body.Class != "badc" || rows[1].Body.Text != "daily.json is not valid JSON" {
		t.Errorf("the error row's note reads %+v, want the reader's words in the alert class", rows[1].Body)
	}
	if len(rows[1].Head) != 1 || rows[1].Head[0].Text != "curlew" {
		t.Errorf("the error row's head reads %+v, want its name alone", rows[1].Head)
	}

	// The stale bird wears the amber mark and sorts after the fresh and failed
	// rows. It wrote no page and no summary, so its head carries its name, the
	// STALE chip and the age of the report it did write.
	if rows[2].Class != "brow warnc" {
		t.Errorf("a stale row is drawn as %q, want the amber mark", rows[2].Class)
	}
	wantStale := []birdDrawnCell{
		{Tag: "SPAN", Text: "heron", Class: "bname"},
		{Tag: "SPAN", Text: "STALE", Class: "badge warnc"},
		{Tag: "SPAN", Text: "written 30m ago", Class: "bage"},
	}
	if !slices.Equal(rows[2].Head, wantStale) {
		t.Errorf("the stale row's head reads %+v, want its name, STALE chip and age: %+v", rows[2].Head, wantStale)
	}
	if rows[2].Body != nil {
		t.Errorf("the stale row drew a body line it has no words for: %+v", rows[2].Body)
	}
}

// The two branches drawBirds takes with no rows: a read that failed says so in
// its own words and in the alert class, and a read that worked draws the quiet
// line saying no bird has written yet. The two must not read alike.
func TestBirdPanelDrawsAReadWithNoBirds(t *testing.T) {
	t.Parallel()

	stamp := time.Now().UTC().Format(time.RFC3339)
	cases := []struct {
		name      string
		status    map[string]any
		wantClass string
		wantText  string
		wantNote  string
	}{
		{
			name:      "a failed read",
			status:    map[string]any{"at": stamp, "every_sec": 60, "error": true, "stale": true},
			wantClass: "badc",
			wantText:  "the birds read failed",
			wantNote:  "read failed",
		},
		{
			name:      "a read that found nobody",
			status:    map[string]any{"at": stamp, "every_sec": 60},
			wantClass: "empty",
			wantText:  "no bird has written a daily report yet",
		},
	}
	for _, tc := range cases {
		dom := newPageDOM(t)
		dom.call(t, "renderBirds", map[string]any{
			"birds": []any{},
			"reads": map[string]any{"birds": tc.status},
		})
		if got := dom.element(t, "birds").Text; got != tc.wantText {
			t.Errorf("%s: the panel reads %q, want %q", tc.name, got, tc.wantText)
		}
		if classes := birdLines(t, dom); !slices.Equal(classes, []string{tc.wantClass}) {
			t.Errorf("%s: the panel's line is drawn as %v, want %q", tc.name, classes, tc.wantClass)
		}
		if tc.wantNote != "" {
			if note := dom.element(t, "birdsnote").Text; !strings.Contains(note, tc.wantNote) {
				t.Errorf("%s: the panel's note reads %q, want it to name %q", tc.name, note, tc.wantNote)
			}
		}
	}
}

// A row in error draws the note the reader wrote for it, and a withheld row
// with no note names its own state: the block never draws an empty alert line.
func TestBirdPanelDrawsARowsNoteOrItsState(t *testing.T) {
	t.Parallel()

	stamp := time.Now().UTC().Format(time.RFC3339)
	dom := newPageDOM(t)
	dom.call(t, "renderBirds", map[string]any{
		"birds": []any{
			map[string]any{"bird": "curlew", "state": "error", "note": "daily.json is not valid JSON"},
			map[string]any{"bird": "heron", "state": "withheld"},
		},
		"reads": map[string]any{"birds": map[string]any{"at": stamp, "every_sec": 60}},
	})

	rows := birdRows(t, dom)
	if len(rows) != 2 {
		t.Fatalf("the block drew %d rows, want the reader's two: %+v", len(rows), rows)
	}
	if got := rows[0].Body; got == nil || got.Class != "badc" || got.Text != "daily.json is not valid JSON" {
		t.Errorf("the error row reads %+v, want the reader's note in the alert class", got)
	}
	if got := rows[1].Body; got == nil || got.Class != "badc" || got.Text != "withheld" {
		t.Errorf("a withheld row with no note reads %+v, want its state in the alert class", got)
	}
}

// The pane redraws when its read status moves even though the rows it drew did
// not: a read that failed while the birds it last read stayed put is a change
// the pane's push guard must see (gt-quuq5).
func TestBirdPanelRedrawsWhenOnlyItsReadStatusChanged(t *testing.T) {
	t.Parallel()

	stamp := time.Now().UTC().Format(time.RFC3339)
	dom := newPageDOM(t)
	// The panel starts closed, and a closed pane draws into a detached box, so
	// the reader opens it to watch the body the draws land in.
	dom.press(t, "birds", "click")
	draws := countPaneDraws(t, dom, "birds")
	reading := func(status map[string]any) map[string]any {
		return map[string]any{
			"birds": []any{map[string]any{"bird": "dunlin", "state": "fresh", "written": stamp, "summary": "the day in one line"}},
			"reads": map[string]any{"birds": status},
		}
	}
	pushState(t, dom, reading(map[string]any{"at": stamp, "every_sec": 60}))
	if got := draws(); got != 1 {
		t.Fatalf("the first reading drew the pane %d times, want once", got)
	}
	// The same bird and the same stamp: only the read behind them failed.
	pushState(t, dom, reading(map[string]any{"at": stamp, "every_sec": 60, "error": true, "stale": true}))
	if got := draws(); got != 2 {
		t.Errorf("a failed birds read drew the pane %d times, want 2: the rows did not move, the status did", got)
	}
	if note := dom.element(t, "birdsnote").Text; !strings.Contains(note, "read failed") {
		t.Errorf("the birds panel's note reads %q after the failed read, want it named", note)
	}
}

// The rows travel to the page in the state's own JSON, under the fields the
// block reads, so a page opened against a running hub draws the rows it was
// sent rather than a picture only a test can see.
func TestBirdPanelStateEncodesTheRows(t *testing.T) {
	t.Parallel()

	written := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	st := State{Birds: []BirdReport{{
		Bird:          "dunlin",
		State:         BirdReportStale,
		Written:       written,
		Stale:         true,
		Summary:       "the day in one line",
		NeedsOperator: true,
		HasPage:       true,
		HasPrev:       true,
		PrevWritten:   written.Add(-24 * time.Hour),
	}}}
	payload, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	for _, want := range []string{
		`"birds":[`,
		`"bird":"dunlin"`,
		`"state":"stale"`,
		`"written":"2026-11-01T09:30:00Z"`,
		`"stale":true`,
		`"summary":"the day in one line"`,
		`"needs_operator":true`,
		`"has_page":true`,
		`"has_prev":true`,
		`"prev_written":"2026-10-31T09:30:00Z"`,
	} {
		if !strings.Contains(string(payload), want) {
			t.Errorf("the state payload has no %s: %s", want, payload)
		}
	}
}

// drawnNeeds is the "Needs you" section as the page drew it: the element, its
// summary line, whether it came up open, and one entry per reporter group with
// the text of each child in order.
type drawnNeeds struct {
	Class   string            `json:"cls"`
	Open    bool              `json:"open"`
	Summary string            `json:"summary"`
	Groups  []drawnNeedsGroup `json:"groups"`
}

type drawnNeedsGroup struct {
	Class   string   `json:"cls"`
	Parts   []string `json:"parts"`
	Classes []string `json:"classes"`
}

// needsYouWalk reads every details element the page drew under the Birds
// panel's body, in document order, with what it shows.
const needsYouWalk = `JSON.stringify((() => {
  const out = [];
  (function walk(n) {
    for (const c of (n.children || [])) {
      if (c.tagName === "DETAILS") {
        out.push({
          cls: c.className,
          open: !!c.open,
          summary: c.children.length ? c.children[0].textContent : "",
          groups: c.children.__arr.slice(1).map(g => ({
            cls: g.className,
            parts: g.children.__arr.map(x => x.textContent),
            classes: g.children.__arr.map(x => x.className)
          }))
        });
      }
      walk(c);
    }
  })(__byId["birds"]);
  return out;
})())`

// drawBirdsInDOM runs the page's own renderBirds over a state and answers the
// "Needs you" sections it drew.
func drawBirdsInDOM(t *testing.T, state map[string]any) []drawnNeeds {
	t.Helper()

	dom := newPageDOM(t)
	dom.call(t, "renderBirds", state)
	v, err := dom.vm.RunString(needsYouWalk)
	if err != nil {
		t.Fatalf("reading the drawn Birds panel: %v", err)
	}
	var got []drawnNeeds
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("reading the drawn Birds panel: %v", err)
	}
	return got
}

// Every reporter's decisions are one list under the rows: grouped by reporter,
// the reporter that wrote most recently first (by name when two wrote in the
// same second), open when a reporter has an item to put, and left out whole
// when no reporter has a decision to show. The blocked reporter's dropped item
// is named as withheld without its text.
func TestBirdsPanelNeedsYou(t *testing.T) {
	t.Parallel()

	older := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC).Format(time.RFC3339)
	newer := time.Date(2026, 11, 1, 11, 0, 0, 0, time.UTC).Format(time.RFC3339)
	item := func(question, recommendation string) map[string]string {
		return map[string]string{"question": question, "recommendation": recommendation}
	}
	bird := func(name, written string, items ...map[string]string) map[string]any {
		b := map[string]any{"bird": name, "state": "fresh", "written": written}
		if len(items) > 0 {
			b["decision_items"] = items
		}
		return b
	}

	albatross := bird("albatross", older, item("old question?", "old answer"))
	albatross["decision_items_withheld"] = []string{"credential-assignment"}
	state := map[string]any{"birds": []map[string]any{
		albatross,
		bird("crane", newer, item("crane question?", "crane answer")),
		bird("dunlin", newer, item("ship it?", "yes"), item("and the other?", "later")),
		bird("egret", older),
		bird("heron", newer),
	}}

	got := drawBirdsInDOM(t, state)
	if len(got) != 1 {
		t.Fatalf("drawBirds drew %d Needs you sections, want 1", len(got))
	}
	ny := got[0]
	if ny.Class != "needsyou" {
		t.Errorf("the section's class is %q, want needsyou", ny.Class)
	}
	if !ny.Open {
		t.Error("the Needs you section is drawn closed, want it open when a reporter has an item to put")
	}
	if ny.Summary != "Needs you (4)" {
		t.Errorf("the summary reads %q, want every reporter's item counted", ny.Summary)
	}

	var names []string
	for _, g := range ny.Groups {
		names = append(names, g.Parts[0])
	}
	if want := []string{"crane", "dunlin", "albatross"}; !slices.Equal(names, want) {
		t.Errorf("the groups are %v, want %v: newest reporter first, by name on a tie", names, want)
	}
	if want := []string{"dunlin", "ship it?", "→ yes", "and the other?", "→ later"}; !slices.Equal(ny.Groups[1].Parts, want) {
		t.Errorf("dunlin's group reads %q, want %q", ny.Groups[1].Parts, want)
	}
	last := ny.Groups[2].Parts
	if len(last) != 4 || last[3] != "withheld: a decision item looks like it holds a secret (credential-assignment)" {
		t.Errorf("albatross's group reads %q, want the withheld item named after its clean one", last)
	}

	// No reporter with an item, no section: the panel is the rows it was.
	none := drawBirdsInDOM(t, map[string]any{"birds": []map[string]any{
		bird("egret", older),
		bird("heron", newer),
	}})
	if len(none) != 0 {
		t.Errorf("drawBirds drew %d Needs you sections for reporters with no items, want none", len(none))
	}
}

// A stale reporter's group sorts after every fresh one however recently it
// wrote, says when its last report was written, and is counted apart in the
// header; a list of only stale items reads "Needs you (0, M stale)" (gt-9t6lr).
func TestBirdsPanelNeedsYouStale(t *testing.T) {
	t.Parallel()

	old := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	mid := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC).Format(time.RFC3339)
	recent := time.Date(2026, 11, 1, 11, 0, 0, 0, time.UTC).Format(time.RFC3339)
	asking := func(name, state, written string) map[string]any {
		b := map[string]any{"bird": name, "state": state, "written": written,
			"decision_items": []map[string]string{{"question": name + " question?", "recommendation": "yes"}}}
		if state == "stale" {
			b["stale"] = true
		}
		return b
	}

	// The stale bird wrote last, and still comes after both fresh ones.
	state := map[string]any{"birds": []map[string]any{
		asking("albatross", "stale", recent),
		asking("crane", "fresh", old),
		asking("dunlin", "fresh", mid),
	}}
	ny := drawBirdsInDOM(t, state)[0]
	if ny.Summary != "Needs you (2, 1 stale)" {
		t.Errorf("the summary reads %q, want the fresh items counted and the stale one named", ny.Summary)
	}
	var names []string
	for _, g := range ny.Groups {
		names = append(names, g.Parts[0])
	}
	if want := []string{"dunlin", "crane", "albatross"}; !slices.Equal(names, want) {
		t.Errorf("the groups are %v, want %v: every fresh group before the stale one", names, want)
	}
	for _, g := range ny.Groups[:2] {
		if len(g.Parts) != 3 {
			t.Errorf("a fresh group reads %q, want its name, question and recommendation only", g.Parts)
		}
	}
	stale := ny.Groups[2]
	if len(stale.Parts) != 4 || stale.Parts[1] != staleLine(t, recent) {
		t.Fatalf("the stale group reads %q, want its name, the stale line dated and timed to the report, then its question", stale.Parts)
	}
	if stale.Classes[1] != "warnc" {
		t.Errorf("the stale line is drawn %q, want warnc, the amber an overdue report carries", stale.Classes[1])
	}

	// Only stale items: the fresh count is 0 and the header still says so.
	only := drawBirdsInDOM(t, map[string]any{"birds": []map[string]any{asking("albatross", "stale", old)}})[0]
	if only.Summary != "Needs you (0, 1 stale)" {
		t.Errorf("the summary reads %q, want Needs you (0, 1 stale)", only.Summary)
	}

	// A stale reporter's withheld note is grouped the same way.
	held := asking("albatross", "stale", recent)
	held["decision_items"] = nil
	held["decision_items_withheld"] = []string{"credential-assignment"}
	got := drawBirdsInDOM(t, map[string]any{"birds": []map[string]any{held, asking("crane", "fresh", old)}})[0]
	if got.Groups[0].Parts[0] != "crane" || got.Groups[1].Parts[0] != "albatross" || got.Groups[1].Parts[1] != staleLine(t, recent) {
		t.Errorf("the groups read %+v, want crane first and albatross's stale withheld group after it", got.Groups)
	}

	// A stale reporter with no report time says so where the date would be,
	// and the line is drawn as the stale line it is.
	undated := drawBirdsInDOM(t, map[string]any{"birds": []map[string]any{asking("egret", "stale", "")}})[0]
	if group := undated.Groups[0]; len(group.Parts) != 4 || group.Parts[1] != "stale: no report time" || group.Classes[1] != "warnc" {
		t.Errorf("an undated stale group reads %q in %q, want the stale line saying no report time, drawn warnc", group.Parts, group.Classes)
	}
}

// staleLine is the stale line the page draws for a report written at iso: the
// date and the time the page's own formatting gives that instant. The expected
// text is read off the page, so it holds in whatever zone the test runs in.
func staleLine(t *testing.T, iso string) string {
	t.Helper()

	dom := newPageDOM(t)
	return "stale: last report " + pageExpr(t, dom,
		`new Date("`+iso+`").toLocaleDateString([], {month: "short", day: "numeric"}) + " " + clock("`+iso+`").slice(0, 5)`)
}

// A reporter whose every decision item was withheld still reaches the combined
// list: its text is gone, so the note that one was withheld is the only thing
// the operator would otherwise never see.
func TestBirdsPanelNeedsYouWithheldOnly(t *testing.T) {
	t.Parallel()

	written := time.Date(2026, 11, 1, 11, 0, 0, 0, time.UTC).Format(time.RFC3339)
	state := map[string]any{"birds": []map[string]any{
		{"bird": "fulmar", "state": "fresh", "written": written,
			"decision_items_withheld": []string{"credential-assignment"}},
		{"bird": "heron", "state": "fresh", "written": written},
	}}

	got := drawBirdsInDOM(t, state)
	if len(got) != 1 {
		t.Fatalf("drawBirds drew %d Needs you sections, want 1 for the reporter whose item was withheld", len(got))
	}
	ny := got[0]
	if ny.Summary != "Needs you (1 withheld)" {
		t.Errorf("the summary reads %q, want the withheld count when no item can be shown", ny.Summary)
	}
	if len(ny.Groups) != 1 || len(ny.Groups[0].Parts) != 2 {
		t.Fatalf("the section drew %+v, want one group naming fulmar and its withheld item", ny.Groups)
	}
	if ny.Groups[0].Parts[0] != "fulmar" {
		t.Errorf("the group names %q, want fulmar", ny.Groups[0].Parts[0])
	}
	if want := "withheld: a decision item looks like it holds a secret (credential-assignment)"; ny.Groups[0].Parts[1] != want {
		t.Errorf("the group reads %q, want %q", ny.Groups[0].Parts[1], want)
	}
}

// The Birds panel is a top-level section, first among the panels, and a reader
// with no saved choice sees it closed: its header still counts the birds and
// the ones that need the operator, so a closed panel says whether anything
// waits (gt-0g6z9, gt-gm9lq).
func TestBirdsPanelClosedByDefault(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	keys := dom.foldKeys(t)
	birdsAt := slices.Index(keys, "birds")
	if birdsAt < 0 {
		t.Fatalf("the page's sections are %v, want a Birds panel among them", keys)
	}
	if birdsAt != 0 {
		t.Errorf("the page's sections are %v, want the Birds panel first among the panels", keys)
	}
	wantFold(t, dom.fold(t, "birds"), "birds", true)

	// A reading of eleven birds, eight of them asking for the operator.
	stamp := time.Now().UTC().Format(time.RFC3339)
	names := []string{"overseer", "dunlin", "heron", "curlew", "egret", "kestrel", "merlin", "osprey", "plover", "redshank", "sanderling"}
	rows := make([]any, 0, len(names))
	for i, name := range names {
		rows = append(rows, map[string]any{"bird": name, "state": "fresh", "written": stamp, "needs_operator": i < 8})
	}
	pushState(t, dom, map[string]any{
		"birds": rows,
		"reads": map[string]any{"birds": map[string]any{"at": stamp, "every_sec": 60}},
	})

	// The heading's note is what dashboard-watch.sh reads a section's staleness
	// from, and the folded header shows its text in the note's place.
	fold := dom.fold(t, "birds")
	wantFold(t, fold, "birds", true)
	if fold.NoteHidden == nil {
		t.Fatal("the Birds heading carries no .r note span")
	}
	for _, want := range []string{"11 birds", "8 need you"} {
		if !strings.Contains(fold.Summary, want) {
			t.Errorf("the closed panel's header reads %q, want it to say %q", fold.Summary, want)
		}
	}
	if note := dom.element(t, "birdsnote").Text; !strings.Contains(note, "11 birds") || !strings.Contains(note, "8 need you") {
		t.Errorf("the panel's note reads %q, want the bird count and the need-you count", note)
	}
}

// The combined Needs you list starts open when at least one reporter has an
// item to put — the operator meets the question without opening anything — and
// is left out whole when none has.
func TestBirdsNeedsYouOpen(t *testing.T) {
	t.Parallel()

	written := time.Date(2026, 11, 1, 11, 0, 0, 0, time.UTC).Format(time.RFC3339)
	withItems := drawBirdsInDOM(t, map[string]any{"birds": []map[string]any{
		{"bird": "dunlin", "state": "fresh", "written": written,
			"decision_items": []map[string]string{{"question": "ship it?", "recommendation": "yes"}}},
		{"bird": "heron", "state": "fresh", "written": written},
	}})
	if len(withItems) != 1 {
		t.Fatalf("the panel drew %d Needs you sections for a reporter with an item, want 1", len(withItems))
	}
	if !withItems[0].Open {
		t.Error("the Needs you section is drawn closed, want it open when a reporter has an item")
	}
	if withItems[0].Summary != "Needs you (1)" {
		t.Errorf("the summary reads %q, want %q", withItems[0].Summary, "Needs you (1)")
	}

	none := drawBirdsInDOM(t, map[string]any{"birds": []map[string]any{
		{"bird": "heron", "state": "fresh", "written": written},
	}})
	if len(none) != 0 {
		t.Errorf("the panel drew %d Needs you sections for reporters with no item, want none", len(none))
	}
}

// A reporter whose summary the reader marked stale keeps its amber marker and
// sorts after the fresh reporters, in the reader's order within each group: a
// bird that wrote today is never pushed down by one a day behind (gt-0g6z9).
func TestBirdsStaleLast(t *testing.T) {
	t.Parallel()

	stamp := time.Now().UTC().Format(time.RFC3339)
	dom := newPageDOM(t)
	dom.call(t, "renderBirds", map[string]any{
		"birds": []any{
			map[string]any{"bird": "heron", "state": "stale", "written": stamp, "stale": true},
			map[string]any{"bird": "dunlin", "state": "fresh", "written": stamp},
			map[string]any{"bird": "curlew", "state": "stale", "written": stamp, "stale": true},
			map[string]any{"bird": "egret", "state": "fresh", "written": stamp},
		},
		"reads": map[string]any{"birds": map[string]any{"at": stamp, "every_sec": 60}},
	})

	rows := birdRows(t, dom)
	if len(rows) != 4 {
		t.Fatalf("the panel drew %d rows, want the reader's four: %+v", len(rows), rows)
	}
	var names []string
	for _, r := range rows {
		names = append(names, r.Head[0].Text)
	}
	if want := []string{"dunlin", "egret", "heron", "curlew"}; !slices.Equal(names, want) {
		t.Errorf("the rows read %v, want the fresh reporters first in the reader's order: %v", names, want)
	}
	for i, r := range rows {
		wantClass := "brow"
		if i >= 2 {
			wantClass = "brow warnc"
		}
		if r.Class != wantClass {
			t.Errorf("row %d (%s) wears %q, want %q", i, r.Head[0].Text, r.Class, wantClass)
		}
	}
}

// A stale report is drawn as a stale one and not as an ask: its row carries a
// STALE chip beside the age of its last report, no "needs the operator" badge
// even when the sidecar set one, and no place in the heading's need count. A
// fresh asking row keeps its badge and its count. The row reads as stale from
// either the flag or the state, so a feed carrying only one still does
// (gt-59ft9w).
func TestBirdsStaleReportIsNotAnAsk(t *testing.T) {
	t.Parallel()

	stamp := time.Now().UTC().Format(time.RFC3339)
	written := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	dom := newPageDOM(t)
	dom.call(t, "renderBirds", map[string]any{
		"birds": []any{
			map[string]any{"bird": "dunlin", "state": "fresh", "written": written, "needs_operator": true},
			map[string]any{"bird": "heron", "state": "stale", "stale": true, "written": written, "needs_operator": true},
			map[string]any{"bird": "curlew", "state": "stale", "written": written, "needs_operator": true},
		},
		"reads": map[string]any{"birds": map[string]any{"at": stamp, "every_sec": 60}},
	})

	rows := birdRows(t, dom)
	if len(rows) != 3 {
		t.Fatalf("the panel drew %d rows, want the reader's three: %+v", len(rows), rows)
	}
	var names []string
	for _, r := range rows {
		names = append(names, r.Head[0].Text)
	}
	if want := []string{"dunlin", "heron", "curlew"}; !slices.Equal(names, want) {
		t.Fatalf("the rows read %v, want the fresh row before the stale ones: %v", names, want)
	}

	// The fresh asking row is unchanged: its badge, then the age of its report.
	wantFresh := []birdDrawnCell{
		{Tag: "SPAN", Text: "dunlin", Class: "bname"},
		{Tag: "SPAN", Text: "needs the operator", Class: "badge warnc"},
		{Tag: "SPAN", Text: "written 2m ago", Class: "bage"},
	}
	if !slices.Equal(rows[0].Head, wantFresh) {
		t.Errorf("the fresh asking row reads %+v, want its badge and age as before: %+v", rows[0].Head, wantFresh)
	}

	for _, r := range rows[1:] {
		want := []birdDrawnCell{
			{Tag: "SPAN", Text: r.Head[0].Text, Class: "bname"},
			{Tag: "SPAN", Text: "STALE", Class: "badge warnc"},
			{Tag: "SPAN", Text: "written 2m ago", Class: "bage"},
		}
		if !slices.Equal(r.Head, want) {
			t.Errorf("the stale row %s reads %+v, want its STALE chip and age with no asking badge: %+v",
				r.Head[0].Text, r.Head, want)
		}
	}

	// The heading counts only the fresh ask; the stale rows are still birds.
	note := dom.element(t, "birdsnote").Text
	if !strings.Contains(note, "3 birds") || !strings.Contains(note, "1 need you") {
		t.Errorf("the heading reads %q, want the three birds and the one fresh ask counted", note)
	}
}

// The previous link carries the age of the page it opens when the reader has
// its own written instant, so the label names no day it cannot vouch for
// (gt-59ft9w).
func TestBirdsPreviousLinkCarriesItsAge(t *testing.T) {
	t.Parallel()

	stamp := time.Now().UTC()
	dom := newPageDOM(t)
	dom.call(t, "renderBirds", map[string]any{
		"birds": []any{
			map[string]any{"bird": "dunlin", "state": "fresh", "written": stamp.Format(time.RFC3339),
				"has_prev": true, "prev_written": stamp.Add(-50 * time.Hour).Format(time.RFC3339)},
			map[string]any{"bird": "heron", "state": "fresh", "written": stamp.Format(time.RFC3339),
				"has_prev": true},
		},
		"reads": map[string]any{"birds": map[string]any{"at": stamp.Format(time.RFC3339), "every_sec": 60}},
	})

	rows := birdRows(t, dom)
	if len(rows) != 2 {
		t.Fatalf("the panel drew %d rows, want the reader's two: %+v", len(rows), rows)
	}
	previous := func(row birdDrawnRow) (birdDrawnCell, bool) {
		for _, c := range row.Head {
			if c.Class == "bprev" {
				return c, true
			}
		}
		return birdDrawnCell{}, false
	}
	aged, ok := previous(rows[0])
	if !ok {
		t.Fatalf("the row with a previous page drew no link: %+v", rows[0].Head)
	}
	if aged.Text != "previous" || aged.Title != "written 2d ago" {
		t.Errorf("the previous link reads %q with hover %q, want \"previous\" and \"written 2d ago\" for a page 50 hours old", aged.Text, aged.Title)
	}
	plain, ok := previous(rows[1])
	if !ok {
		t.Fatalf("the row with a previous page drew no link: %+v", rows[1].Head)
	}
	if plain.Title != "" {
		t.Errorf("the previous link with no written instant carries hover %q, want none", plain.Title)
	}
}

// The report the reader marks as the town's headline leads the panel even when
// it is stale, and the other reporters keep the fresh-before-stale order. A
// reporter's name earns no place of its own: the row named overseer below is
// not the marked one and sorts with the rest (gt-gm9lq, gt-5c4df).
func TestBirdsHeadlineFirst(t *testing.T) {
	t.Parallel()

	stamp := time.Now().UTC().Format(time.RFC3339)
	dom := newPageDOM(t)
	dom.call(t, "renderBirds", map[string]any{
		"birds": []any{
			map[string]any{"bird": "heron", "state": "fresh", "written": stamp},
			map[string]any{"bird": "overseer", "state": "fresh", "written": stamp},
			map[string]any{"bird": "dunlin", "state": "stale", "written": stamp, "stale": true, "headline": true},
			map[string]any{"bird": "curlew", "state": "stale", "written": stamp, "stale": true},
		},
		"reads": map[string]any{"birds": map[string]any{"at": stamp, "every_sec": 60}},
	})

	rows := birdRows(t, dom)
	if len(rows) != 4 {
		t.Fatalf("the panel drew %d rows, want the reader's four: %+v", len(rows), rows)
	}
	var names []string
	for _, r := range rows {
		names = append(names, r.Head[0].Text)
	}
	if want := []string{"dunlin", "heron", "overseer", "curlew"}; !slices.Equal(names, want) {
		t.Errorf("the rows read %v, want the marked headline first and the others fresh before stale: %v", names, want)
	}
	if rows[0].Class != "brow warnc" {
		t.Errorf("the stale headline's row wears %q, want its amber mark kept", rows[0].Class)
	}
}

// A bird's name is the link to its report, opened in a new tab, and a bird that
// wrote no page keeps its name as plain text (gt-gm9lq).
func TestBirdsNameLinksToReport(t *testing.T) {
	t.Parallel()

	stamp := time.Now().UTC().Format(time.RFC3339)
	dom := newPageDOM(t)
	dom.call(t, "renderBirds", map[string]any{
		"birds": []any{
			map[string]any{"bird": "dunlin", "state": "fresh", "written": stamp, "summary": "the day in one line", "has_page": true},
			map[string]any{"bird": "heron", "state": "fresh", "written": stamp, "summary": "wrote no page"},
		},
		"reads": map[string]any{"birds": map[string]any{"at": stamp, "every_sec": 60}},
	})

	rows := birdRows(t, dom)
	if len(rows) != 2 {
		t.Fatalf("the panel drew %d rows, want the reader's two: %+v", len(rows), rows)
	}
	want := birdDrawnCell{Tag: "A", Text: "dunlin", Class: "bname", Href: "/birds/dunlin/daily.html", Target: "_blank", Rel: "noopener"}
	if len(rows[0].Head) == 0 || rows[0].Head[0] != want {
		t.Errorf("the bird's name cell reads %+v, want the name as a new-tab link to its report: %+v", rows[0].Head, want)
	}
	if len(rows[1].Head) == 0 {
		t.Fatalf("the bird without a page drew no cells: %+v", rows[1])
	}
	if cell := rows[1].Head[0]; cell != (birdDrawnCell{Tag: "SPAN", Text: "heron", Class: "bname"}) {
		t.Errorf("a bird without a page draws its name as %+v, want plain text", cell)
	}
}

// The links beside the name read "report" and "previous", and a bird that
// wrote no previous page has no "previous" link (gt-gm9lq, gt-59ft9w).
func TestBirdsReportAndPreviousLabels(t *testing.T) {
	t.Parallel()

	stamp := time.Now().UTC().Format(time.RFC3339)
	dom := newPageDOM(t)
	dom.call(t, "renderBirds", map[string]any{
		"birds": []any{
			map[string]any{"bird": "dunlin", "state": "fresh", "written": stamp, "has_page": true, "has_prev": true},
			map[string]any{"bird": "heron", "state": "fresh", "written": stamp, "has_page": true},
			map[string]any{"bird": "curlew", "state": "fresh", "written": stamp},
		},
		"reads": map[string]any{"birds": map[string]any{"at": stamp, "every_sec": 60}},
	})

	rows := birdRows(t, dom)
	if len(rows) != 3 {
		t.Fatalf("the panel drew %d rows, want the reader's three: %+v", len(rows), rows)
	}
	wantReport := birdDrawnCell{Tag: "A", Text: "report", Class: "bpage", Href: "/birds/dunlin/daily.html", Target: "_blank", Rel: "noopener"}
	wantPrevious := birdDrawnCell{Tag: "A", Text: "previous", Class: "bprev", Href: "/birds/dunlin/daily.prev.html", Target: "_blank", Rel: "noopener"}
	if len(rows[0].Head) < 3 || rows[0].Head[1] != wantReport || rows[0].Head[2] != wantPrevious {
		t.Errorf("the row with both pages reads %+v, want the report and previous links after the name", rows[0].Head)
	}
	wantHeronReport := birdDrawnCell{Tag: "A", Text: "report", Class: "bpage", Href: "/birds/heron/daily.html", Target: "_blank", Rel: "noopener"}
	if len(rows[1].Head) < 2 || rows[1].Head[1] != wantHeronReport {
		t.Errorf("the row with only a report reads %+v, want the report link after the name", rows[1].Head)
	}
	for _, c := range rows[1].Head {
		if c.Class == "bprev" {
			t.Errorf("a bird with no previous page drew the link %+v, want none", c)
		}
	}
	for _, c := range rows[2].Head {
		if c.Class == "bpage" || c.Class == "bprev" {
			t.Errorf("a bird that wrote no page drew the link %+v, want no page link at all", c)
		}
	}
}

// A bird's topic pages are drawn as links under its row, newest first as the
// reader listed them, to the sandboxed route; a row with none draws no list, and
// a name is encoded as one path segment, so it cannot climb out of the bird's
// directory even when the reader did not list it (gt-ylswr).
func TestBirdsPanelDrawsTopicLinks(t *testing.T) {
	t.Parallel()

	stamp := time.Now().UTC().Format(time.RFC3339)
	dom := newPageDOM(t)
	dom.call(t, "renderBirds", map[string]any{
		"birds": []any{
			map[string]any{"bird": "heron", "state": "fresh", "written": stamp, "pages": []any{
				map[string]any{"name": "pool-cutover.html", "title": "The pool <b>cutover</b>", "mtime": stamp},
				map[string]any{"name": "older.html", "title": "", "mtime": stamp},
				map[string]any{"name": "../daily.json", "title": "a name that climbs", "mtime": stamp},
			}},
			map[string]any{"bird": "quiet", "state": "fresh", "written": stamp},
		},
		"reads": map[string]any{"birds": map[string]any{"at": stamp, "every_sec": 60}},
	})

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const out = {};
		__walk(__byId["birds"], n => {
			if (!__cls(n, "brow")) return;
			const name = n.children[0].children[0].textContent;
			const list = __find(n, ".bpages");
			out[name] = list ? list.children.__arr.map(a => ({tag: a.tagName, text: a.textContent, href: a.href || "", target: a.target || "", rel: a.rel || "", title: a.title || ""})) : null;
		});
		return out;
	})())`)
	if err != nil {
		t.Fatalf("reading the topic links: %v", err)
	}
	var got map[string][]struct {
		Tag    string `json:"tag"`
		Text   string `json:"text"`
		Href   string `json:"href"`
		Target string `json:"target"`
		Rel    string `json:"rel"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("reading the topic links: %v", err)
	}

	links := got["heron"]
	if len(links) != 3 {
		t.Fatalf("heron drew %d topic links, want the three the reader listed: %+v", len(links), links)
	}
	if links[2].Href != "/birds/heron/..%2Fdaily.json" {
		t.Errorf("a name that climbs is drawn as %q, want one encoded path segment under the bird", links[2].Href)
	}
	if links[0].Tag != "A" || links[0].Href != "/birds/heron/pool-cutover.html" || links[0].Target != "_blank" || links[0].Rel != "noopener" {
		t.Errorf("heron's first link is %+v, want an anchor to the route, in a new tab, noopener", links[0])
	}
	if links[0].Text != "The pool <b>cutover</b>" {
		t.Errorf("heron's first link reads %q, want the title as text, not markup", links[0].Text)
	}
	if links[1].Text != "older.html" || links[1].Href != "/birds/heron/older.html" {
		t.Errorf("heron's second link is %+v, want the page's name when it has no title", links[1])
	}
	if !strings.HasPrefix(links[0].Title, "written ") {
		t.Errorf("heron's first link carries the hover %q, want when it was written", links[0].Title)
	}
	if list, drawn := got["quiet"]; !drawn || list != nil {
		t.Errorf("quiet drew %+v (row drawn: %v), want a row with no topic list", list, drawn)
	}
}
