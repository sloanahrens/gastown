package polecat

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// The bead is the seat's hook while it has one, and the bead its branch name
// encodes once the hook is cleared (gt-jol0i).
func TestSeatBeadID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		hook   string
		branch string
		want   string
	}{
		{name: "hook wins over the branch", hook: "gt-hook", branch: "polecat/nitro/gt-branch+x1", want: "gt-hook"},
		{name: "branch names the bead when the hook is cleared", branch: "polecat/nitro/gt-branch+x1", want: "gt-branch"},
		{name: "a blank hook falls through to the branch", hook: "  ", branch: "polecat/nitro/gt-branch+x1", want: "gt-branch"},
		{name: "a branch that names no bead", branch: "polecat/nitro-x1", want: ""},
		{name: "not a polecat branch", branch: "main", want: ""},
		{name: "nothing at all", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := SeatBeadID(tt.hook, tt.branch); got != tt.want {
				t.Fatalf("SeatBeadID(%q, %q) = %q; want %q", tt.hook, tt.branch, got, tt.want)
			}
		})
	}
}

// landedSeatRig is a rig whose mayor/rig checkout holds one commit origin/main
// does not: the commits a finished seat's worktree is left with. The returned
// head is that commit.
func landedSeatRig(t *testing.T) (w *world, repoPath string, head string, db *polecatDB) {
	t.Helper()
	_, mayorRig, bd, world := canonicalRig(t)
	for _, id := range []string{"gt-seat", "gt-hook", "gt-other"} {
		bd.Seed(beads.Issue{ID: id, Title: id, Status: "open"})
	}
	head = world.writeAndCommit(t, mayorRig, "feat: seat work", map[string]string{"seat.txt": "work\n"})
	return world, mayorRig, head, bd
}

// ApplyWorkLanded asks the one predicate with the bead the seat's hook or
// branch names, and records the verdict token and detail on the facts.
func TestApplyWorkLandedRecordsTheVerdict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		hook       string
		branch     string
		landedFor  string // bead whose refs/landed/<bead> is set to the seat head
		wantLanded bool
		wantReason string
	}{
		{name: "branch names the bead after the hook is cleared", branch: "polecat/nitro/gt-seat+x1", landedFor: "gt-seat", wantLanded: true, wantReason: land.WorkLandedByRef},
		{name: "hook bead is the one asked", hook: "gt-hook", branch: "polecat/nitro/gt-other+x1", landedFor: "gt-hook", wantLanded: true, wantReason: land.WorkLandedByRef},
		{name: "the branch's bead is not asked while a hook is set", hook: "gt-hook", branch: "polecat/nitro/gt-other+x1", landedFor: "gt-other", wantReason: land.WorkUnpushed},
		{name: "no landed head", branch: "polecat/nitro/gt-seat+x1", wantReason: land.WorkUnpushed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w, repoPath, head, db := landedSeatRig(t)
			if tt.landedFor != "" {
				w.SetRef(t, repoPath, land.LandedRef(tt.landedFor), head)
			}

			facts := WorkstateFacts{UnpushedCommits: 1}
			ApplyWorkLanded(&facts, w.repo(repoPath), db, tt.hook, tt.branch)
			if facts.WorkLanded != tt.wantLanded || facts.WorkLandedReason != tt.wantReason {
				t.Fatalf("facts = landed %v reason %q; want landed %v reason %q", facts.WorkLanded, facts.WorkLandedReason, tt.wantLanded, tt.wantReason)
			}
		})
	}
}

// A seat with nothing unpushed, and a seat whose branch names no bead, leave
// the facts alone and pay no bead read.
func TestApplyWorkLandedSkipsWhatItCannotClear(t *testing.T) {
	t.Parallel()
	w, repoPath, head, db := landedSeatRig(t)
	w.SetRef(t, repoPath, land.LandedRef("gt-seat"), head)

	clean := WorkstateFacts{}
	ApplyWorkLanded(&clean, w.repo(repoPath), db, "", "polecat/nitro/gt-seat+x1")
	unnamed := WorkstateFacts{UnpushedCommits: 1}
	ApplyWorkLanded(&unnamed, w.repo(repoPath), db, "", "main")
	ApplyWorkLanded(&unnamed, nil, db, "gt-seat", "main")

	if clean.WorkLanded || clean.WorkLandedReason != "" || unnamed.WorkLanded || unnamed.WorkLandedReason != "" {
		t.Fatalf("facts = %+v / %+v; want no landing evidence recorded", clean, unnamed)
	}
	if db.readCount() != 0 {
		t.Fatalf("bead reads = %d; want none", db.readCount())
	}
}

// An unreadable bead loses the LANDING RECORD arm; the verdict says the probe
// failed rather than that nothing proves the work landed.
func TestApplyWorkLandedNamesAnUnreadableBead(t *testing.T) {
	t.Parallel()
	w, repoPath, _, db := landedSeatRig(t)
	db.err = errors.New("dolt: connection refused")

	facts := WorkstateFacts{UnpushedCommits: 1}
	ApplyWorkLanded(&facts, w.repo(repoPath), db, "", "polecat/nitro/gt-seat+x1")
	if facts.WorkLanded || facts.WorkLandedReason != land.WorkProbeFailed {
		t.Fatalf("facts = landed %v reason %q; want not landed, %s", facts.WorkLanded, facts.WorkLandedReason, land.WorkProbeFailed)
	}
	if facts.WorkLandedDetail == "" {
		t.Fatalf("detail empty; want the bead read named")
	}
}

// A closed bead is not landing evidence by itself: without a LANDING RECORD the
// seat keeps the unpushed refusal.
func TestApplyWorkLandedClosedBeadWithoutARecordIsNotEvidence(t *testing.T) {
	t.Parallel()
	w, repoPath, _, db := landedSeatRig(t)
	db.Seed(beads.Issue{ID: "gt-seat", Title: "seat", Status: "closed"})

	facts := WorkstateFacts{UnpushedCommits: 1}
	ApplyWorkLanded(&facts, w.repo(repoPath), db, "", "polecat/nitro/gt-seat+x1")
	if facts.WorkLanded || facts.WorkLandedReason != land.WorkUnpushed {
		t.Fatalf("facts = landed %v reason %q; want not landed, %s", facts.WorkLanded, facts.WorkLandedReason, land.WorkUnpushed)
	}
}

// The reuse gate (Manager.WorkstateDispositionForPolecat) reads a landed seat
// the way check-recovery does: commits that look unpushed are the landing's own
// once refs/landed/<bead> holds them, so the seat is safe to nuke as landed.
func TestWorkstateDispositionForPolecatClearsALandedSeat(t *testing.T) {
	t.Parallel()
	mgr, mayorRig, bd, w := canonicalRig(t)
	bd.Seed(beads.Issue{ID: "gt-seat", Title: "seat", Status: "open"})
	p, err := mgr.AddWithOptions("toast", AddOptions{HookBead: "gt-seat"})
	if err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}
	if err := w.repo(p.ClonePath).CleanForce(); err != nil {
		t.Fatal(err)
	}
	// The seat finished: idle, clean, hook cleared, the branch still naming the
	// bead it carried.
	bd.settleAgent(t, mgr.agentBeadID("toast"), p.Branch)
	head := w.writeAndCommit(t, p.ClonePath, "feat: seat work", map[string]string{"seat.txt": "work\n"})
	// The rig's origin is the shared repository itself, which would hold the
	// seat's branch; a remote that lacks it is what a reaped landing leaves.
	remote := filepath.Join(t.TempDir(), "origin.git")
	w.InitBare(t, remote)
	w.Commit(t, remote, "main", "base", map[string]string{"README.md": "# Test\n"})
	w.AddRemote(t, mayorRig, "origin", remote)

	before := mgr.WorkstateDispositionForPolecat("toast", StateIdle, "")
	if before.Verdict != WorkstateVerdictNeedsRecovery || before.Reason != "git-unpushed" {
		t.Fatalf("before the landing: %+v; want NEEDS_RECOVERY git-unpushed", before)
	}

	// A landed head that does not contain the seat's commit clears nothing.
	w.SetRef(t, p.ClonePath, land.LandedRef("gt-seat"), w.rev(t, p.ClonePath, "origin/main"))
	if got := mgr.WorkstateDispositionForPolecat("toast", StateIdle, ""); got.Verdict != WorkstateVerdictNeedsRecovery {
		t.Fatalf("unrelated landed head: %+v; want NEEDS_RECOVERY", got)
	}

	w.SetRef(t, p.ClonePath, land.LandedRef("gt-seat"), head)
	after := mgr.WorkstateDispositionForPolecat("toast", StateIdle, "")
	if after.Verdict != WorkstateVerdictSafeToNuke || after.Reason != "landed" {
		t.Fatalf("after the landing: %+v; want SAFE_TO_NUKE landed", after)
	}
}
