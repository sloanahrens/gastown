package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/steward"
	"github.com/steveyegge/gastown/internal/townhealth"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	stewardStatusSince string
	stewardStatusLast  int
	stewardStatusJSON  bool
)

var stewardCmd = &cobra.Command{
	Use:     "steward",
	GroupID: GroupDiag,
	Short:   "Inspect the steward's landing-queue jobs",
	RunE:    requireSubcommand,
}

var stewardStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Summarize the steward job ledger: outcomes, models, durations, running and stuck jobs",
	Long: `Summarize the steward's job ledger (<town>/.runtime/steward/jobs.jsonl)
over a window: jobs by outcome and by model, median and max duration, the
jobs running now and the ones stuck past their timeout, the escalations the
steward raised, and the newest jobs with their bead and outcome.

A finished job counts when it ended inside the window; a running job always
counts. "Broke" is the jobs that ended in error or timeout, the steward
failing as opposed to a fail verdict on the work; interrupted jobs (a daemon
restart killed them) are in neither the failures nor the error rate.
Escalations are the jobs that ended escalated plus the escalations the daemon
raised about the steward: one per hard-preset ("pro") job, one per stuck job,
and one per error-rate episode.

--json prints the same numbers as one object. gt status --line carries the
steward's failures and stuck jobs as the steward field, and townhealth.json
carries the last hour's counters.

Examples:
  gt steward status
  gt steward status --since 6h --last 20
  gt steward status --json`,
	Args: cobra.NoArgs,
	RunE: runStewardStatus,
}

func init() {
	stewardStatusCmd.Flags().StringVar(&stewardStatusSince, "since", "1h", "Window: a duration back (1h, 6h, 1d) or a time (RFC3339, 2006-01-02 15:04)")
	stewardStatusCmd.Flags().IntVar(&stewardStatusLast, "last", steward.DefaultRecent, "How many of the newest jobs to list (0 lists none)")
	stewardStatusCmd.Flags().BoolVar(&stewardStatusJSON, "json", false, "Print one JSON object instead of text")
	stewardCmd.AddCommand(stewardStatusCmd)
	rootCmd.AddCommand(stewardCmd)
}

// stewardStatusView is everything gt steward status prints, and its --json
// shape.
type stewardStatusView struct {
	// Enabled is whether the town's steward patrol is on; the ledger is read
	// either way, so a town that turned it off still sees its history.
	Enabled   bool   `json:"enabled"`
	Timeout   string `json:"job_timeout"`
	HardAgent string `json:"hard_agent"`
	steward.Stats
	// Alerts counts the escalations the daemon raised about the steward in
	// the window, by kind (pro, stuck, error-rate).
	Alerts map[string]int `json:"alerts"`
}

// buildStewardStatus reads the town's ledger and alert record into a view of
// the window ending at now.
func buildStewardStatus(townRoot string, since, now time.Time, last int) (stewardStatusView, error) {
	cfg := daemon.LoadPatrolConfig(townRoot)
	timeout, hard := daemon.StewardLimits(cfg)
	rows, err := steward.NewLedger(steward.LedgerPath(townRoot)).Latest()
	if err != nil {
		return stewardStatusView{}, fmt.Errorf("reading the job ledger: %w", err)
	}
	alerts, err := steward.NewAlerts(steward.AlertsPath(townRoot)).Read()
	if err != nil {
		return stewardStatusView{}, fmt.Errorf("reading the alert record: %w", err)
	}
	if last == 0 {
		last = -1
	}
	return stewardStatusView{
		Enabled:   daemon.IsPatrolEnabled(cfg, "steward"),
		Timeout:   timeout.String(),
		HardAgent: hard,
		Stats:     steward.Summarize(rows, steward.StatsOptions{Since: since, Now: now, Timeout: timeout, HardAgent: hard, Last: last}),
		Alerts:    steward.CountSince(alerts, since),
	}, nil
}

func runStewardStatus(cmd *cobra.Command, _ []string) error {
	now := time.Now()
	since, err := parseTailSince(stewardStatusSince, now, time.Local)
	if err != nil {
		return err
	}
	if stewardStatusLast < 0 {
		return fmt.Errorf("--last %d: must not be negative", stewardStatusLast)
	}
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}
	v, err := buildStewardStatus(townRoot, since, now, stewardStatusLast)
	if err != nil {
		return err
	}
	if stewardStatusJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	renderStewardStatus(cmd.OutOrStdout(), v, time.Local)
	return nil
}

func renderStewardStatus(w io.Writer, v stewardStatusView, loc *time.Location) {
	state := "on"
	if !v.Enabled {
		state = "off"
	}
	fmt.Fprintf(w, "steward %s: %s to %s (job timeout %s, hard preset %s)\n",
		state, v.Since.In(loc).Format("2006-01-02 15:04"), v.Now.In(loc).Format("15:04"), v.Timeout, v.HardAgent)
	fmt.Fprintf(w, "  jobs        %d (%d finished, %d running)\n", v.Jobs, v.Finished, len(v.Running))
	fmt.Fprintf(w, "  outcomes    %s\n", countsLine(outcomeCounts(v.Outcomes)))
	fmt.Fprintf(w, "  broke       %d of %d attempted ended in error or timeout (%.0f%%)\n", v.Broke, v.Attempted, v.ErrorRate()*100)
	fmt.Fprintf(w, "  models      %s\n", countsLine(v.Models))
	fmt.Fprintf(w, "  pro jobs    %d on %s\n", v.Pro, v.HardAgent)
	fmt.Fprintf(w, "  duration    median %s, max %s\n", townhealth.Short(secs(v.MedianSec)), townhealth.Short(secs(v.MaxSec)))
	fmt.Fprintf(w, "  escalations %d job(s) ended escalated; the daemon raised %s\n", v.Escalated, countsLine(v.Alerts))
	fmt.Fprintf(w, "  running     %s\n", jobsLine(v.Running, v.Now))
	fmt.Fprintf(w, "  stuck       %s\n", jobsLine(v.Stuck, v.Now))
	if len(v.Recent) == 0 {
		return
	}
	fmt.Fprintln(w, "recent jobs, newest first:")
	for _, j := range v.Recent {
		outcome, took := "running", townhealth.Short(v.Now.Sub(j.Started))
		if !j.Ended.IsZero() {
			outcome, took = string(j.Outcome), townhealth.Short(j.Ended.Sub(j.Started))
		}
		line := fmt.Sprintf("  %s  %-9s %-12s %-15s %-10s %5s", j.Started.In(loc).Format("15:04:05"), j.Event, j.Bead, j.Model, outcome, took)
		if j.Summary != "" {
			line += "  " + j.Summary
		}
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
}

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

func outcomeCounts(m map[steward.Outcome]int) map[string]int {
	out := make(map[string]int, len(m))
	for o, n := range m {
		out[string(o)] = n
	}
	return out
}

// countsLine is "a 2, b 1", the biggest count first, "none" when empty.
func countsLine(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		name := k
		if name == "" {
			name = "(unrecorded)"
		}
		parts[i] = fmt.Sprintf("%s %d", name, m[k])
	}
	return strings.Join(parts, ", ")
}

// jobsLine lists jobs as "bead (event, model, job id, running for a time)", "none" for an
// empty list.
func jobsLine(jobs []steward.Job, now time.Time) string {
	if len(jobs) == 0 {
		return "none"
	}
	parts := make([]string, len(jobs))
	for i, j := range jobs {
		parts[i] = fmt.Sprintf("%s (%s, %s, %s, running %s)", j.Bead, j.Event, j.Model, j.ID, townhealth.Short(now.Sub(j.Started)))
	}
	return strings.Join(parts, "; ")
}
