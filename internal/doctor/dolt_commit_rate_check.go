package doctor

import (
	"context"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// DoltCommitRateCheck is the commits-per-day meter (gt-8z769.4): it warns
// when any database made more Dolt commits in the last 24 hours than
// operational.dolt.commits_per_day_warn allows (default 500, the D3 target).
// History growth is commits, not garbage, so this is the number the beads
// commit batching (one commit per bd invocation) is meant to move. Read-only;
// it never nudges anyone.
type DoltCommitRateCheck struct {
	BaseCheck

	// measure reads the meter; nil is doltserver.CommitsLastDay.
	measure func(ctx context.Context, townRoot string) ([]doltserver.DBCommits, error)
}

// NewDoltCommitRateCheck creates the commits-per-day check.
func NewDoltCommitRateCheck() *DoltCommitRateCheck {
	return &DoltCommitRateCheck{
		BaseCheck: BaseCheck{
			CheckName:        "dolt-commit-rate",
			CheckDescription: "Warn when a Dolt database made more commits in 24h than operational.dolt.commits_per_day_warn",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

// Run counts each database's last-24h commits and compares them to the
// threshold. An unreadable meter is Skipped, never OK.
func (c *DoltCommitRateCheck) Run(ctx *CheckContext) *CheckResult {
	measure := c.measure
	if measure == nil {
		measure = doltserver.CommitsLastDay
	}
	limit := config.LoadOperationalConfig(ctx.TownRoot).GetDoltConfig().CommitsPerDayWarnV()

	qctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	counts, err := measure(qctx, ctx.TownRoot)
	if err != nil {
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusSkipped,
			Message:  fmt.Sprintf("Could not read the commits-per-day meter: %v", err),
			Category: c.CheckCategory,
		}
	}
	var failed []string
	for _, r := range counts {
		if r.Err != "" {
			failed = append(failed, fmt.Sprintf("%s: %s", r.Database, r.Err))
		}
	}
	if len(counts) == 0 || len(failed) == len(counts) {
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusSkipped,
			Message:  "No database could be measured for commits per day",
			Details:  failed,
			Category: c.CheckCategory,
		}
	}

	over := doltserver.OverCommitBudget(counts, limit)
	if len(over) == 0 {
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusOK,
			Message:  fmt.Sprintf("Commits in the last 24h within %d per database: %s", limit, doltserver.FormatCommitCounts(counts)),
			Details:  failed,
			Category: c.CheckCategory,
		}
	}
	details := make([]string, 0, len(over)+len(failed))
	for _, r := range over {
		details = append(details, fmt.Sprintf("%s: %d commits in 24h (limit %d)", r.Database, r.Commits, limit))
	}
	details = append(details, failed...)
	return &CheckResult{
		Name:     c.Name(),
		Status:   StatusWarning,
		Message:  fmt.Sprintf("%d database(s) above %d commits/24h: %s", len(over), limit, doltserver.FormatCommitCounts(over)),
		Details:  details,
		FixHint:  "Dolt history grows by commits. Find the writer making per-write commits (bd should commit once per invocation, gt-8z769 item 2); raise operational.dolt.commits_per_day_warn in settings/config.json only if the rate is intended.",
		Category: c.CheckCategory,
	}
}
