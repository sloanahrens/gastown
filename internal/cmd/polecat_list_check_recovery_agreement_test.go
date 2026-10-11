package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
)

// listRecoveryDB is the beads seam both commands read in the agreement test:
// the fake's Client surface plus the one finder check-recovery adds.
type listRecoveryDB struct{ *beadsfake.Fake }

func (db *listRecoveryDB) FindMRForBranchAny(string) (*beads.Issue, error) { return nil, nil }

// newAgreementSeatRepo makes a worktree on a polecat branch cut from main, with
// origin set up so the live probe has a remote to measure against. Nothing is
// ahead of main until the caller commits.
func newAgreementSeatRepo(t *testing.T, branch string) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "repo")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(root, "init", "--bare", "-b", "main", remote)
	git(root, "init", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(repo, "add", "README.md")
	git(repo, "commit", "-m", "base")
	git(repo, "remote", "add", "origin", remote)
	git(repo, "push", "-u", "origin", "main")
	git(repo, "switch", "-c", branch, "main")
	git(repo, "push", "-u", "origin", branch)
	return repo
}

func commitInAgreementSeatRepo(t *testing.T, repo string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, "work.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "work.txt"}, {"commit", "-m", "work"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// TestPolecatListAgreesWithCheckRecoveryOnAFinishedStuckSeat is gt-gdm14.
// dementus, a finished deep-review seat, was left agent_state=stuck with its
// bead closed and nothing ahead of main. check-recovery never reads agent_state
// and said SAFE_TO_NUKE; the list read the state as active-work and said
// NEEDS_RECOVERY, which the dashboard pane then showed. Both commands are driven
// from one fixture, and the control rows pin that agreeing did not mean
// clearing: unpushed commits and an open bead still need recovery in both.
func TestPolecatListAgreesWithCheckRecoveryOnAFinishedStuckSeat(t *testing.T) {
	t.Parallel()

	const (
		rigName     = "gastown"
		polecatName = "dementus"
		beadID      = "gt-review"
		branch      = "polecat/dementus/gt-review"
	)

	tests := []struct {
		name         string
		beadStatus   string
		agentState   beads.AgentState
		unpushed     bool
		wantRecovery bool
	}{
		{
			name:       "closed bead, nothing ahead of main, stuck",
			beadStatus: string(beads.StatusClosed),
			agentState: beads.AgentStateStuck,
		},
		{
			name:       "closed bead, nothing ahead of main, awaiting gate",
			beadStatus: string(beads.StatusClosed),
			agentState: beads.AgentStateAwaitingGate,
		},
		{
			name:         "closed bead with unpushed commits, stuck",
			beadStatus:   string(beads.StatusClosed),
			agentState:   beads.AgentStateStuck,
			unpushed:     true,
			wantRecovery: true,
		},
		{
			name:         "open bead, nothing ahead of main, stuck",
			beadStatus:   string(beads.StatusOpen),
			agentState:   beads.AgentStateStuck,
			wantRecovery: true,
		},
		{
			name:         "hooked bead, nothing ahead of main, stuck",
			beadStatus:   string(beads.IssueStatusHooked),
			agentState:   beads.AgentStateStuck,
			wantRecovery: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := &rig.Rig{Name: rigName, Path: filepath.Join(t.TempDir(), rigName)}
			agentBeadID := polecatBeadIDForRig(r, rigName, polecatName)
			repo := newAgreementSeatRepo(t, branch)
			if tt.unpushed {
				commitInAgreementSeatRepo(t, repo)
			}

			db := &listRecoveryDB{Fake: beadsfake.New()}
			description := "role_type: polecat\nrig: " + rigName +
				"\nagent_state: " + string(tt.agentState) +
				"\nhook_bead: " + beadID +
				"\ncleanup_status: clean\nactive_mr: null\nbranch: " + branch + "\n"
			db.Seed(beads.Issue{
				ID: agentBeadID, Title: "Polecat " + polecatName, Status: "open",
				Labels: []string{"gt:agent"}, Description: description,
			})
			db.Seed(beads.Issue{
				ID: beadID, Title: "deep review", Status: tt.beadStatus,
				Assignee: rigName + "/polecats/" + polecatName,
			})

			fields := &beads.AgentFields{
				AgentState:    string(tt.agentState),
				CleanupStatus: string(polecat.CleanupClean),
				HookBead:      beadID,
				Branch:        branch,
			}
			item := buildPolecatInventoryItem(rigName, polecatName, fields, nil, polecatSessionSet{}, polecatInventoryEnv{
				WorktreePath: repo,
				IssueSource:  db,
			})

			p := &polecat.Polecat{
				Name: polecatName, Rig: rigName, State: polecat.StateIdle,
				ClonePath: repo, Branch: branch, Issue: beadID,
			}
			status := checkRecoveryForPolecat(db, nil, r, rigName, polecatName, p, false)

			if item.Disposition.Verdict != status.Verdict {
				t.Fatalf("list says %s (reason %s, blockers %v), check-recovery says %s (blockers %v)",
					item.Disposition.Verdict, item.Disposition.Reason, item.Disposition.Blockers,
					status.Verdict, status.Blockers)
			}
			if item.Disposition.NeedsRecovery != tt.wantRecovery || status.NeedsRecovery != tt.wantRecovery {
				t.Fatalf("needs_recovery list=%v check-recovery=%v, want %v (list %+v, check-recovery %+v)",
					item.Disposition.NeedsRecovery, status.NeedsRecovery, tt.wantRecovery, item.Disposition, status)
			}
			if tt.wantRecovery {
				return
			}
			if !item.Disposition.SafeToNuke || !status.SafeToNuke {
				t.Fatalf("a finished seat must be safe to nuke in both: list %+v, check-recovery %+v", item.Disposition, status)
			}
			if item.Disposition.Reason == "active-work" {
				t.Fatalf("list still reports reason active-work: %+v", item.Disposition)
			}
		})
	}
}
