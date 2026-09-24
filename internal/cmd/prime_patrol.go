package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/steveyegge/gastown/internal/cli"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/deacon"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/style"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// patrolConfigForRole returns the patrol wisp config for a patrol role; ok is
// false for roles that run no patrol cycles.
//
// Prime's emitters, prime's seed step, `gt patrol report` and `gt patrol new`
// all read the wisp through this one builder, because they must agree on the
// assignee, molecule name and beads dir: a wisp seeded under an address the
// emitter does not query is a patrol the role cannot see (gt-cut was that drift,
// gt-e1ie is what it costs).
func patrolConfigForRole(ctx RoleContext) (PatrolConfig, bool) {
	switch ctx.Role {
	case RoleWitness:
		return PatrolConfig{
			RoleName:      "witness",
			PatrolMolName: constants.MolWitnessPatrol,
			BeadsDir:      ctx.TownRoot,
			Assignee:      patrolAssignee("witness", ctx.Rig),
			ExtraVars:     buildWitnessPatrolVars(ctx),
		}, true
	case RoleRefinery:
		return PatrolConfig{
			RoleName:      "refinery",
			PatrolMolName: constants.MolRefineryPatrol,
			BeadsDir:      ctx.TownRoot,
			Assignee:      patrolAssignee("refinery", ctx.Rig),
			ExtraVars:     buildRefineryPatrolVars(ctx),
		}, true
	case RoleDeacon:
		return PatrolConfig{
			RoleName:      "deacon",
			PatrolMolName: constants.MolDeaconPatrol,
			BeadsDir:      ctx.TownRoot,
			Assignee:      patrolAssignee("deacon", ""),
		}, true
	}
	return PatrolConfig{}, false
}

// patrolWorkLoopSteps returns the cycle-end instructions every patrol role
// prints; only the handoff subject names the role.
func patrolWorkLoopSteps(roleName string) []string {
	subject := cases.Title(language.English).String(roleName)
	return []string{
		"Work through each patrol step in sequence (see checklist below)",
		"At cycle end:\n   - If context LOW:\n     * Report and loop: `" + cli.Name() + " patrol report --summary \"<brief summary of observations>\"`\n     * This closes the current patrol and starts a new cycle\n   - If context HIGH:\n     * Send handoff: `" + cli.Name() + " handoff -s \"" + subject + " patrol\" -m \"<observations>\"`\n     * Exit cleanly (daemon respawns fresh session)",
	}
}

// patrolSuspend is an operator stop that keeps a role's patrol deliberately
// idle. unreadable is set when the stop state itself could not be read; that
// is not a confirmed stop, so callers must surface it as uncertain rather
// than a real operator pause, while still not seeding under uncertainty.
type patrolSuspend struct {
	reason     string
	pause      *deacon.PauseState
	unreadable error
}

// refineryActiveSafetyStopFn is a seam so tests can drive the precheck
// deterministically instead of depending on a live bd binary.
var refineryActiveSafetyStopFn = refinery.ActiveSafetyStop

// patrolSuspended reports whether this patrol role's patrol is suspended right
// now. An empty reason means the role should be running a patrol.
func patrolSuspended(ctx RoleContext) patrolSuspend {
	switch ctx.Role {
	case RoleDeacon:
		paused, state, err := deacon.IsPaused(ctx.TownRoot)
		if err != nil {
			return patrolSuspend{
				reason:     fmt.Sprintf("Deacon pause state unreadable (%v)", err),
				unreadable: err,
			}
		}
		if paused {
			return patrolSuspend{reason: "Deacon is paused", pause: state}
		}
	case RoleWitness:
		if ctx.Rig == "" {
			return patrolSuspend{reason: "No rig resolved for this witness session"}
		}
		if stopped, why := IsRigParkedOrDocked(ctx.TownRoot, ctx.Rig); stopped {
			return patrolSuspend{reason: fmt.Sprintf("Rig %s is %s", ctx.Rig, why)}
		}
	case RoleRefinery:
		// A wisp seeded without a rig carries the assignee "/refinery", which
		// nothing looks up: the rig has to be known before one is created.
		if ctx.Rig == "" {
			return patrolSuspend{reason: "No rig resolved for this refinery session"}
		}
		if stopped, why := IsRigParkedOrDocked(ctx.TownRoot, ctx.Rig); stopped {
			return patrolSuspend{reason: fmt.Sprintf("Rig %s is %s", ctx.Rig, why)}
		}
		stop, err := refineryActiveSafetyStopFn(ctx.TownRoot, ctx.Rig)
		if err != nil {
			return patrolSuspend{
				reason:     fmt.Sprintf("Refinery %s safety stop unreadable (%v)", ctx.Rig, err),
				unreadable: err,
			}
		}
		if stop != nil {
			return patrolSuspend{
				reason: fmt.Sprintf("Refinery %s is %s", ctx.Rig, stop.Reason()),
			}
		}
	}
	return patrolSuspend{}
}

// primePatrolStatus is what prime established about a patrol role's wisp:
// a live patrol, the operator stop that makes one unnecessary, or a state
// prime could not confirm this cycle.
type primePatrolStatus struct {
	Role Role
	// Config, Formula and Pause carry the display context outputPatrolContext
	// and primePatrolSection need without re-querying: Config is the same
	// PatrolConfig ensurePrimePatrol resolved, so the molecule section's
	// output*PatrolContext functions don't call patrolConfigForRole (and its
	// file-backed buildWitnessPatrolVars/buildRefineryPatrolVars) a second
	// time; PatrolLine is the bead listing line for an already-active patrol;
	// Pause is the deacon pause record for the rich paused message.
	Config     PatrolConfig
	Formula    string
	PatrolID   string
	PatrolLine string
	Seeded     bool
	Suspended  string
	Pause      *deacon.PauseState
	// Uncertain is set when discovery or seeding could not be confirmed this
	// cycle (a transient bd error, an unreadable operator-stop check, or a
	// wisp created but not confirmed hooked). PatrolID may still carry a
	// best-known candidate. This is deliberately not a fatal error: the rest
	// of prime's payload (mail, directives, hooked work) still renders, and
	// the next prime call retries (gt-e1ie).
	Uncertain string
	// PrecheckUncertain marks the one Uncertain case that must still hard-stop
	// with no checklist, matching patrolSuspended's original all-or-nothing
	// gate: the operator-stop check itself (deacon pause file, refinery safety
	// stop) came back unreadable, so whether a patrol should even run is
	// unknown — not just whether one already exists.
	PrecheckUncertain bool
}

// Seams for tests: the seed step's two data calls are stubbed so every
// missing-wisp path can be driven without a live Dolt.
var (
	findPatrolFn  = findActivePatrol
	spawnPatrolFn = autoSpawnPatrol
)

// ensurePrimePatrol returns the role's live patrol wisp, creating one when it
// is missing. It fails (non-nil error) only when patrol absence is confirmed
// and seeding one also fails outright — every other outcome, including a
// discovery blip or an unreadable operator-stop check, is folded into the
// returned status's Uncertain field so the rest of prime's payload still
// renders instead of going dark on one bad bd call.
//
// Prime is the only auto-seed path for a patrol wisp and both of its routes can
// skip the seed — the molecule section is droppable under the hook budget, and
// the compact/resume path never assembles a payload at all — so the guarantee
// lives here, outside anything the budget can drop (gt-e1ie).
func ensurePrimePatrol(ctx RoleContext) (primePatrolStatus, error) {
	cfg, ok := patrolConfigForRole(ctx)
	if !ok {
		return primePatrolStatus{}, nil
	}
	status := primePatrolStatus{Role: ctx.Role, Formula: cfg.PatrolMolName, Config: cfg}

	suspend := patrolSuspended(ctx)
	if suspend.reason != "" {
		if suspend.unreadable != nil {
			// Not a confirmed stop — fail closed (no seed) but say so distinctly
			// from a real operator pause, so it doesn't read as calm and
			// intentional when it is actually a read failure worth investigating.
			style.PrintWarning("%v", suspend.unreadable)
			status.Uncertain = suspend.reason
			status.PrecheckUncertain = true
			return status, nil
		}
		status.Suspended = suspend.reason
		status.Pause = suspend.pause
		return status, nil
	}

	patrolID, patrolLine, found, err := findPatrolFn(cfg)
	if err != nil {
		// A second read: discovery failures are usually one bad bd call, and the
		// cost of believing a bad read is a patrol that never starts.
		patrolID, patrolLine, found, err = findPatrolFn(cfg)
	}
	if err != nil {
		style.PrintWarning("could not read %s patrol state: %v", cfg.RoleName, err)
		status.Uncertain = fmt.Sprintf("could not read patrol state: %v", err)
		return status, nil
	}
	if found {
		status.PatrolID = patrolID
		status.PatrolLine = patrolLine
		return status, nil
	}

	patrolID, err = spawnPatrolFn(cfg)
	if errors.Is(err, refinery.ErrSafetyStopped) {
		// The stop landed between the check above and the spawn.
		status.Suspended = err.Error()
		return status, nil
	}
	if patrolID == "" {
		// Confirmed: no live patrol exists and seeding one produced nothing to
		// hook onto. This is the one path prime must fail loudly over.
		if err == nil {
			err = errors.New("seed returned no wisp id")
		}
		return status, fmt.Errorf("no live %s patrol and seeding one failed: %w", cfg.RoleName, err)
	}
	if err != nil {
		// A wisp was created but its hook status could not be confirmed — not
		// the same as confirmed absence, so this is uncertain, not fatal. The
		// next prime's findActivePatrol call re-checks it via the hook.
		status.PatrolID = patrolID
		status.Uncertain = fmt.Sprintf("seeded patrol %s but it is not confirmed hooked: %v", patrolID, err)
		return status, nil
	}

	status.PatrolID = patrolID
	status.Seeded = true
	return status, nil
}

// primePatrolSection renders the patrol line prime keeps regardless of budget:
// the role learns which patrol it is on and how to read the checklist, even
// when the molecule section is the one the budget drops.
func primePatrolSection(status primePatrolStatus) string {
	if status.Role == "" {
		return ""
	}
	if status.Suspended != "" {
		return fmt.Sprintf("\n**Patrol:** suspended — %s.\n", status.Suspended)
	}
	if status.Uncertain != "" {
		if status.PatrolID == "" {
			return fmt.Sprintf("\n**Patrol:** state unknown — %s. Retry `%s prime`; if it persists run `%s patrol new`.\n",
				status.Uncertain, cli.Name(), cli.Name())
		}
		return fmt.Sprintf("\n**Patrol:** state unknown — %s. Best-known id %s; verify with `%s hook` before relying on it.\n",
			status.Uncertain, status.PatrolID, cli.Name())
	}
	state := "hooked"
	if status.Seeded {
		state = "created and hooked"
	}
	return fmt.Sprintf("\n**Patrol:** %s %s. Read its checklist with `%s prime --step 1 --formula %s`.\n",
		status.PatrolID, state, cli.Name(), status.Formula)
}

// firePrimePatrolMissingEscalation is a seam for tests.
var firePrimePatrolMissingEscalation = func(actor, detail string) {
	msg := fmt.Sprintf("patrol role primed with no patrol wisp: agent=%s detail=%s — gt-e1ie", actor, detail)
	cmd := exec.Command("gt", "escalate", "--severity", "high", "--reason", "patrol-wisp-missing", msg)
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "gt prime: escalation failed: %v\n", err)
	}
}

// reportPrimeMissingPatrol makes a patrol role's missing wisp fatal and visible.
//
// Fatal, not advisory: every later instruction to this agent assumes a hooked
// patrol, so a prime that returned success here would hand back an agent that
// looks idle by design. The non-zero exit makes the daemon respawn the role and
// the escalation puts the state in front of the Mayor.
func reportPrimeMissingPatrol(ctx RoleContext, err error) error {
	actor := getAgentIdentity(ctx)
	fmt.Fprintf(os.Stderr, "\n%s\n", style.Bold.Render("## ⚠️  NO PATROL WISP — PRIME FAILED ⚠️"))
	fmt.Fprintf(os.Stderr, "%s primed without a live patrol: %v\n", actor, err)
	fmt.Fprintf(os.Stderr, "This role has nothing to run until the patrol exists.\n")
	fmt.Fprintf(os.Stderr, "Retry `%s prime`; if it fails again run `%s patrol new`, then escalate to the Mayor.\n\n", cli.Name(), cli.Name())
	firePrimePatrolMissingEscalation(actor, err.Error())
	return fmt.Errorf("prime: %s has no patrol wisp: %w", actor, err)
}
