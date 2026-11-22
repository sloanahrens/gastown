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

// add counts what each actor wrote since the previous read into buckets under
// its name at at. The first read adds nothing, since the counters began before
// the dashboard did; a counter that went down (the gateway restarted) adds the
// new count; an actor first seen on a later read adds its count.
func (w *doltWriteDeltas) add(buckets *dashboard.Buckets, at time.Time, writes map[string]uint64) {
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

// doltWritesLimit is how many actors the section lists.
const doltWritesLimit = 6

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
	first time.Time // the first read, zero before it
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
// nothing in the span is not listed.
func (s *doltWritesSection) read(ctx context.Context, _ time.Time) (dashboard.DoltSection, error) {
	writes, err := s.stats(ctx)
	if err != nil {
		return dashboard.DoltSection{}, err
	}
	now := s.now()
	s.mu.Lock()
	if s.first.IsZero() {
		s.first = now
	}
	first := s.first
	s.mu.Unlock()
	s.deltas.add(&s.buckets, now, writes)

	spanStart := now.Truncate(10 * time.Minute).Add(-10 * time.Minute)
	if first.After(spanStart) {
		spanStart = first
	}
	minutes := now.Sub(spanStart).Minutes()
	type row struct {
		actor string
		rate  float64
	}
	var rows []row
	for _, actor := range s.buckets.Keys() {
		series := s.buckets.Series(now, actor)
		if total := series[len(series)-1] + series[len(series)-2]; total > 0 && minutes > 0 {
			rows = append(rows, row{actor, float64(total) / minutes})
		}
	}
	slices.SortFunc(rows, func(a, b row) int {
		return cmp.Or(cmp.Compare(b.rate, a.rate), strings.Compare(a.actor, b.actor))
	})
	if len(rows) > doltWritesLimit {
		rows = rows[:doltWritesLimit]
	}
	out := dashboard.DoltSection{Title: "bd writes by caller and verb", Head: []string{"actor", "writes/min"}}
	for _, r := range rows {
		out.Rows = append(out.Rows, []string{r.actor, strconv.FormatFloat(r.rate, 'f', 1, 64)})
	}
	return out, nil
}
