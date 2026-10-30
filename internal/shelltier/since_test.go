package shelltier

import (
	"bytes"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/tiertrigger"
)

// memFiles is tiertrigger.Files over a map, so no case touches the disk.
type memFiles map[string][]byte

func (m memFiles) ReadFile(path string) ([]byte, error) {
	data, ok := m[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return data, nil
}

func (m memFiles) WriteFile(path string, data []byte) error {
	m[path] = data
	return nil
}

func (m memFiles) RemoveFile(path string) error {
	delete(m, path)
	return nil
}

// failingWrites is a Files whose writes fail, so a save error can be driven.
type failingWrites struct{ memFiles }

func (failingWrites) WriteFile(string, []byte) error { return errors.New("disk full") }

const (
	oldCommit  = "1111111111111111111111111111111111111111"
	headCommit = "2222222222222222222222222222222222222222"
	town       = "/town"
)

// sinceCheckout is a Checkout whose HEAD is head and whose diff is g.
func sinceCheckout(head string, g tiertrigger.Git) Checkout {
	return Checkout{Head: func() (string, error) { return head, nil }, Git: g}
}

func utcZone() (*time.Location, error) { return time.UTC, nil }

// seedState writes st to the shell state file under town.
func seedState(t *testing.T, files memFiles, st tiertrigger.State) {
	t.Helper()
	if err := (tiertrigger.Store{Path: StatePath(town), Files: files}).Save(st); err != nil {
		t.Fatalf("seed state: %v", err)
	}
}

// loadState reads the shell state file under town back.
func loadState(t *testing.T, files tiertrigger.Files) tiertrigger.State {
	t.Helper()
	st, err := tiertrigger.Store{Path: StatePath(town), Files: files}.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	return st
}

func TestSinceGreenRunsOnlyTheSelectedEntries(t *testing.T) {
	t.Parallel()
	a := noRunScript("scripts/a_test.sh")
	b := noRunScript("scripts/b_test.sh")
	files := memFiles{}
	st := tiertrigger.State{}
	st.Ran(a.Path, oldCommit, utcAt(29, 9, 0))
	seedState(t, files, st)

	var errw bytes.Buffer
	plan := PlanSinceGreen([]Entry{a, b}, town, files, sinceCheckout(headCommit, fakeGit{}), utcZone, utcAt(29, 12, 0), &errw)
	if got := paths(plan.Run); !slices.Equal(got, []string{b.Path}) {
		t.Fatalf("run = %v, want only %s", got, b.Path)
	}
	if plan.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", plan.Skipped)
	}
	if errw.Len() != 0 {
		t.Fatalf("error stream = %q, want nothing", errw.String())
	}
}

func TestSinceGreenRecordsOnlyPassedEntries(t *testing.T) {
	t.Parallel()
	a := noRunScript("scripts/a_test.sh")
	b := noRunScript("scripts/b_test.sh")
	files := memFiles{}
	st := tiertrigger.State{}
	st.Ran(b.Path, oldCommit, utcAt(28, 9, 0))
	seedState(t, files, st)

	var errw bytes.Buffer
	plan := PlanSinceGreen([]Entry{a, b}, town, files, sinceCheckout(headCommit, fakeGit{}), utcZone, utcAt(29, 12, 0), &errw)
	plan.Finish(Result{Passed: 1, Failed: 1, FailedNames: []string{b.Path}}, utcAt(29, 12, 5), &errw)

	got := loadState(t, files)
	if rec, ok := got[a.Path]; !ok || rec.Commit != headCommit {
		t.Fatalf("record for %s = %+v, want the green run at %s", a.Path, rec, headCommit)
	}
	if rec := got[b.Path]; rec.Commit != oldCommit {
		t.Fatalf("record for failed %s = %q, want its old record %s", b.Path, rec.Commit, oldCommit)
	}
}

func TestSinceGreenFailsOpenOnAnUnreadableState(t *testing.T) {
	t.Parallel()
	a := noRunScript("scripts/a_test.sh")
	b := noRunScript("scripts/b_test.sh")
	corrupt := []byte("{not json")
	files := memFiles{StatePath(town): corrupt}

	var errw bytes.Buffer
	plan := PlanSinceGreen([]Entry{a, b}, town, files, sinceCheckout(headCommit, fakeGit{}), utcZone, utcAt(29, 12, 0), &errw)
	if got := paths(plan.Run); !slices.Equal(got, []string{a.Path, b.Path}) {
		t.Fatalf("run = %v, want every entry", got)
	}
	if plan.Skipped != 0 {
		t.Fatalf("skipped = %d, want 0", plan.Skipped)
	}
	if !strings.Contains(errw.String(), "read the shell state") {
		t.Fatalf("error stream = %q, want it to say the state is unreadable", errw.String())
	}

	plan.Finish(Result{Passed: 2}, utcAt(29, 12, 5), &errw)
	if got := files[StatePath(town)]; !bytes.Equal(got, corrupt) {
		t.Fatalf("state file = %q, want the unreadable file left as it was", got)
	}
}

func TestSinceGreenUnreadableHeadRunsEverythingRecordsNothing(t *testing.T) {
	t.Parallel()
	a := noRunScript("scripts/a_test.sh")
	b := noRunScript("scripts/b_test.sh")
	files := memFiles{}
	st := tiertrigger.State{}
	st.Ran(a.Path, oldCommit, utcAt(29, 9, 0))
	seedState(t, files, st)
	before := files[StatePath(town)]

	headErr := errors.New("rev-parse: not a git repository")
	co := Checkout{Head: func() (string, error) { return "", headErr }, Git: fakeGit{}}
	var errw bytes.Buffer
	plan := PlanSinceGreen([]Entry{a, b}, town, files, co, utcZone, utcAt(29, 12, 0), &errw)
	if got := paths(plan.Run); !slices.Equal(got, []string{a.Path, b.Path}) {
		t.Fatalf("run = %v, want every entry", got)
	}
	if !strings.Contains(errw.String(), "read HEAD") || !strings.Contains(errw.String(), headErr.Error()) {
		t.Fatalf("error stream = %q, want the HEAD failure named", errw.String())
	}

	plan.Finish(Result{Passed: 2}, utcAt(29, 12, 5), &errw)
	if got := files[StatePath(town)]; !bytes.Equal(got, before) {
		t.Fatalf("state file changed to %q, want it untouched", got)
	}
}

func TestSinceGreenNamesAnEntryWhoseRecordCannotBeRead(t *testing.T) {
	t.Parallel()
	a := noRunScript("scripts/a_test.sh")
	files := memFiles{}
	st := tiertrigger.State{}
	st.Ran(a.Path, oldCommit, utcAt(29, 9, 0))
	seedState(t, files, st)

	diffErr := errors.New("bad object")
	var errw bytes.Buffer
	plan := PlanSinceGreen([]Entry{a}, town, files, sinceCheckout(headCommit, fakeGit{err: diffErr}), utcZone, utcAt(29, 12, 0), &errw)
	if got := paths(plan.Run); !slices.Equal(got, []string{a.Path}) {
		t.Fatalf("run = %v, want the entry run", got)
	}
	if !strings.Contains(errw.String(), a.Path) || !strings.Contains(errw.String(), "running it") {
		t.Fatalf("error stream = %q, want %s named as run anyway", errw.String(), a.Path)
	}
}

func TestSinceGreenSaysWhenTheStateCannotBeSaved(t *testing.T) {
	t.Parallel()
	a := noRunScript("scripts/a_test.sh")
	files := failingWrites{memFiles{}}
	var errw bytes.Buffer
	plan := PlanSinceGreen([]Entry{a}, town, files, sinceCheckout(headCommit, fakeGit{}), utcZone, utcAt(29, 12, 0), &errw)
	plan.Finish(Result{Passed: 1}, utcAt(29, 12, 5), &errw)
	if !strings.Contains(errw.String(), "save the shell state") || !strings.Contains(errw.String(), "disk full") {
		t.Fatalf("error stream = %q, want the save failure named", errw.String())
	}
}

func TestSinceGreenSkipsTheSaveWhenNothingPassed(t *testing.T) {
	t.Parallel()
	a := noRunScript("scripts/a_test.sh")
	files := memFiles{}
	var errw bytes.Buffer
	plan := PlanSinceGreen([]Entry{a}, town, files, sinceCheckout(headCommit, fakeGit{}), utcZone, utcAt(29, 12, 0), &errw)
	plan.Finish(Result{Failed: 1, FailedNames: []string{a.Path}}, utcAt(29, 12, 5), &errw)
	if _, ok := files[StatePath(town)]; ok {
		t.Fatal("state file was written although no entry passed")
	}
}

func TestAllPlanRunsEverythingAndRecordsNothing(t *testing.T) {
	t.Parallel()
	a := noRunScript("scripts/a_test.sh")
	plan := AllEntries([]Entry{a})
	if got := paths(plan.Run); !slices.Equal(got, []string{a.Path}) {
		t.Fatalf("run = %v, want %s", got, a.Path)
	}
	if plan.Skipped != 0 {
		t.Fatalf("skipped = %d, want 0", plan.Skipped)
	}
	plan.Finish(Result{Passed: 1}, utcAt(29, 12, 5), &bytes.Buffer{})
}
