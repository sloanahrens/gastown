package censustest_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/censustest"
	"github.com/steveyegge/gastown/internal/testutil"
)

// recordingTB stands in for *testing.T so a test can drive AssertBudget's
// failure branch: it keeps the messages instead of failing the test.
type recordingTB struct {
	testing.TB
	failures []string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// TestMain runs the helper's tests under the hermetic harness (gt-lwi): the
// fake answers bd in-process, and the poisoned Dolt ports refuse anything that
// tries to reach a real one.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

// listOpen is one bd call through the fake.
func listOpen(t *testing.T, store *beads.Beads) {
	t.Helper()
	if _, err := store.List(beads.ListOptions{Status: "open"}); err != nil {
		t.Fatalf("list: %v", err)
	}
}

// TestFakeBDAnswersInProcess: the store's call reaches the answer, not a bd
// binary, and the recorder holds the caller's own argv.
//
//testpolicy:allow parallel — a call made beside another test's would be counted in the other test's census
func TestFakeBDAnswersInProcess(t *testing.T) {
	var answered int
	store, rec := censustest.FakeBD(t.TempDir(), func(argv []string) ([]byte, error) {
		answered++
		return []byte("[]"), nil
	})

	listOpen(t, store)

	if answered != 1 {
		t.Errorf("answer ran %d times, want 1", answered)
	}
	argvs := rec.Argvs()
	if len(argvs) != 1 || argvs[0] != "list --json --status=open --limit=0 --flat" {
		t.Errorf("recorded %q, want the caller's one list call", argvs)
	}
}

// TestCheckBudgetPassesAtTheBudget: a function that makes exactly its budget
// passes.
//
//testpolicy:allow parallel — the census counters are process-wide, so a parallel test's calls would land in this assertion
func TestCheckBudgetPassesAtTheBudget(t *testing.T) {
	store, _ := censustest.FakeBD(t.TempDir(), nil)

	err := censustest.CheckBudget(2, func() {
		listOpen(t, store)
		listOpen(t, store)
	})

	if err != nil {
		t.Errorf("two calls against a budget of two: %v", err)
	}
}

// TestCheckBudgetFailsOneCallTooMany: the third call against a budget of two
// fails the assertion, naming both numbers.
//
//testpolicy:allow parallel — the census counters are process-wide, so a parallel test's calls would land in this assertion
func TestCheckBudgetFailsOneCallTooMany(t *testing.T) {
	store, _ := censustest.FakeBD(t.TempDir(), nil)

	err := censustest.CheckBudget(2, func() {
		listOpen(t, store)
		listOpen(t, store)
		listOpen(t, store)
	})

	if err == nil {
		t.Fatal("three calls against a budget of two passed")
	}
	for _, want := range []string{"3", "2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must name %s", err, want)
		}
	}
}

// TestAssertBudgetReportsOneCallTooMany: the assertion a package calls reports
// the over-budget call to its testing.TB, naming the count and the budget,
// while an under-budget call reports nothing (gt-gojo0).
//
//testpolicy:allow parallel — the census counters are process-wide, so a parallel test's calls would land in this assertion
func TestAssertBudgetReportsOneCallTooMany(t *testing.T) {
	store, _ := censustest.FakeBD(t.TempDir(), nil)

	ok := &recordingTB{}
	censustest.AssertBudget(ok, 2, func() {
		listOpen(t, store)
		listOpen(t, store)
	})
	if len(ok.failures) != 0 {
		t.Fatalf("two calls against a budget of two reported %q", ok.failures)
	}

	over := &recordingTB{}
	censustest.AssertBudget(over, 2, func() {
		listOpen(t, store)
		listOpen(t, store)
		listOpen(t, store)
	})
	if len(over.failures) != 1 {
		t.Fatalf("three calls against a budget of two reported %d failures, want 1: %q", len(over.failures), over.failures)
	}
	for _, want := range []string{"3", "2"} {
		if !strings.Contains(over.failures[0], want) {
			t.Errorf("failure %q must name %s", over.failures[0], want)
		}
	}
}

// TestBudgetExcludesTheCapabilityProbe: the store probes the fake bd for
// --allow-stale before every call, and that probe is not a call the caller
// made, so a budget of one holds one verb (gt-gojo0).
//
//testpolicy:allow parallel — the census counters are process-wide, so a parallel test's calls would land in this assertion
func TestBudgetExcludesTheCapabilityProbe(t *testing.T) {
	store, rec := censustest.FakeBD(t.TempDir(), nil)

	var counted int
	if err := censustest.CheckBudget(1, func() {
		listOpen(t, store)
		counted = beads.CensusTotal()
	}); err != nil {
		t.Fatalf("one call against a budget of one: %v (the capability probe was counted)", err)
	}

	if counted != 1 {
		t.Errorf("the census counted %d calls, want 1", counted)
	}
	if argvs := rec.Argvs(); len(argvs) != 1 {
		t.Errorf("the recorder saw %d calls, want only the caller's: %q", len(argvs), argvs)
	}
}
