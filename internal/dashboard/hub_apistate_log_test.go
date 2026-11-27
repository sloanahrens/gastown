package dashboard

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// steppedClock hands out its times in order and keeps the last one once they run
// out. It states a poll's wait and run, and then the /api/state request's own
// start, without waiting for a real clock.
type steppedClock struct {
	mu    sync.Mutex
	times []time.Time
	next  int
}

func (c *steppedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.next >= len(c.times) {
		return c.times[len(c.times)-1]
	}
	t := c.times[c.next]
	c.next++
	return t
}

// logCapture is the hub's warning seam as a test reads it: every line answerState
// writes, formatted as warnf would format it, in order.
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (c *logCapture) logf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func (c *logCapture) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

// newCapturedHub builds a hub whose warning lines land in the returned capture
// rather than on stderr.
func newCapturedHub(cfg Config) (*Hub, *logCapture) {
	h := NewHub(cfg)
	c := &logCapture{}
	h.logf = c.logf
	return h, c
}

// A wait that ends by apiStateBound is the case the operator cannot see from the
// page: the request answered, but no health read arrived inside the bound. The
// hub then writes one line naming the viewers it waited with and the last health
// poll's timing, which says whether the poll was starved. A read that arrives in
// time, and a hub with a page and a read already current, answer without the
// line (gt-ewdxmq).
func TestAnswerStateLogsWhenTheBoundFires(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	rows := []struct {
		name string
		run  func(t *testing.T) []string
		want []string
	}{
		{
			name: "the bound fires with no health read",
			run: func(t *testing.T) []string {
				t.Helper()
				old := Health{Line: "old read", Verdict: "green", ReadAt: t0}
				clk := &steppedClock{times: []time.Time{
					t0,                             // the poll takes its slot
					t0.Add(12 * time.Millisecond),  // it starts after waiting 12ms
					t0.Add(352 * time.Millisecond), // it ran 340ms
				}}
				h, c := newCapturedHub(Config{
					Health: func() Health { return old },
					Now:    clk.Now,
				})
				// The last health poll was taken before the request: it is the
				// timing the line must name, and its read does not answer the wait.
				h.runPoll(context.Background(), h.pollHealth)
				boundTimer(h, true)

				askAPIState(t, h, context.Background(), "the /api/state request did not answer on its bound")

				return c.all()
			},
			want: []string{
				// The request's own subscription is the viewer at the bound.
				"/api/state answered without a health read after 6s: 1 viewers, health poll waited 12 ms, ran 340 ms",
			},
		},
		{
			name: "a health read in time logs nothing",
			run: func(t *testing.T) []string {
				t.Helper()
				h, c := newCapturedHub(Config{
					Health: func() Health { return Health{Line: "fresh", Verdict: "green", ReadAt: t0} },
					Now:    func() time.Time { return t0 },
				})
				h.after = func(time.Duration) (<-chan time.Time, func()) {
					h.pollHealth() // the read lands while the request waits
					return nil, func() {}
				}

				st := askAPIState(t, h, context.Background(), "the /api/state request was not answered by the health read")
				assert.Equal(t, t0, st.Health.ReadAt, "the answer carries the read that ended the wait")

				return c.all()
			},
		},
		{
			name: "a watched hub answers at once and logs nothing",
			run: func(t *testing.T) []string {
				t.Helper()
				h, c := newCapturedHub(Config{
					Health: func() Health { return Health{Line: "fresh", Verdict: "green", ReadAt: t0} },
					Now:    func() time.Time { return t0 },
				})
				h.pollHealth()
				page, _ := h.Subscribe()
				defer h.Unsubscribe(page)
				bound := boundTimer(h, false)

				askAPIState(t, h, context.Background(), "the /api/state request did not answer at once")
				assert.Empty(t, bound.asked, "a page is open and the state is read, so the request does not wait")

				return c.all()
			},
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			assert.Equal(t, r.want, r.run(t))
		})
	}
}
