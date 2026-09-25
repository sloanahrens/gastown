package cmd

import (
	"context"
	"fmt"
	"io"
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

	mqBatchRunMinAge   string
	mqBatchRunMax      int
	mqBatchRunMinCount int
	mqBatchRunOnly     []string
	mqBatchRunJSON     bool
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

P0 MRs are never batched — they always process individually so critical fixes
aren't held up waiting for a batch to assemble. P1 MRs batch alongside P2
since 2026-09-19: on a town that files nearly all of its work at P1,
excluding them meant batching (enabled, min_count 3) never engaged at all.

Typical formula usage:
  gt mq batch candidates gastown --min-age 1h   # how many are eligible?
  gt mq batch run gastown --min-age 1h --max 12 # assemble, gate, land/bisect`,
}

var mqBatchCandidatesCmd = &cobra.Command{
	Use:   "candidates <rig>",
	Short: "List ready MRs eligible for batching",
	Long: `List ready, non-P0 MRs old enough to be batch-eligible, sorted by
priority score (highest first) and capped at --max.

MRs a previous batch isolated as culprits are excluded while their branch
still carries the head that batch judged — the mark is the batch-culprit:<sha>
label 'gt mq batch run' writes on the MR bead, and a rework that moves the
branch head clears it. Those MRs stay ready and take the single-MR path.

Read-only — makes no git or bead changes. Use this to decide whether enough
MRs have queued up to be worth batching, and to get the MR list to review
in parallel before calling 'gt mq batch run'.`,
	Args: cobra.ExactArgs(1),
	RunE: runMQBatchCandidates,
}

var mqBatchRunCmd = &cobra.Command{
	Use:   "run <rig>",
	Short: "Assemble, gate, and land a batch of ready MRs",
	Long: `Assembles a batch from ready, non-P0, min-age-eligible MRs (same
selection as 'gt mq batch candidates'), stacks them via ancestry-preserving
merges, runs the configured gate commands once, and either fast-forwards
the whole batch to the target branch or bisects to isolate a red MR and
lands the rest.

When fewer than batch_min_count MRs are eligible, this exits without
batching (exit 0, nothing touched) so the single-MR path picks them up
instead of paying a batch's overhead for one or two MRs. The threshold is
enforced here, not just in the refinery formula's prose.

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
	mqBatchRunCmd.Flags().IntVar(&mqBatchRunMinCount, "min-count", 0, "Minimum number of eligible MRs required to batch at all; below it the run exits without batching. Default: rig's merge_queue.batch_min_count, or 4")
	mqBatchRunCmd.Flags().StringSliceVar(&mqBatchRunOnly, "only", nil, "Restrict the batch to these MR IDs (comma-separated); an MR held back as a batch culprit is not batch-eligible, so process it singly instead")
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

// resolveBatchMinCount resolves the --min-count flag, falling back to the
// rig's configured batch_min_count (or its own default of 4) when the flag is
// <= 0. Parallel to resolveBatchMax: a non-positive flag means "unset", since
// GetBatchMinCount already treats zero as "use the default".
func resolveBatchMinCount(flagValue int, mq *config.MergeQueueConfig) int {
	if flagValue > 0 {
		return flagValue
	}
	if mq != nil {
		return mq.GetBatchMinCount()
	}
	return 4
}

// belowBatchMinCount reports whether a candidate list is too short to be worth
// batching, writing the reason to w when it is.
//
// batch_min_count used to live only in the refinery formula's prose, so the
// check was advisory: a refinery that ran 'gt mq batch run' anyway batched
// whatever it found. Enforcing it here makes the threshold real regardless of
// which caller invokes the command.
func belowBatchMinCount(w io.Writer, candidates, minCount int) bool {
	if candidates >= minCount {
		return false
	}
	fmt.Fprintf(w, "%s batch: %d eligible < min_count %d, not batching\n", style.Dim.Render("ℹ"), candidates, minCount)
	return true
}

// selectBatchCandidates returns ready MRs eligible for batching, plus the
// ready MRs a current batch-culprit mark excluded. P0 MRs are excluded (they
// always process individually, first), MRs younger than minAge are excluded,
// and the result is sorted by priority score, highest first. No size cap is
// applied here — use Engineer.AssembleBatch for that, since it also knows how
// to skip MRs blocked by something not in the batch.
//
// The marked MRs are returned too, so a caller that decides not to batch can
// say why the queue looked shorter than 'gt mq list' reports.
func selectBatchCandidates(eng *refinery.Engineer, minAge time.Duration) ([]*refinery.MRInfo, []*refinery.MRInfo, error) {
	ready, err := eng.ListReadyMRs()
	if err != nil {
		return nil, nil, fmt.Errorf("listing ready MRs: %w", err)
	}
	eligible, marked := partitionBatchCandidates(ready, minAge, time.Now())
	return eligible, marked, nil
}

// filterAndSortBatchCandidates is the pure selection/sort logic behind
// selectBatchCandidates, split out so it's testable without a live Engineer.
func filterAndSortBatchCandidates(ready []*refinery.MRInfo, minAge time.Duration, now time.Time) []*refinery.MRInfo {
	eligible, _ := partitionBatchCandidates(ready, minAge, now)
	return eligible
}

// partitionBatchCandidates splits ready MRs into batch-eligible and
// batch-culprit-marked, and sorts the eligible ones by priority score.
//
// An MR a previous batch isolated as a culprit, at a head its branch still
// carries, is not eligible (gt-gz8l, refinery.MRMarkedBatchCulprit): stacking
// it again re-runs the full gate, the flaky retry, and the bisection to
// re-derive the same answer, while the batch's good MRs wait behind it. It
// takes the single-MR path instead. This is candidate selection only —
// ListReadyMRs still returns it, so 'gt mq next' and process-branch see it.
func partitionBatchCandidates(ready []*refinery.MRInfo, minAge time.Duration, now time.Time) (eligible, marked []*refinery.MRInfo) {
	eligible = make([]*refinery.MRInfo, 0, len(ready))
	for _, mr := range ready {
		if mr.Priority == 0 { // P0: always single-MR; P1 batches since 2026-09-19
			continue
		}
		if mr.CreatedAt.IsZero() || now.Sub(mr.CreatedAt) < minAge {
			continue
		}
		// Checked after the age gate so the returned bucket counts only the
		// MRs the mark alone kept out of the batch.
		if refinery.MRMarkedBatchCulprit(mr) {
			marked = append(marked, mr)
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

	return eligible, marked
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

// buildBatchGateSteps returns the rig's configured setup/typecheck/lint/build/
// test commands in the order each gates the next (mirrors the single-MR
// formula's run-tests step). Returns nil if none are configured.
//
// Named steps rather than one "&&"-joined command: the batch could not say
// which step failed, and it ran the rig's `make lint` with nothing to wait out
// golangci-lint's module lock, so a contended lint was reported as the
// batch's own test failure and every MR in it was ejected as a culprit
// (gt-ijqw).
func buildBatchGateSteps(mq *config.MergeQueueConfig) []refinery.GateStep {
	if mq == nil {
		return nil
	}
	var steps []refinery.GateStep
	for _, s := range []refinery.GateStep{
		{Name: "setup", Cmd: mq.SetupCommand},
		{Name: "typecheck", Cmd: mq.TypecheckCommand},
		{Name: "lint", Cmd: mq.LintCommand},
		{Name: "build", Cmd: mq.BuildCommand},
		{Name: "test", Cmd: mq.TestCommand},
	} {
		if strings.TrimSpace(s.Cmd) != "" {
			steps = append(steps, s)
		}
	}
	return steps
}

// applyBatchEditorialConfig wires the rig's resolved merge_queue.editorial
// section onto the batch Engineer, the same way Engineer.LoadConfig defaults
// it (WithDefaults) for every other caller. refinery.NewEngineer never loads
// config.json itself, and runMQBatchRun otherwise only threads through
// RunTests/BatchGateSteps/RetryFlakyTests — so without this call
// e.config.Editorial stayed nil for every batch Engineer regardless of the
// rig's config, and reviewBatchCandidates/ejectPatchIDChanged both treat a
// nil Editorial as "this rig never asked for review" and pass every
// candidate straight through unreviewed. That is what let 'gt mq batch run'
// land 4 MRs on gastown with no om editorial review despite
// editorial.required=true (gt-ww20): a silent bypass, not a refusal.
func applyBatchEditorialConfig(eng *refinery.Engineer, mq *config.MergeQueueConfig) {
	if mq == nil || mq.Editorial == nil {
		return
	}
	defaulted := mq.Editorial.WithDefaults()
	eng.Config().Editorial = &defaulted
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
	eligible, markedCulprits, err := selectBatchCandidates(eng, minAge)
	if err != nil {
		return err
	}
	batch := eng.AssembleBatch(eligible, newBatchConfig(max))

	if mqBatchCandidatesJSON {
		// eligible_total is the pre-filter count (age + priority only, no cap,
		// no blocker-awareness, minus MRs marked as batch culprits) — the right
		// number for a "has enough queued up to be worth batching" threshold
		// check. batch_total is len(batch): the post-filter count after applying
		// --max and excluding MRs blocked by something outside the batch. The
		// two can differ; callers that want "how many will actually be batched"
		// should use batch_total or `.batch | length`, not eligible_total.
		return outputJSON(struct {
			Eligible       int                `json:"eligible_total"`
			CulpritSkipped int                `json:"batch_culprit_skipped"`
			BatchTotal     int                `json:"batch_total"`
			Batch          []*refinery.MRInfo `json:"batch"`
		}{Eligible: len(eligible), CulpritSkipped: len(markedCulprits), BatchTotal: len(batch), Batch: batch})
	}

	fmt.Printf("%s Batch candidates for '%s' (min-age=%s, max=%d):\n\n", style.Bold.Render("📦"), rigName, minAge, max)
	if len(batch) == 0 {
		fmt.Printf("  %s\n", style.Dim.Render(fmt.Sprintf("(none — %d ready MR(s) eligible by age/priority, 0 after blocker filtering)", len(eligible))))
		printBatchCulpritSkipped(markedCulprits)
		return nil
	}
	for i, mr := range batch {
		fmt.Printf("  %d. %s [P%d] %s → %s (age %s)\n", i+1, mr.ID, mr.Priority, mr.Branch, mr.Target, formatMRAge(mr.CreatedAt.Format(time.RFC3339)))
	}
	if len(eligible) > len(batch) {
		fmt.Printf("\n  %s\n", style.Dim.Render(fmt.Sprintf("(%d more eligible beyond --max=%d)", len(eligible)-len(batch), max)))
	}
	printBatchCulpritSkipped(markedCulprits)
	return nil
}

// printBatchCulpritSkipped says which ready MRs the batch-culprit marks kept
// out of this list, so a shrunken batch is never read as an empty queue — the
// MRs are still ready and still visible to 'gt mq next' and process-branch.
func printBatchCulpritSkipped(marked []*refinery.MRInfo) {
	if len(marked) == 0 {
		return
	}
	ids := make([]string, len(marked))
	for i, mr := range marked {
		ids[i] = mr.ID
	}
	fmt.Printf("  %s\n", style.Dim.Render(fmt.Sprintf("(%d held back as batch culprits at their current head: %s — process-branch handles them alone)",
		len(marked), strings.Join(ids, ", "))))
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
func acquireBatchGateSlot(townRoot, rigName string, hasGate bool) (*slot.Handle, error) {
	if !hasGate {
		return nil, nil
	}
	return slot.AcquirePool(townRoot, rigName+"/refinery-batch", batchSlotTimeout, containerGatePool(townRoot))
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
	minCount := resolveBatchMinCount(mqBatchRunMinCount, mq)

	eng := refinery.NewEngineer(r)
	applyBatchEditorialConfig(eng, mq)

	gateSteps := buildBatchGateSteps(mq)
	hasGate := len(gateSteps) > 0
	if hasGate {
		eng.Config().RunTests = true
		eng.Config().BatchGateSteps = gateSteps
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

	eligible, markedCulprits, err := selectBatchCandidates(eng, minAge)
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

	// Say which MRs the marks held back before anything is decided on the
	// shortened list, so a batch that declines to assemble names the reason.
	// --only does not re-admit them: it filters this same list, which is what
	// its help says.
	printBatchCulpritSkipped(markedCulprits)

	// Too few candidates to be worth batching: exit before assembling or
	// touching the gate slot, so the single-MR path (which runs right after
	// this in the refinery formula) handles them instead. Exit 0 — a short
	// queue is a normal condition, not an error.
	//
	// Counted after --only, i.e. against what would actually be batched. An
	// --only that whittles the list below the floor therefore does not batch;
	// the printed line says why, and --min-count lowers the floor when a
	// deliberately small batch is what's wanted.
	if belowBatchMinCount(cmd.OutOrStdout(), len(eligible), minCount) {
		return nil
	}

	batchCfg := newBatchConfig(max)
	batch := eng.AssembleBatch(eligible, batchCfg)
	if len(batch) == 0 {
		fmt.Printf("%s No MRs eligible for batching on '%s' (min-age=%s)\n", style.Dim.Render("ℹ"), rigName, minAge)
		return nil
	}

	target := r.DefaultBranch()
	ctx := context.Background()

	h, slotErr := acquireBatchGateSlot(townRoot, rigName, hasGate)
	if slotErr != nil {
		return fmt.Errorf("acquiring container-gate slot for batch gate: %w", slotErr)
	}
	if h != nil {
		defer func() { _ = h.Release() }()
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

	// The gate is done; don't hold its slot through the post-merge install.
	// Release is idempotent, so the deferred Release above stays harmless.
	if h != nil {
		_ = h.Release()
	}
	// stderr, not stdout: --json mode's stdout must stay a single JSON document.
	runBatchPostMergeCommand(townRoot, rigName, r.Path, mq, result, target, cmd.ErrOrStderr())

	// One landed batch is one unit. A batch whose push landed but whose
	// cleanup errored still archives its members' MERGE_READY, but keeps the
	// session: the agent must stay alive to recover the failed members.
	if result.MergeCommit != "" && len(result.Merged) > 0 {
		errOut := cmd.ErrOrStderr()
		sess := refinerySessionFor(r.Name)
		workDir := refineryWorkDir(r.Path)
		mrs := make([]unitMR, 0, len(result.Merged))
		for _, m := range result.Merged {
			if m == nil {
				continue
			}
			mrs = append(mrs, unitMR{ID: m.ID, Branch: m.Branch, Worker: m.Worker, SourceIssue: m.SourceIssue, Target: m.Target})
		}
		rep := completeUnitAndCycle(unitCycleParams{
			Rig:             r.Name,
			Mode:            unitBatch,
			RefinerySession: sess,
			WorkDir:         workDir,
			MRs:             mrs,
			MergeCommit:     result.MergeCommit,
			CycleEnabled:    mq != nil && mq.CycleSessionAfterMerge,
			BatchErrored:    result.Error != nil,
		}, defaultUnitCycleDeps(townRoot, r.BeadsPath(), workDir, sess, errOut))
		if rep.SkipCause != "" {
			fmt.Fprintf(errOut, "  %s session kept: %s\n", style.Dim.Render("○"), rep.SkipCause)
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
