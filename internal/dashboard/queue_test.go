package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A leaked molecule step wisp is not blocked work (gt-vcr3z): it waits on a
// molecule no patrol will advance. The blocked list is split by this one
// classification, so its verdicts are pinned bead by bead.
func TestIsLeakedStepWispTellsAStepFromBlockedWork(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, id, typ, parent string
		want                  bool
	}{
		{"a bead waiting on another bead", "gt-real", "task", "", false},
		{"a step wisp by its id", "gt-wisp-4bac0", "task", "", true},
		{"a molecule poured as a wisp", "gt-wisp-eutau", "molecule", "", true},
		{"a step poured under a molecule", "gt-step-1", "task", "gt-wisp-eutau", true},
		{"a child of a durable epic", "gt-epic-child", "task", "gt-epic", false},
		{"a molecule whose parent is another", "gt-mol-1", "molecule", "gt-wisp-eutau", false},
		{"a bead with no type", "gt-x", "", "gt-wisp-eutau", false},
	} {
		if got := IsLeakedStepWisp(tc.id, tc.typ, tc.parent, nil); got != tc.want {
			t.Errorf("%s (%s/%s/%s): wisp = %v, want %v", tc.name, tc.id, tc.typ, tc.parent, got, tc.want)
		}
		if IsLiveStep(tc.id, tc.typ, tc.parent, nil) {
			t.Errorf("%s: no molecule is live, so no step is", tc.name)
		}
	}
}

// A step is leaked only when no seat runs its molecule (gt-xmys9): the later
// steps of a running polecat's checklist wait on the molecule that polecat is
// walking. The two verdicts are exclusive, and a bead that is no step is
// neither whatever is live.
func TestStepVerdictFollowsTheLiveMolecules(t *testing.T) {
	t.Parallel()
	live := map[string]bool{"gt-wisp-live": true, "gt-mol-live": true}

	for _, tc := range []struct {
		name, id, typ, parent string
		leaked, liveStep      bool
	}{
		{"a step of a running molecule", "gt-wisp-s1", "task", "gt-wisp-live", false, true},
		{"the running molecule's root", "gt-wisp-live", "molecule", "", false, true},
		{"a step of a molecule no seat runs", "gt-wisp-s2", "task", "gt-wisp-dead", true, false},
		{"a dead molecule's root", "gt-wisp-dead", "molecule", "", true, false},
		{"a step that names no molecule", "gt-wisp-s3", "task", "", true, false},
		{"blocked work", "gt-real", "task", "", false, false},
		{"a child of a durable epic", "gt-child", "task", "gt-epic", false, false},
		{"a bead under a durable molecule a seat holds", "gt-s1", "task", "gt-mol-live", false, false},
	} {
		leaked := IsLeakedStepWisp(tc.id, tc.typ, tc.parent, live)
		liveStep := IsLiveStep(tc.id, tc.typ, tc.parent, live)
		if leaked != tc.leaked || liveStep != tc.liveStep {
			t.Errorf("%s: leaked = %v, live = %v; want %v, %v", tc.name, leaked, liveStep, tc.leaked, tc.liveStep)
		}
	}
}

func TestSortQueueRowsOrdersLikeTheDispatcher(t *testing.T) {
	t.Parallel()
	d := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	rows := []QueueBead{
		{ID: "gt-c", Priority: 3, CreatedAt: d},
		{ID: "gt-b", Priority: 1, CreatedAt: d.Add(time.Hour)},
		{ID: "gt-a", Priority: 1, CreatedAt: d},
		{ID: "gt-z", Priority: 1, CreatedAt: d},
	}
	SortQueueRows(rows)
	got := rows[0].ID + rows[1].ID + rows[2].ID + rows[3].ID
	if got != "gt-agt-zgt-bgt-c" {
		t.Errorf("order = %s (priority, then oldest, then id)", got)
	}
}

func TestCapQueueRowsCapsPerStoreAndKeepsExactTotals(t *testing.T) {
	t.Parallel()
	var rows []QueueBead
	for i := 0; i < queueMaxRows+15; i++ {
		rows = append(rows, QueueBead{Rig: "hq"})
	}
	rows = append(rows, QueueBead{Rig: "gastown", ID: "gt-1"}, QueueBead{Rig: "gastown", ID: "gt-2"})
	kept, total, perRig := CapQueueRows(rows)
	hq, gastown := 0, 0
	for _, r := range kept {
		if r.Rig == "hq" {
			hq++
		} else {
			gastown++
		}
	}
	if hq != queueMaxRows || gastown != 2 {
		t.Errorf("kept hq=%d gastown=%d: a busy store must not crowd another out", hq, gastown)
	}
	if total != queueMaxRows+17 || perRig["hq"] != queueMaxRows+15 || perRig["gastown"] != 2 {
		t.Errorf("total=%d perRig=%v", total, perRig)
	}
	if empty, n, _ := CapQueueRows(nil); empty == nil || n != 0 {
		t.Errorf("an empty list must encode as [], not null: %v %d", empty, n)
	}
}

func TestClipDetailBoundsEveryField(t *testing.T) {
	t.Parallel()
	d := &BeadDetail{
		Description: strings.Repeat("d", detailDescMax+5),
		Notes:       strings.Repeat("n", detailNotesMax+5),
		Comments:    make([]DetailComment, detailComments+3),
	}
	for i := range d.Comments {
		d.Comments[i].Text = strings.Repeat("c", detailCommentMax+1)
		d.Comments[i].Author = string(rune('a' + i))
	}
	ClipDetail(d)
	if !d.Truncated {
		t.Error("a cut must be reported")
	}
	if n := len([]rune(d.Description)); n != detailDescMax+1 {
		t.Errorf("description %d runes", n)
	}
	if len(d.Comments) != detailComments || d.Comments[len(d.Comments)-1].Author != string(rune('a'+detailComments+2)) {
		t.Errorf("comments must keep the newest %d: %d", detailComments, len(d.Comments))
	}
	if short, cut := ClipText("héllo", 3); short != "hél…" || !cut {
		t.Errorf("clip is by rune: %q %v", short, cut)
	}
}

func TestValidBeadRefRefusesAnythingThatCouldBeAnArgument(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		rig, id string
		ok      bool
	}{
		{"gastown", "gt-7hgug", true}, {"hq", "hq-wisp-abc", true}, {"gastown", "gt-4k3fj.8.8", true},
		{"gastown", "", false}, {"", "gt-1", false},
		{"gastown", "--help", false}, {"gastown", "-x", false},
		{"gastown", "gt-1; rm -rf", false}, {"gastown", "gt-1 --json", false}, {"gastown", "../etc", false},
		{"../gastown", "gt-1", false}, {"gas/town", "gt-1", false}, {"-rig", "gt-1", false},
		{"gastown", strings.Repeat("a", 65), false},
	} {
		if got := ValidBeadRef(c.rig, c.id); got != c.ok {
			t.Errorf("ValidBeadRef(%q, %q) = %v, want %v", c.rig, c.id, got, c.ok)
		}
	}
}

func TestBeadDetailCachesForTheTTLAndRemembersNothingOnFailure(t *testing.T) {
	t.Parallel()
	var reads atomic.Int32
	fail := false
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	h := NewHub(Config{
		Now: func() time.Time { return now },
		Bead: func(rig, id string) (*BeadDetail, error) {
			reads.Add(1)
			if fail {
				return nil, errors.New("dolt is down: secret internals")
			}
			return &BeadDetail{ID: id, Rig: rig, Title: "t"}, nil
		},
	})
	for i := 0; i < 3; i++ {
		if d, err := h.beadDetail("gastown", "gt-1"); err != nil || d.ID != "gt-1" {
			t.Fatalf("read %d: %v %v", i, d, err)
		}
	}
	if reads.Load() != 1 {
		t.Fatalf("%d reads inside the ttl, want 1", reads.Load())
	}
	now = now.Add(detailTTL)
	h.beadDetail("gastown", "gt-1")
	if reads.Load() != 2 {
		t.Fatalf("%d reads after the ttl, want 2", reads.Load())
	}
	fail = true
	_, err := h.beadDetail("gastown", "gt-2")
	if !errors.Is(err, errDetailUnavailable) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("a read failure must not leak its cause: %v", err)
	}
	fail = false
	if d, err := h.beadDetail("gastown", "gt-2"); err != nil || d == nil {
		t.Fatalf("a failure must not be cached: %v %v", d, err)
	}
	if _, err := h.beadDetail("gastown", "--help"); err == nil || reads.Load() != 4 {
		t.Errorf("an invalid reference must not reach the reader: err=%v reads=%d", err, reads.Load())
	}
}

func TestBeadDetailCacheIsBounded(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	h := NewHub(Config{
		Now:  func() time.Time { now = now.Add(time.Millisecond); return now },
		Bead: func(rig, id string) (*BeadDetail, error) { return &BeadDetail{ID: id}, nil },
	})
	for i := 0; i < detailCacheMax+20; i++ {
		if _, err := h.beadDetail("gastown", "gt-"+strings.Repeat("a", 1)+string(rune('a'+i%26))+strings.Repeat("b", i/26)); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(h.detailCache); n > detailCacheMax {
		t.Errorf("cache grew to %d, max %d", n, detailCacheMax)
	}
}

func TestServeBead(t *testing.T) {
	t.Parallel()
	h := NewHub(Config{Bead: func(rig, id string) (*BeadDetail, error) {
		if id == "gt-bad" {
			return nil, errors.New("boom")
		}
		return &BeadDetail{ID: id, Rig: rig, Title: "the title", Description: "the text"}, nil
	}}).Handler()
	get := func(target, host string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", target, nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := get("/api/bead?rig=gastown&id=gt-1", "127.0.0.1:8787"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"the text"`) || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("ok case: %d %s", rec.Code, rec.Body.String())
	}
	if rec := get("/api/bead?rig=gastown&id=--help", "127.0.0.1:8787"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad reference: %d", rec.Code)
	}
	if rec := get("/api/bead?rig=gastown&id=gt-bad", "127.0.0.1:8787"); rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("read failure: %d %q", rec.Code, rec.Body.String())
	}
	if rec := get("/api/bead?rig=gastown&id=gt-1", "evil.example.com"); rec.Code != http.StatusForbidden {
		t.Errorf("a rebinding host must be refused: %d", rec.Code)
	}
}

func TestPollQueueFillsState(t *testing.T) {
	t.Parallel()
	h := NewHub(Config{Queue: func() *Queue { return &Queue{ReadyTotal: 2, Ready: []QueueBead{{ID: "gt-1"}, {ID: "gt-2"}}} }})
	h.pollQueue()
	if q := h.State().Queue; q == nil || q.ReadyTotal != 2 || len(q.Ready) != 2 {
		t.Fatalf("queue = %+v", q)
	}
}

// A ready row the dispatcher passed over carries the verdict beside its shape,
// and a row with no verdict carries none of the fields, so the page can tell
// "the ledger says nothing" from "the ledger says skipped" (gt-ckncx).
func TestQueueSkipFieldsCarryTheVerdictToThePage(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 10, 29, 14, 0, 0, 0, time.UTC)
	b, err := json.Marshal(QueueBead{
		ID: "gt-1", Shape: "skipped", ShapeNote: "overlap hold: gt-2 is in progress",
		SkipClass: "skipped", SkipReason: "overlap hold: gt-2 is in progress", SkippedSince: since, SkipTicks: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"shape":"skipped"`,
		`"skip_class":"skipped"`,
		`"skip_reason":"overlap hold: gt-2 is in progress"`,
		`"skipped_since":"2026-10-29T14:00:00Z"`,
		`"skip_ticks":3`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("a skipped row does not carry %s: %s", want, b)
		}
	}
	bare, err := json.Marshal(QueueBead{ID: "gt-2", Shape: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"skip_class", "skip_reason", "skipped_since", "skip_ticks"} {
		if strings.Contains(string(bare), absent) {
			t.Errorf("a row with no verdict must omit %s: %s", absent, bare)
		}
	}
	// The queue itself says when its verdicts were taken, and marks them stale
	// only when they are: a zero time must not reach the page as 1970.
	if b, err := json.Marshal(Queue{}); err != nil || strings.Contains(string(b), "verdicts_at") || strings.Contains(string(b), "verdicts_stale") {
		t.Errorf("a queue with no ledger must set neither field: %s err=%v", b, err)
	}
	if b, err := json.Marshal(Queue{VerdictsAt: since, VerdictsStale: true}); err != nil ||
		!strings.Contains(string(b), `"verdicts_at":"2026-10-29T14:00:00Z"`) || !strings.Contains(string(b), `"verdicts_stale":true`) {
		t.Errorf("a queue with old verdicts does not say so: %s err=%v", b, err)
	}
}
