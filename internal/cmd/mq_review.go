package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

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
)

var mqReviewCmd = &cobra.Command{
	Use:   "review <mr-id>",
	Short: "Run the om editorial gate against a merge request",
	Long: `Run the om editorial gate against a merge request — the single invoker of om.

Rehearses the MR onto its target (or reviews an already-rehearsed head with
--rehearsed), asserts the harness manifest version, invokes the rig's
editorial gate script, classifies the outcome, retries once for transient
failures, and on a verdict writes a git note (proof, refs/notes/om) and a
receipt bead (aggregation). An approve verdict records
editorial_reviewed_head on the MR bead; a verdict carrying major findings
still gets follow-up beads filed even when it approves.

Exit code: 0 approve, 1 request_changes, 2 infra failure (never an approval).

Examples:
  gt mq review gt-mr-abc123
  gt mq review gt-mr-abc123 --rehearsed temp-branch
  gt mq review gt-mr-abc123 --json`,
	Args: cobra.ExactArgs(1),
	RunE: runMQReview,
}

func init() {
	mqReviewCmd.Flags().StringVar(&mqReviewRehearsed, "rehearsed", "", "Already-rehearsed ref/sha to review instead of rehearsing the branch onto its target")
	mqReviewCmd.Flags().BoolVar(&mqReviewJSON, "json", false, "Output the result as JSON")
	mqReviewCmd.Flags().BoolVar(&mqReviewForce, "force", false, "Review even when merge_queue.editorial.required is false for the rig")
	mqReviewCmd.Flags().IntVar(&mqReviewAttempt, "attempt", 1, "Resubmit attempt number recorded on the note")
	mqCmd.AddCommand(mqReviewCmd)
}

func runMQReview(cmd *cobra.Command, args []string) error {
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
		PriorFindings: buildPriorFindings(bd, fields.SourceIssue, mqReviewAttempt),
		Config:        editorialCfg,
	}

	deps := editorial.Deps{
		Git:      git.NewGit(gitDir),
		Beads:    beads.New(r.Path),
		Recorder: plugin.NewRecorder(townRoot),
		Exec:     editorial.RunGateScript,
	}

	return editorial.Run(context.Background(), req, deps), nil
}

// priorFindingLineRE matches the interim MERGE REJECTION finding-line format
// ("- id:<hex> sev:<severity> <path>:<line> — <title>"); om-gate T10 owns
// writing these lines and may refine the format.
var priorFindingLineRE = regexp.MustCompile(`^-\s*id:(\S+)\s+sev:(\S+)\s+([^:]+):(\d+)\s+—\s+(.*)$`)

// buildPriorFindings collects prior MERGE REJECTION findings for sourceIssue
// so the reviewer classifies them resolved/unresolved/regressed instead of
// rediscovering them from scratch. Best-effort: an unparsable or missing
// source issue yields no prior findings rather than an error.
func buildPriorFindings(bd *beads.Beads, sourceIssue string, attempt int) []editorial.PriorFinding {
	if sourceIssue == "" {
		return nil
	}
	issue, err := bd.Show(sourceIssue)
	if err != nil || issue == nil {
		return nil
	}
	var findings []editorial.PriorFinding
	for _, line := range strings.Split(issue.Notes, "\n") {
		m := priorFindingLineRE.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		lineNo, _ := strconv.Atoi(m[4])
		findings = append(findings, editorial.PriorFinding{
			ID:       m[1],
			Severity: m[2],
			Path:     m[3],
			Line:     lineNo,
			Title:    m[5],
			Attempt:  attempt,
		})
	}
	return findings
}

func printMQReviewResult(result editorial.ReviewResult) {
	if mqReviewJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(result)
	} else {
		switch result.Exit {
		case 0:
			fmt.Printf("approve (score %.2f)\n", result.Note.Score)
		case 1:
			fmt.Printf("request_changes (score %.2f, %d finding(s))\n", result.Note.Score, result.Note.FindingsCount)
		default:
			fmt.Printf("failed: %s\n", result.Class)
			if result.Stderr != "" {
				fmt.Fprintln(os.Stderr, result.Stderr)
			}
		}
	}
}
