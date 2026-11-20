package dashboard

import (
	"encoding/json"
	"testing"
	"time"
)

// A hub with a Dolt reader carries the panel it read, with the source marked
// read and the panel under the state's `dolt` key: that key is what the Dolt
// panel draws from.
func TestHubDoltRead(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)
	panel := &DoltPanel{
		At: now,
		Sections: []DoltSection{{
			Key:   "latency",
			Title: "Query latency",
			Head:  []string{"p50", "p95", "max"},
			Rows:  [][]string{{"1ms", "2ms", "9ms"}},
		}},
	}
	h := NewHub(Config{
		Now:  func() time.Time { return now },
		Dolt: func() *DoltPanel { return panel },
	})
	if st := h.State(); st.Dolt != nil {
		t.Fatalf("dolt before any read: %+v", st.Dolt)
	}
	h.pollDolt()

	st := h.State()
	if st.Dolt != panel {
		t.Fatalf("the hub holds %+v, want the reader's panel", st.Dolt)
	}
	if r := st.Reads[SourceDolt]; r.Error || r.Stale || r.At.IsZero() {
		t.Errorf("a good dolt read is flagged: %+v", r)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if _, ok := payload["dolt"]; !ok {
		t.Fatalf("the state carries no dolt key: %s", raw)
	}
}

// A failed read (a nil panel) keeps the last good panel and marks the source
// failed and stale, so the page draws the old sections marked rather than an
// empty panel. A hub with no reader has no panel and records no read.
func TestHubDoltReadFailureKeepsLast(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)
	read := true
	h := NewHub(Config{
		Now: func() time.Time { return now },
		Dolt: func() *DoltPanel {
			if !read {
				return nil
			}
			return &DoltPanel{Sections: []DoltSection{{Key: "latency", Title: "Query latency"}}}
		},
	})
	h.pollDolt()
	read = false
	h.pollDolt()

	st := h.State()
	if st.Dolt == nil || len(st.Dolt.Sections) != 1 || st.Dolt.Sections[0].Key != "latency" {
		t.Fatalf("a failed read left dolt %+v, want the last good panel", st.Dolt)
	}
	if r := st.Reads[SourceDolt]; !r.Error || !r.Stale {
		t.Errorf("a failed dolt read is not flagged error and stale: %+v", r)
	}
	if got := st.Reads[SourceDolt].At; !got.Equal(now) {
		t.Errorf("a failed dolt read moved At to %v, want the last good read %v", got, now)
	}
}

// The Dolt poll defaults to five minutes, the maintenance jobs' clock.
func TestDoltEveryDefaultsToFiveMinutes(t *testing.T) {
	t.Parallel()

	var c Config
	c.defaults()
	if c.DoltEvery != 5*time.Minute {
		t.Errorf("DoltEvery defaults to %v, want 5m", c.DoltEvery)
	}
}

// A hub with no Dolt reader has no panel and records no read: the page tells a
// town without the panel from one whose read has not answered.
func TestHubDoltUnconfigured(t *testing.T) {
	t.Parallel()

	h := NewHub(Config{})
	h.pollDolt()

	st := h.State()
	if st.Dolt != nil {
		t.Errorf("a hub with no dolt reader carries %+v", st.Dolt)
	}
	if _, ok := st.Reads[SourceDolt]; ok {
		t.Errorf("a hub with no dolt reader recorded a read: %+v", st.Reads[SourceDolt])
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if _, ok := payload["dolt"]; ok {
		t.Errorf("a hub with no dolt reader carries a dolt key: %s", raw)
	}
}
