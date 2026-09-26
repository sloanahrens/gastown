package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	Use:   "review [<mr-id>] | --landed <sha> [<mr-id>]",
	Short: "Run the om editorial gate against a merge request or a landed commit",
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

--landed <sha> reviews a commit that is already on the target branch, for the
verdict that was never recorded before its merge: no rehearsal, no MR bead —
the range is derived from the commit graph and the note is stamped on the
landed commit itself. The base is the merge-base of the merge's two parents,
never ^1: ^1 is the target's tip at merge time, so diffing against it reports
everything the target gained meanwhile as deletions the branch never made.
The commit must be on origin/<target>'s first-parent chain — the commits the
coverage check reads — and its landing must be a merge of its parents;
anything else is refused with exit 2. The rig defaults to the caller's and the
target to the rig's remote default branch (--rig, --target). The positional MR
id is optional, and only routes the verdict to that bead in addition to the
note.

An MR review is refused (exit 2) while a ready MR that gt mq next ranks higher
is waiting, naming that MR; --out-of-order "<reason>" overrides for a real
exception such as a stack that depends on this MR (gt-tgey7).
Exit code: 0 approve, 1 request_changes, 2 infra failure or out-of-order
refusal (never an approval).

Examples:
  gt mq review gt-mr-abc123
  gt mq review gt-mr-abc123 --rehearsed temp-branch
  gt mq review gt-mr-abc123 --timeout 900
  gt mq review gt-mr-abc123 --json
  gt mq review gt-mr-abc123 --out-of-order "stacked on gt-mr-xyz"
  gt mq review --landed 3107a0d
  gt mq review --landed 3107a0d --target main --rig gastown`,
	Args: mqReviewArgs,
	RunE: runMQReview,
	// A verdict signals through its exit code, and RunE returns a non-nil
	// SilentExitError for request_changes — so without these cobra would print
	// "Error: exit 1" and the usage block after a verdict, on every invocation
	// the refinery patrol and the workers drive. The same two settings also
	// silence cobra for flag and argument errors and leave them on cobra's
	// exit-1 path, the request_changes code: mqReviewArgs, the flag error func
	// and runMQReview's own failure branch are what put every non-verdict
	// failure back on exit 2 (gt-twt2).
	SilenceUsage:  true,
	SilenceErrors: true,
}

// mqReviewUsage is the invocation line every usage error prints, so the three
// callers cannot drift apart on what a valid invocation looks like.
const mqReviewUsage = "usage: gt mq review <mr-id> | gt mq review --landed <sha> [mr-id]"

// mqReviewArgs rejects the wrong number of positional args with the exit-2
// usage shape rather than cobra's exit 1 (gt-twt2).
func mqReviewArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.RangeArgs(0, 2)(cmd, args); err != nil {
		return mqReviewUsageError(err)
	}
	return nil
}

// mqReviewUsageError reports a failure cobra raises before RunE — an unknown
// or unparsable flag, the wrong number of positional args — as the exit-2
// result this command reserves for everything that is not a verdict.
//
// Cobra's own error path cannot carry it: mqReviewCmd silences the error line
// so a verdict is not followed by "Error: exit 1" (see the command literal),
// and that path exits 1 — the request_changes code. Left there, `gt mq review
// --timeout=abc` printed nothing and exited 1, and the refinery patrol read a
// mistyped invocation as a rejection: the MR closed, the source bead reopened
// with MERGE REJECTION, the polecat redispatched to fix code the gate never
// reviewed (gt-twt2). ConfigError is the class the rig's gate script gives its
// own usage errors.
func mqReviewUsageError(err error) error {
	printMQReviewResult(editorial.ReviewResult{
		Exit:   2,
		Class:  editorial.ConfigError,
		Stderr: fmt.Sprintf("%v\n%s", err, mqReviewUsage),
	})
	return NewSilentExit(2)
}

func init() {
	mqReviewCmd.Flags().StringVar(&mqReviewRehearsed, "rehearsed", "", "Already-rehearsed ref/sha to review instead of rehearsing the branch onto its target")
	mqReviewCmd.Flags().BoolVar(&mqReviewJSON, "json", false, "Output the result as JSON")
	mqReviewCmd.Flags().BoolVar(&mqReviewForce, "force", false, "Review even when merge_queue.editorial.required is false for the rig")
	mqReviewCmd.Flags().IntVar(&mqReviewAttempt, "attempt", 1, "Resubmit attempt number recorded on the note")
	mqReviewCmd.Flags().IntVar(&mqReviewTimeout, "timeout", 0, "Override the rig's .om.json backend timeout for this review, in whole seconds (passed to om, recorded on the om note)")
	mqReviewCmd.Flags().BoolVar(&mqReviewReroll, "reroll", false, "Re-review a head that already carries a recorded verdict for the same diff and rubric, replacing it (recorded in the note's attempt history)")
	mqReviewCmd.Flags().StringVar(&mqReviewLanded, "landed", "", "Review a commit that already landed on the target branch instead of a submitted MR: no rehearsal, no MR bead — the verdict is stamped on the landed commit itself (positional MR id optional)")
	mqReviewCmd.Flags().StringVar(&mqReviewTarget, "target", "", "Target branch a --landed commit landed on (default: the rig's remote default branch)")
	mqReviewCmd.Flags().StringVar(&mqReviewOutOfOrder, "out-of-order", "", "Review this MR even though a ready, higher-priority MR outranks it; the value is the reason (e.g. a stack that depends on it)")
	mqReviewCmd.Flags().StringVar(&mqReviewRigFlag, "rig", "", "Rig to review a --landed commit in (default: the current rig)")
	// An unknown flag or an unparsable flag value never reaches runMQReview
	// (gt-twt2).
	mqReviewCmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return mqReviewUsageError(err)
	})
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
			Stderr: mqReviewUsage,
		})
		return NewSilentExit(2)
	}
	if err != nil {
		// This command silences cobra's error line (a verdict's exit code is
		// the signal, and an error line after one reads as part of the
		// verdict), so a real failure prints its one message here — and exits
		// 2 itself rather than returning the bare error, which Execute would
		// map to 1, the request_changes code (gt-twt2).
		fmt.Fprintf(cmd.ErrOrStderr(), "Error: %v\n", err)
		return NewSilentExit(2)
	}
	printMQReviewResult(result)
	if result.Exit == 0 {
		// Approve is not an error: returning a non-nil error here would put
		// cobra's error line on a successful review.
		return nil
	}
	// SilentExitError, not os.Exit: Execute owns the process exit code, as it
	// does for the usage errors above.
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

	if refusal := mqReviewOrderRefusal(issue, fields, fields.Rig, r.BeadsPath()); refusal != "" {
		return editorial.ReviewResult{Exit: 2, Class: editorial.ConfigError, Stderr: refusal}, nil
	}
	if mqReviewOutOfOrder != "" {
		fmt.Fprintf(os.Stderr, "Reviewing %s out of priority order: %s\n", mrID, mqReviewOutOfOrder)
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

	return runEditorialReview(req, deps), nil
}

// runEditorialReview is the seam the gate runs through, so tests can review
// without invoking om. Production always runs the real gate.
var runEditorialReview = func(req editorial.ReviewRequest, deps editorial.Deps) editorial.ReviewResult {
	return editorial.Run(context.Background(), req, deps)
}

// doMQReviewLanded reviews a commit that already landed (the --landed flag):
// no rehearsal, no MR bead to resolve. The rig defaults to the caller's cwd
// (findCurrentRig, falling back to GT_RIG), the target to the rig's remote
// default branch, and the review range is derived from the commit graph alone
// (ResolveLandedRange). The verdict is stamped on the landed commit itself.
func doMQReviewLanded(args []string) (editorial.ReviewResult, error) {
	if len(args) > 1 {
		// Silently ignoring a second id would review the wrong handle.
		return editorial.ReviewResult{
			Exit:   2,
			Class:  editorial.ConfigError,
			Stderr: "usage: gt mq review --landed <sha> [<mr-id>] — at most one MR id",
		}, nil
	}
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

	// Every way of failing to establish the range is a refusal, and a refusal
	// is a config error: exit 2, never 1. An unresolvable sha, a commit that
	// never landed, a root commit, a diff with no patch-id, a landing that is
	// not a merge of its parents — each means this commit has no reviewable
	// landed diff, and a caller must not read any of them as request_changes
	// and send an author off to fix code that was never reviewed.
	landed, err := editorial.ResolveLandedRange(g, mqReviewLanded, target)
	if err != nil {
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

	// No MR bead is minted. The verdict's record is the note on the landed
	// commit, and the gate script reads an absent --mr as "do not route this
	// verdict to a bead" — so a second bead would buy nothing and cost
	// something: a bead labeled gt:merge-request is queue-visible to the
	// dozens of consumers that key on that label (mq list, backpressure,
	// capacity, reaper, ...), and one left open by a crash would read as a
	// phantom merge request until wisp GC ran.
	mrID := ""
	if len(args) > 0 {
		mrID = args[0]
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
		Beads:    beads.New(r.Path),
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

// printMQReviewResult renders a review result. Note is never nil on exit 0 or
// 1 — Run reaches those codes only by writing a note — so the 0/1 branches
// read it directly.
func printMQReviewResult(result editorial.ReviewResult) {
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
