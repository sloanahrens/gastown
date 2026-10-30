package shelltier

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/shelltier/shelltiertest"
	"github.com/steveyegge/gastown/internal/tiertrigger"
)

type memFiles = shelltiertest.MemFiles

// failingWrites is a Files whose writes fail, so a save error can be driven.
type failingWrites struct{ memFiles }

func (failingWrites) WriteFile(string, []byte) error { return errors.New("disk full") }

const (
	oldCommit  = "1111111111111111111111111111111111111111"
	headCommit = shelltiertest.HeadCommit
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

func TestSinceGreenRecordsNothingWhenTheResultDoesNotAccountForEveryEntry(t *testing.T) {
	t.Parallel()
	a := noRunScript("scripts/a_test.sh")
	b := noRunScript("scripts/b_test.sh")
	tests := []struct {
		name string
		res  Result
	}{
		{"an entry neither passed nor failed", Result{Passed: 1}},
		{"a failed name outside the run", Result{Passed: 1, Failed: 1, FailedNames: []string{"scripts/x_test.sh"}}},
		{"a failed name listed twice", Result{Failed: 2, FailedNames: []string{a.Path, a.Path}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := memFiles{}
			var errw bytes.Buffer
			plan := PlanSinceGreen([]Entry{a, b}, town, files, sinceCheckout(headCommit, fakeGit{}), utcZone, utcAt(29, 12, 0), &errw)
			plan.Finish(tc.res, utcAt(29, 12, 5), &errw)
			if _, ok := files[StatePath(town)]; ok {
				t.Fatal("state file was written from a result that does not account for every entry")
			}
			if !strings.Contains(errw.String(), "does not account for every entry") {
				t.Fatalf("error stream = %q, want the inconsistent result named", errw.String())
			}
		})
	}
}

func TestSinceGreenDoesNotRecordAnEntryThatTimedOut(t *testing.T) {
	t.Parallel()
	a := noRunScript("scripts/a_test.sh")
	b := noRunScript("scripts/b_test.sh")
	files := memFiles{}
	var errw bytes.Buffer
	plan := PlanSinceGreen([]Entry{a, b}, town, files, sinceCheckout(headCommit, fakeGit{}), utcZone, utcAt(29, 12, 0), &errw)

	// The second script's budget has already run out, so its context is done
	// when Exec returns 0: the runner counts it as timed out, not passed.
	calls := 0
	withTimeout := func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		calls++
		if calls == 2 {
			c, cancel := context.WithCancel(ctx)
			cancel()
			return c, cancel
		}
		return context.WithCancel(ctx)
	}
	res := Run(context.Background(), Config{
		Entries:     plan.Run,
		Root:        ".",
		LogDir:      "/logs",
		Budget:      time.Minute,
		Exec:        func(context.Context, Command) (int, error) { return 0, nil },
		Now:         func() time.Time { return utcAt(29, 12, 5) },
		WithTimeout: withTimeout,
	})
	if res.Passed != 1 || res.TimedOut != 1 {
		t.Fatalf("result = %+v, want one pass and one timeout", res)
	}
	plan.Finish(res, utcAt(29, 12, 5), &errw)

	got := loadState(t, files)
	if rec, ok := got[a.Path]; !ok || rec.Commit != headCommit {
		t.Fatalf("record for the passing %s = %+v, want commit %s", a.Path, rec, headCommit)
	}
	if _, ok := got[b.Path]; ok {
		t.Fatalf("the timed-out %s was recorded as green", b.Path)
	}
}

func TestSinceGreenKeepsRecordsWrittenWhileTheRunWent(t *testing.T) {
	t.Parallel()
	a := noRunScript("scripts/a_test.sh")
	b := noRunScript("scripts/b_test.sh")
	const otherCommit = "3333333333333333333333333333333333333333"
	tests := []struct {
		name    string
		written string
		at      time.Time
		wantA   string
	}{
		{"a record for another entry is kept", b.Path, utcAt(29, 12, 2), headCommit},
		{"a newer record for this entry wins", a.Path, utcAt(29, 12, 10), otherCommit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := memFiles{}
			var errw bytes.Buffer
			plan := PlanSinceGreen([]Entry{a}, town, files, sinceCheckout(headCommit, fakeGit{}), utcZone, utcAt(29, 12, 0), &errw)

			// Another run records tc.written after this plan read the state.
			other := tiertrigger.State{}
			other.Ran(tc.written, otherCommit, tc.at)
			seedState(t, files, other)

			plan.Finish(Result{Passed: 1}, utcAt(29, 12, 5), &errw)
			got := loadState(t, files)
			if rec := got[tc.written]; rec.Commit != otherCommit {
				t.Fatalf("record for %s = %q, want the other run's %s kept", tc.written, rec.Commit, otherCommit)
			}
			if rec := got[a.Path]; rec.Commit != tc.wantA {
				t.Fatalf("record for %s = %q, want %s", a.Path, rec.Commit, tc.wantA)
			}
		})
	}
}

func TestSinceGreenRecordsNothingWhenTheStateCannotBeReread(t *testing.T) {
	t.Parallel()
	a := noRunScript("scripts/a_test.sh")
	files := memFiles{}
	var errw bytes.Buffer
	plan := PlanSinceGreen([]Entry{a}, town, files, sinceCheckout(headCommit, fakeGit{}), utcZone, utcAt(29, 12, 0), &errw)

	// The state file is corrupted while the run goes on: it must be left alone.
	corrupt := []byte("{not json")
	files[StatePath(town)] = corrupt
	plan.Finish(Result{Passed: 1}, utcAt(29, 12, 5), &errw)
	if got := files[StatePath(town)]; !bytes.Equal(got, corrupt) {
		t.Fatalf("state file = %q, want the unreadable file left as it was", got)
	}
	if !strings.Contains(errw.String(), "re-read the shell state") {
		t.Fatalf("error stream = %q, want the re-read failure named", errw.String())
	}
}
