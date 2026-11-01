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

// birdDrawnCell is one element the Birds block drew: its text and class, the
// link it carries when it is one, and how many children it holds — a leaf
// written through textContent has none.
type birdDrawnCell struct {
	Text   string `json:"text"`
	Class  string `json:"class"`
	Href   string `json:"href"`
	Target string `json:"target"`
	Rel    string `json:"rel"`
	Kids   int    `json:"kids"`
}

// birdDrawnRow is one row of the Birds block: the class it wears, the cells of
// its head in the order the block appended them, and its body line when it drew
// one.
type birdDrawnRow struct {
	Class string          `json:"class"`
	Head  []birdDrawnCell `json:"head"`
	Body  *birdDrawnCell  `json:"body"`
}

// birdRows reads the rows the panel drew under the Birds heading. A panel that
// drew no such block fails here rather than reading as an empty list.
func birdRows(t *testing.T, dom *pageDOM) []birdDrawnRow {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const b = __byId["overseer"].children.find(x => x.children[0].children[0].textContent === "Birds");
		if (!b) return null;
		const cell = c => ({text: c.textContent, class: c.className, href: c.href || "", target: c.target || "", rel: c.rel || "", kids: c.children.length});
		return b.children[1].children.map(row => ({
			class: row.className,
			head: (row.children[0] ? row.children[0].children : []).map(cell),
			body: row.children[1] ? cell(row.children[1]) : null
		}));
	})())`)
	if err != nil {
		t.Fatalf("reading the Birds block's rows: %v", err)
	}
	if v.String() == "null" {
		t.Fatal("the panel drew no Birds block")
	}
	var out []birdDrawnRow
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the Birds block's rows: %v", err)
	}
	return out
}

// The block draws the row the reader decided: the name, the summary and the age
// of it, the amber mark on a stale row, a row in error named in the alert
// class, the badge a bird asks for the operator with, and the two pages it
// wrote as links that open in a new tab. Every cell is the page's own text, so
// a tag a bird wrote shows as that text rather than as markup.
func TestBirdPanelDrawsAgeStaleSummaryAndLinks(t *testing.T) {
	t.Parallel()

	written := time.Now().UTC().Add(-30 * time.Minute).Format(time.RFC3339)
	stamp := time.Now().UTC().Format(time.RFC3339)
	dom := newPageDOM(t)
	dom.call(t, "renderOverseer", map[string]any{
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
		{Text: "dunlin", Class: "bname"},
		{Text: "page", Class: "bpage", Href: "/birds/dunlin/daily.html", Target: "_blank", Rel: "noopener"},
		{Text: "previous", Class: "bprev", Href: "/birds/dunlin/daily.prev.html", Target: "_blank", Rel: "noopener"},
		{Text: "needs the operator", Class: "badge warnc"},
		{Text: "written 30m ago", Class: "bage"},
	}
	if !slices.Equal(rows[0].Head, wantHead) {
		t.Errorf("the row's head reads %+v, want %+v", rows[0].Head, wantHead)
	}
	wantBody := birdDrawnCell{Text: "the <b>day</b> in one line", Class: "bsum"}
	if rows[0].Body == nil || *rows[0].Body != wantBody {
		t.Errorf("the row's summary reads %+v, want %+v", rows[0].Body, wantBody)
	}

	// The stale bird wears the amber mark, wrote no page and no summary, so its
	// head carries only its name and the age.
	if rows[1].Class != "brow warnc" {
		t.Errorf("a stale row is drawn as %q, want the amber mark", rows[1].Class)
	}
	if len(rows[1].Head) != 2 || rows[1].Head[0].Text != "heron" || rows[1].Head[1].Class != "bage" {
		t.Errorf("the stale row's head reads %+v, want its name and age", rows[1].Head)
	}
	if rows[1].Body != nil {
		t.Errorf("the stale row drew a body line it has no words for: %+v", rows[1].Body)
	}

	// The bird the reader could not use shows what is wrong in the alert class,
	// and no age, because the reader had no instant to show.
	if rows[2].Body == nil || rows[2].Body.Class != "badc" || rows[2].Body.Text != "daily.json is not valid JSON" {
		t.Errorf("the error row's note reads %+v, want the reader's words in the alert class", rows[2].Body)
	}
	if len(rows[2].Head) != 1 || rows[2].Head[0].Text != "curlew" {
		t.Errorf("the error row's head reads %+v, want its name alone", rows[2].Head)
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
		dom.call(t, "renderOverseer", map[string]any{
			"birds": []any{},
			"reads": map[string]any{"birds": tc.status},
		})
		block := blockNamed(t, overseerBlocks(t, dom), "Birds")
		if block.Text != tc.wantText {
			t.Errorf("%s: the block reads %q, want %q", tc.name, block.Text, tc.wantText)
		}
		if classes := blockClasses(t, dom, "Birds"); !slices.Equal(classes, []string{tc.wantClass}) {
			t.Errorf("%s: the block's line is drawn as %v, want %q", tc.name, classes, tc.wantClass)
		}
		if tc.wantNote != "" && !strings.Contains(block.Note, tc.wantNote) {
			t.Errorf("%s: the block's note reads %q, want it to name %q", tc.name, block.Note, tc.wantNote)
		}
	}
}

// A row in error draws the note the reader wrote for it, and a withheld row
// with no note names its own state: the block never draws an empty alert line.
func TestBirdPanelDrawsARowsNoteOrItsState(t *testing.T) {
	t.Parallel()

	stamp := time.Now().UTC().Format(time.RFC3339)
	dom := newPageDOM(t)
	dom.call(t, "renderOverseer", map[string]any{
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
	draws := countPaneDraws(t, dom, "overseer")
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
	if note := blockNamed(t, overseerBlocks(t, dom), "Birds").Note; !strings.Contains(note, "read failed") {
		t.Errorf("the birds block's note reads %q after the failed read, want it named", note)
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
	Class string   `json:"cls"`
	Parts []string `json:"parts"`
}

// needsYouWalk reads every details element the page drew under #overseer, in
// document order, with what it shows.
const needsYouWalk = `JSON.stringify((() => {
  const out = [];
  (function walk(n) {
    for (const c of (n.children || [])) {
      if (c.tagName === "DETAILS") {
        out.push({
          cls: c.className,
          open: !!c.open,
          summary: c.children.length ? c.children[0].textContent : "",
          groups: c.children.slice(1).map(g => ({
            cls: g.className,
            parts: g.children.map(x => x.textContent)
          }))
        });
      }
      walk(c);
    }
  })(document.getElementById("overseer"));
  return out;
})())`

// drawBirdsInDOM runs the page's own drawBirds over a state and answers the
// "Needs you" sections it drew.
func drawBirdsInDOM(t *testing.T, state map[string]any) []drawnNeeds {
	t.Helper()

	dom := newPage(t, nil, "")
	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if _, err := dom.vm.RunString("drawBirds(document.getElementById(\"overseer\"), " + string(payload) + ");"); err != nil {
		t.Fatalf("drawBirds: %v", err)
	}
	v, err := dom.vm.RunString(needsYouWalk)
	if err != nil {
		t.Fatalf("reading the drawn Birds block: %v", err)
	}
	var got []drawnNeeds
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("reading the drawn Birds block: %v", err)
	}
	return got
}

// Every reporter's decisions are one list under the rows: grouped by reporter,
// the reporter that wrote most recently first (by name when two wrote in the
// same second), closed until the operator opens it, and left out whole when no
// reporter has a decision to show. The blocked reporter's dropped item is
// named as withheld without its text.
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
	if ny.Open {
		t.Error("the Needs you section is drawn open, want it closed until the operator opens it")
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
