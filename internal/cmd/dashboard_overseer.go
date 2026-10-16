package cmd

import (
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/overseer"
)

// The Overseer panel: whether the overseer session is alive, its last journal
// lines, and a seven-day count of its interventions by kind. It reads the two
// files gt overseer beat and gt overseer log write, and counts the open
// overseer questions the way the Needs you panel lists them. Read-only.
//
// A file that cannot be read makes the pane unavailable rather than silent or
// empty: the pane must not tell the operator the overseer has gone quiet when
// it is the reader that failed.

const (
	// dashOverseerLines is how many journal lines the pane draws.
	dashOverseerLines = 8
	// dashOverseerWindow is the span the interventions are counted over.
	dashOverseerWindow = 7 * 24 * time.Hour
)

// dashOverseerReader reads the pane. The functions are injectable so the
// judging is testable without a town.
type dashOverseerReader struct {
	heartbeat     func() (overseer.Heartbeat, bool, error)
	journal       func() ([]overseer.Entry, error)
	silentAfter   func() time.Duration
	openQuestions func() (int, error)
}

// newDashOverseerReader reads the town's files. silentAfter is read from the
// settings on every poll, so an edit takes effect without a restart.
func newDashOverseerReader(townRoot string, openQuestions func() (int, error)) *dashOverseerReader {
	return &dashOverseerReader{
		heartbeat: func() (overseer.Heartbeat, bool, error) { return overseer.ReadHeartbeat(townRoot) },
		journal:   func() ([]overseer.Entry, error) { return overseer.ReadJournal(townRoot) },
		silentAfter: func() time.Duration {
			// A settings file that does not parse falls back to the default
			// window; the daemon, which reads the same file, says so in its log.
			cfg, _ := config.LoadOperationalConfigChecked(townRoot)
			return cfg.GetOverseerConfig().SilentAfterD()
		},
		openQuestions: openQuestions,
	}
}

// read is the pane's whole reading at now.
func (r *dashOverseerReader) read(now time.Time) *dashboard.Overseer {
	after := r.silentAfter()
	hb, have, err := r.heartbeat()
	if err != nil {
		return &dashboard.Overseer{Unavailable: true}
	}
	entries, err := r.journal()
	if err != nil {
		return &dashboard.Overseer{Unavailable: true}
	}

	out := &dashboard.Overseer{
		Status:         dashboard.OverseerNone,
		SilentAfterSec: int64(after / time.Second),
		Journal:        []dashboard.OverseerLine{},
		Interventions:  []dashboard.OverseerCount{},
	}
	if have {
		out.Status = dashboard.OverseerAlive
		if overseer.Silent(hb, have, now, after) {
			out.Status = dashboard.OverseerSilent
		}
		out.State, out.Session = hb.State, hb.Session
		age := overseerAgeSec(now, hb.TS)
		out.LastActiveSec = &age
	}
	for _, e := range overseer.Recent(entries, dashOverseerLines) {
		out.Journal = append(out.Journal, dashboard.OverseerLine{
			AgeSec: overseerAgeSec(now, e.TS), Kind: e.Kind, Text: e.Text, Bead: e.Bead,
		})
	}
	for _, c := range overseer.CountByKind(entries, now, dashOverseerWindow) {
		out.Interventions = append(out.Interventions, dashboard.OverseerCount{Kind: c.Kind, Count: c.Count})
	}
	if r.openQuestions != nil {
		// A count that cannot be read stays nil, so the pane says so instead of
		// drawing "0 open".
		if n, err := r.openQuestions(); err == nil {
			out.OpenQuestions = &n
		}
	}
	return out
}

// overseerAgeSec is how long before now ts was, never negative: a clock that
// ran ahead of the reader's would otherwise draw a line from the future.
func overseerAgeSec(now, ts time.Time) int64 {
	if age := int64(now.Sub(ts) / time.Second); age > 0 {
		return age
	}
	return 0
}
