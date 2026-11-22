package cmd

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// doltWriteDeltas turns the gateway's cumulative writes-per-actor counters into
// the writes between two reads (row (3) of the operator's 2026-11-18 list, gt-e14l54).
// The panel reads on several goroutines, so a read holds the lock.
type doltWriteDeltas struct {
	mu   sync.Mutex
	seen bool
	prev map[string]uint64
}

// rebase takes writes as the counters' baseline without counting anything: the
// read after a gap too long to place its writes in time (gt-tiwtir).
func (w *doltWriteDeltas) rebase(writes map[string]uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seen, w.prev = true, writes
}

// add counts what each actor wrote since the previous read into buckets under
// its name at at. The first read adds nothing, since the counters began before
// the dashboard did; a counter that went down (the gateway restarted) adds the
// new count; an actor first seen on a later read adds its count.
func (w *doltWriteDeltas) add(buckets *dashboard.Buckets, at time.Time, writes map[string]uint64) {
	// Buckets drops a count dated more than a minute after its latest time, and
	// only a Series call moves that time to the read: a read five minutes after the
	// last would lose its deltas, so the read time is shown to the buckets first.
	buckets.Series(at, "")
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen {
		for actor, now := range writes {
			delta := now
			if before := w.prev[actor]; now >= before {
				delta = now - before
			}
			buckets.Add(at, actor, int(delta))
		}
	}
	w.seen, w.prev = true, writes
}

// doltWritesLimit is how many actors the section lists, and doltWritesBucket the
// width of a dashboard.Buckets bucket. The overflow key "(other)" (the actors past
// the 64th) is not an actor: it takes none of the six places and follows them on its
// own line when it wrote anything.
const (
	doltWritesLimit  = 6
	doltWritesBucket = 10 * time.Minute
)

// errDoltWritesNoStats is the production read's answer until GatewayPusher serves
// /v1/stats (gt-yg7xjl): a section that cannot read its counters says so rather
// than showing none.
var errDoltWritesNoStats = errors.New("the gateway's per-actor stats are not readable yet (gt-yg7xjl)")

// doltWritesStats reads the gateway's writes per actor, cumulative since its start.
type doltWritesStats func(ctx context.Context) (map[string]uint64, error)

// doltWritesSection is the panel's "writes" section: the busiest actors by writes
// per minute (row (3) of the operator's 2026-11-18 list, gt-tiwtir). The stats
// reader and the clock are injected.
type doltWritesSection struct {
	stats   doltWritesStats
	now     func() time.Time
	deltas  doltWriteDeltas
	buckets dashboard.Buckets

	mu    sync.Mutex
	first time.Time // where the observed span starts: the first read, or the read after a gap
	last  time.Time // the previous read, zero before the first
}

func newDoltWritesSection(stats doltWritesStats, now func() time.Time) *doltWritesSection {
	return &doltWritesSection{stats: stats, now: now}
}

func init() {
	registerDoltSection("writes", newDoltWritesSection(func(context.Context) (map[string]uint64, error) {
		return nil, errDoltWritesNoStats
	}, time.Now).read)
}

// read adds what the actors wrote since the last read and lists the six busiest
// by writes per minute over the current ten-minute bucket and the one before it,
// divided by the minutes observed inside that span: the span starts at the earlier
// bucket's start or the first read, whichever is later, so a dashboard that has
// run five minutes does not report a twenty-minute average. An actor that wrote
// nothing in the span is not listed. A read more than one bucket after the previous
// one counts nothing and restarts the span, since the timing of its writes is
// unknown: a sparse dashboard under-reports rather than misattributes.
func (s *doltWritesSection) read(ctx context.Context, _ time.Time) (dashboard.DoltSection, error) {
	writes, err := s.stats(ctx)
	if err != nil {
		return dashboard.DoltSection{}, err
	}
	now := s.now()
	s.mu.Lock()
	gap := !s.last.IsZero() && now.Sub(s.last) > doltWritesBucket
	if s.first.IsZero() || gap {
		s.first = now
	}
	first := s.first
	s.last = now
	s.mu.Unlock()
	if gap {
		// More than a bucket since the last read: the writes since then cannot be
		// placed in time, so they count nothing and the span restarts here.
		s.deltas.rebase(writes)
	} else {
		s.deltas.add(&s.buckets, now, writes)
	}

	spanStart := now.Truncate(doltWritesBucket).Add(-doltWritesBucket)
	if first.After(spanStart) {
		spanStart = first
	}
	minutes := now.Sub(spanStart).Minutes()
	type row struct {
		actor string
		rate  float64
	}
	var rows []row
	var other *row
	for _, actor := range s.buckets.Keys() {
		series := s.buckets.Series(now, actor)
		if total := series[len(series)-1] + series[len(series)-2]; total > 0 && minutes > 0 {
			if r := (row{actor, float64(total) / minutes}); actor == "(other)" {
				other = &r
			} else {
				rows = append(rows, r)
			}
		}
	}
	slices.SortFunc(rows, func(a, b row) int {
		return cmp.Or(cmp.Compare(b.rate, a.rate), strings.Compare(a.actor, b.actor))
	})
	if len(rows) > doltWritesLimit {
		rows = rows[:doltWritesLimit]
	}
	if other != nil {
		rows = append(rows, *other)
	}
	out := dashboard.DoltSection{Title: "bd writes by caller and verb", Head: []string{"actor", "writes/min"}}
	for _, r := range rows {
		out.Rows = append(out.Rows, []string{r.actor, strconv.FormatFloat(r.rate, 'f', 1, 64)})
	}
	return out, nil
}
