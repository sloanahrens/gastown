//go:build integration

package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
)

// TestIntegrationCheckRecoveryMissingCleanupStatusEscape is the cmd-level test for the
// gt-ui2x escape (gt-2ugv): a polecat whose agent bead was read but never
// recorded a cleanup_status may clear check-recovery's verdict once a live
// probe of its worktree and the bead's own facts prove nothing is at risk.
// The escape's unit tests (ResolveIgnoreCleanupStatus, NewWorkstateInput) and
// the `gt polecat list` end-to-end table both pass with the escape wired wrong
// in checkRecoveryForPolecat, the fact-gathering `gt polecat nuke` and the
// witness read the verdict from. So this drives checkRecoveryForPolecat itself
// with a real worktree and an beadsfake database, and pins both sides: the escape
// clears the missing status, and every other blocker still holds.
func TestIntegrationCheckRecoveryMissingCleanupStatusEscape(t *testing.T) {
	t.Parallel()

	const (
		rigName     = "gastown"
		polecatName = "topaz"
		branch      = "polecat/topaz"
		sourceID    = "gt-src1"
		mrID        = "gt-wisp-mr1"
	)

	// A polecat worktree cut from origin/main with the work on it: branch at
	// the main tip, nothing uncommitted, nothing stashed, nothing unpushed.
	cleanWorktree := func(t *testing.T) string {
		t.Helper()
		repo := setupGitStateRemoteRepo(t)
		runGitCmd(t, repo, "switch", "-c", branch, "main")
		return repo
	}
	dirtyWorktree := func(t *testing.T) string {
		t.Helper()
		repo := cleanWorktree(t)
		writeTestFile(t, filepath.Join(repo, "wip.txt"), "uncommitted work\n")
		return repo
	}
	unpushedWorktree := func(t *testing.T) string {
		t.Helper()
		repo := cleanWorktree(t)
		writeTestFile(t, filepath.Join(repo, "feature.txt"), "unpushed work\n")
		runGitCmd(t, repo, "add", "feature.txt")
		runGitCmd(t, repo, "commit", "-m", "unpushed work")
		return repo
	}

	tests := []struct {
		name        string
		worktree    func(t *testing.T) string
		agentBead   string // description of the agent bead; "" makes bd report it absent
		sourceState string // status bd reports for sourceID
		wantSafe    bool
		wantVerdict string
		// wantIgnored: the escape fired — the missing status is named in a
		// diagnostic as ignored rather than left to veto the verdict.
		wantIgnored bool
	}{
		{
			// The gt-ui2x acceptance case, at the CLI: bead read, status never
			// self-reported, source closed, live worktree clean.
			name:        "missing status on a read agent bead with a clean live worktree clears",
			worktree:    cleanWorktree,
			agentBead:   "role_type: polecat\nrig: gastown\nagent_state: idle\nhook_bead: null\ncleanup_status: null\nactive_mr: null\n",
			sourceState: "closed",
			wantSafe:    true,
			wantVerdict: polecat.WorkstateVerdictSafeToNuke,
			wantIgnored: true,
		},
		{
			name:        "missing status with uncommitted work in the live worktree still blocks",
			worktree:    dirtyWorktree,
			agentBead:   "role_type: polecat\nrig: gastown\nagent_state: idle\nhook_bead: null\ncleanup_status: null\nactive_mr: null\n",
			sourceState: "closed",
			wantVerdict: polecat.WorkstateVerdictNeedsRecovery,
		},
		{
			name:        "missing status with an unpushed commit in the live worktree still blocks",
			worktree:    unpushedWorktree,
			agentBead:   "role_type: polecat\nrig: gastown\nagent_state: idle\nhook_bead: null\ncleanup_status: null\nactive_mr: null\n",
			sourceState: "closed",
			wantVerdict: polecat.WorkstateVerdictNeedsRecovery,
		},
		{
			// hook_bead names a source that is still open: the polecat holds
			// live work, which a clean worktree does not disprove.
			name:        "missing status with a hook bead still open still blocks",
			worktree:    cleanWorktree,
			agentBead:   "role_type: polecat\nrig: gastown\nagent_state: idle\nhook_bead: gt-src1\ncleanup_status: null\nactive_mr: null\n",
			sourceState: "open",
			wantVerdict: polecat.WorkstateVerdictNeedsRecovery,
		},
		{
			name:        "missing status with an active MR still pending still blocks",
			worktree:    cleanWorktree,
			agentBead:   "role_type: polecat\nrig: gastown\nagent_state: idle\nhook_bead: null\ncleanup_status: null\nactive_mr: gt-wisp-mr1\n",
			sourceState: "closed",
			wantVerdict: polecat.WorkstateVerdictNeedsRecovery,
		},
		{
			// gt-14a: the escape needs the bead to have been read. An absent
			// agent bead leaves hook_bead/active_mr unverifiable, so a clean
			// worktree alone must not clear it.
			name:        "an unreadable agent bead is not cleared by a clean live worktree",
			worktree:    cleanWorktree,
			agentBead:   "",
			sourceState: "closed",
			wantVerdict: polecat.WorkstateVerdictNeedsRecovery,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := &rig.Rig{Name: rigName, Path: filepath.Join(t.TempDir(), rigName)}
			agentBeadID := polecatBeadIDForRig(r, rigName, polecatName)
			assignee := rigName + "/polecats/" + polecatName

			db := &recoveryDB{Fake: beadsfake.New()}
			if tt.agentBead != "" {
				db.Seed(beads.Issue{ID: agentBeadID, Title: "Polecat topaz", Status: "open", Labels: []string{"gt:agent"}, Description: tt.agentBead})
			}
			db.Seed(
				beads.Issue{ID: sourceID, Title: "source", Status: tt.sourceState, Assignee: assignee},
				beads.Issue{ID: mrID, Title: "MR", Status: "open", Labels: []string{"gt:merge-request"}, Description: "branch: " + branch + "\nsource_issue: " + sourceID + "\ntarget: main\n"},
			)
			b := db

			p := &polecat.Polecat{Name: polecatName, Rig: rigName, State: polecat.StateIdle, ClonePath: tt.worktree(t), Branch: branch, Issue: sourceID}
			status := checkRecoveryForPolecat(b, r, rigName, polecatName, p, false)

			if status.SafeToNuke != tt.wantSafe || status.Verdict != tt.wantVerdict {
				t.Fatalf("SafeToNuke/Verdict = %v/%s, want %v/%s (status %+v)", status.SafeToNuke, status.Verdict, tt.wantSafe, tt.wantVerdict, status)
			}
			if !tt.wantSafe && status.Reusable {
				t.Errorf("Reusable = true for a polecat that must not clear (status %+v)", status)
			}
			ignored := strings.Contains(strings.Join(status.Diagnostics, ";"), "ignored_cleanup_status=")
			if ignored != tt.wantIgnored {
				t.Errorf("escape diagnostic present = %v, want %v (diagnostics %v)", ignored, tt.wantIgnored, status.Diagnostics)
			}
		})
	}
}

// recoveryDB is a beadsfake database with the agent-bead and merge-request
// helpers check-recovery reads, each reduced to Client operations.
type recoveryDB struct{ *beadsfake.Fake }

func (db *recoveryDB) GetAgentBead(id string) (*beads.Issue, *beads.AgentFields, error) {
	issue, err := db.Show(id)
	if err != nil {
		return nil, nil, err
	}
	fields := beads.ParseAgentFields(issue.Description)
	fields.AgentState = beads.ResolveAgentState(issue.Description, issue.AgentState)
	return issue, fields, nil
}

func (db *recoveryDB) FindMRForBranchAny(branch string) (*beads.Issue, error) {
	mrs, err := db.List(beads.ListOptions{Label: "gt:merge-request", Status: "all", Priority: -1})
	if err != nil {
		return nil, err
	}
	for _, mr := range mrs {
		if strings.HasPrefix(mr.Description, "branch: "+branch+"\n") {
			return mr, nil
		}
	}
	return nil, nil
}

func (db *recoveryDB) UpdateAgentCleanupStatus(id, status string) error {
	return beads.UpdateAgentDescriptionFields(db.Fake, id, beads.AgentFieldUpdates{CleanupStatus: &status})
}
