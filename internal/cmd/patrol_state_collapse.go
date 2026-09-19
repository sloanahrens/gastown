package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
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
    issue, but it never reached the target branch and no open MR covers it —
    the strictly worse case, since nothing in the system will land the fix
    and no queue entry exists as a reminder. The branch check resolves the
    rig's open MR queue first and skips any issue whose fix is still in
    flight (gt-akap: a source issue self-closed as "pending_mr: <mr>" while
    the MR waits in the queue is the ordinary workflow, not a strand).

Closing a source issue normally happens alongside closing its MR during a
merge. Earlier instances of this pattern were each caught by an agent
reading source by hand, never by a mechanism. This command is that
mechanism, run at patrol time.

If the open MR queue cannot be resolved, the branch check reports no
findings rather than unqualified ones, and says so — an unchecked scan is
not an all-clear.

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
	BranchMRLookup bool                           `json:"branch_mr_lookup_ran"`
	BranchOpenMRs  int                            `json:"branch_open_mrs,omitempty"`
	BranchFindings []witness.BranchStrandFinding  `json:"branch_findings,omitempty"`
	Errors         []string                       `json:"errors,omitempty"`
}

// openMRRefSource returns a witness.BranchRefSource input that resolves the
// rig's open merge-request queue.
//
// It reads through the rig's beads database rather than the scanned rig's
// working directory because merge-request beads are created as wisps
// (gt mq submit, GH#2446), which `bd list --label=gt:merge-request` does not
// return; ListMergeRequests queries both the issues and wisps tables, and is
// the same call `gt mq list` makes. Without this the branch scan would
// report "has no MR" for every in-flight MR in the queue (gt-akap).
func openMRRefSource(rigName string) func() ([]witness.OpenMRRef, error) {
	return func() ([]witness.OpenMRRef, error) {
		_, r, err := getRig(rigName)
		if err != nil {
			return nil, err
		}
		mrs, err := beads.New(r.BeadsPath()).ListMergeRequests(beads.ListOptions{
			Label:    "gt:merge-request",
			Status:   "open",
			Rig:      rigName,
			Priority: -1, // no priority filter
		})
		if err != nil {
			return nil, err
		}
		refs := make([]witness.OpenMRRef, 0, len(mrs))
		for _, mr := range mrs {
			ref := witness.OpenMRRef{ID: mr.ID}
			if fields := beads.ParseMRFields(mr); fields != nil {
				ref.SourceIssue = fields.SourceIssue
				ref.Branch = fields.Branch
			}
			refs = append(refs, ref)
		}
		return refs, nil
	}
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
	branchRefs := witness.DefaultBranchRefSource(mayorRigPath)
	branchRefs.ListOpenMRs = openMRRefSource(rigName)
	branchResult := witness.DetectStrandedBranches(bd, branchRefs, townRoot, rigName, targetBranch, router)

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
			BranchMRLookup: branchResult.MRLookupRan,
			BranchOpenMRs:  branchResult.OpenMRsSeen,
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
			fmt.Printf("  - %s is CLOSED but branch %s was never merged and no open MR covers it (target=%s)\n",
				f.IssueID, f.Branch, targetBranch)
		}
	}

	// A branch scan that never resolved the open-MR queue reports no
	// findings because it declined to judge, not because there are none
	// (gt-akap). Say so loudly: "no findings" and "not checked" must not
	// look alike.
	if !branchResult.MRLookupRan {
		fmt.Printf("%s Branch strand scan skipped: the open merge-request queue could not be resolved, so no "+
			"branch was judged. Unchecked is not an all-clear — see the errors below.\n",
			style.Warning.Render("⚠"))
	}

	for _, e := range result.Errors {
		fmt.Fprintf(os.Stderr, "gt patrol state-collapse: %v\n", e)
	}
	for _, e := range branchResult.Errors {
		fmt.Fprintf(os.Stderr, "gt patrol state-collapse: %v\n", e)
	}

	return nil
}
