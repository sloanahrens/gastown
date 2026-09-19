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
    still open in the queue. Both checks read the queue through the
    wisps-aware lookup (beads.ListMergeRequests, the call "gt mq list" makes)
    rather than a plain "bd list --label=gt:merge-request", which does not
    return MR wisps — before gt-92ry the MR-driven check was structurally
    blind to the queue and reported it as empty.
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

If the open MR queue cannot be resolved, neither check reports findings —
they decline to judge rather than judge unqualified — and the summary says
so in place of an all-clear. "Checked and clean" and "never ran" must not
print alike (gt-akap, gt-92ry).

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
	MRLookupRan    bool                           `json:"mr_lookup_ran"`
	OpenMRs        int                            `json:"open_mrs,omitempty"`
	Findings       []witness.StateCollapseFinding `json:"findings,omitempty"`
	BranchChecked  int                            `json:"branch_checked"`
	BranchMRLookup bool                           `json:"branch_mr_lookup_ran"`
	BranchOpenMRs  int                            `json:"branch_open_mrs,omitempty"`
	BranchFindings []witness.BranchStrandFinding  `json:"branch_findings,omitempty"`
	Errors         []string                       `json:"errors,omitempty"`
	AllClear       bool                           `json:"all_clear"`
}

// openMRRefSource returns a witness.BranchRefSource input that resolves the
// rig's open merge-request queue.
//
// It reads through the rig's beads database rather than the scanned rig's
// working directory because merge-request beads are created as wisps
// (gt mq submit, GH#2446), which `bd list --label=gt:merge-request` does not
// return; ListMergeRequests queries both the issues and wisps tables, and is
// the same call `gt mq list` makes. Without this both detectors would report
// "has no MR" for every in-flight MR in the queue (gt-akap), and the
// MR-driven detector would see an empty queue outright (gt-92ry).
//
// The queue is resolved once per process: both detectors read the same
// snapshot, so one run's report cannot describe two different queues, and a
// single patrol cycle costs one lookup rather than one per detector. An
// error is memoized with the same intent — both detectors see the same
// unavailability rather than one of them silently getting a second chance.
func openMRRefSource(rigName string) func() ([]witness.OpenMRRef, error) {
	var (
		resolved bool
		refs     []witness.OpenMRRef
		err      error
	)
	return func() ([]witness.OpenMRRef, error) {
		if resolved {
			return refs, err
		}
		resolved = true

		_, r, getErr := getRig(rigName)
		if getErr != nil {
			err = getErr
			return nil, err
		}
		mrs, listErr := beads.New(r.BeadsPath()).ListMergeRequests(beads.ListOptions{
			Label:    "gt:merge-request",
			Status:   "open",
			Rig:      rigName,
			Priority: -1, // no priority filter
		})
		if listErr != nil {
			err = listErr
			return nil, err
		}
		refs = make([]witness.OpenMRRef, 0, len(mrs))
		for _, mr := range mrs {
			ref := witness.OpenMRRef{ID: mr.ID, Status: mr.Status}
			if fields := beads.ParseMRFields(mr); fields != nil {
				ref.SourceIssue = fields.SourceIssue
				ref.Branch = fields.Branch
				ref.Target = fields.Target
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

	targetBranch := "main"
	if rigCfg, err := rig.LoadRigConfig(filepath.Join(townRoot, rigName)); err == nil && rigCfg.DefaultBranch != "" {
		targetBranch = rigCfg.DefaultBranch
	}
	mayorRigPath := filepath.Join(townRoot, rigName, "mayor", "rig")
	refs := witness.DefaultBranchRefSource(mayorRigPath)
	refs.ListOpenMRs = openMRRefSource(rigName)

	// Both detectors read the same injected MR-queue source: the wisps-aware
	// lookup, not `bd list --label=gt:merge-request` (gt-92ry).
	result := witness.DetectStateCollapse(bd, refs, townRoot, rigName, router)
	branchResult := witness.DetectStrandedBranches(bd, refs, townRoot, rigName, targetBranch, router)

	summary, allClear := witness.StateCollapseSummary(result, branchResult, rigName)

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
			MRLookupRan:    result.MRLookupRan,
			OpenMRs:        result.OpenMRsSeen,
			Findings:       result.Findings,
			BranchChecked:  branchResult.Checked,
			BranchMRLookup: branchResult.MRLookupRan,
			BranchOpenMRs:  branchResult.OpenMRsSeen,
			BranchFindings: branchResult.Findings,
			Errors:         errs,
			AllClear:       allClear,
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if allClear {
		fmt.Printf("%s %s\n", style.Success.Render("✓"), summary)
	} else {
		// Covers both findings and an unresolved MR queue. The summary is
		// what keeps a scan that never ran from printing as the same thing
		// as a scan that ran and found nothing (gt-92ry, gt-akap).
		fmt.Printf("%s %s\n", style.Warning.Render("⚠"), summary)
	}
	for _, f := range result.Findings {
		fmt.Printf("  - %s is CLOSED but %s is still %s (branch=%s target=%s)\n",
			f.IssueID, f.MRID, f.MRStatus, f.Branch, f.Target)
	}
	for _, f := range branchResult.Findings {
		fmt.Printf("  - %s is CLOSED but branch %s was never merged and no open MR covers it (target=%s)\n",
			f.IssueID, f.Branch, targetBranch)
	}

	for _, e := range result.Errors {
		fmt.Fprintf(os.Stderr, "gt patrol state-collapse: %v\n", e)
	}
	for _, e := range branchResult.Errors {
		fmt.Fprintf(os.Stderr, "gt patrol state-collapse: %v\n", e)
	}

	return nil
}
