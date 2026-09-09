package beads

import "time"

// ReconcileRow is one differing agent field in a reconcile table.
type ReconcileRow struct {
	Field  string
	Rig    string // value on the canonical rig row
	Town   string // value on the legacy town row
	Winner string // "rig", "town", "clear", or a severity-qualified variant
}

// parseIssueTime parses an Issue.UpdatedAt string, trying RFC3339Nano then
// RFC3339. An unparseable or empty value returns the zero time, which loses
// every recency comparison — the safer default when we can't tell.
func parseIssueTime(s string) time.Time {
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
// blocking (has_uncommitted, has_stash, has_unpushed, anything unrecognised)
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
func MergeLegacyAgentBead(rig, town *Issue, beadExists func(id string) bool) (AgentFieldUpdates, []ReconcileRow) {
	var updates AgentFieldUpdates
	var rows []ReconcileRow
	r, t := ParseAgentFields(rig.Description), ParseAgentFields(town.Description)
	if r == nil || t == nil {
		return updates, rows
	}
	townNewer := parseIssueTime(town.UpdatedAt).After(parseIssueTime(rig.UpdatedAt))

	pick := func(field, rigVal, townVal string, dst **string, isRef bool) {
		if rigVal == townVal {
			return
		}
		winner, val := "rig", rigVal
		if townNewer {
			winner, val = "town", townVal
		}
		if isRef && val != "" && !beadExists(val) {
			winner, val = "clear", ""
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
	return updates, rows
}
