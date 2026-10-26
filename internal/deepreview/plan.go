package deepreview

import (
	"sort"
	"time"
)

// Plan is one run's window and the landings it reviews.
type Plan struct {
	// Since is where the run starts: the ledger's newest cursor line, or the
	// default window when it has none.
	Since Timestamp
	// Cursor is the slice path the newest cursor line named, empty when none.
	Cursor string
	// Landings are the risky landings after Since, oldest first.
	Landings []Record
	// Records counts the ledger lines that classified ok, Unparsed those that
	// are not JSON.
	Records  int
	Unparsed int
	// Quiet is true when no landing after Since touched a risk path.
	Quiet bool
	// ReviewedUntil is the timestamp the next cursor line carries: the newest
	// risky landing, or Since when the run is quiet.
	ReviewedUntil Timestamp
}

// BuildPlan returns the plan for one run: the window the ledger's notes set,
// the classified landings, and the risky ones after the window oldest first.
//
// A ledger the plan cannot trust is an error, never an empty plan: a quiet
// verdict from an unreadable landings file is the one answer the review must
// not give.
func BuildPlan(notes string, landings []byte, now time.Time) (Plan, error) {
	window, err := ReviewWindow(notes, now)
	if err != nil {
		return Plan{}, err
	}
	file, err := ClassifyLandings(landings)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{
		Since:    window.Since,
		Cursor:   window.Cursor,
		Landings: []Record{},
		Records:  file.Records,
		Unparsed: file.Unparsed,
	}
	for _, c := range file.Lines {
		if c.Kind == KindOK && len(c.Record.RiskPaths) > 0 && c.Record.LandedAt.After(window.Since) {
			plan.Landings = append(plan.Landings, c.Record)
		}
	}
	// Stable, so two landings at one instant keep the ledger's order in both
	// plans rather than swapping by whichever sort ran.
	sort.SliceStable(plan.Landings, func(i, j int) bool {
		return plan.Landings[i].LandedAt.Before(plan.Landings[j].LandedAt)
	})
	plan.ReviewedUntil = window.Since
	if len(plan.Landings) == 0 {
		plan.Quiet = true
	} else {
		plan.ReviewedUntil = plan.Landings[len(plan.Landings)-1].LandedAt
	}
	return plan, nil
}
