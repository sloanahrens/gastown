package land

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// TestPrecheckCarInfraErrorsEndThePass: a check before the merge that cannot
// read its input ends the pass with the error for that stage, and lands
// nothing. A work with no target is not ready and is refused before any read.
func TestPrecheckCarInfraErrorsEndThePass(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		fault func(t *testing.T, f *landFixture, l *Lander, w *Work)
		// stage is the InfraError stage the pass ends on; "" means ErrNotReady.
		stage string
	}{
		{"a work with no target is not ready", func(_ *testing.T, _ *landFixture, _ *Lander, w *Work) {
			w.Target = ""
		}, ""},
		{"an unreadable bead ends the pass", func(_ *testing.T, f *landFixture, l *Lander, _ *Work) {
			l.Beads = showFailsBeads{Beads: f.bd}
		}, "read bead"},
		{"an unreadable landings file ends the pass", func(t *testing.T, _ *landFixture, l *Lander, _ *Work) {
			blocker := filepath.Join(t.TempDir(), "not-a-directory")
			if err := os.WriteFile(blocker, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			l.Landings = &LandingsFile{Path: filepath.Join(blocker, "landings.jsonl")}
		}, "read landings file"},
		{"an unresolvable target ends the pass", func(_ *testing.T, f *landFixture, l *Lander, _ *Work) {
			l.openRepo = func(dir string) Repo { return revFailsRepo{Repo: f.git.Open(dir)} }
		}, "resolve target"},
		{"a failed ancestry read ends the pass", func(_ *testing.T, f *landFixture, l *Lander, _ *Work) {
			l.openRepo = func(dir string) Repo { return ancestryFailsRepo{Repo: f.git.Open(dir)} }
		}, "ancestry"},
		{"a failed range check ends the pass", func(_ *testing.T, _ *landFixture, l *Lander, _ *Work) {
			l.RangeChecks = []RangeCheck{func(CommitReader, string, string) (*Rejection, error) {
				return nil, errors.New("git log: transient failure")
			}}
		}, "range check"},
		{"a failed ancestry read of the target ends the pass", func(_ *testing.T, f *landFixture, l *Lander, _ *Work) {
			l.openRepo = func(dir string) Repo { return targetAncestryFailsRepo{Repo: f.git.Open(dir), target: f.base} }
		}, "ancestry"},
		{"a tree comparison that fails ends the pass", func(_ *testing.T, f *landFixture, l *Lander, _ *Work) {
			l.openRepo = func(dir string) Repo { return treesFailsRepo{Repo: f.git.Open(dir)} }
		}, "compare trees"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			l := f.lander()
			w := f.work
			tc.fault(t, f, l, &w)
			_, err := l.Land(context.Background(), w)
			if tc.stage == "" {
				if !errors.Is(err, ErrNotReady) {
					t.Fatalf("Land = %v, want ErrNotReady", err)
				}
			} else {
				var infra *InfraError
				if !errors.As(err, &infra) || infra.Stage != tc.stage {
					t.Fatalf("Land = %v, want an InfraError at stage %q", err, tc.stage)
				}
			}
			if got := f.originMain(); got != f.base {
				t.Errorf("origin/main moved to %s; the pass must land nothing", got)
			}
		})
	}
}

// TestPrecheckCarRepairThatCannotRecordEndsWithItsResult: a landing whose close
// keeps failing is recorded, and the next pass repairs it without landing it
// again. When that repair cannot record either, the pass ends with the
// RecordError that carries the landed commit.
func TestPrecheckCarRepairThatCannotRecordEndsWithItsResult(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	l.Beads = closeFailsBeads{Fake: f.bd}
	_, err := l.Land(context.Background(), f.work)
	var rec *RecordError
	if !errors.As(err, &rec) || rec.Result.LandedCommit != f.originMain() {
		t.Fatalf("first Land = %v, want a RecordError for the landed %s", err, f.originMain())
	}
	_, err = l.Land(context.Background(), f.work)
	if !errors.As(err, &rec) || rec.Result.LandedCommit != f.originMain() {
		t.Fatalf("repair Land = %v, want a RecordError for the landed %s", err, f.originMain())
	}
	if n := len(f.landingLines()); n != 1 {
		t.Errorf("landings lines = %d, want 1", n)
	}
}

// TestPrecheckCarUnreadableRiskPathsLandUnlabelled: a risk-path file that cannot
// be read is logged and the landing goes on unlabelled. It neither refuses the
// landing nor stops the pass (gt-vsct7.4).
func TestPrecheckCarUnreadableRiskPathsLandUnlabelled(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	var out bytes.Buffer
	l := f.lander()
	l.Out = &out
	l.openRepo = func(dir string) Repo { return riskPathsUnreadableRepo{Repo: f.git.Open(dir)} }
	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land = %v; an unreadable risk-path file must not stop the landing", err)
	}
	if len(res.RiskPaths) != 0 || res.LandedCommit != f.originMain() {
		t.Errorf("result = %+v with origin/main at %s; want an unlabelled landing", res, f.originMain())
	}
	if !strings.Contains(out.String(), "reading risk paths") {
		t.Errorf("log = %q; want the risk-path read failure", out.String())
	}
}

// showFailsBeads fails the bead read the pass opens with.
type showFailsBeads struct{ Beads }

func (showFailsBeads) Show(string) (*beads.Issue, error) {
	return nil, errors.New("bd show: database is locked")
}

// closeFailsBeads fails every close, so a landing is recorded but never closed.
type closeFailsBeads struct{ *beadsfake.Fake }

func (closeFailsBeads) ForceCloseWithReason(string, ...string) error {
	return errors.New("database is locked")
}

// revFailsRepo fails every ref resolution.
type revFailsRepo struct{ Repo }

func (revFailsRepo) Rev(string) (string, error) {
	return "", errors.New("git rev-parse: unknown revision")
}

// targetAncestryFailsRepo fails the ancestry read of the target tip, the read
// the pass makes once the head is on origin.
type targetAncestryFailsRepo struct {
	Repo
	target string
}

func (r targetAncestryFailsRepo) IsAncestor(ancestor, descendant string) (bool, error) {
	if descendant == r.target {
		return false, errors.New("git merge-base: transient failure")
	}
	return r.Repo.IsAncestor(ancestor, descendant)
}

// treesFailsRepo fails the tree comparison the merge is checked by.
type treesFailsRepo struct{ Repo }

func (treesFailsRepo) TreesIdentical(string, string) (bool, error) {
	return false, errors.New("git diff-tree: transient failure")
}

// riskPathsUnreadableRepo fails the read of the risk-path file and passes every
// other read through.
type riskPathsUnreadableRepo struct{ Repo }

func (r riskPathsUnreadableRepo) ShowFileAtRev(ref, path string) (string, error) {
	if path == RiskPathsFile {
		return "", errors.New("git show: unable to read the risk-path file")
	}
	return r.Repo.ShowFileAtRev(ref, path)
}
