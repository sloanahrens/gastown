package dashboard

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
