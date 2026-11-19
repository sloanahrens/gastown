package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The Jobs section is the maintenance job list the page draws, folded from the
// JSON array `gt jobs --json` prints. The command is another program's output,
// so the parser names every key it reads and trusts nothing else: a row it
// cannot draw is refused, and a state or outcome it does not know is drawn
// unknown rather than as a job that is well.

// jobsRunCap is how many of a job's runs the section carries.
const jobsRunCap = 5

// The states a job row can be in, as internal/jobs names them. They are spelled
// here rather than imported: internal/jobs reads internal/daemon, which reads
// this package.
const (
	jobsStateNeverRun  = "never_run"
	jobsStateOverdue   = "overdue"
	jobsStateOK        = "ok"
	jobsStateNoCadence = "no_cadence"
	jobsStateUnknown   = "unknown"
)

// The outcomes a run records (beads gt-6zvcd2, gt-zxwd3h). An empty outcome is
// a run that neither failed nor did its work: a no-op run.
const (
	jobsOutcomeOK       = "ok"
	jobsOutcomeFailed   = "failed"
	jobsOutcomeRefused  = "refused"
	jobsOutcomeDeferred = "deferred"
)

// jobsHeadlineStats is the order a row's one headline stat is picked in: the
// first of them the row holds. used_percent is the node disk figure the
// node_disk rows carry (bead gt-wufb42).
var jobsHeadlineStats = []string{"used_percent", "reclaimed_bytes", "build_cache_bytes", "bytes_freed", "duration_ms"}

// Jobs is the Maintenance jobs section: one row per job `gt jobs --json`
// printed, and the worst state across them.
type Jobs struct {
	Rows []JobRow `json:"rows"`
	// Worst is the worst of the rows' states, worst first: unknown, overdue,
	// failed, never_run, no_cadence, refused, deferred, ok. It is empty when
	// the array held no rows, since nothing was read to judge.
	Worst string `json:"worst"`
}

// JobRow is one job as the section carries it.
type JobRow struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Outcome string `json:"outcome,omitempty"`
	// LastRun is the job's last completion, absent when none is recorded.
	LastRun *time.Time `json:"last_run,omitempty"`
	// DurationMS is how long the job's newest finished run took, absent when
	// no run recorded both its ends.
	DurationMS *int64 `json:"duration_ms,omitempty"`
	// Headline is the one stat the row leads with, absent when the row holds
	// none of jobsHeadlineStats.
	Headline *JobStat `json:"headline,omitempty"`
	// Runs are the job's newest runs, newest first, at most jobsRunCap.
	Runs []JobRun `json:"runs,omitempty"`
}

// JobStat is a job's headline figure. Name is the stat's own, so a reader knows
// whether Value is bytes, a percentage, or milliseconds.
type JobStat struct {
	Name  string `json:"name"`
	Value int64  `json:"value"`
}

// JobRun is one run of a job as the section carries it.
type JobRun struct {
	StartedAt time.Time `json:"started_at,omitzero"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
	Outcome   string    `json:"outcome,omitempty"`
}

// jobsReportRow is one row of the array `gt jobs --json` prints, as this
// section reads it: the job's name, state, outcome, last run, stats and runs.
type jobsReportRow struct {
	Name    string           `json:"name"`
	State   string           `json:"state"`
	Outcome string           `json:"outcome"`
	LastRun *time.Time       `json:"last_run"`
	Stats   map[string]int64 `json:"stats"`
	Runs    []jobsReportRun  `json:"runs"`
}

// jobsReportRun is one run of jobsReportRow's runs, keyed as jobledger.Run
// marshals its fields.
type jobsReportRun struct {
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Outcome   string    `json:"outcome"`
}

// ParseJobs folds the JSON array `gt jobs --json` prints into the Maintenance
// jobs section. Anything that is not that array is an error, so a command that
// printed nothing or something else reads as a failed read, never as a town
// with no jobs.
func ParseJobs(out []byte) (*Jobs, error) {
	out = bytes.TrimSpace(out)
	if len(out) == 0 || out[0] != '[' {
		return nil, errors.New("jobs: not a JSON array")
	}
	var report []jobsReportRow
	if err := json.Unmarshal(out, &report); err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	j := &Jobs{Rows: make([]JobRow, 0, len(report))}
	for i, r := range report {
		row, err := jobRowOf(i, r)
		if err != nil {
			return nil, err
		}
		j.Rows = append(j.Rows, row)
	}
	j.Worst = jobsWorstOf(j.Rows)
	return j, nil
}

// jobRowOf converts one report row. A row with no name cannot be drawn, so it
// is an error rather than a blank line in the pane.
func jobRowOf(i int, r jobsReportRow) (JobRow, error) {
	name := strings.TrimSpace(r.Name)
	if name == "" {
		return JobRow{}, fmt.Errorf("jobs: row %d has no name", i)
	}
	row := JobRow{Name: name, Outcome: strings.TrimSpace(r.Outcome)}
	row.State = jobsStateOf(r.State)
	if !jobsOutcomeKnown(row.Outcome) {
		row.State = jobsStateUnknown
	}
	if r.LastRun != nil && !r.LastRun.IsZero() {
		at := *r.LastRun
		row.LastRun = &at
	}
	row.Runs = jobsNewestRuns(r.Runs)
	row.DurationMS = jobsDuration(row.Runs)
	row.Headline = jobsHeadline(r.Stats)
	return row, nil
}

// jobsStateOf normalizes a reported state. A state the parser does not name is
// unknown, since drawing it as ok would show a job the command does not vouch
// for as healthy.
func jobsStateOf(s string) string {
	switch s = strings.TrimSpace(s); s {
	case jobsStateNeverRun, jobsStateOverdue, jobsStateOK, jobsStateNoCadence, jobsStateUnknown:
		return s
	}
	return jobsStateUnknown
}

// jobsOutcomeKnown reports whether an outcome is one this section names. The
// empty outcome is known: it is a run that neither failed nor did its work.
func jobsOutcomeKnown(s string) bool {
	switch s {
	case "", jobsOutcomeOK, jobsOutcomeFailed, jobsOutcomeRefused, jobsOutcomeDeferred:
		return true
	}
	return false
}

// jobsNewestRuns converts the reported runs and keeps the newest jobsRunCap,
// newest first. The report's order is not trusted: the ledger it tails is read
// oldest first, so the newest runs may come last.
func jobsNewestRuns(in []jobsReportRun) []JobRun {
	if len(in) == 0 {
		return nil
	}
	runs := make([]JobRun, 0, len(in))
	for _, r := range in {
		runs = append(runs, JobRun{StartedAt: r.StartedAt, EndedAt: r.EndedAt, Outcome: strings.TrimSpace(r.Outcome)})
	}
	slices.SortStableFunc(runs, func(a, b JobRun) int {
		return jobsRunAt(b).Compare(jobsRunAt(a))
	})
	if len(runs) > jobsRunCap {
		runs = runs[:jobsRunCap]
	}
	return runs
}

// jobsRunAt is when a run happened: its end when it recorded one, else its
// start. A run with neither sorts last.
func jobsRunAt(r JobRun) time.Time {
	if !r.EndedAt.IsZero() {
		return r.EndedAt
	}
	return r.StartedAt
}

// jobsDuration is the wall time of the newest run that recorded both its ends,
// in milliseconds. A run still in flight has no duration, so the row falls back
// to the newest finished run; a row with no finished run has none.
func jobsDuration(runs []JobRun) *int64 {
	for _, r := range runs {
		if r.StartedAt.IsZero() || r.EndedAt.IsZero() || r.EndedAt.Before(r.StartedAt) {
			continue
		}
		ms := r.EndedAt.Sub(r.StartedAt).Milliseconds()
		return &ms
	}
	return nil
}

// jobsHeadline picks the one stat the row leads with: the first of
// jobsHeadlineStats it holds, nil when it holds none.
func jobsHeadline(stats map[string]int64) *JobStat {
	for _, name := range jobsHeadlineStats {
		if v, ok := stats[name]; ok {
			return &JobStat{Name: name, Value: v}
		}
	}
	return nil
}

// The ranks of the states a row can be in, worst last. A job that was refused
// or deferred, never ran, or has no cadence is not ok, but neither is it a
// failure: it ranks above ok and below one.
const (
	jobsRankOK = iota
	jobsRankWarn
	jobsRankFailed
	jobsRankOverdue
	jobsRankUnknown
)

// jobsRankOf ranks one row by the worst thing true of it.
func jobsRankOf(r JobRow) int {
	switch {
	case r.State == jobsStateUnknown:
		return jobsRankUnknown
	case r.State == jobsStateOverdue:
		return jobsRankOverdue
	case r.Outcome == jobsOutcomeFailed:
		return jobsRankFailed
	case r.State == jobsStateNeverRun, r.State == jobsStateNoCadence,
		r.Outcome == jobsOutcomeRefused, r.Outcome == jobsOutcomeDeferred:
		return jobsRankWarn
	}
	return jobsRankOK
}

// jobsWorstOf is the worst state across rows: the first row at the highest
// rank names it. It is empty for no rows, since nothing was judged.
func jobsWorstOf(rows []JobRow) string {
	worst, rank := "", jobsRankOK-1
	for _, r := range rows {
		if rk := jobsRankOf(r); rk > rank {
			worst, rank = jobsWorstLabel(r), rk
		}
	}
	return worst
}

// jobsWorstLabel names a row for the section's Worst: the worst thing true of
// it, so a job whose state is ok but whose run was refused or failed is named
// by the run, not by its state.
func jobsWorstLabel(r JobRow) string {
	switch jobsRankOf(r) {
	case jobsRankFailed:
		return jobsOutcomeFailed
	case jobsRankWarn:
		if r.State == jobsStateNeverRun || r.State == jobsStateNoCadence {
			return r.State
		}
		return r.Outcome
	}
	return r.State
}
