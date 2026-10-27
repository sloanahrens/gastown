package dashboard

import (
	"encoding/json"
	"fmt"
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
