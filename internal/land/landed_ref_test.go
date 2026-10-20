package land

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// landedRefRepo is a Repo that fails the writes and reads of refs/landed/*
// it is told to and passes everything else through, and notes how the ref was
// pushed.
type landedRefRepo struct {
	Repo
	failUpdate, failList, failPush bool
	// staleListing, when set, is what the remote is reported to hold at the
	// ref, however far it has moved since.
	staleListing string
	pushes       *[]string
}

func isLandedRef(s string) bool { return strings.Contains(s, "refs/landed/") }

func (r landedRefRepo) UpdateRef(ref, commit string) error {
	if r.failUpdate && isLandedRef(ref) {
		return errors.New("update-ref: unable to lock")
	}
	return r.Repo.UpdateRef(ref, commit)
}

func (r landedRefRepo) ListRemoteRefsWithHashes(remote, prefix string) ([]git.RemoteRef, error) {
	if isLandedRef(prefix) {
		if r.failList {
			return nil, errors.New("ls-remote: remote hung up")
		}
		if r.staleListing != "" {
			return []git.RemoteRef{{Hash: r.staleListing, Name: prefix}}, nil
		}
	}
	return r.Repo.ListRemoteRefsWithHashes(remote, prefix)
}

func (r landedRefRepo) Push(remote, refspec string, force bool) error {
	if isLandedRef(refspec) {
		if r.pushes != nil {
			*r.pushes = append(*r.pushes, refspec)
		}
		if force {
			return errors.New("refs/landed/ is never pushed with --force")
		}
		if r.failPush {
			return errors.New("push: remote: permission denied")
		}
	}
	return r.Repo.Push(remote, refspec, force)
}

// A landing whose refs/landed write fails is not recorded and its bead is not
// closed: nothing could answer by ancestry for the branch the landing worker
// reaps next. The merge is already on the target, so the next pass finishes the
// landing (completeUnrecordedLanding), writes the ref and closes the bead
// (gt-as9v8).
func TestLandRefWriteFailureLeavesTheLandingUnrecorded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		repo func(Repo) Repo
		want string
	}{
		{"local update-ref", func(g Repo) Repo { return landedRefRepo{Repo: g, failUpdate: true} }, "setting refs/landed/gt-abc"},
		{"remote read", func(g Repo) Repo { return landedRefRepo{Repo: g, failList: true} }, "reading refs/landed/gt-abc"},
		{"remote push", func(g Repo) Repo { return landedRefRepo{Repo: g, failPush: true} }, "pushing refs/landed/gt-abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			f.squashRange(t)
			l := f.lander()
			l.openRepo = func(dir string) Repo { return tc.repo(f.git.Open(dir)) }

			_, err := l.Land(context.Background(), f.work)
			var rec *RecordError
			if !errors.As(err, &rec) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Land = %T %v, want a RecordError naming %q", err, err, tc.want)
			}
			if b := f.bead(); b.Status == "closed" || strings.Contains(b.Notes, LandingNoteMarker) {
				t.Errorf("the bead was closed or recorded without its ref: status=%s notes=%q", b.Status, b.Notes)
			}
			if n := len(f.landingLines()); n != 0 {
				t.Errorf("landings lines = %d, want none: the file marks the bead done", n)
			}

			// The next pass, with the ref writable, finishes the landing.
			res, err := f.lander().Land(context.Background(), f.work)
			if err != nil {
				t.Fatalf("the retry did not finish the landing: %v", err)
			}
			if res.LandedCommit != f.originMain() {
				t.Errorf("the retry landed again: %s, origin main %s", res.LandedCommit, f.originMain())
			}
			if b := f.bead(); b.Status != "closed" {
				t.Errorf("bead after the retry: status=%s", b.Status)
			}
			for _, dir := range []string{f.repo, f.origin} {
				if got := f.refAt(dir, LandedRef("gt-abc")); got != f.work.Head {
					t.Errorf("%s in %s = %q, want %q", LandedRef("gt-abc"), dir, got, f.work.Head)
				}
			}
		})
	}
}

// A record that names no head is a caller bug: it is an error, never a landing
// recorded without a ref.
func TestWriteLandedRefRefusesAnEmptyHead(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	err := f.lander().writeLandedRef(f.git.Open(f.repo), LandingRecord{BeadID: "gt-abc"})
	if err == nil || !strings.Contains(err.Error(), "names no head") {
		t.Fatalf("writeLandedRef with no head = %v, want an error", err)
	}
	if got := f.refAt(f.repo, LandedRef("gt-abc")); got != "" {
		t.Errorf("an empty head left %s at %q", LandedRef("gt-abc"), got)
	}
}

// The first write of the ref is a plain push, never forced. A re-landing finds
// the remote holding the first head, says so in the log and moves the ref with
// a lease on that value; a ref the remote moved after it was read is refused,
// not overwritten.
func TestLandedRefPushIsLeasedNotForced(t *testing.T) {
	t.Parallel()
	ref := LandedRef("gt-abc")

	t.Run("first landing pushes plainly", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		var pushes []string
		l := f.lander()
		l.openRepo = func(dir string) Repo { return landedRefRepo{Repo: f.git.Open(dir), pushes: &pushes} }
		if _, err := l.Land(context.Background(), f.work); err != nil {
			t.Fatalf("Land: %v", err)
		}
		if len(pushes) != 1 || pushes[0] != f.work.Head+":"+ref {
			t.Errorf("refs/landed pushes = %v, want one plain push of the head", pushes)
		}
	})

	t.Run("a re-landing moves the ref under a lease and logs it", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		first := f.work.Head
		var log strings.Builder
		land := func() {
			t.Helper()
			l := f.lander()
			l.Out = &log
			if _, err := l.Land(context.Background(), f.work); err != nil {
				t.Fatalf("Land: %v", err)
			}
		}
		land()
		f.base = f.originMain()
		f.git.SetRef(t, f.origin, "refs/heads/"+fixtureBranch, f.base)
		second := f.git.Commit(t, f.origin, fixtureBranch, "feat: add c", map[string]string{"c.txt": "more\n"})
		f.work.Head = second
		f.ready(second)
		land()
		want := "repairing " + ref + " on origin from " + shortSHA(first) + " to " + shortSHA(second)
		if !strings.Contains(log.String(), want) {
			t.Errorf("landing log lacks %q:\n%s", want, log.String())
		}
		if got := f.refAt(f.origin, ref); got != second {
			t.Errorf("%s on origin = %q, want %q", ref, got, second)
		}
	})

	t.Run("a ref moved after it was read is not overwritten", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		other := f.git.Commit(t, f.origin, "polecat/other", "someone else", map[string]string{"o.txt": "o\n"})
		f.git.SetRef(t, f.origin, ref, other)
		l := f.lander()
		l.openRepo = func(dir string) Repo {
			// The listing reports a value the remote no longer holds, so the
			// lease the push carries is stale.
			return landedRefRepo{Repo: f.git.Open(dir), staleListing: f.base}
		}
		_, err := l.Land(context.Background(), f.work)
		var rec *RecordError
		if !errors.As(err, &rec) || !strings.Contains(err.Error(), "moving "+ref) {
			t.Fatalf("Land = %T %v, want a RecordError for the refused lease", err, err)
		}
		if got := f.refAt(f.origin, ref); got != other {
			t.Errorf("%s on origin = %q, want the other writer's %q left alone", ref, got, other)
		}
	})
}
