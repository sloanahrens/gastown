package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var deepReviewPageNow = time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)

// deepReviewState is the reading the Deep review block is drawn from: the
// payload under the panel's state, with nothing else configured.
func deepReviewState(d map[string]any) map[string]any {
	return map[string]any{"deepreview": d}
}

// The Deep review block draws one row per configured schedule: the schedule's
// own name, when it is next due, and the newest run it filed with what that run
// counted — so a schedule the config adds beside the review's slice and risk
// paths is a row the moment it exists, which is what the reader sends
// (gt-uv08f).
func TestDeepReviewBlockDrawsARowPerConfiguredSchedule(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderOverseer", deepReviewState(map[string]any{
		"schedules": []any{
			map[string]any{"name": "deep-review-slice", "rig": "gastown", "next": "in 3h 20m",
				"last": map[string]any{"id": "gt-r01", "status": "closed", "findings": 3, "filed": 2, "digest": "gt-d01 · 4 comments"}},
			map[string]any{"name": "deep-review-risk", "rig": "gastown", "next": "due now",
				"last": map[string]any{"id": "gt-r02", "status": "open", "findings": 1, "filed": 0, "unparsed": true}},
			map[string]any{"name": "agentlab", "rig": "gastown", "next": "gated: runs when needed"},
		},
	}))

	rows := blockTable(t, dom, "Deep review")
	if len(rows) != 4 {
		t.Fatalf("the block drew %d rows, want a header and the three schedules: %v", len(rows), rows)
	}
	want := [][]string{
		{"Schedule", "Next", "Last run", "Findings", "Filed", "Digest"},
		{"deep-review-slice", "in 3h 20m", "gt-r01", "3", "2", "gt-d01 · 4 comments"},
		{"deep-review-risk", "due now", "gt-r02 open unparsed", "1", "0", "–"},
		{"agentlab", "gated: runs when needed", "–", "–", "–", "–"},
	}
	for i, w := range want {
		if len(rows[i]) != len(w) {
			t.Fatalf("row %d drew %v, want %v", i, rows[i], w)
		}
		for j := range w {
			if rows[i][j] != w[j] {
				t.Errorf("row %d cell %d = %q, want %q", i, j, rows[i][j], w[j])
			}
		}
	}
	// The rows are the schedules the town configures, and the header above them
	// is the six cells the block draws.
	if block := blockNamed(t, overseerBlocks(t, dom), "Deep review"); !strings.Contains(block.Text, "Last run") {
		t.Errorf("the block's rows carry no run: %q", block.Text)
	}
}

// A schedule's row says which of the reader's states its next-run cell carries,
// so a held town and a schedule with no usable interval are told apart from one
// that is simply waiting.
func TestDeepReviewBlockNumbersTheStatesNextToTheirRows(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderOverseer", deepReviewState(map[string]any{
		"paused": "operator dispatch hold present",
		"schedules": []any{
			map[string]any{"name": "deep-review-slice", "next": "running"},
			map[string]any{"name": "deep-review-risk", "next": "not scheduled"},
		},
	}))

	block := blockNamed(t, overseerBlocks(t, dom), "Deep review")
	if block.Note != "paused" {
		t.Errorf("the block's heading note reads %q, want the hold named there", block.Note)
	}
	if !strings.Contains(block.Text, "paused: operator dispatch hold present") {
		t.Errorf("the block does not name the hold: %q", block.Text)
	}
	// A held town starts no run whatever an interval says, so its rows are
	// still drawn: the schedules and their last runs are facts of their own.
	if rows := blockTable(t, dom, "Deep review"); len(rows) != 3 {
		t.Errorf("a held town drew %v, want the schedules still listed", rows)
	}
}

// A read that failed, and a town that configures no schedule at all, are named
// rather than drawn as an empty block: "no deep review has run" and "the runs
// could not be read" must not look alike.
func TestDeepReviewBlockNamesWhatItCouldNotRead(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		payload map[string]any
		want    string
	}{
		"unavailable":  {map[string]any{"unavailable": true}, "unavailable: deep reviews could not be read"},
		"reading":      {map[string]any{"schedules": []any{}}, "no deep review schedule configured"},
		"not reported": {nil, "reading…"},
	} {
		dom := newPageDOM(t)
		state := map[string]any{}
		if tc.payload != nil {
			state["deepreview"] = tc.payload
		}
		dom.call(t, "renderOverseer", state)
		block := blockNamed(t, overseerBlocks(t, dom), "Deep review")
		if !strings.Contains(block.Text, tc.want) {
			t.Errorf("%s: the block reads %q, want %q", name, block.Text, tc.want)
		}
	}
}

// The block is drawn inside one section — the panel's — and nothing the reader
// sends is written as markup: a schedule's name and a run's bead are text.
func TestDeepReviewBlockIsDrawnInThePanelAndSetsNoMarkup(t *testing.T) {
	t.Parallel()

	if all := pageFunc(t, "renderAll"); !strings.Contains(all, "renderOverseer(state)") {
		t.Error("renderAll does not draw the Overseer panel")
	}
	draw := pageFunc(t, "drawDeepReview")
	for _, bad := range []string{"innerHTML", "insertAdjacentHTML", "outerHTML", "document.write"} {
		if strings.Contains(draw, bad) {
			t.Errorf("drawDeepReview uses %s", bad)
		}
	}
	if !strings.Contains(string(indexHTML), `<section data-fold="overseer"><h2>Overseer <span class="r" id="overseernote"></span></h2><div class="body" id="overseer"></div></section>`) {
		t.Error("the Overseer section is not the markup its neighbours are")
	}
	// The four panels this one replaces are gone from the markup, and with them
	// the ids a watcher or a bookmark could still be holding.
	html := string(indexHTML)
	for _, id := range []string{`id="deepreview"`, `id="deepreviewnote"`, `id="report"`, `id="reportnote"`, `id="disp"`, `id="dispnote"`} {
		if strings.Contains(html, id) {
			t.Errorf("index.html still carries %s", id)
		}
	}
}

// The state the page reads carries the block, and a read that failed reaches it
// as itself: /api/state serves the unavailable flag, not an empty list.
func TestDeepReviewReachesTheAPI(t *testing.T) {
	t.Parallel()

	reads := []*DeepReview{
		{Schedules: []DeepReviewSchedule{{Name: "deep-review-slice", Next: "in 3h 20m",
			Last: &DeepReviewRun{ID: "gt-run1", Status: DeepReviewClosed, Findings: 2, Filed: 1}}}},
		{Unavailable: true},
	}
	for _, d := range reads {
		h := NewHub(Config{
			Now:        func() time.Time { return deepReviewPageNow },
			DeepReview: func() *DeepReview { return d },
		})
		h.pollDeepReview()

		req := httptest.NewRequest("GET", "/api/state", nil)
		req.Host = "127.0.0.1:8787"
		rec := httptest.NewRecorder()
		h.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/state = %d", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `"deepreview"`) {
			t.Fatalf("/api/state does not carry the deep review the hub holds: %s", body)
		}
		if got := strings.Contains(body, `"unavailable":true`); got != d.Unavailable {
			t.Errorf("/api/state unavailable = %v, want %v: %s", got, d.Unavailable, body)
		}
		if st := h.State(); st.DeepReview != d {
			t.Errorf("the hub holds %+v, want the reader's %+v", st.DeepReview, d)
		}
	}
}

// A reader that reports nothing leaves the state as it was; an unconfigured hub
// has no payload at all.
func TestDeepReviewNilReadKeepsTheLastState(t *testing.T) {
	t.Parallel()

	var next *DeepReview
	h := NewHub(Config{DeepReview: func() *DeepReview { return next }})
	h.pollDeepReview()
	if h.State().DeepReview != nil {
		t.Fatal("a nil read set the payload")
	}
	next = &DeepReview{Schedules: []DeepReviewSchedule{{Name: "deep-review-slice", Next: "due now"}}}
	h.pollDeepReview()
	next = nil
	h.pollDeepReview()
	if got := h.State().DeepReview; got == nil || len(got.Schedules) != 1 || got.Schedules[0].Next != "due now" {
		t.Errorf("a nil read replaced the last state with %+v", got)
	}
}

// The poll defaults to a minute.
func TestDeepReviewEveryDefaultsToAMinute(t *testing.T) {
	t.Parallel()

	var c Config
	c.defaults()
	if c.DeepReviewEvery != 60*time.Second {
		t.Errorf("DeepReviewEvery defaults to %v, want 1m", c.DeepReviewEvery)
	}
}
