package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The first poll on a busy town returns the whole 2h preface at once. A page
// already subscribed holds 64 frames, so a frame per entry dropped it before it
// drew anything (gt-bfh09); the batch must reach it whole.
func TestPollFeedFirstBurstNotDropped(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 19, 5, 0, 0, 0, time.UTC)
	const preface = 100
	h := NewHub(Config{
		Feed: func() []Entry {
			out := make([]Entry, preface)
			for i := range out {
				out[i] = Entry{At: now, Rig: "gastown", Kind: "daemon", Text: fmt.Sprintf("line %d", i)}
			}
			return out
		},
		Now: func() time.Time { return now },
	})
	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	h.pollFeed()

	require.Equal(t, 1, h.State().Viewers, "the page was dropped by the burst")
	delivered := 0
	for len(page.C) > 0 {
		f := string(<-page.C)
		_, data, _ := strings.Cut(f, "\ndata: ")
		switch {
		case strings.HasPrefix(f, "event: entry"):
			delivered++
		case strings.HasPrefix(f, "event: backlog"):
			var entries []Entry
			require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(data)), &entries))
			delivered += len(entries)
		}
	}
	assert.Equal(t, preface, delivered, "every entry of the preface reaches the page")
}

// The Claude samples the plan poll reads reach the Cost charts, which is what
// makes the plan-equivalent series appear beside the DeepSeek bars at all; a
// chart rebuilt without them would be absent in the town and present in every
// builder test (gt-yxx98).
func TestClaudePollPutsTheSamplesOnTheCostCharts(t *testing.T) {
	t.Parallel()

	now := time.Now()
	spend, err := json.Marshal(map[string]any{
		"per_day": map[string]any{now.Format("2006-01-02"): map[string]any{"spent": 1.0}},
	})
	require.NoError(t, err)
	h := NewHub(Config{
		Now:        func() time.Time { return now },
		Spend:      func() json.RawMessage { return spend },
		SpendHours: func() SpendRead { return SpendRead{} },
		ClaudePlan: func() *ClaudePlan {
			return &ClaudePlan{
				PlanUSDMonth: ClaudePlanDefaultUSD,
				Samples:      claudeSeries(ClaudePlanSample{T: now.Unix(), SevenDay: 40}),
			}
		},
	})

	h.pollSpend()
	h.pollClaudePlan()

	charts := h.State().CostCharts
	require.NotNil(t, charts, "no Cost charts after both polls")
	require.NotNil(t, charts.Daily, "the spend report drew no daily chart")
	require.NotNil(t, charts.Daily.Claude, "the Claude samples did not reach the daily chart: %s", charts.Daily.ClaudeWhy)
	assert.Equal(t, claudePlanSeriesName, charts.Daily.Claude.Name)
}

// eventCount is how many frames in frames are named event.
func eventCount(frames, event string) int {
	return strings.Count(frames, "event: "+event+"\n")
}

// frameData returns the data of the first frame named event, and whether one was
// in frames.
func frameData(frames [][]byte, event string) (string, bool) {
	for _, f := range frames {
		head, data, ok := strings.Cut(string(f), "\ndata: ")
		if ok && head == "event: "+event {
			return strings.TrimSuffix(data, "\n\n"), true
		}
	}
	return "", false
}

// The queue and landing lists leave the state frame and go out as their own
// events, and only when their bytes changed: a poll that leaves them as they were
// sends the state alone, and the next change sends the list again (gt-6ecd0).
func TestHubSendsTheListEventsOnlyWhenTheyChange(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	ready := []QueueBead{{ID: "gt-1", Rig: "gastown", Title: "first"}}
	landed := []LandingRow{{At: now, Bead: "gt-9", Outcome: "landed"}}
	h := NewHub(Config{
		Now:   func() time.Time { return now },
		Trend: func() *Trend { return &Trend{Recent: landed} },
		Queue: func() *Queue { return &Queue{At: now, Ready: ready, ReadyTotal: len(ready)} },
	})
	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	lists := []string{"trend.recent", "queue.ready", "queue.blocked"}

	h.pollTrend()
	h.pollQueue()
	first := drainFrames(page)
	for _, ev := range lists {
		assert.Equal(t, 1, eventCount(first, ev), "%s is sent the first time it is read", ev)
	}

	h.pollTrend()
	h.pollQueue()
	again := drainFrames(page)
	assert.Equal(t, 2, eventCount(again, "state"), "the state goes out on every poll")
	for _, ev := range lists {
		assert.Equal(t, 0, eventCount(again, ev), "%s did not change, so it is not sent again", ev)
	}

	ready = append(ready, QueueBead{ID: "gt-2", Rig: "gastown", Title: "second"})
	h.pollQueue()
	changed := drainFrames(page)
	assert.Equal(t, 1, eventCount(changed, "queue.ready"), "queue.ready changed and was not sent")
	assert.Equal(t, 0, eventCount(changed, "trend.recent"), "trend.recent did not change and was sent")
	assert.Equal(t, 0, eventCount(changed, "queue.blocked"), "queue.blocked did not change and was sent")

	landed = append(landed, LandingRow{At: now, Bead: "gt-10", Outcome: "landed"})
	h.pollTrend()
	assert.Equal(t, 1, eventCount(drainFrames(page), "trend.recent"), "trend.recent changed and was not sent")
}

// A list that will not encode is left out of its event: a null in its place would
// read as an empty list. The page keeps the list it had, and the next poll sends
// the list once it encodes again (gt-w346j).
func TestHubDoesNotSendAListThatFailsToEncode(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	// time.Time does not encode a year past 9999, so this row fails to marshal.
	unencodable := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := []LandingRow{{At: now, Bead: "gt-9", Outcome: "landed"}}
	h := NewHub(Config{
		Now:   func() time.Time { return now },
		Trend: func() *Trend { return &Trend{Recent: recent} },
	})
	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	h.pollTrend()
	assert.Equal(t, 1, eventCount(drainFrames(page), "trend.recent"), "a list that encodes was not sent")

	recent = []LandingRow{{At: unencodable, Bead: "gt-10", Outcome: "landed"}}
	h.pollTrend()
	broken := drainFrames(page)
	assert.Equal(t, 0, eventCount(broken, "trend.recent"), "a list that fails to encode was sent")
	assert.NotContains(t, broken, "event: trend.recent\ndata: null", "a list that fails to encode was sent as null, which reads as empty")
	assert.Equal(t, 1, eventCount(broken, "state"), "the state frame did not go out while a list failed to encode")

	recent = []LandingRow{{At: now, Bead: "gt-11", Outcome: "landed"}}
	h.pollTrend()
	assert.Equal(t, 1, eventCount(drainFrames(page), "trend.recent"), "the list was not sent again once it encoded")
}

// A state frame that will not encode is left out of the page's first frames: a
// null in its place would read as an empty state (gt-utjwd).
func TestHubDoesNotSendAStateFrameThatFailsToEncodeOnSubscribe(t *testing.T) {
	t.Parallel()

	// time.Time does not encode a year past 9999, so a State carrying this Now
	// fails to marshal.
	unencodable := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	h := NewHub(Config{Now: func() time.Time { return unencodable }})

	page, initial := h.Subscribe()
	defer h.Unsubscribe(page)

	first := framesText(initial)
	assert.Equal(t, 0, eventCount(first, "state"), "a state frame that fails to encode was sent")
	assert.NotContains(t, first, "event: state\ndata: null", "a state frame that fails to encode was sent as null, which reads as empty")
}

// A state frame that will not encode is left out of a poll's frames too: the
// page keeps the state it had, and the next poll sends the state once it encodes
// again (gt-utjwd).
func TestHubDoesNotSendAStateFrameThatFailsToEncodeOnPoll(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	// time.Time does not encode a year past 9999, so a Trend hour of this time
	// fails to marshal inside the state frame.
	unencodable := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	hours := []TrendHour{{Hour: now, Landed: 1}}
	h := NewHub(Config{
		Now:   func() time.Time { return now },
		Trend: func() *Trend { return &Trend{Hours: hours} },
	})
	page, initial := h.Subscribe()
	defer h.Unsubscribe(page)
	assert.Equal(t, 1, eventCount(framesText(initial), "state"), "the state encodes, so it is sent")

	hours = []TrendHour{{Hour: unencodable, Landed: 1}}
	h.pollTrend()
	broken := drainFrames(page)
	assert.Equal(t, 0, eventCount(broken, "state"), "a state frame that fails to encode was sent")
	assert.NotContains(t, broken, "event: state\ndata: null", "a state frame that fails to encode was sent as null, which reads as empty")

	hours = []TrendHour{{Hour: now.Add(time.Minute), Landed: 2}}
	h.pollTrend()
	assert.Equal(t, 1, eventCount(drainFrames(page), "state"), "the state was not sent again once it encoded")
}

// The frame table takes the state, the entries, the alerts and the backlog
// alike, so each of them that will not encode is left out rather than sent as
// null, which the page reads as an empty entry, alert or feed (gt-hndxa).
func TestHubDoesNotSendAnEntryThatFailsToEncode(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	// time.Time does not encode a year past 9999, so this entry fails to marshal.
	unencodable := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	pending := []Entry{{At: unencodable, Rig: "gastown", Kind: "daemon", Text: "a line"}}
	h := NewHub(Config{
		Now:  func() time.Time { return now },
		Feed: func() []Entry { out := pending; pending = nil; return out },
	})
	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	h.pollFeed()
	got := drainFrames(page)
	assert.Equal(t, 0, eventCount(got, "entry"), "an entry that fails to encode was sent")
	assert.NotContains(t, got, "event: entry\ndata: null", "an entry that fails to encode was sent as null, which the page cannot draw")
}

// The backlog frame is framed by the same table, so a ring that will not encode
// is left out too, and the page keeps the feed it drew (gt-hndxa).
func TestHubDoesNotSendABacklogThatFailsToEncode(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	unencodable := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	// One entry past the burst cap, with one member that will not marshal.
	entries := make([]Entry, feedBurstMax+1)
	for i := range entries {
		entries[i] = Entry{At: now, Rig: "gastown", Kind: "daemon", Text: fmt.Sprintf("line %d", i)}
	}
	entries[0].At = unencodable
	h := NewHub(Config{
		Now:  func() time.Time { return now },
		Feed: func() []Entry { out := entries; entries = nil; return out },
	})
	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	h.pollFeed()
	got := drainFrames(page)
	assert.Equal(t, 0, eventCount(got, "backlog"), "a backlog that fails to encode was sent")
	assert.NotContains(t, got, "event: backlog\ndata: null", "a backlog that fails to encode was sent as null, which the page reads as an empty feed")
}

// An alert takes the same table, so one whose reading will not encode is left
// out rather than sent as null (gt-hndxa).
func TestHubDoesNotSendAnAlertThatFailsToEncode(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	unencodable := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	// The attention line's own timestamp carries into the alert, so a bad one
	// makes the alert frame fail to marshal while the clock stays sound.
	pending := []attention.Event{{
		TS: unencodable, Class: attention.KindRedMain, Severity: attention.SeverityHigh,
		Text: "slot held 3h", Key: "slot:gastown", State: attention.EventNew,
		Rule: attention.RuleSlotHeld, Handled: attention.HandledOperatorAction, Wake: true,
	}}
	h := NewHub(Config{
		Now: func() time.Time { return now },
		Attention: func() ([]attention.Event, attention.Acks) {
			out := pending
			pending = nil
			return out, attention.Acks{}
		},
	})
	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	h.pollAttention()
	got := drainFrames(page)
	assert.NotContains(t, got, "slot:gastown", "an alert that fails to encode was sent")
	assert.NotContains(t, got, "event: alert\ndata: null", "an alert that fails to encode was sent as null")
}

// A state the page cannot draw is not dropped in silence: the hub raises an
// alert for it, once per cooldown, so a state that stays broken does not fill
// the feed with the same line (gt-hndxa).
func TestHubRaisesAnAlertWhenTheStateWillNotEncode(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	unencodable := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	hours := []TrendHour{{Hour: now, Landed: 1}}
	h := NewHub(Config{
		Now:   func() time.Time { return now },
		Trend: func() *Trend { return &Trend{Hours: hours} },
	})
	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	hours = []TrendHour{{Hour: unencodable, Landed: 1}}
	h.pollTrend()
	assert.Equal(t, 1, strings.Count(drainFrames(page), `"key":"state:encode"`), "a state that fails to encode was dropped without an alert")

	h.pollTrend()
	assert.Equal(t, 0, strings.Count(drainFrames(page), `"key":"state:encode"`), "the same state failure was raised again inside the cooldown")
}

// A page that connects while the state will not encode starts from the last
// state that did, so it draws a state instead of waiting for the poll that
// makes the live one encode (gt-hndxa).
func TestHubNewPageStartsWithTheLastStateThatEncoded(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	unencodable := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	hours := []TrendHour{{Hour: now, Landed: 1}}
	h := NewHub(Config{
		Now:   func() time.Time { return now },
		Trend: func() *Trend { return &Trend{Hours: hours} },
	})
	page, initial := h.Subscribe()
	defer h.Unsubscribe(page)
	good, ok := frameData(initial, "state")
	require.True(t, ok, "the state encodes, so the first page got one")

	hours = []TrendHour{{Hour: unencodable, Landed: 1}}
	h.pollTrend()
	later, laterInitial := h.Subscribe()
	defer h.Unsubscribe(later)

	got, ok := frameData(laterInitial, "state")
	require.True(t, ok, "a page that connected while the state was broken got no state")
	assert.Equal(t, good, got, "the page did not start from the last state that encoded")
}

// framesText joins frames as one page would receive them.
func framesText(frames [][]byte) string {
	var b strings.Builder
	for _, f := range frames {
		b.Write(f)
	}
	return b.String()
}

// A page that connects late gets both lists in its first frames. The state frame
// it gets keeps each list's parent, the queue's totals and landing list and the
// trend's hours, but not the lists themselves, and /api/state still returns the
// whole state, lists included (gt-6ecd0).
func TestHubLateSubscriberGetsBothListsAndTheStateWithoutThem(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	h := NewHub(Config{
		Now: func() time.Time { return now },
		Trend: func() *Trend {
			return &Trend{Hours: []TrendHour{{Hour: now, Landed: 1}}, Recent: []LandingRow{{At: now, Bead: "gt-9", Outcome: "landed"}}}
		},
		Queue: func() *Queue {
			return &Queue{
				At:           now,
				Ready:        []QueueBead{{ID: "gt-1", Rig: "gastown"}},
				ReadyTotal:   1,
				Blocked:      []QueueBead{{ID: "gt-2", Rig: "gastown"}},
				BlockedTotal: 1,
				Landing:      []QueueBead{{ID: "gt-3", Rig: "gastown"}},
				LandingTotal: 1,
			}
		},
	})
	h.pollTrend()
	h.pollQueue()

	page, initial := h.Subscribe()
	defer h.Unsubscribe(page)

	var joined strings.Builder
	for _, f := range initial {
		joined.Write(f)
	}
	for _, ev := range []string{"trend.recent", "queue.ready", "queue.blocked"} {
		assert.Equal(t, 1, eventCount(joined.String(), ev), "%s is not in the first frames of a late page", ev)
	}

	data, ok := frameData(initial, "state")
	require.True(t, ok, "the first frames carry no state")
	var st State
	require.NoError(t, json.Unmarshal([]byte(data), &st))
	require.NotNil(t, st.Queue)
	assert.Nil(t, st.Queue.Ready, "the state frame still carries queue.ready")
	assert.Nil(t, st.Queue.Blocked, "the state frame still carries queue.blocked")
	assert.Equal(t, 1, st.Queue.ReadyTotal, "the state frame lost the queue's totals")
	assert.Len(t, st.Queue.Landing, 1, "the state frame lost the landing list, which is not a list event")
	require.NotNil(t, st.Trend)
	assert.Nil(t, st.Trend.Recent, "the state frame still carries trend.recent")
	assert.Len(t, st.Trend.Hours, 1, "the state frame lost the trend's hours")

	req := httptest.NewRequest("GET", "http://localhost/api/state", nil)
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var whole State
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &whole))
	assert.Len(t, whole.Trend.Recent, 1, "/api/state lost trend.recent")
	assert.Len(t, whole.Queue.Ready, 1, "/api/state lost queue.ready")
	assert.Len(t, whole.Queue.Blocked, 1, "/api/state lost queue.blocked")
}

// fieldNames is the sorted keys of a JSON object.
func fieldNames(m map[string]any) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// The list events carry every field a row has in /api/state, so a row loses no
// information when it leaves the state frame. Each row is fully populated, so the
// omitempty fields are in the comparison too (gt-6ecd0).
func TestHubListEventRowsCarryEveryFieldOfTheirStateRows(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	score, secs := 0.9, 12.5
	bead := QueueBead{
		ID: "gt-1", Rig: "gastown", Title: "t", Type: "task", Status: "open",
		Assignee: "warboy", Priority: 2, Labels: []string{"l"}, CreatedAt: now,
		UpdatedAt: now, BlockedBy: []string{"gt-0"}, RigParked: true,
	}
	blocked := bead
	blocked.Wisp = true
	h := NewHub(Config{
		Now: func() time.Time { return now },
		Trend: func() *Trend {
			return &Trend{Recent: []LandingRow{{
				At: now, Bead: "gt-9", Rig: "gastown", Title: "t", Polecat: "warboy", Crew: true,
				Outcome: "rejected", Verdict: "error", Kind: "gate", Score: &score,
				LintSecs: &secs, GateSecs: &secs, OMSecs: &secs, ShipSecs: &secs,
			}}}
		},
		Queue: func() *Queue {
			return &Queue{At: now, Ready: []QueueBead{bead}, ReadyTotal: 1, Blocked: []QueueBead{blocked}, BlockedTotal: 1}
		},
	})
	h.pollTrend()
	h.pollQueue()

	page, initial := h.Subscribe()
	defer h.Unsubscribe(page)

	req := httptest.NewRequest("GET", "http://localhost/api/state", nil)
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var whole map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &whole))

	for _, c := range []struct{ event, section, list string }{
		{"trend.recent", "trend", "recent"},
		{"queue.ready", "queue", "ready"},
		{"queue.blocked", "queue", "blocked"},
	} {
		section, _ := whole[c.section].(map[string]any)
		rows, _ := section[c.list].([]any)
		require.NotEmpty(t, rows, "/api/state has no %s.%s row", c.section, c.list)
		want := fieldNames(rows[0].(map[string]any))

		data, ok := frameData(initial, c.event)
		require.True(t, ok, "%s is not in the first frames", c.event)
		var listed []map[string]any
		require.NoError(t, json.Unmarshal([]byte(data), &listed))
		require.NotEmpty(t, listed, "%s carries no rows", c.event)

		assert.Equal(t, want, fieldNames(listed[0]), "a %s row lost a field /api/state's %s.%s row has", c.event, c.section, c.list)
	}
}

// testGuard bounds every wait in the tests below. The hub's fakes drive what
// happens; a signal that does not arrive inside the guard fails the test with
// what did not happen, so a missed event is never a hang.
const testGuard = 10 * time.Second

// wantSignal receives one value from c, or fails the test with what when none
// arrives inside testGuard.
func wantSignal[T any](t *testing.T, c <-chan T, what string) T {
	t.Helper()
	//testpolicy:allow no-sleep — failure-only watchdog: a passing run never reaches it
	timer := time.NewTimer(testGuard)
	defer timer.Stop()
	select {
	case v := <-c:
		return v
	case <-timer.C:
		t.Fatalf("%s", what)
		var zero T
		return zero
	}
}

// fetchAPIState asks the hub's handler for /api/state and decodes the answer. It
// calls no testing method, so a test may run it in a goroutine and bound the wait.
func fetchAPIState(h *Hub, ctx context.Context) (State, error) {
	req := httptest.NewRequest("GET", "/api/state", nil).WithContext(ctx)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return State{}, fmt.Errorf("/api/state answered %d", rec.Code)
	}
	var st State
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		return State{}, fmt.Errorf("/api/state answer: %w", err)
	}
	return st, nil
}

// askAPIState asks /api/state in a goroutine and waits for the answer under
// testGuard, so a request the hub never answers fails the test with a message
// instead of hanging it. The request runs under a context this helper cancels on
// every exit, a failed wait included (Fatalf runs the deferred cancel), so a
// request the hub never answered ends as a client disconnect and releases its
// subscription instead of outliving the test.
func askAPIState(t *testing.T, h *Hub, ctx context.Context, what string) State {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type answer struct {
		st  State
		err error
	}
	answers := make(chan answer, 1)
	go func() {
		st, err := fetchAPIState(h, ctx)
		answers <- answer{st, err}
	}()
	a := wantSignal(t, answers, what)
	require.NoError(t, a.err)
	return a.st
}

// fakeBound is the hub's bound timer as a test drives it. It records each bound
// asked for and each stop.
type fakeBound struct {
	asked   []time.Duration
	stopped int
}

// boundTimer replaces the hub's bound timer. The timer fires at once when fire is
// set and never otherwise.
func boundTimer(h *Hub, fire bool) *fakeBound {
	f := &fakeBound{}
	h.after = func(d time.Duration) (<-chan time.Time, func()) {
		f.asked = append(f.asked, d)
		c := make(chan time.Time, 1)
		if fire {
			c <- time.Time{}
		}
		return c, func() { f.stopped++ }
	}
	return f
}

// A page that connects while a worker's run is in flight is not lost: once the
// interval has passed the worker runs again. The first run blocks until the second
// page has connected and the clock has moved on.
func TestWorkerRunsAgainForAPageThatConnectsDuringItsRun(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	clock := t0
	h := NewHub(Config{Now: func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}})
	setClock := func(at time.Time) {
		clockMu.Lock()
		defer clockMu.Unlock()
		clock = at
	}
	wake := h.wakeChan()
	first, _ := h.Subscribe()
	defer h.Unsubscribe(first)

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRun := func() { releaseOnce.Do(func() { close(release) }) }
	runs := 0 // read and written by the worker only
	fn := func() {
		runs++
		started <- struct{}{}
		if runs == 1 {
			<-release
		}
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.workFrom(ctx, nil, wake, time.Minute, fn)
	}()
	t.Cleanup(func() {
		stop()
		releaseRun()
		wantSignal(t, done, "the worker did not stop")
	})

	wantSignal(t, started, "the worker's first run did not start")
	second, _ := h.Subscribe()
	defer h.Unsubscribe(second)
	setClock(t0.Add(2 * time.Minute))
	releaseRun()

	wantSignal(t, started, "the worker's second run did not start")
	assert.Equal(t, 2, runs, "the connect during the first run is served by a second run")
}

// With no page open, a request wakes the workers, and the health worker's read is
// what answers it. The worker is started as Run starts it, parked on a wake taken
// before the request, with a tick that never fires, so the wake is what drives the
// read. The bound is the fake that never fires, so the answer can only come from
// the read, and the request is asked in a goroutine so a wake that never arrives
// fails the test with a message.
func TestAPIStateWithNoViewerWakesPollersAndAnswersLive(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	polled := make(chan struct{}, 1)
	h := NewHub(Config{
		Health: func() Health {
			select {
			case polled <- struct{}{}:
			default:
			}
			return Health{Line: "all green", Verdict: "green", ReadAt: now}
		},
		Now: func() time.Time { return now },
	})
	bound := boundTimer(h, false)
	ctx, stop := context.WithCancel(context.Background())
	wake := h.wakeChan()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.workFrom(ctx, nil, wake, time.Hour, h.pollHealth)
	}()
	t.Cleanup(func() {
		stop()
		wantSignal(t, done, "the health worker did not stop")
	})

	st := askAPIState(t, h, context.Background(), "the /api/state request was not answered by the health read it woke")

	wantSignal(t, polled, "the health poll the request woke did not run")
	assert.Equal(t, now, st.Health.ReadAt, "the answer carries the health read the request woke")
	assert.Equal(t, "green", st.Health.Verdict)
	assert.Equal(t, []time.Duration{apiStateBound}, bound.asked)
	assert.Equal(t, 1, bound.stopped, "the bound timer is stopped once the request answers")
	assert.Equal(t, 0, h.viewers(), "the request released its own page")
}

func TestAPIStateReleasesItsSubscription(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	h := NewHub(Config{
		Health: func() Health { return Health{Verdict: "green", ReadAt: now} },
		Now:    func() time.Time { return now },
	})
	boundTimer(h, true)

	st := askAPIState(t, h, context.Background(), "the /api/state request did not answer")

	assert.Equal(t, 1, st.Viewers, "the answer is taken while the request is its page")
	assert.Equal(t, 0, h.viewers(), "the request's subscription is gone once it has answered")
}

// The client is already gone, and the bound never fires, so the wait can end
// only through the request's context.
func TestAPIStateReleasesItsSubscriptionWhenTheClientLeaves(t *testing.T) {
	t.Parallel()

	h := NewHub(Config{
		Health: func() Health { return Health{Verdict: "unknown"} },
		Now:    func() time.Time { return time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC) },
	})
	bound := boundTimer(h, false)
	ctx, leave := context.WithCancel(context.Background())
	leave()

	st := askAPIState(t, h, ctx, "the /api/state request did not answer a client that had left")

	assert.True(t, st.Health.ReadAt.IsZero(), "no read landed before the client left")
	assert.Equal(t, 1, bound.stopped, "a client that leaves mid-wait stops the bound timer")
	assert.Equal(t, 0, h.viewers(), "a client that leaves mid-wait leaves no subscription behind")
}

// Each way a wait ends stops the bound timer: a read, a client that disconnects,
// and a page the hub drops, which the request replaces before it answers.
func TestAPIStateStopsItsBoundTimer(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		setup func(h *Hub, bound *fakeBound, ctx context.Context) context.Context
	}{
		{"a read", func(h *Hub, _ *fakeBound, ctx context.Context) context.Context {
			h.pollHealth()
			return ctx
		}},
		{"a client that disconnects", func(_ *Hub, _ *fakeBound, _ context.Context) context.Context {
			ctx, leave := context.WithCancel(context.Background())
			leave()
			return ctx
		}},
		{"a page the hub drops", func(h *Hub, bound *fakeBound, ctx context.Context) context.Context {
			// The fake drops the request's page inside the wait, after its
			// subscription is taken. The request replaces the page, and the
			// read it then waits for lands on the replacement.
			h.after = func(d time.Duration) (<-chan time.Time, func()) {
				bound.asked = append(bound.asked, d)
				wake := h.wakeChan() // closed when the request replaces its page
				h.mu.Lock()
				for s := range h.subs {
					h.dropLocked(s)
				}
				h.mu.Unlock()
				go func() {
					select {
					case <-wake:
						h.pollHealth()
					case <-ctx.Done():
					}
				}()
				return nil, func() { bound.stopped++ }
			}
			return ctx
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := NewHub(Config{
				Health: func() Health { return Health{Verdict: "green", ReadAt: now} },
				Now:    func() time.Time { return now },
			})
			bound := boundTimer(h, false)
			ctx, stop := context.WithCancel(context.Background())
			t.Cleanup(stop)
			ctx = c.setup(h, bound, ctx)

			askAPIState(t, h, ctx, "the /api/state request did not answer")

			assert.Equal(t, 1, bound.stopped, "the bound timer is stopped once the wait ends")
			assert.Equal(t, 0, h.viewers(), "the request released its own page")
		})
	}
}

// A page the hub drops while the request waits is replaced once: the request is a
// viewer again and keeps waiting for the read, and answers with the read taken
// after the replacement.
func TestAPIStateResubscribesAfterTheHubDropsItsPage(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	h := NewHub(Config{
		Health: func() Health { return Health{Line: "after the drop", Verdict: "green", ReadAt: now} },
		Now:    func() time.Time { return now },
	})
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	bound := &fakeBound{}
	replaced := make(chan struct{})
	h.after = func(d time.Duration) (<-chan time.Time, func()) {
		bound.asked = append(bound.asked, d)
		wake := h.wakeChan() // closed when the request replaces its page
		h.mu.Lock()
		for s := range h.subs {
			h.dropLocked(s)
		}
		h.mu.Unlock()
		go func() {
			select {
			case <-wake:
				close(replaced)
				h.pollHealth() // the read that a page again allows to happen
			case <-ctx.Done():
			}
		}()
		return nil, func() { bound.stopped++ }
	}

	st := askAPIState(t, h, ctx, "the /api/state request was not answered after replacing its dropped page")

	wantSignal(t, replaced, "the request did not replace the page the hub dropped")

	assert.Equal(t, "after the drop", st.Health.Line, "the answer carries the read taken after the replacement")
	assert.Equal(t, now, st.Health.ReadAt)
	assert.Equal(t, 1, st.Viewers, "the request is a viewer again when it answers")
	assert.Equal(t, 1, bound.stopped, "the bound timer is stopped once the request answers")
	assert.Equal(t, 0, h.viewers(), "the request released its replacement page")
}

func TestAPIStateTimesOutAndAnswers(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	clock := t0
	h := NewHub(Config{
		Health: func() Health { return Health{Line: "last read", Verdict: "green", ReadAt: t0} },
		Now:    func() time.Time { return clock },
	})
	h.pollHealth()
	clock = t0.Add(time.Hour)
	bound := boundTimer(h, true)

	st := askAPIState(t, h, context.Background(), "the /api/state request did not answer on its bound")

	assert.Equal(t, []time.Duration{apiStateBound}, bound.asked, "the wait is bounded by the constant")
	assert.Equal(t, 1, bound.stopped, "the bound timer is stopped once the request answers")
	assert.Equal(t, "last read", st.Health.Line, "on timeout the answer is what the state holds")
	assert.Equal(t, t0, st.Health.ReadAt)
}

func TestAPIStateWithAnOpenPageAnswersAtOnce(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	h := NewHub(Config{
		Health: func() Health { return Health{Verdict: "green", ReadAt: now} },
		Now:    func() time.Time { return now },
	})
	h.pollHealth()
	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)
	bound := boundTimer(h, false)

	st := askAPIState(t, h, context.Background(), "the /api/state request did not answer at once")

	assert.Empty(t, bound.asked, "a page is open and the state is read, so the request does not wait")
	assert.Equal(t, 1, st.Viewers)
	assert.Equal(t, now, st.Health.ReadAt)
}

func TestAPIStateWithoutAHealthReaderAnswersAtOnce(t *testing.T) {
	t.Parallel()

	h := NewHub(Config{Now: func() time.Time { return time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC) }})
	bound := boundTimer(h, false)

	st := askAPIState(t, h, context.Background(), "the /api/state request did not answer without a health reader")

	assert.Empty(t, bound.asked, "no health reader means no read to wait for")
	assert.Equal(t, 1, st.Viewers, "the request counts as the page it was")
	assert.Equal(t, 0, h.viewers())
}

// A read counts for a request only when it was taken at or after the request
// started: a read from before the request, or none, leaves the request waiting.
func TestAPIStateCountsOnlyAReadTakenSinceTheRequest(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"never read", time.Time{}, false},
		{"read before the request", start.Add(-time.Second), false},
		{"read as the request started", start, true},
		{"read after the request started", start.Add(time.Second), true},
	}
	for _, c := range cases {
		h := NewHub(Config{})
		h.state.Health.ReadAt = c.at
		assert.Equal(t, c.want, h.healthReadSince(start), c.name)
	}
}
