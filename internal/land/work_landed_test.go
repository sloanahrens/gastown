package land

import (
	"errors"
	"os"
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

// failing wraps a repository and fails the one read a test names, so a probe
// failure is asserted to fail closed rather than assumed to.
type failing struct {
	WorkLandedRepo
	refExists, revLanded, isAncestor, mergeBase, diffNames, diffPaths error
	noTouchedFiles                                                    bool
}

func (f failing) RefExists(ref string) (bool, error) {
	if f.refExists != nil {
		return false, f.refExists
	}
	return f.WorkLandedRepo.RefExists(ref)
}

func (f failing) Rev(ref string) (string, error) {
	if f.revLanded != nil && strings.HasPrefix(ref, "refs/landed/") {
		return "", f.revLanded
	}
	return f.WorkLandedRepo.Rev(ref)
}

func (f failing) IsAncestor(a, d string) (bool, error) {
	if f.isAncestor != nil {
		return false, f.isAncestor
	}
	return f.WorkLandedRepo.IsAncestor(a, d)
}

func (f failing) MergeBase(a, b string) (string, error) {
	if f.mergeBase != nil {
		return "", f.mergeBase
	}
	return f.WorkLandedRepo.MergeBase(a, b)
}

func (f failing) DiffNameOnly(base, head string) ([]string, error) {
	if f.diffNames != nil {
		return nil, f.diffNames
	}
	if f.noTouchedFiles {
		return nil, nil
	}
	return f.WorkLandedRepo.DiffNameOnly(base, head)
}

func (f failing) DiffPathsNameOnly(a, b string, paths []string) ([]string, error) {
	if f.diffPaths != nil {
		return nil, f.diffPaths
	}
	return f.WorkLandedRepo.DiffPathsNameOnly(a, b, paths)
}

// TestWorkLandedOnTargetRefsLandedAncestry is the seat case of gt-jol0i: the
// worktree holds an earlier point of a branch another seat carried on and the
// landing kept at refs/landed/<bead>, so ancestry answers where branch
// presence cannot.
func TestWorkLandedOnTargetRefsLandedAncestry(t *testing.T) {
	t.Parallel()
	w := newLandedWorld(t)
	w.git.SetRef(t, w.repo, LandedRef("gt-abc"), w.later)

	for name, head := range map[string]string{"earlier point of the landed branch": w.earlier, "the landed head itself": w.later} {
		v := WorkLandedOnTarget(w.worktree(), WorkLandedQuery{Head: head, BeadID: "gt-abc"})
		if !v.Landed || v.Reason != WorkLandedByRef || v.Detail != "" {
			t.Fatalf("WorkLandedOnTarget(%s) = %+v; want landed %s", name, v, WorkLandedByRef)
		}
	}
}

// A refs/landed head that does not contain this worktree proves nothing about
// it: the worktree may hold work the landing never took.
func TestWorkLandedOnTargetRejectsAnUnrelatedLandedHead(t *testing.T) {
	t.Parallel()
	w := newLandedWorld(t)
	w.git.SetRef(t, w.repo, LandedRef("gt-abc"), w.base)

	v := WorkLandedOnTarget(w.worktree(), WorkLandedQuery{Head: w.earlier, BeadID: "gt-abc"})
	if v.Landed || v.Reason != WorkUnpushed {
		t.Fatalf("WorkLandedOnTarget(landed head at base) = %+v; want not landed, %s", v, WorkUnpushed)
	}
}

// The LANDING RECORD arm: the closed bead says what it landed as, and the
// worktree's content for the files its own commits touched is that content.
func TestWorkLandedOnTargetMatchesTheLandingRecordContent(t *testing.T) {
	t.Parallel()
	w := newLandedWorld(t)
	issue := &beads.Issue{ID: "gt-abc", Status: "closed", Notes: w.recordNote(w.later)}

	v := WorkLandedOnTarget(w.worktree(), WorkLandedQuery{Head: w.earlier, BeadID: "gt-abc", Issue: issue})
	if !v.Landed || v.Reason != WorkLandedByRecord {
		t.Fatalf("WorkLandedOnTarget(earlier point, equal content) = %+v; want landed %s", v, WorkLandedByRecord)
	}
}

// The bead landed, but the content is not this worktree's: the file names are
// the Detail an operator compares, and the Reason stays a short token.
func TestWorkLandedOnTargetNamesFilesTheLandingRecordDisagreesOn(t *testing.T) {
	t.Parallel()
	w := newLandedWorld(t)
	// A different attempt at the same work: same file, different content.
	w.git.SetRef(t, w.origin, "refs/heads/"+landedFixtureBranch, w.base)
	other := w.git.Commit(t, w.origin, landedFixtureBranch, "feat: another attempt", map[string]string{"b.txt": "different\n"})
	w.git.SetRef(t, w.repo, "refs/remotes/origin/"+landedFixtureBranch, other)

	issue := &beads.Issue{ID: "gt-abc", Status: "closed", Notes: w.recordNote(other)}
	v := WorkLandedOnTarget(w.worktree(), WorkLandedQuery{Head: w.earlier, BeadID: "gt-abc", Issue: issue})
	if v.Landed || v.Reason != WorkLandedDiffers {
		t.Fatalf("WorkLandedOnTarget(differing content) = %+v; want not landed, %s", v, WorkLandedDiffers)
	}
	if !strings.Contains(v.Detail, "landed from another head") || !strings.Contains(v.Detail, "b.txt") {
		t.Fatalf("detail = %q; want it to name b.txt", v.Detail)
	}
}

// An open bead with work that is on no target and has no refs/landed head is
// the case the recovery refusal exists for.
func TestWorkLandedOnTargetRefusesAnOpenBeadWithNoEvidence(t *testing.T) {
	t.Parallel()
	w := newLandedWorld(t)
	issue := &beads.Issue{ID: "gt-abc", Status: "open", Notes: w.recordNote(w.later)}

	v := WorkLandedOnTarget(w.worktree(), WorkLandedQuery{Head: w.earlier, BeadID: "gt-abc", Issue: issue})
	if v.Landed || v.Reason != WorkUnpushed {
		t.Fatalf("WorkLandedOnTarget(open bead, no landing) = %+v; want not landed, %s", v, WorkUnpushed)
	}
}

// When main already holds the work, the worktree's own commit is an ancestor
// of it and the question is answered without a bead or a landed ref.
func TestWorkLandedOnTargetSeesTheCommitOnTheTarget(t *testing.T) {
	t.Parallel()
	w := newLandedWorld(t)
	w.git.SetRef(t, w.origin, "refs/heads/main", w.earlier)
	w.git.SetRef(t, w.repo, "refs/remotes/origin/main", w.earlier)

	v := WorkLandedOnTarget(w.worktree(), WorkLandedQuery{Head: w.earlier})
	if !v.Landed || v.Reason != WorkLandedByTarget {
		t.Fatalf("WorkLandedOnTarget(HEAD on origin/main) = %+v; want landed %s", v, WorkLandedByTarget)
	}
}

// The record's head was gc'd out of this clone, so the LANDING RECORD arm reads
// its LandedCommit instead. The landed commit adds a file the worktree's commit
// does not, so its patch is not on the target and the content arm decides.
func TestWorkLandedOnTargetFallsBackWhenTheHeadIsNotInTheClone(t *testing.T) {
	t.Parallel()
	w := newLandedWorld(t)
	missing := strings.Repeat("e", 40)
	landed := w.git.Commit(t, w.origin, "main", "land: gt-abc", map[string]string{"b.txt": "work\n", "d.txt": "squashed\n"})
	w.git.SetRef(t, w.repo, "refs/remotes/origin/main", landed)
	record := LandingRecord{BeadID: "gt-abc", Head: missing, LandedCommit: landed, Target: "main"}
	issue := &beads.Issue{ID: "gt-abc", Status: "closed", Notes: record.NoteBlock()}

	v := WorkLandedOnTarget(w.worktree(), WorkLandedQuery{Head: w.earlier, BeadID: "gt-abc", Issue: issue})
	if !v.Landed || v.Reason != WorkLandedByRecord {
		t.Fatalf("WorkLandedOnTarget(record head missing from clone) = %+v; want landed %s", v, WorkLandedByRecord)
	}
}

// A record whose head and landed commit are both absent from the clone is a
// failed probe, not unpushed: the record is broken, and reading it as unlanded
// would hide that. The two missing commits differ, and one path has no head, so
// each route to the alarm runs (gt-rrt9z).
func TestWorkLandedOnTargetAlarmsWhenNeitherRecordedCommitIsInTheClone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		head string
	}{
		{name: "head and landed commit both missing", head: strings.Repeat("e", 40)},
		{name: "no head and landed commit missing", head: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := newLandedWorld(t)
			record := LandingRecord{BeadID: "gt-abc", Head: tt.head, LandedCommit: strings.Repeat("f", 40), Target: "main"}
			issue := &beads.Issue{ID: "gt-abc", Status: "closed", Notes: record.NoteBlock()}

			v := WorkLandedOnTarget(w.worktree(), WorkLandedQuery{Head: w.earlier, BeadID: "gt-abc", Issue: issue})
			if v.Landed || v.Reason != WorkProbeFailed {
				t.Fatalf("WorkLandedOnTarget(neither recorded commit in clone) = %+v; want not landed, %s", v, WorkProbeFailed)
			}
			if !strings.Contains(v.Detail, "gt-abc") || !strings.Contains(v.Detail, "names no commit") {
				t.Fatalf("detail = %q; want it to name gt-abc and say the record names no commit", v.Detail)
			}
		})
	}
}

// Every way the proof can be missing returns not-landed with a Reason, never a
// landed answer: a missing record, an unreadable ref, an unreadable object, a
// failed comparison, and a worktree with no files to compare.
func TestWorkLandedOnTargetFailsClosed(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")

	tests := []struct {
		name       string
		issue      func(w *landedWorld) *beads.Issue
		landedRef  bool
		wrap       func(r WorkLandedRepo) WorkLandedRepo
		wantReason string
		wantDetail string
	}{
		{
			name:       "closed bead without a landing record",
			issue:      func(*landedWorld) *beads.Issue { return &beads.Issue{ID: "gt-abc", Status: "closed"} },
			wantReason: WorkUnpushed,
			wantDetail: "no landing record",
		},
		{
			name:       "bead not read",
			wantReason: WorkUnpushed,
			wantDetail: "bead not read",
		},
		{
			name:       "landed ref exists but will not resolve",
			landedRef:  true,
			wrap:       func(r WorkLandedRepo) WorkLandedRepo { return failing{WorkLandedRepo: r, revLanded: boom} },
			wantReason: WorkProbeFailed,
			wantDetail: "refs/landed/gt-abc",
		},
		{
			name:       "landed ref presence cannot be checked",
			wrap:       func(r WorkLandedRepo) WorkLandedRepo { return failing{WorkLandedRepo: r, refExists: boom} },
			wantReason: WorkProbeFailed,
			wantDetail: "refs/landed/gt-abc",
		},
		{
			name:       "ancestry check errors",
			landedRef:  true,
			wrap:       func(r WorkLandedRepo) WorkLandedRepo { return failing{WorkLandedRepo: r, isAncestor: boom} },
			wantReason: WorkProbeFailed,
			wantDetail: "compare HEAD",
		},
		{
			name:       "landing record names a commit this repository lacks",
			issue:      closedWithRecord("no-such-commit"),
			wantReason: WorkProbeFailed,
			wantDetail: "names no commit",
		},
		{
			name:       "merge-base fails",
			issue:      closedWithRecordAtLater,
			wrap:       func(r WorkLandedRepo) WorkLandedRepo { return failing{WorkLandedRepo: r, mergeBase: boom} },
			wantReason: WorkProbeFailed,
			wantDetail: "merge-base",
		},
		{
			name:       "touched-file listing fails",
			issue:      closedWithRecordAtLater,
			wrap:       func(r WorkLandedRepo) WorkLandedRepo { return failing{WorkLandedRepo: r, diffNames: boom} },
			wantReason: WorkProbeFailed,
			wantDetail: "touched",
		},
		{
			name:       "content comparison fails",
			issue:      closedWithRecordAtLater,
			wrap:       func(r WorkLandedRepo) WorkLandedRepo { return failing{WorkLandedRepo: r, diffPaths: boom} },
			wantReason: WorkProbeFailed,
			wantDetail: "compare worktree",
		},
		{
			name:       "no touched files is not a match",
			issue:      closedWithRecordAtLater,
			wrap:       func(r WorkLandedRepo) WorkLandedRepo { return failing{WorkLandedRepo: r, noTouchedFiles: true} },
			wantReason: WorkUnpushed,
			wantDetail: "touch no files",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := newLandedWorld(t)
			if tt.landedRef {
				w.git.SetRef(t, w.repo, LandedRef("gt-abc"), w.later)
			}
			var issue *beads.Issue
			if tt.issue != nil {
				issue = tt.issue(w)
			}
			var repo WorkLandedRepo = w.worktree()
			if tt.wrap != nil {
				repo = tt.wrap(repo)
			}

			v := WorkLandedOnTarget(repo, WorkLandedQuery{Head: w.earlier, BeadID: "gt-abc", Issue: issue})
			if v.Landed {
				t.Fatalf("WorkLandedOnTarget() = %+v; a failed or missing proof must not read as landed", v)
			}
			if v.Reason != tt.wantReason {
				t.Fatalf("reason = %q; want %q (detail %q)", v.Reason, tt.wantReason, v.Detail)
			}
			if !strings.Contains(v.Detail, tt.wantDetail) {
				t.Fatalf("detail = %q; want it to mention %q", v.Detail, tt.wantDetail)
			}
		})
	}
}

// A failed read in one arm does not hide a proof another arm makes: the ref
// cannot be read, but the closed bead's record matches the worktree.
func TestWorkLandedOnTargetAProvenArmWinsOverAFailedOne(t *testing.T) {
	t.Parallel()
	w := newLandedWorld(t)
	w.git.SetRef(t, w.repo, LandedRef("gt-abc"), w.later)
	repo := failing{WorkLandedRepo: w.worktree(), revLanded: errors.New("boom")}

	v := WorkLandedOnTarget(repo, WorkLandedQuery{Head: w.earlier, BeadID: "gt-abc", Issue: closedWithRecordAtLater(w)})
	if !v.Landed || v.Reason != WorkLandedByRecord {
		t.Fatalf("WorkLandedOnTarget() = %+v; want the record arm to prove it", v)
	}
}

// A repository that cannot be read at all is a failed probe, not a landed one.
func TestWorkLandedOnTargetWithoutARepositoryIsAFailedProbe(t *testing.T) {
	t.Parallel()
	v := WorkLandedOnTarget(nil, WorkLandedQuery{BeadID: "gt-abc"})
	if v.Landed || v.Reason != WorkProbeFailed {
		t.Fatalf("WorkLandedOnTarget(nil repo) = %+v; want %s", v, WorkProbeFailed)
	}
}

// TestWorkLandedOnTargetUsesTheRigDefaultBranch is the disagreeing-resolver
// case of gt-cmutl: the rig lands on its configured default branch, and its
// remote's HEAD still points at main. An empty target resolved from git alone
// asks about a branch the landing never touched.
func TestWorkLandedOnTargetUsesTheRigDefaultBranch(t *testing.T) {
	t.Parallel()
	g := gitfake.New()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	rigPath := filepath.Join(root, "rig")
	repo := filepath.Join(rigPath, "polecats", "opal", "rig")

	g.InitBare(t, origin)
	base := g.Commit(t, origin, "main", "main: seed", map[string]string{"a.txt": "one\n"})
	g.SetRef(t, origin, "refs/heads/"+landedFixtureBranch, base)
	head := g.Commit(t, origin, landedFixtureBranch, "feat: the work", map[string]string{"b.txt": "work\n"})
	// The landing squashed the work onto develop; main never took it.
	g.SetRef(t, origin, "refs/heads/develop", base)
	landed := g.Commit(t, origin, "develop", "feat: the work (squashed)", map[string]string{"b.txt": "work\n"})
	g.Clone(t, origin, repo)
	g.SetRef(t, repo, "refs/remotes/origin/develop", landed)

	// The rig config a `gt rig add --default-branch develop` writes.
	cfg := `{"type": "rig", "name": "rig", "default_branch": "develop"}`
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write rig config: %v", err)
	}
	issue := &beads.Issue{ID: "gt-abc", Status: "closed", Notes: LandingRecord{
		BeadID: "gt-abc", Head: head, LandedCommit: landed, Target: "develop",
	}.NoteBlock()}

	// Without the rig, the predicate can only ask git, and git answers "main":
	// the branch the landing never touched. This is the reading gt-cmutl
	// removes when the caller knows the rig.
	if v := WorkLandedOnTarget(g.Open(repo), WorkLandedQuery{Head: head, BeadID: "gt-abc"}); v.Landed {
		t.Fatalf("WorkLandedOnTarget(no rig, origin/HEAD on main) = %+v; want not landed", v)
	}

	v := WorkLandedOnTarget(g.Open(repo), WorkLandedQuery{Head: head, RigPath: rigPath, BeadID: "gt-abc"})
	if !v.Landed || v.Reason != WorkLandedByTarget {
		t.Fatalf("WorkLandedOnTarget(rig default develop) = %+v; want landed %s", v, WorkLandedByTarget)
	}

	// The closed bead's landing record goes on proving the work whichever
	// branch the target resolves to.
	if v := WorkLandedOnTarget(g.Open(repo), WorkLandedQuery{Head: head, RigPath: rigPath, BeadID: "gt-abc", Issue: issue}); !v.Landed {
		t.Fatalf("WorkLandedOnTarget(landing record on develop) = %+v; want landed", v)
	}
}

// TestWorkLandedOnTargetResolvesTheRigDefaultBranch pins what an empty target
// becomes once a caller names a rig: the config's default_branch, "main" when
// there is no config or it names no branch, and a failed probe when the config
// will not decode. origin's HEAD points at trunk throughout, so a rig-shaped
// caller that still fell back to the remote's HEAD would read the work on main
// as unlanded; no landing consults origin/HEAD, and neither may the predicate
// (gt-cmutl).
func TestWorkLandedOnTargetResolvesTheRigDefaultBranch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		config     string // "" writes no config.json
		wantLanded bool
		wantReason string
		wantDetail string
	}{
		{
			name:       "the config names the branch its landings target",
			config:     `{"type": "rig", "name": "rig", "default_branch": "main"}`,
			wantLanded: true,
			wantReason: WorkLandedByTarget,
		},
		{
			name:       "a rig with no config falls back to main",
			wantLanded: true,
			wantReason: WorkLandedByTarget,
		},
		{
			name:       "a config naming no branch falls back to main, not the remote HEAD",
			config:     `{"type": "rig", "name": "rig"}`,
			wantLanded: true,
			wantReason: WorkLandedByTarget,
		},
		{
			name:       "a config that will not decode fails closed",
			config:     `{"default_branch": `,
			wantReason: WorkProbeFailed,
			wantDetail: "resolve the rig's default branch",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := gitfake.New()
			root := t.TempDir()
			origin := filepath.Join(root, "origin.git")
			rigPath := filepath.Join(root, "rig")
			repo := filepath.Join(rigPath, "polecats", "opal", "rig")

			g.InitBare(t, origin)
			base := g.Commit(t, origin, "trunk", "trunk: seed", map[string]string{"a.txt": "one\n"})
			// The landing took the work onto main, which is not the remote's HEAD.
			g.SetRef(t, origin, "refs/heads/main", base)
			head := g.Commit(t, origin, "main", "feat: the work", map[string]string{"b.txt": "work\n"})
			g.SetHead(t, origin, "trunk")
			g.Clone(t, origin, repo)
			if tt.config != "" {
				if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(tt.config), 0o644); err != nil {
					t.Fatalf("write rig config: %v", err)
				}
			}

			v := WorkLandedOnTarget(g.Open(repo), WorkLandedQuery{Head: head, RigPath: rigPath})
			if v.Landed != tt.wantLanded || v.Reason != tt.wantReason {
				t.Fatalf("WorkLandedOnTarget() = %+v; want landed=%v reason %q", v, tt.wantLanded, tt.wantReason)
			}
			if !strings.Contains(v.Detail, tt.wantDetail) {
				t.Fatalf("detail = %q; want it to mention %q", v.Detail, tt.wantDetail)
			}
		})
	}
}

func closedWithRecord(head string) func(*landedWorld) *beads.Issue {
	return func(w *landedWorld) *beads.Issue {
		return &beads.Issue{ID: "gt-abc", Status: "closed", Notes: w.recordNote(head)}
	}
}

func closedWithRecordAtLater(w *landedWorld) *beads.Issue {
	return closedWithRecord(w.later)(w)
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
