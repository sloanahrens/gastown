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

	runs := make([]*PluginRunBead, 0, len(issues))
	for _, issue := range issues {
		runs = append(runs, runFromIssue(issue))
	}

	return runs, nil
}

// runFromIssue reads one receipt from the ledger: its ID, title and labels,
// created_at parsed and the result taken from the result: label.
func runFromIssue(issue *beads.Issue) *PluginRunBead {
	run := &PluginRunBead{
		ID:     issue.ID,
		Title:  issue.Title,
		Labels: issue.Labels,
	}
	if t, err := time.Parse(time.RFC3339, issue.CreatedAt); err == nil {
		run.CreatedAt = t
	}
	for _, label := range issue.Labels {
		if result, ok := strings.CutPrefix(label, "result:"); ok {
			run.Result = RunResult(result)
			break
		}
	}
	return run
}

// RunLedger is one read of the plugin-run wisps, grouped by plugin name, so a
// sweep that gates many plugins lists the ledger once instead of once per
// plugin (gt-3bqfy).
type RunLedger struct {
	// loadedAt is the instant the ledger's cutoffs are measured from, so every
	// answer a sweep draws from one ledger agrees on what "since" means.
	loadedAt time.Time
	// since is the span the ledger was read back through; 0 is every run the
	// wisps hold. A longer per-plugin window cannot be answered from it.
	since time.Duration
	// runs holds each plugin's receipts newest first, as bd returns them.
	runs map[string][]*PluginRunBead
}

// LoadRunLedger lists the plugin-run wisps once, back through since, and
// groups them by their plugin: label. The caller passes the longest cooldown
// window it will ask about — every shorter window is answered from the same
// read. since 0 reads every receipt the wisps plane holds.
//
// Receipts are ephemeral, so like queryRuns this reads the wisps plane.
func (r *Recorder) LoadRunLedger(since time.Duration) (*RunLedger, error) {
	if since < 0 {
		return nil, fmt.Errorf("loading plugin run ledger: negative window %s", since)
	}
	opts := beads.ListOptions{
		Status:    "all", // Include closed receipts too
		Labels:    []string{"type:plugin-run"},
		Ephemeral: true,
		Priority:  -1, // any priority
	}
	loadedAt := time.Now()
	if since > 0 {
		opts.CreatedAfter = loadedAt.Add(-since).UTC()
	}

	issues, err := r.bd.List(opts)
	if err != nil {
		return nil, fmt.Errorf("querying plugin runs: %w", err)
	}

	ledger := &RunLedger{
		loadedAt: loadedAt,
		since:    since,
		runs:     make(map[string][]*PluginRunBead, len(issues)),
	}
	for _, issue := range issues {
		name := pluginLabel(issue.Labels)
		if name == "" {
			// A run wisp with no plugin label belongs to no plugin's gate.
			continue
		}
		ledger.runs[name] = append(ledger.runs[name], runFromIssue(issue))
	}
	return ledger, nil
}

// pluginLabel is the plugin name a receipt's labels carry, "" when it has
// none.
func pluginLabel(labels []string) string {
	for _, label := range labels {
		if name, ok := strings.CutPrefix(label, "plugin:"); ok {
			return name
		}
	}
	return ""
}

// checkWindow refuses a window the ledger cannot answer. A longer one than the
// ledger was loaded with would count a prefix of the runs and read as fewer
// runs than happened, which opens a cooldown gate that should be closed.
func (l *RunLedger) checkWindow(window time.Duration) error {
	if l.since == 0 {
		return nil
	}
	if window > 0 && window <= l.since {
		return nil
	}
	return fmt.Errorf("plugin run ledger: window %s is not within the %s it was loaded with", window, l.since)
}

// cutoff is the instant window reaches back to, the zero time for a window
// that reaches back through every run the ledger holds.
func (l *RunLedger) cutoff(window time.Duration) time.Time {
	if window <= 0 {
		return time.Time{}
	}
	return l.loadedAt.Add(-window)
}

// Count returns the number of pluginName's runs within window, printed
// receipts excluded — the same number CountRunsSince returns for the same
// ledger and window (gt-o1z7: a printed receipt buys no cooldown).
func (l *RunLedger) Count(pluginName string, window time.Duration) (int, error) {
	if err := l.checkWindow(window); err != nil {
		return 0, err
	}
	cutoff := l.cutoff(window)
	count := 0
	for _, run := range l.runs[pluginName] {
		if cutoff.IsZero() || !run.CreatedAt.Before(cutoff) {
			if run.Result != ResultPrinted {
				count++
			}
		}
	}
	return count, nil
}

// LastRun returns pluginName's most recent run in the ledger, nil when it has
// none. Unlike GetLastRun it cannot see a run older than the span the ledger
// was loaded with.
func (l *RunLedger) LastRun(pluginName string) *PluginRunBead {
	var last *PluginRunBead
	for _, run := range l.runs[pluginName] {
		if last == nil || run.CreatedAt.After(last.CreatedAt) {
			last = run
		}
	}
	return last
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
