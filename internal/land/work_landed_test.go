package land

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// landedWorld is origin with main at base and one polecat branch that was cut
// from it, advanced to earlier, and carried on to later. A clone is what a
// seat's worktree is: it holds origin's branches as tracking refs, so a
// worktree left anywhere on the branch can be asked about.
type landedWorld struct {
	t                    *testing.T
	git                  *gitfake.Fake
	origin, repo         string
	base, earlier, later string
}

const landedFixtureBranch = "polecat/opal/gt-abc+x1"

func newLandedWorld(t *testing.T) *landedWorld {
	t.Helper()
	root := t.TempDir()
	w := &landedWorld{t: t, git: gitfake.New(), origin: filepath.Join(root, "origin.git"), repo: filepath.Join(root, "repo")}
	w.git.InitBare(t, w.origin)
	w.base = w.git.Commit(t, w.origin, "main", "main: seed", map[string]string{"a.txt": "one\n"})
	w.git.SetRef(t, w.origin, "refs/heads/"+landedFixtureBranch, w.base)
	w.earlier = w.git.Commit(t, w.origin, landedFixtureBranch, "feat: earlier attempt", map[string]string{"b.txt": "work\n"})
	w.later = w.git.Commit(t, w.origin, landedFixtureBranch, "feat: carried on", map[string]string{"c.txt": "more\n"})
	w.git.Clone(t, w.origin, w.repo)
	return w
}

// recordNote is a LANDING RECORD naming head (and the same commit as the
// target's) for gt-abc.
func (w *landedWorld) recordNote(head string) string {
	return LandingRecord{BeadID: "gt-abc", Head: head, LandedCommit: head, Target: "main"}.NoteBlock()
}

func (w *landedWorld) worktree() gitfake.Repo { return w.git.Open(w.repo) }

// TestWorkLandedOnTargetRefsLandedAncestry is the seat case of gt-jol0i: the
// worktree holds an earlier point of a branch another seat carried on and the
// landing kept at refs/landed/<bead>, so ancestry answers where branch
// presence cannot.
func TestWorkLandedOnTargetRefsLandedAncestry(t *testing.T) {
	t.Parallel()
	w := newLandedWorld(t)
	w.git.SetRef(t, w.repo, LandedRef("gt-abc"), w.later)

	landed, reason := WorkLandedOnTarget(w.worktree(), w.earlier, "", "gt-abc", nil)
	if !landed || reason != "landed" {
		t.Fatalf("WorkLandedOnTarget(earlier of the landed branch) = %v, %q; want true, landed", landed, reason)
	}

	landed, reason = WorkLandedOnTarget(w.worktree(), w.later, "", "gt-abc", nil)
	if !landed || reason != "landed" {
		t.Fatalf("WorkLandedOnTarget(the landed head itself) = %v, %q; want true, landed", landed, reason)
	}
}

// A refs/landed head that does not contain this worktree proves nothing about
// it: the worktree may hold work the landing never took.
func TestWorkLandedOnTargetRejectsAnUnrelatedLandedHead(t *testing.T) {
	t.Parallel()
	w := newLandedWorld(t)
	w.git.SetRef(t, w.repo, LandedRef("gt-abc"), w.base)

	landed, reason := WorkLandedOnTarget(w.worktree(), w.earlier, "", "gt-abc", nil)
	if landed || reason != "" {
		t.Fatalf("WorkLandedOnTarget(landed head at base) = %v, %q; want false, no reason", landed, reason)
	}
}

// The LANDING RECORD arm: the closed bead says what it landed as, and the
// worktree's content for the files its own commits touched is that content.
func TestWorkLandedOnTargetMatchesTheLandingRecordContent(t *testing.T) {
	t.Parallel()
	w := newLandedWorld(t)
	issue := &beads.Issue{ID: "gt-abc", Status: "closed", Notes: w.recordNote(w.later)}

	landed, reason := WorkLandedOnTarget(w.worktree(), w.earlier, "", "gt-abc", issue)
	if !landed || reason != "landed" {
		t.Fatalf("WorkLandedOnTarget(earlier point, equal content) = %v, %q; want true, landed", landed, reason)
	}
}

// The bead landed, but the content is not this worktree's: say which files an
// operator must compare before discarding it.
func TestWorkLandedOnTargetNamesFilesTheLandingRecordDisagreesOn(t *testing.T) {
	t.Parallel()
	w := newLandedWorld(t)
	// A different attempt at the same work: same file, different content.
	w.git.SetRef(t, w.origin, "refs/heads/"+landedFixtureBranch, w.base)
	other := w.git.Commit(t, w.origin, landedFixtureBranch, "feat: another attempt", map[string]string{"b.txt": "different\n"})
	w.git.SetRef(t, w.repo, "refs/remotes/origin/"+landedFixtureBranch, other)

	issue := &beads.Issue{ID: "gt-abc", Status: "closed", Notes: w.recordNote(other)}
	landed, reason := WorkLandedOnTarget(w.worktree(), w.earlier, "", "gt-abc", issue)
	if landed {
		t.Fatalf("WorkLandedOnTarget(differing content) = true; want false")
	}
	if !strings.Contains(reason, "landed from another head") || !strings.Contains(reason, "b.txt") {
		t.Fatalf("reason = %q; want it to name b.txt", reason)
	}
}

// An open bead with work that is on no target and has no refs/landed head is
// the case the recovery refusal exists for.
func TestWorkLandedOnTargetRefusesAnOpenBeadWithNoEvidence(t *testing.T) {
	t.Parallel()
	w := newLandedWorld(t)
	issue := &beads.Issue{ID: "gt-abc", Status: "open", Notes: w.recordNote(w.later)}

	landed, reason := WorkLandedOnTarget(w.worktree(), w.earlier, "", "gt-abc", issue)
	if landed || reason != "" {
		t.Fatalf("WorkLandedOnTarget(open bead, no landing) = %v, %q; want false, no reason", landed, reason)
	}
}

// A closed bead with no LANDING RECORD is not evidence either: closing is a
// statement about the issue, not about this worktree's content.
func TestWorkLandedOnTargetRefusesAClosedBeadWithoutARecord(t *testing.T) {
	t.Parallel()
	w := newLandedWorld(t)
	issue := &beads.Issue{ID: "gt-abc", Status: "closed"}

	landed, reason := WorkLandedOnTarget(w.worktree(), w.earlier, "", "gt-abc", issue)
	if landed || reason != "" {
		t.Fatalf("WorkLandedOnTarget(closed bead, no record) = %v, %q; want false, no reason", landed, reason)
	}
}

// When main already holds the work, the worktree's own commit is an ancestor
// of it and the question is answered without a bead or a landed ref.
func TestWorkLandedOnTargetSeesTheCommitOnTheTarget(t *testing.T) {
	t.Parallel()
	w := newLandedWorld(t)
	w.git.SetRef(t, w.origin, "refs/heads/main", w.earlier)
	w.git.SetRef(t, w.repo, "refs/remotes/origin/main", w.earlier)

	landed, reason := WorkLandedOnTarget(w.worktree(), w.earlier, "", "", nil)
	if !landed || reason != "landed" {
		t.Fatalf("WorkLandedOnTarget(HEAD on origin/main) = %v, %q; want true, landed", landed, reason)
	}
}

// ParseLandingRecord reads the newest block: a reworked bead carries one per
// landing, and only the last says what the branch landed as.
func TestParseLandingRecordReadsTheNewestBlock(t *testing.T) {
	t.Parallel()
	first := LandingRecord{BeadID: "gt-abc", Head: "old", LandedCommit: "old", Target: "main"}
	second := LandingRecord{BeadID: "gt-abc", Head: "new", LandedCommit: "new", Target: "main", PatchID: "p2"}
	notes := first.NoteBlock() + "\n\n" + second.NoteBlock()

	rec, ok := ParseLandingRecord(notes)
	if !ok {
		t.Fatalf("ParseLandingRecord() = false; want the second block")
	}
	if rec.LandedCommit != "new" || rec.Head != "new" || rec.PatchID != "p2" {
		t.Fatalf("ParseLandingRecord() = %+v; want the newest block", rec)
	}
}
