package cmd

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
)

// mrlessBeads is a beads fake with no merge requests.
type mrlessBeads struct{ *beadsfake.Fake }

func (mrlessBeads) FindMRForBranchAny(string) (*beads.Issue, error) { return nil, nil }

// A sandbox seat's host worktree is a stale leftover, so check-recovery takes
// its git facts from the sandbox probe and never measures the host clone: a
// gone sandbox with nothing hooked reads clean, a running one or a gateway that
// cannot be asked reads unknown and keeps the seat out of reuse and nuke. A host
// seat is measured as before, here against a clone that does not exist (gt-qq6rg.2).
func TestCheckRecoveryOfASandboxSeat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		agent       string
		probe       func(agent, session string) (polecat.LiveGitState, bool)
		wantLive    bool // the git source is "live": the seat was judged, not failed
		wantUnknown bool
	}{
		{"sandbox_gone_reads_clean", sandboxSeatAgent,
			func(string, string) (polecat.LiveGitState, bool) {
				return polecat.SandboxSeatGitState("", false, nil, "gt-ember"), true
			}, true, false},
		{"sandbox_running_is_unknown", sandboxSeatAgent,
			func(string, string) (polecat.LiveGitState, bool) {
				return polecat.SandboxSeatGitState("", true, nil, "gt-ember"), true
			}, false, true},
		{"gateway_error_is_unknown", sandboxSeatAgent,
			func(string, string) (polecat.LiveGitState, bool) {
				return polecat.SandboxSeatGitState("", false, errors.New("connection refused"), "gt-ember"), true
			}, false, true},
		{"host_seat_is_measured", "deepseek-flash",
			func(string, string) (polecat.LiveGitState, bool) { return polecat.LiveGitState{}, false }, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			r := &rig.Rig{Name: "gastown", Path: filepath.Join(town, "gastown")}
			db := mrlessBeads{beadsfake.New()}
			agentID := polecatBeadIDForRig(r, "gastown", "ember")
			if _, err := db.Create(beads.CreateOptions{
				ID: agentID, Title: agentID, Labels: []string{"gt:agent"}, Priority: -1,
				Description: beads.FormatAgentDescription(agentID, &beads.AgentFields{
					RoleType: "polecat", Rig: "gastown", AgentState: string(beads.AgentStateDone),
					CleanupStatus: string(polecat.CleanupClean), Agent: tt.agent, Branch: "polecat/ember/gt-a+x",
				}),
			}); err != nil {
				t.Fatal(err)
			}
			p := &polecat.Polecat{
				Name: "ember", State: polecat.StateDone, Branch: "polecat/ember/gt-a+x",
				// The host clone: gone, so any host measurement of it fails.
				ClonePath: filepath.Join(town, "gastown", "polecats", "ember", "gastown"),
			}

			status := checkRecoveryForPolecatWith(sandboxSeatEnv{probe: tt.probe}, db, nil, r, "gastown", "ember", p, false)

			if live := status.GitStateSource == polecat.GitStateSourceLive; live != tt.wantLive {
				t.Errorf("git state source = %q, want live=%v (verdict %s, blockers %v)", status.GitStateSource, tt.wantLive, status.Verdict, status.Blockers)
			}
			if unknown := status.GitStateSource == polecat.GitStateSourceUnknown; unknown != tt.wantUnknown {
				t.Errorf("git state source = %q, want unknown=%v (reason %q)", status.GitStateSource, tt.wantUnknown, status.GitStateReason)
			}
			if tt.wantLive && strings.Contains(strings.Join(status.Blockers, "\n"), "git_state") {
				t.Errorf("a gone sandbox seat carries a git blocker: %v", status.Blockers)
			}
			if tt.wantUnknown && (status.Reusable || status.SafeToNuke) {
				t.Errorf("a seat with unknown git state is reusable=%v safe_to_nuke=%v", status.Reusable, status.SafeToNuke)
			}
		})
	}
}

// The sandbox git state the recovery and nuke checks read: a gone sandbox is
// clean, anything the gateway did not affirm is an error every caller fails
// closed on, and the active-MR "git safe" answer needs an affirmed clean state.
func TestSandboxSeatGitStateHelpers(t *testing.T) {
	t.Parallel()
	gone := polecat.SandboxSeatGitState("", false, nil, "gt-ember")
	running := polecat.SandboxSeatGitState("", true, nil, "gt-ember")
	failed := polecat.SandboxSeatGitState("", false, errors.New("connection refused"), "gt-ember")

	if st, err := sandboxSeatGitState(gone); err != nil || !st.Clean || st.UnpushedCommits != 0 {
		t.Errorf("gone sandbox = %+v, %v; want a clean state", st, err)
	}
	for name, live := range map[string]polecat.LiveGitState{"running": running, "gateway_error": failed} {
		if st, err := sandboxSeatGitState(live); err == nil || st != nil {
			t.Errorf("%s = %+v, %v; want an error and no state", name, st, err)
		}
	}
	if !sandboxSeatGitSafe(gone) {
		t.Error("a gone sandbox is not git safe")
	}
	for name, live := range map[string]polecat.LiveGitState{
		"running":       running,
		"gateway_error": failed,
		"unpushed":      {Source: polecat.GitStateSourceLive, UnpushedCommits: 1},
		"dirty":         {Source: polecat.GitStateSourceLive, Dirty: true},
		"stash":         {Source: polecat.GitStateSourceLive, StashCount: 1},
	} {
		if sandboxSeatGitSafe(live) {
			t.Errorf("%s reads as git safe", name)
		}
	}
}

// With the sandbox gone, check-recovery tells lost work from a finished seat: a
// bead the seat left unfinished with no branch on origin and no resume_branch
// is lost work (NEEDS_RECOVERY, sandbox-work-lost, not safe to nuke); a branch on
// origin, a resume_branch, a finished bead, or no bead at all is not, and an
// origin that cannot be asked leaves the git state unknown (gt-qq6rg.3).
func TestCheckRecoveryOfAGoneSandboxSeatTellsLostWork(t *testing.T) {
	t.Parallel()
	const branch = "polecat/ember/gt-a+x"
	tests := []struct {
		name       string
		hook       string // the agent bead's hook_bead
		source     string // its last_source_issue
		workStatus string // the work bead's status
		resume     bool   // the work bead carries a resume_branch note
		onOrigin   bool
		originErr  error
		wantReason string // checked when set
		wantLost   bool
		wantSafe   bool
		wantUnk    bool
	}{
		{name: "hooked_open_nothing_pushed", hook: "gt-work", workStatus: "open", wantReason: "sandbox-work-lost", wantLost: true},
		{name: "source_open_hook_cleared_nothing_pushed", source: "gt-work", workStatus: "open", wantReason: "sandbox-work-lost", wantLost: true},
		{name: "hooked_open_branch_on_origin", hook: "gt-work", workStatus: "open", onOrigin: true, wantReason: "hook-still-set"},
		{name: "hooked_open_resume_branch", hook: "gt-work", workStatus: "open", resume: true, wantReason: "hook-still-set"},
		{name: "source_open_branch_on_origin", source: "gt-work", workStatus: "open", onOrigin: true, wantSafe: true},
		{name: "bead_closed", hook: "gt-work", workStatus: "closed", wantSafe: true},
		{name: "no_bead", wantSafe: true},
		{name: "origin_cannot_be_asked", source: "gt-work", workStatus: "open", originErr: errors.New("forgejo unreachable"), wantUnk: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			r := &rig.Rig{Name: "gastown", Path: filepath.Join(town, "gastown")}
			db := mrlessBeads{beadsfake.New()}
			agentID := polecatBeadIDForRig(r, "gastown", "ember")
			if _, err := db.Create(beads.CreateOptions{
				ID: agentID, Title: agentID, Labels: []string{"gt:agent"}, Priority: -1,
				Description: beads.FormatAgentDescription(agentID, &beads.AgentFields{
					RoleType: "polecat", Rig: "gastown", AgentState: string(beads.AgentStateIdle),
					CleanupStatus: string(polecat.CleanupClean), Agent: sandboxSeatAgent, Branch: branch,
					HookBead: tt.hook, LastSourceIssue: tt.source,
				}),
			}); err != nil {
				t.Fatal(err)
			}
			if tt.workStatus != "" {
				if _, err := db.Create(beads.CreateOptions{ID: "gt-work", Title: "work", Priority: 2}); err != nil {
					t.Fatal(err)
				}
				status := tt.workStatus
				upd := beads.UpdateOptions{Status: &status}
				if tt.resume {
					upd.AppendNotes = "resume_branch: " + branch
				}
				if err := db.Update("gt-work", upd); err != nil {
					t.Fatal(err)
				}
			}
			p := &polecat.Polecat{Name: "ember", State: polecat.StateIdle, Branch: branch, ClonePath: filepath.Join(town, "gastown", "polecats", "ember", "gastown")}
			env := sandboxSeatEnv{
				probe: func(string, string) (polecat.LiveGitState, bool) {
					return polecat.SandboxSeatGitState("", false, nil, "gt-ember"), true
				},
				remoteBranch: func(_, b string) (bool, error) {
					if b != branch {
						t.Errorf("origin asked about %q, want the branch the agent bead recorded", b)
					}
					return tt.onOrigin, tt.originErr
				},
			}

			status := checkRecoveryForPolecatWith(env, db, nil, r, "gastown", "ember", p, false)

			if lost := status.Reason == "sandbox-work-lost"; lost != tt.wantLost {
				t.Errorf("lost work = %v, want %v (verdict %s reason %s blockers %v)", lost, tt.wantLost, status.Verdict, status.Reason, status.Blockers)
			}
			if tt.wantLost && (status.SafeToNuke || status.Reusable || !status.NeedsRecovery) {
				t.Errorf("lost work is safe_to_nuke=%v reusable=%v needs_recovery=%v", status.SafeToNuke, status.Reusable, status.NeedsRecovery)
			}
			if tt.wantReason != "" && status.Reason != tt.wantReason {
				t.Errorf("reason = %q, want %q (blockers %v)", status.Reason, tt.wantReason, status.Blockers)
			}
			if tt.wantSafe && !status.SafeToNuke {
				t.Errorf("verdict %s reason %s blockers %v, want safe to nuke", status.Verdict, status.Reason, status.Blockers)
			}
			if tt.wantUnk && status.GitStateSource != polecat.GitStateSourceUnknown {
				t.Errorf("git state source = %q, want unknown when origin cannot be asked", status.GitStateSource)
			}
		})
	}
}
