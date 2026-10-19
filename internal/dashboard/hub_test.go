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
