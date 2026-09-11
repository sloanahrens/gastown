package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/style"
)

// batchSlotTimeout bounds how long runMQBatchRun waits for the
// container-gate slot before giving up on the batch (see gt-tuiy). It is
// independent of gt slot run's own --timeout flag.
const batchSlotTimeout = 60 * time.Minute

// MQ batch command flags. candidates and run each get their own vars so a
// test invoking both commands in the same process can't see stale state
// bleed from one into the other.
var (
	mqBatchCandidatesMinAge string
	mqBatchCandidatesMax    int
	mqBatchCandidatesJSON   bool

	mqBatchRunMinAge string
	mqBatchRunMax    int
	mqBatchRunOnly   []string
	mqBatchRunJSON   bool
)

var mqBatchCmd = &cobra.Command{
	Use:   "batch",
	Short: "Batch-gate multiple ready MRs (stack, one suite run, bisect on red)",
	RunE:  requireSubcommand,
	Long: `Batch-then-bisect processing for the merge queue.

Instead of rebasing, gating, and pushing one ready MR at a time (~10 min
each), 'gt mq batch run' stacks several ready MRs onto a single rebase
branch, runs the full gate suite once, and — if green — fast-forwards all
of them to the target branch in one push. If the suite is red, it bisects
the stack to isolate the culprit MR(s) and lands the rest.

P0/P1 MRs are never batched — they always process individually so critical
fixes aren't held up waiting for a batch to assemble.

Typical formula usage:
  gt mq batch candidates gastown --min-age 1h   # how many are eligible?
  gt mq batch run gastown --min-age 1h --max 12 # assemble, gate, land/bisect`,
}

var mqBatchCandidatesCmd = &cobra.Command{
	Use:   "candidates <rig>",
	Short: "List ready MRs eligible for batching",
	Long: `List ready, non-P0/P1 MRs old enough to be batch-eligible, sorted by
priority score (highest first) and capped at --max.

Read-only — makes no git or bead changes. Use this to decide whether enough
MRs have queued up to be worth batching, and to get the MR list to review
in parallel before calling 'gt mq batch run'.`,
	Args: cobra.ExactArgs(1),
	RunE: runMQBatchCandidates,
}

var mqBatchRunCmd = &cobra.Command{
	Use:   "run <rig>",
	Short: "Assemble, gate, and land a batch of ready MRs",
	Long: `Assembles a batch from ready, non-P0/P1, min-age-eligible MRs (same
selection as 'gt mq batch candidates'), stacks them via ancestry-preserving
merges, runs the configured gate commands once, and either fast-forwards
the whole batch to the target branch or bisects to isolate a red MR and
lands the rest.

Use --only to restrict the batch to a specific MR-ID subset (e.g. after
dropping any MR that an external per-MR review rejected).

Exits non-zero only on an infrastructure error. A batch that lands with
conflicts set aside or a culprit isolated by bisection is a normal, expected
outcome (exit 0) — those MRs simply remain open/ready for the next cycle.`,
	Args: cobra.ExactArgs(1),
	RunE: runMQBatchRun,
}

func init() {
	mqBatchCandidatesCmd.Flags().StringVar(&mqBatchCandidatesMinAge, "min-age", "", "Minimum queue age to be batch-eligible (e.g. 1h, 30m). Default: rig's merge_queue.batch_min_age, or 1h")
	mqBatchCandidatesCmd.Flags().IntVar(&mqBatchCandidatesMax, "max", 0, "Maximum batch size. Default: rig's merge_queue.batch_max, or 12")
	mqBatchCandidatesCmd.Flags().BoolVar(&mqBatchCandidatesJSON, "json", false, "Output as JSON")

	mqBatchRunCmd.Flags().StringVar(&mqBatchRunMinAge, "min-age", "", "Minimum queue age to be batch-eligible (e.g. 1h, 30m). Default: rig's merge_queue.batch_min_age, or 1h")
	mqBatchRunCmd.Flags().IntVar(&mqBatchRunMax, "max", 0, "Maximum batch size. Default: rig's merge_queue.batch_max, or 12")
	mqBatchRunCmd.Flags().StringSliceVar(&mqBatchRunOnly, "only", nil, "Restrict the batch to these MR IDs (comma-separated)")
	mqBatchRunCmd.Flags().BoolVar(&mqBatchRunJSON, "json", false, "Output as JSON")

	mqBatchCmd.AddCommand(mqBatchCandidatesCmd)
	mqBatchCmd.AddCommand(mqBatchRunCmd)
	mqCmd.AddCommand(mqBatchCmd)
}

// resolveBatchMinAge parses the --min-age flag, falling back to the rig's
// configured batch_min_age (or its own "1h" default) when the flag is unset.
func resolveBatchMinAge(flagValue string, mq *config.MergeQueueConfig) (time.Duration, error) {
	raw := strings.TrimSpace(flagValue)
	if raw == "" {
		if mq != nil {
			raw = mq.GetBatchMinAge()
		} else {
			raw = "1h"
		}
	}
	dur, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid min-age %q: %w", raw, err)
	}
	return dur, nil
}

// resolveBatchMax resolves the --max flag, falling back to the rig's
// configured batch_max (or its own default of 12) when the flag is <= 0.
func resolveBatchMax(flagValue int, mq *config.MergeQueueConfig) int {
	if flagValue > 0 {
		return flagValue
	}
	if mq != nil {
		return mq.GetBatchMax()
	}
	return 12
}

// selectBatchCandidates returns ready MRs eligible for batching: P0/P1 MRs
// are excluded (they always process individually, first), MRs younger than
// minAge are excluded, and the result is sorted by priority score, highest
// first. No size cap is applied here — use Engineer.AssembleBatch for that,
// since it also knows how to skip MRs blocked by something not in the batch.
func selectBatchCandidates(eng *refinery.Engineer, minAge time.Duration) ([]*refinery.MRInfo, error) {
	ready, err := eng.ListReadyMRs()
	if err != nil {
		return nil, fmt.Errorf("listing ready MRs: %w", err)
	}
	return filterAndSortBatchCandidates(ready, minAge, time.Now()), nil
}

// filterAndSortBatchCandidates is the pure selection/sort logic behind
// selectBatchCandidates, split out so it's testable without a live Engineer.
func filterAndSortBatchCandidates(ready []*refinery.MRInfo, minAge time.Duration, now time.Time) []*refinery.MRInfo {
	eligible := make([]*refinery.MRInfo, 0, len(ready))
	for _, mr := range ready {
		if mr.Priority <= 1 { // P0, P1: always single-MR, never batched
			continue
		}
		if mr.CreatedAt.IsZero() || now.Sub(mr.CreatedAt) < minAge {
			continue
		}
		eligible = append(eligible, mr)
	}

	scoreOf := func(mr *refinery.MRInfo) float64 {
		return refinery.ScoreMRWithDefaults(refinery.ScoreInput{
			Priority:        mr.Priority,
			MRCreatedAt:     mr.CreatedAt,
			ConvoyCreatedAt: mr.ConvoyCreatedAt,
			RetryCount:      mr.RetryCount,
			Now:             now,
		})
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		return scoreOf(eligible[i]) > scoreOf(eligible[j])
	})

	return eligible
}

// newBatchConfig builds a BatchConfig from refinery.DefaultBatchConfig with
// MaxBatchSize overridden. Starting from the default (rather than a bare
// &refinery.BatchConfig{MaxBatchSize: max} literal) keeps RetryBatchOnFlaky
// and BatchWaitTime at their sensible defaults instead of silently zeroing
// them out.
func newBatchConfig(max int) *refinery.BatchConfig {
	cfg := refinery.DefaultBatchConfig()
	cfg.MaxBatchSize = max
	return cfg
}

// buildBatchGateCommand chains the rig's configured setup/typecheck/lint/
// build/test commands into a single "&&"-joined shell command, run in that
// order so each step gates the next (mirrors the single-MR formula's
// run-tests step). Returns "" if none are configured.
func buildBatchGateCommand(mq *config.MergeQueueConfig) string {
	if mq == nil {
		return ""
	}
	var steps []string
	for _, c := range []string{mq.SetupCommand, mq.TypecheckCommand, mq.LintCommand, mq.BuildCommand, mq.TestCommand} {
		if strings.TrimSpace(c) != "" {
			steps = append(steps, c)
		}
	}
	return strings.Join(steps, " && ")
}

func runMQBatchCandidates(cmd *cobra.Command, args []string) error {
	rigName := args[0]
	townRoot, r, err := getRig(rigName)
	if err != nil {
		return err
	}
	mq := rig.ResolveMergeQueueConfig(townRoot, rigName)

	minAge, err := resolveBatchMinAge(mqBatchCandidatesMinAge, mq)
	if err != nil {
		return err
	}
	max := resolveBatchMax(mqBatchCandidatesMax, mq)

	eng := refinery.NewEngineer(r)
	eligible, err := selectBatchCandidates(eng, minAge)
	if err != nil {
		return err
	}
	batch := eng.AssembleBatch(eligible, newBatchConfig(max))

	if mqBatchCandidatesJSON {
		// eligible_total is the pre-filter count (age + priority only, no cap,
		// no blocker-awareness) — the right number for a "has enough queued up
		// to be worth batching" threshold check. batch_total is len(batch): the
		// post-filter count after applying --max and excluding MRs blocked by
		// something outside the batch. The two can differ; callers that want
		// "how many will actually be batched" should use batch_total or
		// `.batch | length`, not eligible_total.
		return outputJSON(struct {
			Eligible   int                `json:"eligible_total"`
			BatchTotal int                `json:"batch_total"`
			Batch      []*refinery.MRInfo `json:"batch"`
		}{Eligible: len(eligible), BatchTotal: len(batch), Batch: batch})
	}

	fmt.Printf("%s Batch candidates for '%s' (min-age=%s, max=%d):\n\n", style.Bold.Render("📦"), rigName, minAge, max)
	if len(batch) == 0 {
		fmt.Printf("  %s\n", style.Dim.Render(fmt.Sprintf("(none — %d ready MR(s) eligible by age/priority, 0 after blocker filtering)", len(eligible))))
		return nil
	}
	for i, mr := range batch {
		fmt.Printf("  %d. %s [P%d] %s → %s (age %s)\n", i+1, mr.ID, mr.Priority, mr.Branch, mr.Target, formatMRAge(mr.CreatedAt.Format(time.RFC3339)))
	}
	if len(eligible) > len(batch) {
		fmt.Printf("\n  %s\n", style.Dim.Render(fmt.Sprintf("(%d more eligible beyond --max=%d)", len(eligible)-len(batch), max)))
	}
	return nil
}

// acquireBatchGateSlot acquires the container-gate slot for the whole batch
// gate run, or returns (nil, nil) when there is nothing to guard.
//
// Batching's whole point is running the gate suite ONCE for the whole batch
// instead of once per MR (see batch-scan step docs), so the slot is acquired
// once here around the entire ProcessBatch call — including any bisection
// retries it does internally — rather than per-MR (gt-tuiy). Only bother
// when a gate command is actually configured; a batch with zero
// verification never touches Docker.
//
// Split out from runMQBatchRun so the acquire-around-batch behavior is
// testable without a full cobra/rig/engine harness (gt-tuiy attempt 2: this
// path shipped with no test coverage).
func acquireBatchGateSlot(townRoot, rigName, gateCmd string) (*slot.Handle, error) {
	if gateCmd == "" {
		return nil, nil
	}
	return slot.Acquire(townRoot, rigName+"/refinery-batch", batchSlotTimeout)
}

func runMQBatchRun(cmd *cobra.Command, args []string) error {
	rigName := args[0]
	townRoot, r, err := getRig(rigName)
	if err != nil {
		return err
	}
	mq := rig.ResolveMergeQueueConfig(townRoot, rigName)

	if mq != nil && mq.MergeStrategy == "pr" {
		fmt.Printf("%s '%s' uses merge_strategy=pr — batching would bypass PR review/branch protection, so it is disabled for this rig; MRs will be processed individually by the normal single-MR path\n", style.Dim.Render("ℹ"), rigName)
		return nil
	}

	minAge, err := resolveBatchMinAge(mqBatchRunMinAge, mq)
	if err != nil {
		return err
	}
	max := resolveBatchMax(mqBatchRunMax, mq)

	eng := refinery.NewEngineer(r)

	gateCmd := buildBatchGateCommand(mq)
	if gateCmd != "" {
		eng.Config().RunTests = true
		eng.Config().TestCommand = gateCmd
	} else {
		fmt.Fprintf(cmd.ErrOrStderr(), "%s No setup/typecheck/lint/build/test commands configured for '%s' — batch will land with zero verification\n", style.Dim.Render("⚠"), rigName)
	}
	// config.MergeQueueConfig.RetryFlakyTests is a plain int, so a
	// zero value is ambiguous between "explicitly set to 0" and "not
	// configured" — unlike the single-MR path's raw *int JSON parsing,
	// which preserves that distinction. Only override the engineer's own
	// default (1, see DefaultEngineerConfig) when the resolved value is
	// positive; leave it alone rather than forcing a minimum of 1, which
	// would silently clobber an intentional 0.
	if mq != nil && mq.RetryFlakyTests > 0 {
		eng.Config().RetryFlakyTests = mq.RetryFlakyTests
	}

	eligible, err := selectBatchCandidates(eng, minAge)
	if err != nil {
		return err
	}

	if len(mqBatchRunOnly) > 0 {
		allow := make(map[string]bool, len(mqBatchRunOnly))
		for _, id := range mqBatchRunOnly {
			allow[strings.TrimSpace(id)] = true
		}
		filtered := eligible[:0]
		for _, mr := range eligible {
			if allow[mr.ID] {
				filtered = append(filtered, mr)
			}
		}
		eligible = filtered
	}

	batchCfg := newBatchConfig(max)
	batch := eng.AssembleBatch(eligible, batchCfg)
	if len(batch) == 0 {
		fmt.Printf("%s No MRs eligible for batching on '%s' (min-age=%s)\n", style.Dim.Render("ℹ"), rigName, minAge)
		return nil
	}

	target := r.DefaultBranch()
	ctx := context.Background()

	h, slotErr := acquireBatchGateSlot(townRoot, rigName, gateCmd)
	if slotErr != nil {
		return fmt.Errorf("acquiring container-gate slot for batch gate: %w", slotErr)
	}
	if h != nil {
		defer h.Release()
	}

	result := eng.ProcessBatch(ctx, batch, target, batchCfg)

	if mqBatchRunJSON {
		type jsonResult struct {
			Merged      []*refinery.MRInfo    `json:"merged"`
			Culprits    []*refinery.MRInfo    `json:"culprits"`
			Conflicts   []*refinery.MRInfo    `json:"conflicts"`
			Reviewed    []refinery.ReviewedMR `json:"reviewed,omitempty"`
			Ejected     []refinery.EjectedMR  `json:"ejected,omitempty"`
			Skipped     []refinery.SkippedMR  `json:"skipped,omitempty"`
			MergeCommit string                `json:"merge_commit,omitempty"`
			Error       string                `json:"error,omitempty"`
		}
		out := jsonResult{
			Merged:      result.Merged,
			Culprits:    result.Culprits,
			Conflicts:   result.Conflicts,
			Reviewed:    result.Reviewed,
			Ejected:     result.Ejected,
			Skipped:     result.Skipped,
			MergeCommit: result.MergeCommit,
		}
		if result.Error != nil {
			out.Error = result.Error.Error()
		}
		if jsonErr := outputJSON(out); jsonErr != nil {
			return jsonErr
		}
	} else {
		fmt.Printf("%s Batch result for '%s' → %s (%d MRs attempted):\n\n", style.Bold.Render("📦"), rigName, target, len(batch))
		printMQBatchIDs("Merged", result.Merged)
		printMQBatchIDs("Culprits (isolated by bisection)", result.Culprits)
		printMQBatchIDs("Conflicts (left in queue)", result.Conflicts)
		if len(result.Ejected) > 0 {
			ids := make([]string, len(result.Ejected))
			for i, ej := range result.Ejected {
				ids[i] = fmt.Sprintf("%s (%s)", ej.ID, ej.Reason)
			}
			fmt.Printf("  %s: %s\n", "Ejected (patch-id changed on stack)", strings.Join(ids, ", "))
		}
		if len(result.Skipped) > 0 {
			ids := make([]string, len(result.Skipped))
			for i, sk := range result.Skipped {
				ids[i] = fmt.Sprintf("%s (%s)", sk.ID, sk.Reason)
			}
			fmt.Printf("  %s: %s\n", "Skipped (ineligible, left in queue)", strings.Join(ids, ", "))
		}
		if result.MergeCommit != "" {
			fmt.Printf("  Merge commit: %s\n", result.MergeCommit)
		}
	}

	if result.Error != nil {
		return fmt.Errorf("batch processing error: %w", result.Error)
	}
	return nil
}

func printMQBatchIDs(label string, mrs []*refinery.MRInfo) {
	if len(mrs) == 0 {
		return
	}
	ids := make([]string, len(mrs))
	for i, mr := range mrs {
		ids[i] = mr.ID
	}
	fmt.Printf("  %s: %s\n", label, strings.Join(ids, ", "))
}
