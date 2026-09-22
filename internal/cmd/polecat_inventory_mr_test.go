package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
)

// fakeInventoryIssueReader stands in for the rig's beads database: source
// issues only, since MR ids are the index's job.
type fakeInventoryIssueReader map[string]*beads.Issue

func (f fakeInventoryIssueReader) Show(id string) (*beads.Issue, error) {
	if issue, ok := f[id]; ok {
		return issue, nil
	}
	return nil, beads.ErrNotFound
}

// TestPolecatInventoryDanglingMRGate pins the list path's half of gt-wprt: a
// pointer whose wisp is gone stops reading as idle-pr-open once the work it
// carried is provably on origin/main, and keeps failing closed when the work
// did not land, the MR is still in the queue, or nothing can be measured.
//
// These are real repositories because the evidence under test is git's: the
// submitted tip is on the integration branch by ancestry or by a squash merge
// that leaves no ancestry behind.
func TestPolecatInventoryDanglingMRGate(t *testing.T) {
	landedRepo, landedBranch := initOrphanCleanupRepo(t, true)
	runOrphanCleanupGit(t, landedRepo, "checkout", landedBranch)
	unlandedRepo, unlandedBranch := initOrphanCleanupRepo(t, false)
	runOrphanCleanupGit(t, unlandedRepo, "checkout", unlandedBranch)

	sourceIssues := fakeInventoryIssueReader{
		"gt-open": {ID: "gt-open", Status: "open"},
	}

	tests := []struct {
		name        string
		activeMR    string
		index       []*beads.Issue
		repo        string
		branch      string
		noReader    bool
		wantBlocker string
		wantReuse   string
	}{
		{
			name:     "dangling MR whose work landed frees the slot",
			activeMR: "gt-mr-gone",
			repo:     landedRepo,
			branch:   landedBranch,
			// The source issue is still open, so only landed-work evidence can
			// clear this MR.
			wantBlocker: "",
			wantReuse:   "idle-preserved",
		},
		{
			name:        "dangling MR whose work did not land still blocks",
			activeMR:    "gt-mr-gone",
			repo:        unlandedRepo,
			branch:      unlandedBranch,
			wantBlocker: "active_mr=gt-mr-gone status=missing",
			wantReuse:   "idle-pr-open",
		},
		{
			name:        "queued MR blocks even when the work landed",
			activeMR:    "gt-mr-open",
			index:       []*beads.Issue{mrBead("gt-mr-open", "slate", "gastown", "open", "")},
			repo:        landedRepo,
			branch:      landedBranch,
			wantBlocker: "active_mr=gt-mr-open status=ready",
			wantReuse:   "idle-pr-open",
		},
		{
			name:        "rejected MR whose work did not land blocks",
			activeMR:    "gt-mr-rejected",
			index:       []*beads.Issue{mrBead("gt-mr-rejected", "slate", "gastown", "closed", "rejected")},
			repo:        unlandedRepo,
			branch:      unlandedBranch,
			wantBlocker: "active_mr=gt-mr-rejected status=rejected",
			wantReuse:   "idle-pr-open",
		},
		{
			// The counts-only capacity projection passes no source and no
			// worktree: an unresolvable pointer must stay blocking rather than
			// be read as gone.
			name:        "no source and no worktree fails closed",
			activeMR:    "gt-mr-gone",
			noReader:    true,
			repo:        landedRepo,
			branch:      landedBranch,
			wantBlocker: "active_mr=gt-mr-gone status=missing",
			wantReuse:   "idle-pr-open",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			index, err := loadPolecatMRIndex(&fakePolecatMRLister{mrs: tt.index}, "gastown")
			if err != nil {
				t.Fatalf("loadPolecatMRIndex: %v", err)
			}
			env := polecatInventoryEnv{
				MRs: index,
				// The worktree is what makes the landed probe affordable at
				// all; capacity passes none.
				WorktreePath: tt.repo,
			}
			if !tt.noReader {
				env.ActiveMRSource = polecatActiveMRReader{index: index, bd: sourceIssues}
			}

			item := buildPolecatInventoryItem(
				"gastown",
				"slate",
				&beads.AgentFields{
					AgentState:      string(beads.AgentStateIdle),
					CleanupStatus:   string(polecat.CleanupClean),
					ActiveMR:        tt.activeMR,
					LastSourceIssue: "gt-open",
					Branch:          tt.branch,
				},
				nil,
				polecatSessionSet{},
				env,
			)

			blockers := item.Disposition.Blockers
			switch {
			case tt.wantBlocker == "" && len(blockers) != 0:
				t.Fatalf("blockers = %v, want none", blockers)
			case tt.wantBlocker != "" && (len(blockers) != 1 || blockers[0] != tt.wantBlocker):
				t.Fatalf("blockers = %v, want [%s]", blockers, tt.wantBlocker)
			}
			if item.Disposition.ReuseStatus != tt.wantReuse {
				t.Fatalf("reuse status = %q, want %q", item.Disposition.ReuseStatus, tt.wantReuse)
			}
		})
	}
}
