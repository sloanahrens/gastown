package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/witness"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	patrolStateCollapseJSON bool
	patrolStateCollapseRig  string
)

var patrolStateCollapseCmd = &cobra.Command{
	Use:   "state-collapse",
	Short: "Find issues closed while their fix isn't verified in force",
	Long: `Scan for the gt-zzd/gt-3ii state-collapse signature: a source issue
recorded as done (bead closed) while the fix it describes is not verified
to be in force.

Two independent checks run:

  - MR-driven (gt-zzd): the merge-request bead created to land the fix is
    still open in the queue.
  - Branch-driven (gt-3ii): a polecat branch exists on the remote for the
    issue, but it never reached the target branch and no MR bead was ever
    created for it — the strictly worse case, since nothing in the system
    will land the fix and no queue entry exists as a reminder.

Closing a source issue normally happens alongside closing its MR during a
merge. Earlier instances of this pattern were each caught by an agent
reading source by hand, never by a mechanism. This command is that
mechanism, run at patrol time.

Findings are recorded as a bead comment on the source issue and escalated to
the rig's mayor by mail.

Examples:
  gt patrol state-collapse                # Scan current rig
  gt patrol state-collapse --rig gastown  # Scan specific rig
  gt patrol state-collapse --json         # Machine-readable output`,
	RunE: runPatrolStateCollapse,
}

func init() {
	patrolStateCollapseCmd.Flags().BoolVar(&patrolStateCollapseJSON, "json", false, "Output as JSON")
	patrolStateCollapseCmd.Flags().StringVar(&patrolStateCollapseRig, "rig", "", "Rig to scan (default: infer from cwd or GT_RIG)")
	patrolCmd.AddCommand(patrolStateCollapseCmd)
}

// PatrolStateCollapseOutput is the JSON output format for `gt patrol state-collapse`.
type PatrolStateCollapseOutput struct {
	Rig            string                         `json:"rig"`
	Checked        int                            `json:"checked"`
	Findings       []witness.StateCollapseFinding `json:"findings,omitempty"`
	BranchChecked  int                            `json:"branch_checked"`
	BranchFindings []witness.BranchStrandFinding  `json:"branch_findings,omitempty"`
	Errors         []string                       `json:"errors,omitempty"`
}

func runPatrolStateCollapse(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	rigName := patrolStateCollapseRig
	if rigName == "" {
		rigName = os.Getenv("GT_RIG")
		if rigName == "" {
			rigName, err = inferRigFromCwd(townRoot)
			if err != nil {
				return fmt.Errorf("could not determine rig: %w\nUse --rig to specify", err)
			}
		}
	}

	bd := witness.DefaultBdCli()
	router := mail.NewRouter(townRoot)

	result := witness.DetectStateCollapse(bd, townRoot, rigName, router)

	targetBranch := "main"
	if rigCfg, err := rig.LoadRigConfig(filepath.Join(townRoot, rigName)); err == nil && rigCfg.DefaultBranch != "" {
		targetBranch = rigCfg.DefaultBranch
	}
	mayorRigPath := filepath.Join(townRoot, rigName, "mayor", "rig")
	branchResult := witness.DetectStrandedBranches(bd, witness.DefaultBranchRefSource(mayorRigPath), townRoot, rigName, targetBranch, router)

	if patrolStateCollapseJSON {
		errs := make([]string, 0, len(result.Errors)+len(branchResult.Errors))
		for _, e := range result.Errors {
			errs = append(errs, e.Error())
		}
		for _, e := range branchResult.Errors {
			errs = append(errs, e.Error())
		}
		out := PatrolStateCollapseOutput{
			Rig:            rigName,
			Checked:        result.Checked,
			Findings:       result.Findings,
			BranchChecked:  branchResult.Checked,
			BranchFindings: branchResult.Findings,
			Errors:         errs,
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	totalFindings := len(result.Findings) + len(branchResult.Findings)
	if totalFindings == 0 {
		fmt.Printf("%s No state collapse found (%d open MR(s), %d closed-issue branch(es) checked in %s)\n",
			style.Success.Render("✓"), result.Checked, branchResult.Checked, rigName)
	} else {
		fmt.Printf("%s Found %d state collapse(s) in %s (%d open MR(s), %d closed-issue branch(es) checked):\n",
			style.Warning.Render("⚠"), totalFindings, rigName, result.Checked, branchResult.Checked)
		for _, f := range result.Findings {
			fmt.Printf("  - %s is CLOSED but %s is still %s (branch=%s target=%s)\n",
				f.IssueID, f.MRID, f.MRStatus, f.Branch, f.Target)
		}
		for _, f := range branchResult.Findings {
			fmt.Printf("  - %s is CLOSED but branch %s was never merged and has no MR (target=%s)\n",
				f.IssueID, f.Branch, targetBranch)
		}
	}

	for _, e := range result.Errors {
		fmt.Fprintf(os.Stderr, "gt patrol state-collapse: %v\n", e)
	}
	for _, e := range branchResult.Errors {
		fmt.Fprintf(os.Stderr, "gt patrol state-collapse: %v\n", e)
	}

	return nil
}
