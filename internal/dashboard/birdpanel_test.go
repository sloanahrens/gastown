package dashboard

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// A birds read that returns no rows is a read that failed, not a town holding
// no birds: the rows it last read stay on the page, and the read is flagged so
// the block ages them and says so instead of drawing an empty list (gt-q6h8e).
func TestBirdPanelPollKeepsTheLastRowsOnAFailedRead(t *testing.T) {
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

	// The next read returns no rows. Two birds cannot become none without the
	// runtime directory being unreadable, so the rows stand and the read is
	// what is marked.
	read = nil
	h.pollBirds()
	st = h.State()
	if len(st.Birds) != 2 || st.Birds[0].Bird != "dunlin" || st.Birds[1].Bird != "heron" {
		t.Fatalf("a read with no rows dropped the last rows: %+v", st.Birds)
	}
	if r := st.Reads[SourceBirds]; !r.Error || r.At.IsZero() {
		t.Errorf("a read with no rows is not flagged with its last good age: %+v", r)
	}

	// A hub with no birds reader starts no poller: the run returns without a
	// read, and the poll is a no-op rather than a read of nothing.
	h2 := NewHub(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h2.Run(ctx)
	h2.pollBirds()
	st = h2.State()
	if st.Birds != nil {
		t.Errorf("a hub with no birds reader drew rows: %+v", st.Birds)
	}
	if _, ok := st.Reads[SourceBirds]; ok {
		t.Errorf("a hub with no birds reader recorded a read: %+v", st.Reads[SourceBirds])
	}
}

// The block draws the row the reader decided: every field through el(), which
// writes textContent, the summary and the age of it, the amber mark on a stale
// row, the note in the alarm class for a row in error or withheld, the badge a
// bird asks for the operator with, and the two pages it wrote as links that
// open in a new tab. The page's own helper words the age, so the block and the
// digest say "written … ago" alike.
func TestBirdPanelDrawsAgeStaleSummaryAndLinks(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "drawBirds")
	for _, want := range []string{
		`const rows = s.birds || [];`,
		`const row = el("div", "brow" + (b.stale ? " warnc" : ""));`,
		`if (b.state === "error" || b.state === "withheld") row.append(el("div", "badc", b.note || b.state));`,
		`else if (b.summary) row.append(el("div", "bsum", b.summary));`,
		`if (b.needs_operator) head.append(el("span", "badge warnc", "needs the operator"));`,
		`head.append(el("span", "bage", "written " + age(b.written) + " ago"));`,
		`page.href = "/birds/" + b.bird + "/daily.html";`,
		`page.target = "_blank";`,
		`page.rel = "noopener";`,
		`if (b.has_prev) {`,
		`prev.href = "/birds/" + b.bird + "/daily.prev.html";`,
		`no bird has written a daily report yet`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("drawBirds has no %q", want)
		}
	}
	if strings.Contains(draw, "innerHTML") {
		t.Error("drawBirds sets markup from a bird's text")
	}

	// The block is drawn on the Overseer panel the way the digest is, from the
	// reading it draws: a push that moves s.birds or its read status draws it.
	page := string(indexHTML)
	if !strings.Contains(page, "drawBirds(box, s);") {
		t.Error("renderOverseer never draws the Birds block")
	}
	if !strings.Contains(page, `s.birds,`) || !strings.Contains(page, `readInput(s, "birds")`) {
		t.Error("the overseer pane's read list leaves out the birds or their read status")
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
