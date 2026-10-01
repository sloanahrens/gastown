package done

import (
	"os"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/role"
)

// Agent is the identity a gt done run acts as: which role is submitting, and
// the rig and worker name it belongs to.
//
// internal/cmd owns role detection (it is the CLI layer's job, and RoleInfo
// carries command-layer state this package must not see). A caller that
// cannot run detection — an in-process landing path, or this package's own
// tests — either supplies a Detect function or leaves the zero Agent, and the
// run falls back to the assigned polecat worktree it resolved.
type Agent struct {
	Role     role.Role
	Rig      string
	Polecat  string
	TownRoot string
	WorkDir  string
}

// BeadID is the agent bead carrying this identity's state. "" when the
// identity names no bead (an unknown role, or a named role with no rig or
// worker); every write guarded by `agentBeadID != ""` then skips.
func (a Agent) BeadID() string {
	return beads.AgentBeadIDFor(string(a.Role), a.Rig, a.Polecat, a.TownRoot)
}

// Actor is the beads actor string for the identity, the form bead
// attribution and mail addresses use.
func (a Agent) Actor() string {
	return role.Actor(a.Role, a.Rig, a.Polecat)
}

// Detect resolves the caller's role identity from a working directory and the
// town root, reading GT_ROLE and friends through getenv. It returns the
// identity it found, the beads actor string for it, and whether a role was
// named at all — false when detection failed or the role is Unknown, which is
// exactly when the actor must not replace an already-validated sender.
//
// internal/cmd supplies its role detection here; the package cannot import it
// (D10).
type Detect func(cwd, townRoot string, getenv func(string) string) (Agent, string, bool)

// Options is one gt done invocation: the flags the CLI bound, plus the seams
// this package reads the process through. A caller that runs gt done in
// process — the phase-3 landing path — builds it directly instead of through
// cobra.
type Options struct {
	// Status is COMPLETED, ESCALATED or DEFERRED, case-insensitive.
	Status string
	// Issue is the source issue id (--issue). Empty derives it from the
	// branch name, then from the agent's hooked bead.
	Issue string
	// CleanupStatus is the agent-observed git cleanup state
	// (--cleanup-status). Empty observes the worktree.
	CleanupStatus string
	// Target overrides the bead's base_branch and the rig's default branch.
	Target string
	// Bead is the crew work bead to submit (--bead). Crew only.
	Bead string
	// AllowReverts submits a branch that undoes content already merged to the
	// target (refused by default).
	AllowReverts bool
	// AllowThrowawayPaths submits a branch that adds scratch, backup or /tmp
	// files to the target (refused by default).
	AllowThrowawayPaths bool
	// PreVerified skips the crew local presubmit gate. Crew only: a polecat's
	// gt done always runs the gate.
	PreVerified bool

	// Env reads the process environment. Nil is os.Getenv.
	Env func(string) string
	// Getwd returns the process working directory. Nil is os.Getwd.
	Getwd func() (string, error)
	// Detect resolves the caller's role identity. Nil leaves the identity the
	// run resolved from the assigned polecat worktree.
	Detect Detect
	// RecordAgentState writes the run's terminal state onto the caller's agent
	// bead and closes its hooked work bead. internal/cmd owns those writes
	// (role detection, the close invariants, the wisp purge); nil skips them,
	// which is what a caller with no agent bead wants.
	RecordAgentState func(cwd, townRoot, exitType, issueID string) error
}

func (o Options) env() func(string) string {
	if o.Env == nil {
		return os.Getenv
	}
	return o.Env
}

func (o Options) getwd() func() (string, error) {
	if o.Getwd == nil {
		return os.Getwd
	}
	return o.Getwd
}
