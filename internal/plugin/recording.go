package plugin

import (
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// RunResult represents the outcome of a plugin execution.
type RunResult string

const (
	ResultSuccess RunResult = "success"
	ResultFailure RunResult = "failure"
	ResultSkipped RunResult = "skipped"
	// ResultWarning marks a run that finished and acted on its finding: the
	// check succeeded and its signal was escalated to the operator (the
	// compactor-dog's check-only escalation, gt-hrt9). Distinct from
	// ResultFailure — a warning escalates, it does not dispatch a dog — and
	// from ResultSuccess, which a quiet check records.
	ResultWarning RunResult = "warning"
	// ResultPrinted marks a `gt plugin run` receipt for a plugin whose
	// instructions were printed but not executed: distinct from
	// ResultSuccess so history, dashboards and cooldown accounting never
	// read a merely-printed run as work that was actually done (gt-o1z7).
	ResultPrinted RunResult = "printed"
)

// PluginRunRecord represents data for creating a plugin run bead.
type PluginRunRecord struct {
	PluginName  string
	RigName     string
	Result      RunResult
	Title       string
	Body        string
	ExtraLabels []string
}

// PluginRunBead represents a recorded plugin run from the ledger.
type PluginRunBead struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	Labels    []string  `json:"labels"`
	Result    RunResult `json:"-"` // Parsed from labels
}

// Recorder handles plugin run recording and querying.
type Recorder struct {
	bd beads.Client
}

// NewRecorder creates a new plugin run recorder against the town's beads
// database, pinned so the receipts land in the town's ledger and nowhere the
// prefix router might send them.
func NewRecorder(townRoot string) *Recorder {
	return &Recorder{bd: beads.NewPinned(beads.ResolveBeadsDir(townRoot))}
}

// RecordRun creates an ephemeral bead for a plugin run.
// This is pure data writing - the caller decides what result to record.
func (r *Recorder) RecordRun(record PluginRunRecord) (string, error) {
	title := record.Title
	if title == "" {
		title = fmt.Sprintf("Plugin run: %s", record.PluginName)
	}

	// Build labels
	labels := []string{
		"type:plugin-run",
		fmt.Sprintf("plugin:%s", record.PluginName),
		fmt.Sprintf("result:%s", record.Result),
	}
	if record.RigName != "" {
		labels = append(labels, fmt.Sprintf("rig:%s", record.RigName))
	}
	labels = append(labels, record.ExtraLabels...)

	created, err := r.bd.Create(beads.CreateOptions{
		Title:       title,
		Labels:      labels,
		Description: record.Body,
		Ephemeral:   true,
	})
	if err != nil {
		return "", fmt.Errorf("creating plugin run bead: %w", err)
	}

	// Close the receipt immediately — it exists for audit/cooldown-gate queries
	// (which read every status) but should not stay open.
	_ = r.bd.CloseWithReason("plugin run recorded", created.ID) // Best-effort — the reaper catches it if this fails

	return created.ID, nil
}

// GetLastRun returns the most recent run for a plugin.
// Returns nil if no runs found.
func (r *Recorder) GetLastRun(pluginName string) (*PluginRunBead, error) {
	runs, err := r.queryRuns(pluginName, 1, "")
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, nil
	}
	return runs[0], nil
}

// GetRunsSince returns all runs for a plugin since the given duration.
// Duration format: "1h", "24h", "7d", etc.
func (r *Recorder) GetRunsSince(pluginName string, since string) ([]*PluginRunBead, error) {
	return r.queryRuns(pluginName, 0, since)
}

// queryRuns queries plugin run beads from the ledger. Receipts are ephemeral,
// so the read is pinned to the wisps plane: the durable issues table holds
// none of them (gt-5sq).
func (r *Recorder) queryRuns(pluginName string, limit int, since string) ([]*PluginRunBead, error) {
	opts := beads.ListOptions{
		Status:    "all", // Include closed receipts too
		Labels:    []string{"type:plugin-run", fmt.Sprintf("plugin:%s", pluginName)},
		Ephemeral: true,
		Limit:     limit,
		Priority:  -1, // any priority
	}
	if since != "" {
		// Parse as a Go duration and compute an absolute RFC3339 cutoff.
		// bd's compact duration uses "m" for months, but plugin gate
		// durations use Go's time.ParseDuration where "m" means minutes.
		// Passing an absolute timestamp avoids this unit mismatch.
		d, err := time.ParseDuration(since)
		if err != nil {
			return nil, fmt.Errorf("parsing duration %q: %w", since, err)
		}
		opts.CreatedAfter = time.Now().Add(-d).UTC()
	}

	issues, err := r.bd.List(opts)
	if err != nil {
		return nil, fmt.Errorf("querying plugin runs: %w", err)
	}

	// Convert to PluginRunBead with parsed result
	runs := make([]*PluginRunBead, 0, len(issues))
	for _, issue := range issues {
		run := &PluginRunBead{
			ID:     issue.ID,
			Title:  issue.Title,
			Labels: issue.Labels,
		}

		// Parse created_at
		if t, err := time.Parse(time.RFC3339, issue.CreatedAt); err == nil {
			run.CreatedAt = t
		}

		// Extract result from labels
		for _, label := range issue.Labels {
			if result, ok := strings.CutPrefix(label, "result:"); ok {
				run.Result = RunResult(result)
				break
			}
		}

		runs = append(runs, run)
	}

	return runs, nil
}

// CountRunsSince returns the count of runs for a plugin since the given
// duration, for cooldown gate evaluation. A ResultPrinted receipt is
// excluded: `gt plugin run` writes it for a merely-printed, not-yet-executed
// run, and a receipt for work nobody did must not buy the plugin a cooldown
// window (gt-o1z7, finding f53b7b837c35).
func (r *Recorder) CountRunsSince(pluginName string, since string) (int, error) {
	runs, err := r.GetRunsSince(pluginName, since)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, run := range runs {
		if run.Result == ResultPrinted {
			continue
		}
		count++
	}
	return count, nil
}
