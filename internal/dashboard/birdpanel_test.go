package dashboard

import (
	"context"
	"encoding/json"
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
