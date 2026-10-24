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

			status := checkRecoveryForPolecatWith(tt.probe, db, nil, r, "gastown", "ember", p, false)

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
