package cmd

import (
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/polecat"
)

const landedSeatBranch = "polecat/nitro/gt-seat+x1"

// landedSeat is a seat's worktree left at an earlier point of a branch the
// landing carried on to a later head and then reaped from origin (gt-jol0i):
// a modeled repository the landed probe reads through workLandedOpener.
type landedSeat struct {
	fake     *gitfake.Fake
	worktree string
	later    string
}

func newLandedSeat(t *testing.T, keepLandedRef bool) landedSeat {
	t.Helper()
	root := t.TempDir()
	fake := gitfake.New()
	origin, worktree := filepath.Join(root, "origin.git"), filepath.Join(root, "seat")
	fake.InitBare(t, origin)
	base := fake.Commit(t, origin, "main", "main: seed", map[string]string{"a.txt": "one\n"})
	fake.SetRef(t, origin, "refs/heads/"+landedSeatBranch, base)
	earlier := fake.Commit(t, origin, landedSeatBranch, "feat: earlier attempt", map[string]string{"b.txt": "work\n"})
	later := fake.Commit(t, origin, landedSeatBranch, "feat: carried on", map[string]string{"c.txt": "more\n"})
	fake.Clone(t, origin, worktree)
	if err := fake.OpenBranchRepo(worktree).CheckoutNewBranch("seat", earlier); err != nil {
		t.Fatalf("check out the seat's earlier point: %v", err)
	}
	if keepLandedRef {
		fake.SetRef(t, worktree, land.LandedRef("gt-seat"), later)
	}
	return landedSeat{fake: fake, worktree: worktree, later: later}
}

func (s landedSeat) open() workLandedOpener {
	return func(string) land.WorkLandedRepo { return s.fake.OpenBranchRepo(s.worktree) }
}

func seatBeads(t *testing.T) *beadsfake.Fake {
	t.Helper()
	db := beadsfake.New()
	db.Seed(beads.Issue{ID: "gt-seat", Title: "the work", Status: "open"})
	return db
}

// The bead is the seat's hook while it has one, and the bead its branch name
// encodes once the hook is cleared; either reaches the refs/landed ancestry
// check (gt-jol0i).
func TestApplyWorkLandedToWorkstateFactsPicksTheSeatBead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		hook   string
		branch string
	}{
		{name: "hook bead", hook: "gt-seat", branch: "polecat/nitro/gt-elsewhere+x1"},
		{name: "branch names the bead when the hook is cleared", branch: landedSeatBranch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			seat := newLandedSeat(t, true)

			facts := polecat.WorkstateFacts{UnpushedCommits: 1}
			applyWorkLandedToWorkstateFacts(&facts, seat.open(), seat.worktree, "", seatBeads(t), tt.hook, tt.branch)
			if !facts.WorkLanded || facts.WorkLandedReason != land.WorkLandedByRef {
				t.Fatalf("facts = landed %v reason %q; want landed %s", facts.WorkLanded, facts.WorkLandedReason, land.WorkLandedByRef)
			}
		})
	}
}

// A seat with no worktree to read, and a seat with no leftover commits, leave
// the facts alone even though a landed head exists.
func TestApplyWorkLandedToWorkstateFactsLeavesNoEvidenceAlone(t *testing.T) {
	t.Parallel()
	seat := newLandedSeat(t, true)

	noWorktree := polecat.WorkstateFacts{UnpushedCommits: 1}
	applyWorkLandedToWorkstateFacts(&noWorktree, seat.open(), "", "", seatBeads(t), "gt-seat", landedSeatBranch)
	noCommits := polecat.WorkstateFacts{}
	applyWorkLandedToWorkstateFacts(&noCommits, seat.open(), seat.worktree, "", seatBeads(t), "gt-seat", landedSeatBranch)
	if noWorktree.WorkLanded || noCommits.WorkLanded || noWorktree.WorkLandedReason != "" || noCommits.WorkLandedReason != "" {
		t.Fatalf("facts = %+v / %+v; want no landing evidence", noWorktree, noCommits)
	}
}

// gt polecat list judges a finished seat through the same predicate as
// check-recovery: a seat whose commits the landing took is safe to nuke as
// landed, and without the landed head it stays NEEDS_RECOVERY git-unpushed.
func TestBuildPolecatInventoryItemClearsALandedSeat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		landedRef  bool
		wantSafe   bool
		wantReason string
	}{
		{name: "landed head holds the seat's commit", landedRef: true, wantSafe: true, wantReason: "landed"},
		{name: "no landed head", wantReason: "git-unpushed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			seat := newLandedSeat(t, tt.landedRef)
			env := polecatInventoryEnv{
				WorktreePath:   seat.worktree,
				IssueSource:    seatBeads(t),
				openWorkLanded: seat.open(),
				probe: func(string, bool) polecat.LiveGitState {
					return polecat.LiveGitState{Branch: landedSeatBranch, Source: polecat.GitStateSourceLive, UnpushedCommits: 1}
				},
			}
			item := buildPolecatInventoryItem(
				"gastown", "nitro",
				&beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: string(polecat.CleanupClean), Branch: landedSeatBranch},
				nil, polecatSessionSet{}, env,
			)
			d := item.Disposition
			if d.SafeToNuke != tt.wantSafe || d.Reason != tt.wantReason {
				t.Fatalf("disposition = %+v; want safe_to_nuke=%v reason=%q", d, tt.wantSafe, tt.wantReason)
			}
		})
	}
}
