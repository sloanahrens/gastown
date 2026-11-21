package specdispatch

import (
	"encoding/json"
	"testing"
	"time"
)

// The schedule answers each loss with the delay for that loss, measured from the
// loss that set the previous retry, on a clock the test moves (gt-iv5n4l).
func TestContentionNextSchedule(t *testing.T) {
	t.Parallel()
	cases := []struct {
		attempt int
		delay   time.Duration
	}{
		{1, 5 * time.Second},
		{2, 15 * time.Second},
		{3, 45 * time.Second},
		{4, 2 * time.Minute},
		{5, 2 * time.Minute},
		{6, 2 * time.Minute},
	}
	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	var rec ContentionState
	for _, c := range cases {
		next, exhausted := ContentionNext(rec, now)
		if got := next.RetryAt.Sub(now); got != c.delay {
			t.Errorf("loss %d: retry due after %v, want %v", c.attempt, got, c.delay)
		}
		if next.Attempts != c.attempt {
			t.Errorf("loss %d: attempts = %d, want %d", c.attempt, next.Attempts, c.attempt)
		}
		if exhausted {
			t.Errorf("loss %d at %v: exhausted inside the window", c.attempt, now.Sub(next.First))
		}
		rec, now = next, next.RetryAt
	}
}

// The window runs from the first loss: a later loss does not restart it, and the
// schedule is spent once ContentionWindow has passed since that first loss.
func TestContentionNextExhaustedAtWindow(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	firstLoss, _ := ContentionNext(ContentionState{}, first)
	laterLoss, _ := ContentionNext(firstLoss, first.Add(9*time.Minute))
	cases := []struct {
		name string
		rec  ContentionState
		at   time.Time
		want bool
	}{
		{"one nanosecond inside the window", firstLoss, first.Add(ContentionWindow - time.Nanosecond), false},
		{"at the window", firstLoss, first.Add(ContentionWindow), true},
		{"after the window", firstLoss, first.Add(ContentionWindow + time.Minute), true},
		{"a later loss keeps the first loss's start", laterLoss, first.Add(ContentionWindow), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, exhausted := ContentionNext(c.rec, c.at); exhausted != c.want {
				t.Errorf("ContentionNext at %v: exhausted = %v, want %v", c.at, exhausted, c.want)
			}
		})
	}
}

// A record reads as no record when none of its keys are the contention keys, and
// as a spent window when a contention key is present but cannot be read. Spent
// reads exhausted, never as a bead with no losses (gt-iv5n4l).
func TestContentionRecordReads(t *testing.T) {
	t.Parallel()
	firstT := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	retryT := firstT.Add(5 * time.Second)
	valid := map[string]any{
		ContentionFirstKey:    "2026-11-19T00:00:00Z",
		ContentionAttemptsKey: "1",
		ContentionRetryAtKey:  "2026-11-19T00:00:05Z",
	}
	mustJSON := func(m map[string]any) []byte {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}
	withField := func(key string, value any) []byte {
		m := map[string]any{}
		for k, v := range valid {
			m[k] = v
		}
		if value == nil {
			delete(m, key)
		} else {
			m[key] = value
		}
		return mustJSON(m)
	}
	cases := []struct {
		name      string
		meta      []byte
		ok        bool
		spent     bool
		attempts  int
		exhausted bool
	}{
		{"no metadata", nil, false, false, 0, false},
		{"null metadata", []byte(`null`), false, false, 0, false},
		{"another writer's keys only", []byte(`{"attempts":"5"}`), false, false, 0, false},
		{"all three keys as strings", mustJSON(valid), true, false, 1, false},
		{"attempts as a number", withField(ContentionAttemptsKey, 1.0), true, false, 1, false},
		{"not json", []byte("{not json"), true, true, 0, true},
		{"json that is not an object", []byte(`[1]`), true, true, 0, true},
		{"attempts that is not a count", withField(ContentionAttemptsKey, "many"), true, true, 0, true},
		{"attempts that is a fraction", withField(ContentionAttemptsKey, 1.5), true, true, 0, true},
		{"attempts of zero", withField(ContentionAttemptsKey, "0"), true, true, 0, true},
		{"attempts that is a bool", withField(ContentionAttemptsKey, true), true, true, 0, true},
		{"first that is not RFC 3339", withField(ContentionFirstKey, "yesterday"), true, true, 0, true},
		{"first that is not a string", withField(ContentionFirstKey, 1.0), true, true, 0, true},
		{"retry_at that is not RFC 3339", withField(ContentionRetryAtKey, "soon"), true, true, 0, true},
		{"first missing beside the others", withField(ContentionFirstKey, nil), true, true, 0, true},
		{"attempts missing beside the others", withField(ContentionAttemptsKey, nil), true, true, 0, true},
		{"retry_at missing beside the others", withField(ContentionRetryAtKey, nil), true, true, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rec, ok := ContentionRecord(c.meta)
			if ok != c.ok || rec.Spent != c.spent {
				t.Fatalf("ContentionRecord(%s) ok = %v spent = %v, want ok %v spent %v",
					c.meta, ok, rec.Spent, c.ok, c.spent)
			}
			if ok && !rec.Spent {
				if rec.Attempts != c.attempts || !rec.First.Equal(firstT) || !rec.RetryAt.Equal(retryT) {
					t.Errorf("ContentionRecord(%s) = %+v, want attempts %d first %v retry_at %v",
						c.meta, rec, c.attempts, firstT, retryT)
				}
			}
			if _, exhausted := ContentionNext(rec, firstT); exhausted != c.exhausted {
				t.Errorf("ContentionNext on record of %s: exhausted = %v, want %v", c.meta, exhausted, c.exhausted)
			}
		})
	}
}

// A retry is waiting before its retry_at and due at it: ContentionWaiting is true
// strictly before, and ContentionPauseOver is its exact complement for a record
// that is not spent, true at and after it. A record with no retry set is not
// waiting.
func TestContentionWaitingBoundary(t *testing.T) {
	t.Parallel()
	retryAt := time.Date(2026, 11, 19, 0, 0, 5, 0, time.UTC)
	cases := []struct {
		name        string
		rec         ContentionState
		now         time.Time
		waiting     bool
		pauseIsOver bool
	}{
		{"a nanosecond before retry_at", ContentionState{RetryAt: retryAt}, retryAt.Add(-time.Nanosecond), true, false},
		{"at retry_at", ContentionState{RetryAt: retryAt}, retryAt, false, true},
		{"after retry_at", ContentionState{RetryAt: retryAt}, retryAt.Add(time.Nanosecond), false, true},
		{"no retry set", ContentionState{}, retryAt, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := ContentionWaiting(c.rec, c.now); got != c.waiting {
				t.Errorf("ContentionWaiting at %v = %v, want %v", c.now, got, c.waiting)
			}
			if got := ContentionPauseOver(c.rec, c.now); got != c.pauseIsOver {
				t.Errorf("ContentionPauseOver at %v = %v, want %v", c.now, got, c.pauseIsOver)
			}
		})
	}
}

// A record ContentionRecord cannot read is spent, and a spent record is neither
// waiting nor over its pause: it carries no retry time to pass, so
// ContentionPauseOver is false and a caller retries a spent bead only through
// ContentionNext's exhausted answer (gt-7z6wtu).
func TestContentionSpentRecord(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	unreadableRetryAt, err := json.Marshal(map[string]any{
		ContentionFirstKey:    "2026-11-19T00:00:00Z",
		ContentionAttemptsKey: "1",
		ContentionRetryAtKey:  "soon",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cases := []struct {
		name string
		meta []byte
	}{
		{"metadata that is not JSON", []byte("{not json")},
		{"a retry_at that is not RFC 3339", unreadableRetryAt},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rec, ok := ContentionRecord(c.meta)
			if !ok || !rec.Spent {
				t.Fatalf("ContentionRecord(%s) = %+v, ok = %v; want a spent record", c.meta, rec, ok)
			}
			if ContentionWaiting(rec, now) {
				t.Errorf("ContentionWaiting on a spent record = true, want false")
			}
			if ContentionPauseOver(rec, now) {
				t.Errorf("ContentionPauseOver on a spent record = true, want false")
			}
		})
	}
}
