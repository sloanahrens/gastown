package cmd

import (
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/done"
)

// gt done is a thin cobra shell: it binds the flags into done.Options, supplies
// the command layer's seams (role detection, the agent-bead state write), and
// calls the leaf package (D10, gt-638go.11). The submission logic itself lives
// in internal/done so the phase-3 landing path can call it in process.

var (
	doneIssue         string
	doneStatus        string
	doneCleanupStatus string
	doneTarget        string
	doneAllowReverts  bool

	doneAllowThrowawayPaths bool

	// doneBead and donePreVerified are crew submission flags (gt-3e7tk).
	doneBead        string
	donePreVerified bool
)

var doneCmd = &cobra.Command{
	Use:     "done",
	GroupID: GroupWork,
	Short:   "Submit your branch for landing and end the polecat session",
	Long: `Submit your finished branch for landing, record completion on the agent
bead, and end the polecat session. gt done never lands anything on the target branch: the
daemon's landing worker does that (ADR 0004).

For COMPLETED, gt done:
1. Fetches origin and rebases the branch onto the target (default: the rig's
   default branch; --target or the bead's base_branch override it)
2. Squashes auto-save and checkpoint commits into one descriptive commit,
   and strips Co-Authored-By trailers and AI attribution lines from every
   commit message (a subject line that is itself one is refused)
3. Runs the local gate on the rebased tree: make presubmit (lint, go build
   ./..., and go test of the packages the branch changed), or the rig's
   presubmit_command. The landing worker runs the full make gate on the
   merged tree. A rig without go.mod runs its lint_command, build_command
   and test_command
4. Pushes the branch under a lease and reads the tip back
5. Marks the work bead ready to land (label gt:ready-to-land and a
   READY TO LAND notes block naming branch, head and target)
6. Records completion on the agent bead and retires the session

A polecat cannot skip the gate, and gt done never lands directly.

Crew (GT_ROLE or BD_ACTOR <rig>/crew/<name>, or run from a <rig>/crew/<name>
worktree with no polecat identity) submit a
branch they already pushed: gt done checks origin/<branch> is at HEAD, runs
make presubmit (skipped with --pre-verified), then comments "Submitted for
landing: <branch> @ <sha> onto <target>", appends the READY TO LAND block and
adds gt:ready-to-land. The bead comes from --bead, else from the branch name.
BD_ACTOR falls back to git user.name. Crew sessions are not retired.

Exit statuses:
  COMPLETED      - Work done, branch submitted for landing (default)
  ESCALATED      - Hit blocker, needs human intervention
  DEFERRED       - Work paused, issue still open

Process exit codes (the work was not submitted; no completion is recorded
and the session stays up so you can fix it and re-run gt done):
  10  push failed: origin does not have the commit
  11  push unverified: origin is not at the commit gt done would declare
  12  the work bead could not be marked ready to land
  13  the no-code completion could not close the bead
  14  rebase onto the target conflicted
  15  the local gate failed
  16  the local gate could not run (not a verdict on the change)

Examples:
  gt done                              # Submit branch, notify COMPLETED, exit session
  gt done --target feat/my-branch      # Explicit target branch
  gt done --issue gt-abc               # Explicit issue ID
  gt done --status ESCALATED           # Signal blocker, submit nothing
  gt done --status DEFERRED            # Pause work, submit nothing
  gt done --bead gt-abc                # Crew: submit the pushed branch for gt-abc`,
	RunE:         runDone,
	SilenceUsage: true, // Don't print usage on operational errors (confuses agents)
}

// Valid exit types for gt done, re-exported for the command tree.
const (
	ExitCompleted = done.ExitCompleted
	ExitEscalated = done.ExitEscalated
	ExitDeferred  = done.ExitDeferred
)

func init() {
	doneCmd.Flags().StringVar(&doneIssue, "issue", "", "Source issue ID (default: parse from branch name)")
	doneCmd.Flags().StringVar(&doneStatus, "status", ExitCompleted, "Exit status: COMPLETED, ESCALATED, or DEFERRED")
	doneCmd.Flags().StringVar(&doneCleanupStatus, "cleanup-status", "", "Git cleanup status: clean, uncommitted, unpushed, stash, unknown (ZFC: agent-observed)")
	doneCmd.Flags().StringVar(&doneTarget, "target", "", "Explicit target branch (overrides the bead's base_branch and the rig default)")
	doneCmd.Flags().BoolVar(&doneAllowReverts, "allow-reverts", false, "Submit a branch that undoes content already merged to the target (refused by default)")
	doneCmd.Flags().BoolVar(&doneAllowThrowawayPaths, "allow-throwaway-paths", false, "Submit a branch that adds scratch, backup or /tmp files to the target (refused by default)")
	doneCmd.Flags().StringVar(&doneBead, "bead", "", "Crew: the work bead to submit (default: parse from the branch name)")
	doneCmd.Flags().BoolVar(&donePreVerified, "pre-verified", false, "Crew only: skip the local presubmit gate (the landing worker still gates the merged tree)")

	rootCmd.AddCommand(doneCmd)
}

func runDone(cmd *cobra.Command, args []string) error {
	return done.Run(done.Options{
		Status:              doneStatus,
		Issue:               doneIssue,
		CleanupStatus:       doneCleanupStatus,
		Target:              doneTarget,
		Bead:                doneBead,
		AllowReverts:        doneAllowReverts,
		AllowThrowawayPaths: doneAllowThrowawayPaths,
		PreVerified:         donePreVerified,
		Env:                 os.Getenv,
		Getwd:               os.Getwd,
		Detect:              detectDoneAgent,
		RecordAgentState:    updateAgentStateOnDone,
	})
}

// detectDoneAgent is internal/cmd's role detection behind done.Detect. The
// leaf package cannot import cmd (D10), so the CLI layer hands its detection
// down as a function.
func detectDoneAgent(cwd, townRoot string, getenv func(string) string) (done.Agent, string, bool) {
	info, err := getRoleWithContextEnv(cwd, townRoot, getenv)
	if err != nil {
		return done.Agent{}, "", false
	}
	agent := done.Agent{
		Role:     info.Role,
		Rig:      info.Rig,
		Polecat:  info.Polecat,
		TownRoot: townRoot,
		WorkDir:  cwd,
	}
	if info.Role == RoleUnknown {
		// The rig and polecat are still worth refining the seeded identity
		// with; only the log actor is withheld (see resolveDoneAgentIdentity).
		return agent, "", false
	}
	return agent, info.ActorString(), true
}
