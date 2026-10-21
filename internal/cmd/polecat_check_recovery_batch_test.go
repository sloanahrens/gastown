package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
)

// TestRecoveryBatchIdle pins the one condition that lets the batch sweep
// answer a polecat from its preloaded agent bead instead of paying for the
// per-directory bd and git probes (gt-uoez5). Each false row is a seat the
// shortcut must hand to the full check.
func TestRecoveryBatchIdle(t *testing.T) {
	t.Parallel()

	fields := func(state beads.AgentState, hook string) *beads.AgentFields {
		return &beads.AgentFields{AgentState: string(state), HookBead: hook, CleanupStatus: string(polecat.CleanupClean)}
	}
	with := func(f *beads.AgentFields, mutate func(*beads.AgentFields)) *beads.AgentFields {
		mutate(f)
		return f
	}

	tests := []struct {
		name       string
		fields     *beads.AgentFields
		hasSession bool
		want       bool
	}{
		{
			name:   "done, no hook, no session",
			fields: fields(beads.AgentStateDone, ""),
			want:   true,
		},
		{
			name:   "idle, no hook, no session",
			fields: fields(beads.AgentStateIdle, ""),
			want:   true,
		},
		{
			name:       "idle with a live session",
			fields:     fields(beads.AgentStateIdle, ""),
			hasSession: true,
			want:       false,
		},
		{
			name:   "done but still hooked",
			fields: fields(beads.AgentStateDone, "gt-abc"),
			want:   false,
		},
		{
			name:   "working with no session",
			fields: fields(beads.AgentStateWorking, ""),
			want:   false,
		},
		{
			name:   "no preloaded agent bead",
			fields: nil,
			want:   false,
		},
		{
			name:   "done with uncommitted work recorded",
			fields: with(fields(beads.AgentStateDone, ""), func(f *beads.AgentFields) { f.CleanupStatus = string(polecat.CleanupUncommitted) }),
			want:   false,
		},
		{
			name:   "idle with unpushed work recorded",
			fields: with(fields(beads.AgentStateIdle, ""), func(f *beads.AgentFields) { f.CleanupStatus = string(polecat.CleanupUnpushed) }),
			want:   false,
		},
		{
			name:   "done with no cleanup status recorded",
			fields: with(fields(beads.AgentStateDone, ""), func(f *beads.AgentFields) { f.CleanupStatus = "" }),
			want:   false,
		},
		{
			name:   "done with an active merge request",
			fields: with(fields(beads.AgentStateDone, ""), func(f *beads.AgentFields) { f.ActiveMR = "gt-mr1" }),
			want:   false,
		},
		{
			name:   "done after a failed push",
			fields: with(fields(beads.AgentStateDone, ""), func(f *beads.AgentFields) { f.PushFailed = true }),
			want:   false,
		},
		{
			name:   "done after a failed merge request",
			fields: with(fields(beads.AgentStateDone, ""), func(f *beads.AgentFields) { f.MRFailed = true }),
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := recoveryBatchIdle(tt.fields, tt.hasSession); got != tt.want {
				t.Fatalf("recoveryBatchIdle(%+v, hasSession=%v) = %v, want %v", tt.fields, tt.hasSession, got, tt.want)
			}
		})
	}
}

// TestIdleRecoveryStatus holds the shortcut's report to the one the full check
// gives an idle, clean polecat: safe to nuke, reusable, and counted against no
// dispatch capacity, with the idle reason naming the agent bead as the source
// (gt-uoez5).
func TestIdleRecoveryStatus(t *testing.T) {
	t.Parallel()

	p := &polecat.Polecat{Name: "toast", Branch: "polecat/toast/gt-abc"}
	status := idleRecoveryStatus("gastown", "toast", p, &beads.AgentFields{
		AgentState:    string(beads.AgentStateDone),
		CleanupStatus: string(polecat.CleanupClean),
	})

	if status.Rig != "gastown" || status.Polecat != "toast" {
		t.Errorf("status names %s/%s, want gastown/toast", status.Rig, status.Polecat)
	}
	if status.Verdict != polecat.WorkstateVerdictSafeToNuke {
		t.Errorf("verdict = %q, want %q", status.Verdict, polecat.WorkstateVerdictSafeToNuke)
	}
	if status.Reason != "idle-from-agent-bead" {
		t.Errorf("reason = %q, want idle-from-agent-bead", status.Reason)
	}
	if !status.Reusable {
		t.Error("reusable = false, want true")
	}
	if !status.SafeToNuke {
		t.Error("safe_to_nuke = false, want true")
	}
	if status.NeedsRecovery {
		t.Error("needs_recovery = true, want false")
	}
	if status.CountsTowardCapacity {
		t.Error("counts_toward_capacity = true, want false")
	}
	if status.ReuseStatus != "idle-clean" {
		t.Errorf("reuse_status = %q, want idle-clean", status.ReuseStatus)
	}
	if status.MQStatus != "not_required" {
		t.Errorf("mq_status = %q, want not_required", status.MQStatus)
	}
	if status.Branch != p.Branch {
		t.Errorf("branch = %q, want %q from the polecat", status.Branch, p.Branch)
	}

	// A polecat whose branch could not be read from its worktree still names
	// the branch its agent bead recorded.
	noBranch := idleRecoveryStatus("gastown", "toast", &polecat.Polecat{Name: "toast"}, &beads.AgentFields{
		AgentState:    string(beads.AgentStateIdle),
		CleanupStatus: string(polecat.CleanupClean),
		Branch:        "polecat/toast/gt-abc",
	})
	if noBranch.Branch != "polecat/toast/gt-abc" {
		t.Errorf("branch = %q, want the agent bead's branch", noBranch.Branch)
	}
}

// TestRecoveryBatchShortcut drives the sweep's per-polecat decision end to end
// over a preloaded agent bead and a session set. Each false row is a seat the
// batch hands to checkRecoveryForPolecat.
func TestRecoveryBatchShortcut(t *testing.T) {
	t.Parallel()

	const beadID = "gt-gastown-polecat-toast"
	agentBead := func(f beads.AgentFields) map[string]*beads.Issue {
		f.RoleType = "polecat"
		f.Rig = "gastown"
		return map[string]*beads.Issue{beadID: {ID: beadID, Description: beads.FormatAgentDescription("toast", &f)}}
	}
	cleanDone := beads.AgentFields{AgentState: string(beads.AgentStateDone), CleanupStatus: string(polecat.CleanupClean)}
	live := polecatSessionSet{polecatSessionKey("gastown", "toast"): polecatSessionEntry{Name: "gt-toast"}}

	tests := []struct {
		name     string
		sessions polecatSessionSet
		preload  map[string]*beads.Issue
		issue    string
		want     bool
	}{
		{name: "done, clean, no session", preload: agentBead(cleanDone), want: true},
		{name: "another seat's session does not count", preload: agentBead(cleanDone), sessions: polecatSessionSet{polecatSessionKey("gastown", "nux"): polecatSessionEntry{Name: "gt-nux"}}, want: true},
		{name: "live session", preload: agentBead(cleanDone), sessions: live, want: false},
		{name: "assigned work bead", preload: agentBead(cleanDone), issue: "gt-abc", want: false},
		{name: "agent bead not preloaded", preload: nil, want: false},
		{
			name:    "uncommitted work recorded",
			preload: agentBead(beads.AgentFields{AgentState: string(beads.AgentStateDone), CleanupStatus: string(polecat.CleanupUncommitted)}),
			want:    false,
		},
		{
			name:    "no cleanup status recorded",
			preload: agentBead(beads.AgentFields{AgentState: string(beads.AgentStateIdle)}),
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := &polecat.Polecat{Name: "toast", Branch: "polecat/toast/gt-abc", Issue: tt.issue}
			status, ok := recoveryBatchShortcut(tt.sessions, tt.preload, beadID, "gastown", "toast", p)
			if ok != tt.want {
				t.Fatalf("recoveryBatchShortcut ok = %v, want %v (status %+v)", ok, tt.want, status)
			}
			if ok && (status.Reason != "idle-from-agent-bead" || !status.SafeToNuke || status.Polecat != "toast") {
				t.Errorf("shortcut status = %+v, want an idle-from-agent-bead safe-to-nuke report for toast", status)
			}
		})
	}
}
