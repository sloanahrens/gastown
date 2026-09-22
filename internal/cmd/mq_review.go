package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/plugin"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	mqReviewRehearsed string
	mqReviewJSON      bool
	mqReviewForce     bool
	mqReviewAttempt   int
	mqReviewTimeout   int
	mqReviewReroll    bool
	mqReviewLanded    string
	mqReviewTarget    string
	mqReviewRigFlag   string
)

var mqReviewCmd = &cobra.Command{
	Use:   "review <mr-id>",
	Short: "Run the om editorial gate against a merge request",
	Long: `Run the om editorial gate against a merge request — the single invoker of om.

Rehearses the MR onto its target (or reviews an already-rehearsed head with
--rehearsed), asserts the harness manifest version, refuses a diff that drops
or rewrites an existing .om.json criterion unless the MR bead carries the
rubric-retirement label, invokes the rig's editorial gate script, classifies
the outcome, retries once for transient failures, and on a verdict writes a
git note (proof, refs/notes/om) and a receipt bead (aggregation). An approve
verdict records editorial_reviewed_head on the MR bead; a verdict carrying
major findings still gets follow-up beads filed even when it approves.

A diff that already carries a recorded verdict for the same deployed rubric is
answered from that note without invoking om: the gate is an LLM, so a second
invocation on an unchanged diff re-rolls a near-threshold score rather than
measuring anything, and a caller free to re-roll can roll a defective diff
until it clears the threshold (gt-bveg). The lookup is keyed on the diff's
patch-id, not on the reviewed commit, because this command rehearses a fresh
merge commit on every invocation — keying on that head left the guard inert
on exactly the path it exists for (gt-qa2p). A reuse records the head the
verdict was found on as the MR's editorial_reviewed_head, so the push
precondition finds that note. --reroll re-invokes om deliberately; the note
then records every attempt it has seen, so a re-roll leaves a trace instead
of replacing the previous verdict. Before relying on that history, read
Note.Attempts in internal/refinery/editorial/note.go.

--timeout overrides the rig's .om.json backend timeout for this one review,
for the case where a diff legitimately needs longer than the rig default
(a 526-line diff has been measured at 622s wall). The override is passed to
om and recorded on the note, so it is auditable afterwards rather than
living only in whoever ran the command.

Exit code: 0 approve, 1 request_changes, 2 infra failure (never an approval).

Examples:
  gt mq review gt-mr-abc123
  gt mq review gt-mr-abc123 --rehearsed temp-branch
  gt mq review gt-mr-abc123 --timeout 900
  gt mq review gt-mr-abc123 --json`,
	Args: cobra.RangeArgs(0, 2),
	RunE: runMQReview,
}

func init() {
	mqReviewCmd.Flags().StringVar(&mqReviewRehearsed, "rehearsed", "", "Already-rehearsed ref/sha to review instead of rehearsing the branch onto its target")
	mqReviewCmd.Flags().BoolVar(&mqReviewJSON, "json", false, "Output the result as JSON")
	mqReviewCmd.Flags().BoolVar(&mqReviewForce, "force", false, "Review even when merge_queue.editorial.required is false for the rig")
	mqReviewCmd.Flags().IntVar(&mqReviewAttempt, "attempt", 1, "Resubmit attempt number recorded on the note")
	mqReviewCmd.Flags().IntVar(&mqReviewTimeout, "timeout", 0, "Override the rig's .om.json backend timeout for this review, in whole seconds (passed to om, recorded on the om note)")
	mqReviewCmd.Flags().BoolVar(&mqReviewReroll, "reroll", false, "Re-review a head that already carries a recorded verdict for the same diff and rubric, replacing it (recorded in the note's attempt history)")
	mqReviewCmd.Flags().StringVar(&mqReviewLanded, "landed", "", "Review a commit that already landed on the target branch instead of a submitted MR: no rehearsal, no MR bead — the verdict is stamped on the landed commit itself (positional MR id optional)")
	mqReviewCmd.Flags().StringVar(&mqReviewTarget, "target", "", "Branch the commit landed on, for --landed (default: the rig's remote default branch)")
	mqReviewCmd.Flags().StringVar(&mqReviewRigFlag, "rig", "", "Rig to review in, for --landed (default: the current rig)")
	mqCmd.AddCommand(mqReviewCmd)
}

func runMQReview(cmd *cobra.Command, args []string) error {
	// 0 is the "no override" default; an explicitly-passed non-positive
	// value is rejected rather than silently ignored, since a
	// silently-ignored override would look like it had been applied on the
	// next timeout.
	//
	// Reported as exit 2 (infra failure) in the gate's own result shape,
	// NOT as a RunE error: cobra's error path exits 1, and this command's
	// contract is "0 approve, 1 request_changes, 2 infra failure" — a
	// misinvocation reading as request_changes would send the worker off
	// to fix code that was never reviewed. Same rule the gate script
	// states for its own usage errors.
	if cmd.Flags().Changed("timeout") && mqReviewTimeout <= 0 {
		printMQReviewResult(editorial.ReviewResult{
			Exit:   2,
			Class:  editorial.ConfigError,
			Stderr: fmt.Sprintf("--timeout must be a positive number of seconds, got %d", mqReviewTimeout),
		})
		return NewSilentExit(2)
	}
	var result editorial.ReviewResult
	var err error
	if mqReviewLanded != "" {
		result, err = doMQReviewLanded(args)
	} else if len(args) == 1 {
		result, err = doMQReview(args[0])
	} else {
		printMQReviewResult(editorial.ReviewResult{
			Exit:   2,
			Class:  editorial.ConfigError,
			Stderr: "usage: gt mq review <mr-id> | gt mq review --landed <sha> [mr-id]",
		})
		return NewSilentExit(2)
	}
	if err != nil {
		return err
	}
	printMQReviewResult(result)
	// The exit code is the contract (0 approve, 1 request_changes, 2 infra
	// failure), reported as a SilentExitError rather than an os.Exit call:
	// Execute maps it to the process exit code exactly as the other commands'
	// usage errors do. An os.Exit here would kill the test binary — and take
	// every other test in the package down with it — the moment a faked gate
	// returns a code, which is the only path that exists in a test.
	return NewSilentExit(result.Exit)
}

// doMQReview resolves the MR's rig and config and runs the review, without
// printing or exiting — split out from runMQReview so it's callable from
// tests without terminating the test process.
func doMQReview(mrID string) (editorial.ReviewResult, error) {
	workDir, err := os.Getwd()
	if err != nil {
		return editorial.ReviewResult{}, fmt.Errorf("getting current directory: %w", err)
	}
	bd := beads.New(workDir)

	issue, err := bd.Show(mrID)
	if err != nil {
		if err == beads.ErrNotFound {
			return editorial.ReviewResult{}, fmt.Errorf("merge request '%s' not found", mrID)
		}
		return editorial.ReviewResult{}, fmt.Errorf("fetching merge request: %w", err)
	}
	fields := beads.ParseMRFields(issue)
	if fields == nil || fields.Rig == "" {
		return editorial.ReviewResult{}, fmt.Errorf("merge request '%s' has no parsable rig field", mrID)
	}

	townRoot, r, err := getRig(fields.Rig)
	if err != nil {
		return editorial.ReviewResult{}, err
	}

	var editorialCfg config.EditorialConfig
	if mqCfg := rig.ResolveMergeQueueConfig(townRoot, fields.Rig); mqCfg != nil && mqCfg.Editorial != nil {
		editorialCfg = *mqCfg.Editorial
	}
	editorialCfg = editorialCfg.WithDefaults()

	if !editorialCfg.Required && !mqReviewForce {
		return editorial.ReviewResult{
			Exit:  2,
			Class: editorial.ConfigError,
			Stderr: fmt.Sprintf("merge_queue.editorial.required is false for rig %s; pass --force to review anyway",
				fields.Rig),
		}, nil
	}

	gitDir := filepath.Join(r.Path, "refinery", "rig")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		gitDir = filepath.Join(r.Path, "mayor", "rig")
	}

	req := editorial.ReviewRequest{
		RigDir:        r.Path,
		RepoDir:       gitDir,
		MRID:          mrID,
		Worker:        fields.Worker,
		Rig:           fields.Rig,
		Target:        fields.Target,
		Branch:        fields.Branch,
		RehearsedHead: mqReviewRehearsed,
		Attempt:       mqReviewAttempt,
		// 0 (unset) means "use the rig's .om.json timeout" and passes no
		// flag — see ReviewRequest.TimeoutSeconds.
		TimeoutSeconds: mqReviewTimeout,
		Reroll:         mqReviewReroll,
		PriorFindings:  editorial.BuildPriorFindings(bd, fields.SourceIssue, mqReviewAttempt),
		// The MR bead is where a deliberate rubric retirement is marked —
		// see editorial.RetirementLabel.
		RubricRetirement: editorial.HasRetirementLabel(issue.Labels),
		Config:           editorialCfg,
	}

	deps := editorial.Deps{
		Git:      git.NewGit(gitDir),
		Beads:    beads.New(r.Path),
		Recorder: plugin.NewRecorder(townRoot),
		Exec:     editorial.RunGateScript,
	}

	return editorial.Run(context.Background(), req, deps), nil
}

// runEditorialReview wraps editorial.Run so tests can fake the gate.
// (Test-only indirection; production always runs the real gate.)
var runEditorialReview = func(req editorial.ReviewRequest, deps editorial.Deps) editorial.ReviewResult {
	return editorial.Run(context.Background(), req, deps)
}

// doMQReviewLanded reviews a commit that already landed (the --landed flag):
// no rehearsal, no MR bead to resolve. The rig defaults to the caller's
// (GT_RIG or cwd), the target to the rig's remote default branch, and the
// review range is derived from the commit graph alone (ResolveLandedRange).
// The verdict is stamped on the landed commit itself.
var doMQReviewLanded = func(args []string) (editorial.ReviewResult, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return editorial.ReviewResult{}, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	rigName := strings.TrimSpace(mqReviewRigFlag)
	if rigName == "" {
		rigName, _, err = findCurrentRig(townRoot)
		if err != nil {
			return editorial.ReviewResult{}, fmt.Errorf("resolving rig for --landed: %w (pass --rig to name it)", err)
		}
	}
	_, r, err := getRig(rigName)
	if err != nil {
		return editorial.ReviewResult{}, err
	}

	gitDir := filepath.Join(r.Path, "refinery", "rig")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		gitDir = filepath.Join(r.Path, "mayor", "rig")
	}
	g := git.NewGit(gitDir)

	target := strings.TrimSpace(mqReviewTarget)
	if target == "" {
		target = g.RemoteDefaultBranch()
	}
	if target == "" {
		return editorial.ReviewResult{}, fmt.Errorf("cannot determine the target branch for --landed in %s (pass --target)", rigName)
	}

	// The landed commit is not on its target — never landed or reverted: a
	// config error, not a verdict: it reports exit 2, never 1, so a caller
	// cannot read "not reviewable" as "request_changes".
	landed, err := editorial.ResolveLandedRange(g, mqReviewLanded, target)
	if err != nil && strings.Contains(err.Error(), "not on "+target) {
		return editorial.ReviewResult{
			Exit:   2,
			Class:  editorial.ConfigError,
			Stderr: err.Error(),
		}, nil
	}

	var editorialCfg config.EditorialConfig
	if mqCfg := rig.ResolveMergeQueueConfig(townRoot, rigName); mqCfg != nil && mqCfg.Editorial != nil {
		editorialCfg = *mqCfg.Editorial
	}
	editorialCfg = editorialCfg.WithDefaults()

	if !editorialCfg.Required && !mqReviewForce {
		return editorial.ReviewResult{
			Exit:   2,
			Class:  editorial.ConfigError,
			Stderr: fmt.Sprintf("merge_queue.editorial.required is false for rig %s; pass --force to review anyway", rigName),
		}, nil
	}

	// The retro review leaves no MR bead for the gate script's --mr flag or
	// for readers correlating the receipt: it mints a labeled wisp solely as
	// that handle, and closes it when the review finishes. It is never
	// queueable — its description carries no "Rig:" field, so the refinery's
	// rig filter drops it even while open — and wisp GC reaps any that a
	// crash leaves behind.
	bd := beads.New(r.Path)
	mrID := ""
	if len(args) > 0 {
		mrID = args[0]
	}
	if mrID == "" {
		wisp, err := bd.Create(beads.CreateOptions{
			Title:    fmt.Sprintf("retro review of landed %s", shortSHA(landed.Commit)),
			Labels:   []string{"gt:merge-request", "retro_reviewed"},
			Priority: 3,
			Description: fmt.Sprintf("Retro-review handle for landed commit %s on %s (%s).\nNot a merge request: never queueable (no Rig field); closed when the review finishes.",
				landed.Commit, target, shortSHA(landed.Commit)),
			Ephemeral: true,
			Rig:       rigName,
		})
		if err != nil {
			return editorial.ReviewResult{
				Exit:   2,
				Class:  editorial.Tooling,
				Stderr: fmt.Sprintf("mint retro-review wisp: %v", err),
			}, nil
		}
		mrID = wisp.ID
		defer bd.Close(mrID)
	}

	req := editorial.ReviewRequest{
		RigDir:         r.Path,
		RepoDir:        gitDir,
		MRID:           mrID,
		Rig:            rigName,
		Target:         target,
		Landed:         &landed,
		Attempt:        mqReviewAttempt,
		TimeoutSeconds: mqReviewTimeout,
		Reroll:         mqReviewReroll,
		Config:         editorialCfg,
	}

	deps := editorial.Deps{
		Git:      g,
		Beads:    bd,
		Recorder: plugin.NewRecorder(townRoot),
		Exec:     editorial.RunGateScript,
	}

	return runEditorialReview(req, deps), nil
}

// reusedSuffix marks output answered from a diff's recorded verdict rather
// than from a review run now. The exit code is the same either way, so this
// line is the only thing separating "om approved this" from "om approved this
// the first time it was asked" — a caller that cannot tell the two apart
// cannot tell a re-roll from a review either.
func reusedSuffix(result editorial.ReviewResult) string {
	if !result.Reused {
		return ""
	}
	return " [recorded verdict reused: same diff, om not re-invoked — pass --reroll to re-review]"
}

// The gate is faked in tests, where it may return a ReviewResult with no Note
// set (the fakes care only about the exit code). A real om run always writes
// a note on a 0/1 verdict — Run only reaches a 0/1 result by writing one — so
// this can fire on the exit-code path here, and a nil dereference would panic
// the whole package's test binary, taking every other test in it down with it.
func printableNote(n *editorial.Note) *editorial.Note {
	if n == nil {
		return &editorial.Note{}
	}
	return n
}

func printMQReviewResult(result editorial.ReviewResult) {
	result.Note = printableNote(result.Note)
	if mqReviewJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(result)
	} else {
		switch result.Exit {
		case 0:
			fmt.Printf("approve (score %.2f)%s\n", result.Note.Score, reusedSuffix(result))
			if result.Stderr != "" {
				// Non-fatal follow-up work that didn't stop the approve
				// (e.g. filing a major finding's follow-up bead failed) —
				// DECISION 8 says approval never dissolves a finding, so
				// surface it rather than dropping it on the success path.
				fmt.Fprintln(os.Stderr, result.Stderr)
			}
		case 1:
			fmt.Printf("request_changes (score %.2f, %d finding(s))%s\n", result.Note.Score, result.Note.FindingsCount, reusedSuffix(result))
		default:
			fmt.Printf("failed: %s\n", result.Class)
			if result.Stderr != "" {
				fmt.Fprintln(os.Stderr, result.Stderr)
			}
		}
	}
}
