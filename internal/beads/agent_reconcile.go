package beads

import (
	"errors"
	"fmt"
	"time"
)

// ReconcileUnknownWinner is the Winner of a field whose bead reference could
// not be checked because the store did not answer.
const ReconcileUnknownWinner = "unknown (bd could not answer)"

// ReconcileRow is one differing agent field in a reconcile table.
type ReconcileRow struct {
	Field  string
	Rig    string // value on the canonical rig row
	Town   string // value on the legacy town row
	Winner string // "rig", "town", "clear", or a severity-qualified variant
}

// ParseIssueTime parses a timestamp string as issued by bd/Dolt, trying
// RFC3339Nano then RFC3339. An unparseable or empty value returns the zero
// time, which loses every recency comparison — the safer default when we
// can't tell.
//
// RFC3339Nano is tried first because bd-issued timestamps can carry
// fractional seconds, which plain RFC3339 rejects. Callers use this to gate
// recovery paths, so a format mismatch that returned an error (instead of
// the zero time) would silently disable them.
func ParseIssueTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// cleanupSeverity orders cleanup_status values for reconciliation:
// blocking (has_uncommitted, has_stash, has_unpushed, anything unrecognized)
// > unknown (empty or null, which fails closed) > clean. Only 'clean' clears.
func cleanupSeverity(v string) int {
	switch v {
	case "clean":
		return 0
	case "", "null":
		return 1
	default:
		return 2
	}
}

// MergeLegacyAgentBead computes the field updates that bring the canonical
// rig row up to date with a legacy town copy before the copy is deleted.
//
// Rule (gt-a6g): per differing field the row with the newer UpdatedAt wins,
// except:
//   - active_mr and hook_bead values whose bead no longer exists are
//     cleared, never copied.
//   - cleanup_status reconciles by SEVERITY, never by recency (refinery
//     review hq-wisp-j0g): it is the field behind four fail-open P0s. A
//     blocking value beats unknown beats clean regardless of updated_at, so
//     a stale 'clean' can never manufacture a clearance.
//
// Identical rows produce no updates.
//
// beadExists answers (false, nil) only when the store says the bead is absent.
// Any other failure is (false, err): the answer is unknown, and an unknown
// answer is never evidence of absence. A reference whose existence is unknown
// gets the Winner ReconcileUnknownWinner, and the merge then returns no
// updates at all and an error wrapping ErrUnavailable, so a caller cannot
// write a half-decided merge. The rows are still returned for display.
func MergeLegacyAgentBead(rig, town *Issue, beadExists func(id string) (bool, error)) (AgentFieldUpdates, []ReconcileRow, error) {
	var updates AgentFieldUpdates
	var rows []ReconcileRow
	var unknown []error
	r, t := ParseAgentFields(rig.Description), ParseAgentFields(town.Description)
	if r == nil || t == nil {
		return updates, rows, nil
	}
	townNewer := ParseIssueTime(town.UpdatedAt).After(ParseIssueTime(rig.UpdatedAt))

	pick := func(field, rigVal, townVal string, dst **string, isRef bool) {
		if rigVal == townVal {
			return
		}
		winner, val := "rig", rigVal
		if townNewer {
			winner, val = "town", townVal
		}
		if isRef && val != "" {
			exists, err := beadExists(val)
			switch {
			case err != nil:
				unknown = append(unknown, fmt.Errorf("%s %s: %w", field, val, err))
				rows = append(rows, ReconcileRow{Field: field, Rig: rigVal, Town: townVal, Winner: ReconcileUnknownWinner})
				return
			case !exists:
				winner, val = "clear", ""
			}
		}
		if winner != "rig" || isRef {
			v := val
			*dst = &v
		}
		rows = append(rows, ReconcileRow{Field: field, Rig: rigVal, Town: townVal, Winner: winner})
	}

	pick("agent_state", r.AgentState, t.AgentState, &updates.AgentState, false)
	pick("hook_bead", r.HookBead, t.HookBead, &updates.HookBead, true)
	// cleanup_status reconciles by SEVERITY, never by recency (refinery
	// review hq-wisp-j0g): it is the field behind four fail-open P0s. A
	// blocking value beats unknown beats clean regardless of updated_at, so
	// a stale 'clean' can never manufacture a clearance.
	if r.CleanupStatus != t.CleanupStatus {
		winner := "rig"
		if cleanupSeverity(t.CleanupStatus) > cleanupSeverity(r.CleanupStatus) {
			winner = "town"
			v := t.CleanupStatus
			updates.CleanupStatus = &v
		}
		rows = append(rows, ReconcileRow{Field: "cleanup_status", Rig: r.CleanupStatus, Town: t.CleanupStatus, Winner: winner + " (severity)"})
	}
	pick("active_mr", r.ActiveMR, t.ActiveMR, &updates.ActiveMR, true)
	pick("exit_type", r.ExitType, t.ExitType, &updates.ExitType, false)
	pick("mr_id", r.MRID, t.MRID, &updates.MRID, false)
	pick("branch", r.Branch, t.Branch, &updates.Branch, false)
	pick("last_source_issue", r.LastSourceIssue, t.LastSourceIssue, &updates.LastSourceIssue, false)
	pick("completion_time", r.CompletionTime, t.CompletionTime, &updates.CompletionTime, false)
	if len(unknown) > 0 {
		err := errors.Join(unknown...)
		if !errors.Is(err, ErrUnavailable) {
			err = fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		return AgentFieldUpdates{}, rows, fmt.Errorf("cannot tell whether a referenced bead exists: %w", err)
	}
	return updates, rows, nil
}
