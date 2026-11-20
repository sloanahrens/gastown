package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// The Dolt panel's latency section: one row for the query latency probe's
// p50, p95 and max, with the p95 of the last few reads beside it (gt-vfap1c).

const (
	// doltLatencyPanelRounds is how many rounds of the mix one panel read runs:
	// ten keep the read short while its thirty samples still measure a p95.
	doltLatencyPanelRounds = 10
	// doltLatencyPanelBound bounds one panel read's probe. It is shorter than
	// the panel's section timeout, so a stalled server is reported as the
	// probe's own timeout rather than as the section's.
	doltLatencyPanelBound = 15 * time.Second
	// doltLatencyHistoryKept is how many summaries the section keeps in memory;
	// the row draws the most recent doltLatencyTrendShown of them.
	doltLatencyHistoryKept = 12
	// doltLatencyTrendShown is how many of the kept summaries' p95 values the
	// row shows.
	doltLatencyTrendShown = 5
)

// errNoDoltLatencyTownRoot is returned when the read's context carries no town
// root, so there is no store whose address the section could measure.
var errNoDoltLatencyTownRoot = errors.New("the read's context carries no town root")

// doltLatencyTarget resolves where one read's probe connects. A target it
// cannot resolve is an error, never a default address the section would dial
// instead, which could be another town's server.
type doltLatencyTarget func(ctx context.Context) (doltLatencyRun, error)

// townDoltLatencyRun is the town's own bd store as a latency run: the server
// its metadata names and the database on it. A store that is missing or names
// no database is an error naming the directory it looked in.
func townDoltLatencyRun(ctx context.Context) (doltLatencyRun, error) {
	root, ok := doltTownRoot(ctx)
	if !ok {
		return doltLatencyRun{}, errNoDoltLatencyTownRoot
	}
	beadsDir := beads.GetTownBeadsPath(root)
	cfg, ok := doltserver.ReadBeadsRuntimeConfig(beadsDir)
	if !ok {
		return doltLatencyRun{}, fmt.Errorf("no Dolt server-mode store at %s", beadsDir)
	}
	if cfg.Database == "" {
		return doltLatencyRun{}, fmt.Errorf("the store at %s names no database", beadsDir)
	}
	return doltLatencyRun{
		Host:     cfg.Host,
		Port:     cfg.Port,
		Database: cfg.Database,
		Rounds:   doltLatencyPanelRounds,
	}, nil
}

// doltLatencySection is the latency section: where to probe, how, and the
// summaries it has read, most recent last.
type doltLatencySection struct {
	target doltLatencyTarget
	dial   doltLatencyDialer
	now    func() time.Time

	mu      sync.Mutex
	history []doltLatencySummary
}

// newDoltLatencySection builds the section over an injected target, dialer and
// clock, so a test drives its reads with fakes; init gives the town's section
// the probe the command runs.
func newDoltLatencySection(target doltLatencyTarget, dial doltLatencyDialer, now func() time.Time) *doltLatencySection {
	return &doltLatencySection{target: target, dial: dial, now: now}
}

func init() {
	registerDoltSection("latency", newDoltLatencySection(townDoltLatencyRun, dialBeadsLatency, time.Now).read)
}

// read runs the probe once and draws its summary as the section's one row. A
// failed run is the section's Err and adds nothing to the trend. The panel's
// read instant is not used: the steps are timed on the clock the section was
// built with.
func (s *doltLatencySection) read(ctx context.Context, _ time.Time) (dashboard.DoltSection, error) {
	run, err := s.target(ctx)
	if err != nil {
		return dashboard.DoltSection{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, doltLatencyPanelBound)
	defer cancel()
	sum, err := runDoltLatencyProbe(probeCtx, s.dial, s.now, run)
	if err != nil {
		return dashboard.DoltSection{}, err
	}

	s.mu.Lock()
	s.history = append(s.history, sum)
	if extra := len(s.history) - doltLatencyHistoryKept; extra > 0 {
		s.history = slices.Clone(s.history[extra:])
	}
	trend := s.trendLocked()
	s.mu.Unlock()

	return dashboard.DoltSection{
		Title: "Dolt: query latency",
		Head:  []string{"p50", "p95", "max", "slowest", "p95 trend"},
		Rows:  [][]string{{sum.P50.String(), sum.P95.String(), sum.Max.String(), sum.Slowest, trend}},
	}, nil
}

// trendLocked is the p95 of the section's most recent reads, oldest first, at
// most doltLatencyTrendShown of them. The caller holds the lock.
func (s *doltLatencySection) trendLocked() string {
	start := max(len(s.history)-doltLatencyTrendShown, 0)
	values := make([]string, 0, len(s.history)-start)
	for _, sum := range s.history[start:] {
		values = append(values, sum.P95.String())
	}
	return strings.Join(values, " ")
}
