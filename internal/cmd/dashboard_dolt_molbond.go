package cmd

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/bdgateway"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// The mol bond section counts the gateway's mol bond calls per ten minutes, row
// (2) of the operator's 2026-11-18 list of what had to be dug out by hand. It
// reads the gateway's call log rather than a store: the log holds every call,
// and it is the gateway that serves it.

const (
	// doltMolBondSectionKey is the section's key in the Dolt panel.
	doltMolBondSectionKey = "molbond"
	// doltMolBondTitle is the section's heading.
	doltMolBondTitle = "mol bond per 10 minutes"
	// doltMolBondVerb is the verb the gateway logs for bd's mol bond: the
	// policy's path words joined by a space.
	doltMolBondVerb = "mol bond"
	// doltMolBondAttemptsKey and doltMolBondFailuresKey are the two series the
	// section's bucket counter holds.
	doltMolBondAttemptsKey = "attempts"
	doltMolBondFailuresKey = "failures"
	// doltMolBondSpan is the width of one row, the same ten minutes the
	// dashboard's bucket counter counts in.
	doltMolBondSpan = 10 * time.Minute
	// doltMolBondShown is how many ten-minute buckets the section draws.
	doltMolBondShown = 6
	// doltMolBondLimit is how many calls one poll asks for: the gateway's cap
	// on a calls read, so a poll at the log's peak rate fits in one answer.
	doltMolBondLimit = 500
)

func init() {
	registerDoltSection(doltMolBondSectionKey, newDoltMolBondReader(os.Getenv, time.Now))
}

// newDoltMolBondReader is the section's registered read: a counter over the
// gateway the town's settings name, started now, so every panel read after it
// counts the calls logged since the dashboard started.
func newDoltMolBondReader(getenv func(string) string, now func() time.Time) doltSectionRead {
	pusher := bdGatewayPusherFromEnv(getenv)
	counter := newDoltMolBondCounter(pusher.Calls, now, doltMolBondLimit)
	return counter.read
}

// doltMolBondCounter counts the gateway's mol bond calls across the panel's
// reads. It holds the cursor each read starts from and the bucket counter the
// rows are drawn from; the panel runs its reads on several goroutines, so a
// read holds the lock while it counts.
type doltMolBondCounter struct {
	mu sync.Mutex
	// calls is the gateway read; production passes GatewayPusher.Calls and a
	// test passes a fake.
	calls func(ctx context.Context, since time.Time, limit int) (bdgateway.CallsPage, error)
	limit int
	// cursor is the newest stamp counted, and the start the first read asks
	// from; it never moves back.
	cursor time.Time
	counts dashboard.Buckets
	// unreadable is how many of the last read's entries had no readable stamp
	// and so could not be placed in a bucket.
	unreadable int
}

// newDoltMolBondCounter starts a counter at now: the first read counts the
// calls logged after that instant.
func newDoltMolBondCounter(calls func(ctx context.Context, since time.Time, limit int) (bdgateway.CallsPage, error), now func() time.Time, limit int) *doltMolBondCounter {
	return &doltMolBondCounter{calls: calls, limit: limit, cursor: now()}
}

// read polls the gateway, counts the calls past the cursor, and draws the
// section. A read the gateway does not answer fails the section: an empty
// table would read as a gateway with no mol bond calls.
func (c *doltMolBondCounter) read(ctx context.Context, now time.Time) (dashboard.DoltSection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	page, err := c.calls(ctx, c.cursor, c.limit)
	if err != nil {
		return dashboard.DoltSection{}, fmt.Errorf("reading the gateway's calls: %w", err)
	}
	newest := c.cursor
	unreadable := 0
	for _, e := range page.Entries {
		at, err := time.Parse(time.RFC3339Nano, e.TS)
		if err != nil {
			// A call with no readable time cannot be placed in a bucket; it is
			// reported in the note rather than dropped without trace.
			unreadable++
			continue
		}
		if at.After(newest) {
			newest = at
		}
		// The gateway's log stamps a call before it queues it, so a read serves
		// its newest stamp again; the cursor is what keeps that stamp from
		// being counted twice.
		if !at.After(c.cursor) || e.Verb != doltMolBondVerb {
			continue
		}
		c.counts.Add(at, doltMolBondAttemptsKey, 1)
		if e.ExitCode != 0 {
			c.counts.Add(at, doltMolBondFailuresKey, 1)
		}
	}
	c.cursor = newest
	c.unreadable = unreadable
	return c.section(now, len(page.Entries) >= c.limit), nil
}

// section draws the last doltMolBondShown ten-minute buckets, oldest first,
// with the bucket holding now last.
func (c *doltMolBondCounter) section(now time.Time, truncated bool) dashboard.DoltSection {
	attempts := c.counts.Series(now, doltMolBondAttemptsKey)
	failures := c.counts.Series(now, doltMolBondFailuresKey)
	section := dashboard.DoltSection{
		Title: doltMolBondTitle,
		Note:  doltMolBondNote(c.unreadable, truncated),
		Head:  []string{"ten minutes", "attempts", "failures"},
	}
	// The counter's buckets are the ten minutes counted from the Unix epoch,
	// and the epoch falls on one of their boundaries, so Truncate gives the
	// window the counter counted in.
	end := now.Truncate(doltMolBondSpan).Add(doltMolBondSpan)
	for i := doltMolBondShown; i > 0; i-- {
		start := end.Add(-time.Duration(i) * doltMolBondSpan)
		idx := len(attempts) - i
		section.Rows = append(section.Rows, []string{
			start.Format("15:04") + "-" + start.Add(doltMolBondSpan).Format("15:04"),
			strconv.Itoa(attempts[idx]),
			strconv.Itoa(failures[idx]),
		})
	}
	return section
}

// doltMolBondNote is the section's explanation: what each column counts, when
// the counts began, and the two ways a read undercounts.
func doltMolBondNote(unreadable int, truncated bool) string {
	note := "Mol bond calls from the gateway's call log: attempts, and failures where the exit code is not 0. The counts start when the dashboard starts."
	if unreadable > 0 {
		note += fmt.Sprintf(" %d calls had no readable time and were not counted.", unreadable)
	}
	if truncated {
		note += fmt.Sprintf(" The last read returned the gateway's limit of %d calls, so the window may be truncated.", doltMolBondLimit)
	}
	return note
}
