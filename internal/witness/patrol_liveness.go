package witness

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/guard"
)

// PatrolAssignee returns the canonical assignee address for a patrol role:
// town-level roles (deacon) use a trailing slash, rig-scoped roles (witness,
// refinery) do not. This mirrors resolveSelfTarget / canonicalAssigneeAddress
// (internal/cmd/sling_target.go), the address form `gt hook` queries — a
// mismatch here previously made deacon patrol wisps invisible to `gt hook`
// (gt-cut). internal/cmd/patrol_helpers.go's patrolAssignee delegates here so
// there is exactly one implementation.
func PatrolAssignee(role, rig string) string {
	switch role {
	case "deacon":
		return "deacon/"
	case "witness":
		return rig + "/witness"
	case "refinery":
		return rig + "/refinery"
	default:
		return role
	}
}

// PatrolLivenessInput bundles what EvaluatePatrolLiveness needs to judge
// whether one patrol role instance (a specific rig's witness or refinery, or
// the town's deacon) is patrolling.
type PatrolLivenessInput struct {
	// Role is a display label ("witness", "deacon", "refinery").
	Role string
	// Rig is the rig name, or "" for a town-level role.
	Rig string

	// SessionAlive reports whether the role's tmux session and agent process
	// are alive right now. A dead session is zombie/stall detection's job
	// (internal/witness's DetectZombiePolecats/DetectStalledPolecats family),
	// not this detector's — there is nothing to escalate about a role that
	// isn't running at all.
	SessionAlive bool

	// LastCompleted is the most recent completion time of a patrol cycle,
	// meaningful only when LastCompletedResult.IsPass().
	LastCompleted time.Time
	// LastCompletedResult is Pass when LastCompleted was read successfully,
	// Fail when the source confirmed the role has never completed a patrol
	// cycle, and Unknown when the read itself could not be trusted (e.g. bd
	// failed, or the response could not be parsed). See LastCompletedPatrol.
	LastCompletedResult guard.Result

	// Cadence is the expected time between completed patrol cycles.
	Cadence time.Duration
	// Multiplier is how many cadences of silence are tolerated before the
	// role is considered stale. N in "older than N x its cadence".
	Multiplier int

	// Now is the reference instant, passed explicitly so tests and callers
	// agree on it.
	Now time.Time
}

// EvaluatePatrolLiveness flags a patrol role that is awake (its session is
// alive) but NOT patrolling: its last COMPLETED patrol cycle is older than
// Multiplier x Cadence.
//
// This single check covers both mechanisms observed on 2026-09-23 (gt-4z3b7):
// an interrupt read as a user stop (the witness stopped calling `gt patrol
// report` entirely, gt-cyyg) and callback starvation (the deacon's patrol
// molecule stayed hooked/in_progress and never reached `gt patrol report`
// because inbox processing consumed every turn). Both produce the same
// observable symptom — the last CLOSED patrol wisp stops advancing while the
// session keeps answering nudges — so one detector catches both without
// needing to know which mechanism is at fault.
//
// A read failure is Unknown, never Pass (gt-udrrw): a caller that cannot
// determine whether a role is patrolling must not report it healthy.
func EvaluatePatrolLiveness(in PatrolLivenessInput) guard.Result {
	if !in.SessionAlive {
		return guard.Pass()
	}

	if in.LastCompletedResult.IsUnknown() {
		return guard.Unknown(fmt.Errorf("%s: could not determine last completed patrol: %w",
			patrolLivenessLabel(in.Role, in.Rig), in.LastCompletedResult.Err()))
	}

	if in.Cadence <= 0 || in.Multiplier <= 0 {
		return guard.Unknown(fmt.Errorf("%s: patrol cadence/multiplier not configured",
			patrolLivenessLabel(in.Role, in.Rig)))
	}
	threshold := in.Cadence * time.Duration(in.Multiplier)

	if in.LastCompletedResult.IsFail() {
		// Confirmed: no completed patrol cycle exists on record at all, while
		// the session is alive. Maximally stale.
		return guard.Fail(fmt.Sprintf("%s: session is alive but has never completed a patrol cycle (threshold %s)",
			patrolLivenessLabel(in.Role, in.Rig), threshold))
	}

	age := in.Now.Sub(in.LastCompleted)
	if age < 0 {
		// A future-dated completion is not evidence of staleness, but it is
		// not confident evidence of health either — do not invent a Pass.
		return guard.Unknown(fmt.Errorf("%s: last completed patrol is timestamped in the future (%s)",
			patrolLivenessLabel(in.Role, in.Rig), in.LastCompleted.Format(time.RFC3339)))
	}
	if age > threshold {
		return guard.Fail(fmt.Sprintf("%s: last completed patrol %s ago exceeds %dx cadence (threshold %s)",
			patrolLivenessLabel(in.Role, in.Rig), age.Round(time.Second), in.Multiplier, threshold))
	}
	return guard.Pass()
}

func patrolLivenessLabel(role, rig string) string {
	if rig == "" {
		return role
	}
	return rig + "/" + role
}

// LastCompletedPatrol returns the most recent completion time of a patrol
// role's cycle: the ClosedAt (falling back to UpdatedAt when ClosedAt is
// empty) of the most recently closed bead whose title is prefixed with the
// role's patrol molecule name (e.g. "mol-witness-patrol") and assigned to the
// role's canonical address.
//
// This reads bd-owned bookkeeping — the same wisp `gt patrol report` closes
// at the end of every cycle — rather than the agent-hand-maintained
// state.json last_patrol field (internal/witness/patrol_state.go), which can
// run away or simply never get written if the session stops mid-cycle
// (gt-oabl). A session that stops calling `gt patrol report` shows up here
// even though it never got the chance to update its own state file.
//
// Patrol wisps are ephemeral (bd mol wisp create, internal/cmd/patrol_helpers.go)
// and live in the wisps table, not the persistent issues table — `bd list`
// only searches issues and has no --ephemeral flag (internal/beads/beads.go's
// own listEphemeral doc comment). Querying with `bd list` here would silently
// find nothing for every role, reading as "never patrolled" and alarming on
// every healthy one. `bd query` with an explicit ephemeral=true clause is the
// documented way to reach the wisps table (same pattern as
// dogHasHookedFormulaWithID, internal/daemon/handler.go).
//
// The returned guard.Result is Pass when a closed patrol wisp was found,
// Fail when bd resolved the query and confirmed none exists (a role that has
// genuinely never completed one), and Unknown when the read itself could not
// be trusted — never Pass over a failure to read (gt-udrrw).
func LastCompletedPatrol(bd *BdCli, workDir, assignee, patrolMolName string) (time.Time, guard.Result) {
	if bd == nil {
		return time.Time{}, guard.Unknown(fmt.Errorf("no bd client configured"))
	}
	if strings.TrimSpace(assignee) == "" || strings.TrimSpace(patrolMolName) == "" {
		return time.Time{}, guard.Unknown(fmt.Errorf("assignee and patrol molecule name are required"))
	}

	queryExpr := fmt.Sprintf("ephemeral=true AND status=%s AND assignee=%s",
		strconv.Quote("closed"), strconv.Quote(assignee))
	output, err := bd.Exec(workDir, "query", "--json", queryExpr, "--limit=0")
	if err != nil {
		return time.Time{}, guard.Unknown(fmt.Errorf("bd query %q: %w", queryExpr, err))
	}
	if strings.TrimSpace(output) == "" {
		// bd exited zero with no output — an empty result set is normally an
		// empty JSON array, not empty stdout, so this shape is unexplained
		// rather than a resolved "no closed patrols" negative.
		return time.Time{}, guard.Unknown(fmt.Errorf("bd query %q: no output", queryExpr))
	}

	var issues []beads.Issue
	if err := json.Unmarshal([]byte(output), &issues); err != nil {
		return time.Time{}, guard.Unknown(fmt.Errorf("parsing bd query %q output: %w", queryExpr, err))
	}

	var latest time.Time
	found := false
	for _, iss := range issues {
		if !strings.HasPrefix(iss.Title, patrolMolName) {
			continue
		}
		ts := iss.ClosedAt
		if ts == "" {
			ts = iss.UpdatedAt
		}
		if ts == "" {
			continue
		}
		t, parseErr := time.Parse(time.RFC3339, ts)
		if parseErr != nil {
			continue // unparsable timestamp on this one record; keep scanning others
		}
		if !found || t.After(latest) {
			latest = t
			found = true
		}
	}

	if !found {
		// Name the directory the query ran in: a query aimed at the wrong
		// database returns the same bare [] as a genuine "never patrolled",
		// and this message is the only place the distinction can show up
		// (hq-3h7ac: every rig-scoped role was read from its rig database
		// instead of the town database that holds the wisps).
		return time.Time{}, guard.Fail(fmt.Sprintf("no closed %s wisp found for %s in %s",
			patrolMolName, assignee, workDir))
	}
	return latest, guard.Pass()
}
