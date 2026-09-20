package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/plugin"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
	"github.com/steveyegge/gastown/internal/rig"
)

var (
	mqReviewRehearsed string
	mqReviewJSON      bool
	mqReviewForce     bool
	mqReviewAttempt   int
	mqReviewTimeout   int
	mqReviewReroll    bool
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
	Args: cobra.ExactArgs(1),
	RunE: runMQReview,
}

func init() {
	mqReviewCmd.Flags().StringVar(&mqReviewRehearsed, "rehearsed", "", "Already-rehearsed ref/sha to review instead of rehearsing the branch onto its target")
	mqReviewCmd.Flags().BoolVar(&mqReviewJSON, "json", false, "Output the result as JSON")
	mqReviewCmd.Flags().BoolVar(&mqReviewForce, "force", false, "Review even when merge_queue.editorial.required is false for the rig")
	mqReviewCmd.Flags().IntVar(&mqReviewAttempt, "attempt", 1, "Resubmit attempt number recorded on the note")
	mqReviewCmd.Flags().IntVar(&mqReviewTimeout, "timeout", 0, "Override the rig's .om.json backend timeout for this review, in whole seconds (passed to om, recorded on the om note)")
	mqReviewCmd.Flags().BoolVar(&mqReviewReroll, "reroll", false, "Re-review a head that already carries a recorded verdict for the same diff and rubric, replacing it (recorded in the note's attempt history)")
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
	result, err := doMQReview(args[0])
	if err != nil {
		return err
	}
	printMQReviewResult(result)
	os.Exit(result.Exit)
	return nil
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
