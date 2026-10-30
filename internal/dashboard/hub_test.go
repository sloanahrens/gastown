package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// pollFeed's burst frame is the whole ring, which the page has mostly drawn
// already, so the page must replace its feed and counts on a backlog frame:
// appending would draw every row twice (gt-2lbvr).
func TestBacklogHandlerReplacesTheFeed(t *testing.T) {
	t.Parallel()

	_, handler, ok := strings.Cut(string(indexHTML), `addEventListener("backlog"`)
	require.True(t, ok, "index.html has no backlog handler")
	handler, _, _ = strings.Cut(handler, "\n")
	for _, want := range []string{
		`$("feed").replaceChildren()`,
		`delete counts[k]`,
		`pushEntry(e, true)`,
	} {
		assert.Contains(t, handler, want, "the backlog handler must start from an empty feed")
	}
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

// A page that connects late gets both lists in its first frames. The state frame
// it gets keeps the rest of each list's parent but neither list, and /api/state
// still returns the whole state, lists included (gt-6ecd0).
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
