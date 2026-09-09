package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/mail"
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
	Short: "Find issues closed while their merge request is still open",
	Long: `Scan the open merge-request queue for the gt-zzd state-collapse
signature: a source issue recorded as done (bead closed) while the
merge-request bead created to land its fix is still open.

Closing a source issue normally happens alongside closing its MR during a
merge. A closed issue paired with a still-open MR means the fix may not
actually be in force — the earlier instances of this pattern (gt-zzd) were
each caught by an agent reading source by hand, never by a mechanism. This
command is that mechanism, run at patrol time.

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
	Rig      string                         `json:"rig"`
	Checked  int                            `json:"checked"`
	Findings []witness.StateCollapseFinding `json:"findings,omitempty"`
	Errors   []string                       `json:"errors,omitempty"`
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

	if patrolStateCollapseJSON {
		errs := make([]string, len(result.Errors))
		for i, e := range result.Errors {
			errs[i] = e.Error()
		}
		out := PatrolStateCollapseOutput{
			Rig:      rigName,
			Checked:  result.Checked,
			Findings: result.Findings,
			Errors:   errs,
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if len(result.Findings) == 0 {
		fmt.Printf("%s No state collapse found (%d open MR(s) checked in %s)\n", style.Success.Render("✓"), result.Checked, rigName)
	} else {
		fmt.Printf("%s Found %d state collapse(s) in %s (%d open MR(s) checked):\n",
			style.Warning.Render("⚠"), len(result.Findings), rigName, result.Checked)
		for _, f := range result.Findings {
			fmt.Printf("  - %s is CLOSED but %s is still %s (branch=%s target=%s)\n",
				f.IssueID, f.MRID, f.MRStatus, f.Branch, f.Target)
		}
	}

	for _, e := range result.Errors {
		fmt.Fprintf(os.Stderr, "gt patrol state-collapse: %v\n", e)
	}

	return nil
}
