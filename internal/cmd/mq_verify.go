package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/plugin"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
	"github.com/steveyegge/gastown/internal/refinery/overlap"
	"github.com/steveyegge/gastown/internal/rig"
)

// mq verify runs the single-MR refinery path's test suite and om editorial
// review concurrently (gt-cmcv.1): the two used to run one after the other
// (mol-refinery-patrol's run-tests then quality-review steps), spending the
// review's wall time entirely on top of the suite's even though the review
// does not depend on the suite finishing first. This command replaces both
// steps with one Go-orchestrated call — see internal/refinery/overlap.

var (
	mqVerifyRehearsed string
	mqVerifyAttempt   int
	mqVerifyTimeout   int
	mqVerifySetup     string
	mqVerifyTypecheck string
	mqVerifyLint      string
	mqVerifyBuild     string
	mqVerifyTest      string
	mqVerifyJSON      bool
)

var mqVerifyCmd = &cobra.Command{
	Use:   "verify <mr-id> --rehearsed <sha> --test <cmd>",
	Short: "Run the test suite and the om editorial review concurrently against a rehearsed MR head",
	Long: `Run the single-MR refinery path's quality checks/test suite and the om
editorial review as two goroutines under one context, instead of the suite
then the review in series. Both start together against the already-rehearsed,
already-resolved --rehearsed head (never a moving ref, so the review takes
editorial.Run's no-checkout path and never touches the live clone while the
suite runs there) and are joined in-process — no /tmp files, no polling — by
one call to internal/refinery/overlap.Join.

Both gates stay mandatory. A failed suite always wins: any review verdict is
discarded, never acted on, and never sent as a second FIX_NEEDED alongside
the suite's own. A passed suite acts on the review verdict exactly as
gt mq review's own exit codes mean, just joined here instead of run serially.

If the rig has not set merge_queue.editorial.required, the review is not
started at all (mirrors gt mq review's own config_error refusal) and the
suite's own result alone decides the outcome.

On the --timeout bound, both sides are canceled (the review's om subprocess
and the suite's current step are killed by process group) and joined before
this command returns — never left running in the background. A review
canceled before it reached a verdict maps to exit 2, the same fail-closed
rule gt mq review applies to every other infra failure: an empty exit is
never read as an approval.

Exit code: 0 merge (suite passed, and no review required or review
approved), 1 reject_suite (suite failed; any review verdict is discarded),
2 escalate_review (suite passed, review infra failure or timeout), 3
reject_review (suite passed, review requested changes).

Example:
  gt mq verify gt-mr-abc123 --rehearsed temp --attempt 1 --test "make test" --build "make build" --lint "make lint" --json`,
	Args:          cobra.ExactArgs(1),
	RunE:          runMQVerify,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	mqVerifyCmd.Flags().StringVar(&mqVerifyRehearsed, "rehearsed", "", "Already-rehearsed ref/sha to run the suite and review against (required)")
	mqVerifyCmd.Flags().IntVar(&mqVerifyAttempt, "attempt", 1, "Resubmit attempt number recorded on the review note")
	mqVerifyCmd.Flags().IntVar(&mqVerifyTimeout, "timeout", 2700, "Overall bound in seconds for the joined suite+review call; on expiry both are canceled and the review maps to exit 2")
	mqVerifyCmd.Flags().StringVar(&mqVerifySetup, "setup", "", "Setup/install command. Empty = skip.")
	mqVerifyCmd.Flags().StringVar(&mqVerifyTypecheck, "typecheck", "", "Type-check command. Empty = skip.")
	mqVerifyCmd.Flags().StringVar(&mqVerifyLint, "lint", "", "Lint command. Empty = skip.")
	mqVerifyCmd.Flags().StringVar(&mqVerifyBuild, "build", "", "Build command. Empty = skip.")
	mqVerifyCmd.Flags().StringVar(&mqVerifyTest, "test", "", "Test command. Empty = skip. Pass the fully composed command line (including any container-gate slot wrapping the caller already decided it needs).")
	mqVerifyCmd.Flags().BoolVar(&mqVerifyJSON, "json", false, "Output the result as JSON")
	mqCmd.AddCommand(mqVerifyCmd)
}

// mqVerifyResult is the JSON shape gt mq verify prints. It carries LaunchID
// and HeadSHA — see overlap.Result — so the caller (and a test replaying
// output) can confirm the printed verdict belongs to this invocation of
// this head, not some other attempt.
type mqVerifyResult struct {
	LaunchID string                  `json:"launch_id"`
	HeadSHA  string                  `json:"head_sha"`
	TimedOut bool                    `json:"timed_out"`
	Suite    overlap.SuiteResult     `json:"suite"`
	Review   *editorial.ReviewResult `json:"review,omitempty"`
	Decision overlap.Action          `json:"decision"`
	Discard  bool                    `json:"review_discarded"`
}

func runMQVerify(cmd *cobra.Command, args []string) error {
	mrID := args[0]
	if mqVerifyRehearsed == "" {
		return fmt.Errorf("--rehearsed is required: gt mq verify never rehearses on its own, it reviews and tests an already-rehearsed head")
	}

	workDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting current directory: %w", err)
	}
	bd := beads.New(workDir)

	issue, err := bd.Show(mrID)
	if err != nil {
		if err == beads.ErrNotFound {
			return fmt.Errorf("merge request '%s' not found", mrID)
		}
		return fmt.Errorf("fetching merge request: %w", err)
	}
	fields := beads.ParseMRFields(issue)
	if fields == nil || fields.Rig == "" {
		return fmt.Errorf("merge request '%s' has no parsable rig field", mrID)
	}

	townRoot, r, err := getRig(fields.Rig)
	if err != nil {
		return err
	}

	var editorialCfg config.EditorialConfig
	if mqCfg := rig.ResolveMergeQueueConfig(townRoot, fields.Rig); mqCfg != nil && mqCfg.Editorial != nil {
		editorialCfg = *mqCfg.Editorial
	}
	editorialCfg = editorialCfg.WithDefaults()

	gitDir := filepath.Join(r.Path, "refinery", "rig")
	if _, statErr := os.Stat(gitDir); os.IsNotExist(statErr) {
		gitDir = filepath.Join(r.Path, "mayor", "rig")
	}
	g := git.NewGit(gitDir)

	// Resolve once, up front: never a moving ref. This is the same
	// requirement editorial.Run enforces internally for --rehearsed, done
	// here too so the suite and Result.HeadSHA are bound to the identical
	// commit the review runs against, and so the review reuses this exact
	// resolved value instead of re-resolving it under concurrency.
	headSHA, err := g.Rev(mqVerifyRehearsed)
	if err != nil {
		return fmt.Errorf("resolve rehearsed head %q: %w", mqVerifyRehearsed, err)
	}

	suite := []overlap.SuiteStep{
		{Name: "setup", Cmd: mqVerifySetup},
		{Name: "typecheck", Cmd: mqVerifyTypecheck},
		{Name: "lint", Cmd: mqVerifyLint},
		{Name: "build", Cmd: mqVerifyBuild},
		{Name: "test", Cmd: mqVerifyTest},
	}

	req := overlap.Request{
		HeadSHA: headSHA,
		Suite:   suite,
		Timeout: time.Duration(mqVerifyTimeout) * time.Second,
	}

	if editorialCfg.Required {
		reviewReq := editorial.ReviewRequest{
			RigDir:           r.Path,
			RepoDir:          gitDir,
			MRID:             mrID,
			Worker:           fields.Worker,
			Rig:              fields.Rig,
			Target:           fields.Target,
			Branch:           fields.Branch,
			RehearsedHead:    headSHA,
			Attempt:          mqVerifyAttempt,
			PriorFindings:    editorial.BuildPriorFindings(bd, fields.SourceIssue, mqVerifyAttempt),
			RubricRetirement: editorial.HasRetirementLabel(issue.Labels),
			Config:           editorialCfg,
		}
		reviewDeps := editorial.Deps{
			Git:      g,
			Beads:    beads.New(r.Path),
			Recorder: plugin.NewRecorder(townRoot),
			Exec:     editorial.RunGateScript,
		}
		req.Review = func(ctx context.Context) editorial.ReviewResult {
			return editorial.Run(ctx, reviewReq, reviewDeps)
		}
	}

	deps := overlap.Deps{
		RunStep:     overlap.RealRunStep(gitDir),
		Clock:       overlap.RealClock{},
		NewLaunchID: newVerifyLaunchID,
	}

	result := overlap.Join(cmd.Context(), req, deps)
	decision := overlap.Decide(result)

	out := mqVerifyResult{
		LaunchID: result.LaunchID,
		HeadSHA:  result.HeadSHA,
		TimedOut: result.TimedOut,
		Suite:    result.Suite,
		Review:   result.Review,
		Decision: decision.Action,
		Discard:  decision.ReviewDiscarded,
	}
	printMQVerifyResult(out)

	switch decision.Action {
	case overlap.ActionMerge:
		return nil
	case overlap.ActionRejectSuite:
		return NewSilentExit(1)
	case overlap.ActionEscalateReview:
		return NewSilentExit(2)
	default: // ActionRejectReview
		return NewSilentExit(3)
	}
}

func printMQVerifyResult(result mqVerifyResult) {
	if mqVerifyJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(result)
		return
	}
	fmt.Printf("suite: success=%v", result.Suite.Success)
	if result.Suite.FailedStep != "" {
		fmt.Printf(" failed_step=%s", result.Suite.FailedStep)
	}
	fmt.Println()
	if result.Review != nil {
		fmt.Printf("review: exit=%d", result.Review.Exit)
		if result.Review.Note != nil {
			fmt.Printf(" score=%.2f findings=%d", result.Review.Note.Score, result.Review.Note.FindingsCount)
		}
		if result.Review.Class != "" {
			fmt.Printf(" class=%s", result.Review.Class)
		}
		fmt.Println()
	}
	fmt.Printf("decision: %s (review_discarded=%v, timed_out=%v)\n", result.Decision, result.Discard, result.TimedOut)
}

// newVerifyLaunchID gives each gt mq verify invocation a value unique enough
// to distinguish it in logs/output from any other invocation — an audit tag
// on the join, not a lookup key (the review and suite already run and
// return within this one process call, so there is nothing external to key
// a lookup against).
func newVerifyLaunchID() string {
	return fmt.Sprintf("verify-%d", time.Now().UnixNano())
}
