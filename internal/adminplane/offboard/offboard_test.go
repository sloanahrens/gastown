package offboard

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// closedAt is the instant every test's workspace closes, far enough from the
// wall clock that a call the package makes to time.Now instead is visible.
var closedAt = time.Date(2026, 10, 31, 9, 30, 0, 0, time.UTC)

// wantRetention is the 30 days the decision sets, written out so the package's
// constant cannot drift without a test going red (claude-29a.11).
const wantRetention = 30 * 24 * time.Hour

// sameStep reports whether a and b are one step. Due and Ends are compared with
// Equal rather than ==, which also weighs the time's location and monotonic
// reading and so calls two names for one instant unequal.
func sameStep(a, b Step) bool {
	return a.Action == b.Action && a.Kind == b.Kind && a.Due.Equal(b.Due) && a.Ends.Equal(b.Ends)
}

// fullInventory lists every kind, in an order that is not the plan's, so a plan
// that only preserved the input order would fail the order check.
func fullInventory() Inventory {
	return Inventory{Kinds: []Kind{Town, Keys, NodePool, Repos, Caches, Tracker}}
}

// mustPlan returns the plan for inv or fails the test.
func mustPlan(t *testing.T, inv Inventory) []Step {
	t.Helper()
	plan, err := Plan(closedAt, inv)
	if err != nil {
		t.Fatalf("Plan(%+v): %v", inv, err)
	}
	return plan
}

// find returns the one step with action and kind, or fails the test.
func find(t *testing.T, plan []Step, action Action, kind Kind) Step {
	t.Helper()
	for _, step := range plan {
		if step.Action == action && step.Kind == kind {
			return step
		}
	}
	t.Fatalf("plan has no %s step for %q: %+v", action, kind, plan)
	return Step{}
}

// TestPlanSchedule is the acceptance test for the schedule: the keys are due at
// the close, the export offer ends 30 days later, every other kind is deleted
// then, and the plan is in one fixed order.
func TestPlanSchedule(t *testing.T) {
	t.Parallel()
	plan := mustPlan(t, fullInventory())
	ends := closedAt.Add(wantRetention)

	t.Run("the keys are due at the close", func(t *testing.T) {
		t.Parallel()
		step := find(t, plan, DeleteKeys, Keys)
		if !step.Due.Equal(closedAt) {
			t.Errorf("delete-keys due %v, want the close %v", step.Due, closedAt)
		}
		if !step.Ends.IsZero() {
			t.Errorf("delete-keys carries window end %v, want none", step.Ends)
		}
	})

	t.Run("the export offer ends at 30 days", func(t *testing.T) {
		t.Parallel()
		step := find(t, plan, OfferExport, Repos)
		if !step.Due.Equal(closedAt) {
			t.Errorf("offer-export due %v, want the close %v", step.Due, closedAt)
		}
		if !step.Ends.Equal(ends) {
			t.Errorf("offer-export ends %v, want %v", step.Ends, ends)
		}
	})

	t.Run("every other kind is deleted at 30 days", func(t *testing.T) {
		t.Parallel()
		for _, kind := range []Kind{Repos, Tracker, Caches, NodePool, Town} {
			step := find(t, plan, DeleteData, kind)
			if !step.Due.Equal(ends) {
				t.Errorf("delete-data %q due %v, want %v", kind, step.Due, ends)
			}
		}
	})

	t.Run("the plan is ordered by due time, then by kind", func(t *testing.T) {
		t.Parallel()
		want := []Step{
			{Action: OfferExport, Kind: Repos, Due: closedAt, Ends: ends},
			{Action: DeleteKeys, Kind: Keys, Due: closedAt},
			{Action: DeleteData, Kind: Repos, Due: ends},
			{Action: DeleteData, Kind: Tracker, Due: ends},
			{Action: DeleteData, Kind: Caches, Due: ends},
			{Action: DeleteData, Kind: NodePool, Due: ends},
			{Action: DeleteData, Kind: Town, Due: ends},
		}
		if !slices.EqualFunc(plan, want, sameStep) {
			t.Errorf("Plan() = %+v, want %+v", plan, want)
		}
	})
}

// TestPlanOffersTrackerExportWithoutRepos: the export carries the tracker dump
// too, so a workspace with no repos still gets an offer, naming the tracker.
func TestPlanOffersTrackerExportWithoutRepos(t *testing.T) {
	t.Parallel()
	plan := mustPlan(t, Inventory{Kinds: []Kind{Tracker, Keys}})
	want := []Step{
		{Action: OfferExport, Kind: Tracker, Due: closedAt, Ends: closedAt.Add(wantRetention)},
		{Action: DeleteKeys, Kind: Keys, Due: closedAt},
		{Action: DeleteData, Kind: Tracker, Due: closedAt.Add(wantRetention)},
	}
	if !slices.EqualFunc(plan, want, sameStep) {
		t.Errorf("Plan() = %+v, want %+v", plan, want)
	}
}

// TestPlanWithoutExportKind: the export names the repos or the tracker, so a
// workspace holding neither gets no offer, and its keys and data are scheduled
// like every other kind (gt-cswgv).
func TestPlanWithoutExportKind(t *testing.T) {
	t.Parallel()
	plan := mustPlan(t, Inventory{Kinds: []Kind{Keys, Caches}})
	want := []Step{
		{Action: DeleteKeys, Kind: Keys, Due: closedAt},
		{Action: DeleteData, Kind: Caches, Due: closedAt.Add(wantRetention)},
	}
	if !slices.EqualFunc(plan, want, sameStep) {
		t.Errorf("Plan() = %+v, want %+v", plan, want)
	}
}

// TestPlanListsEachKindOnce: a kind listed twice yields one step per action, so
// no resource is deleted twice.
func TestPlanListsEachKindOnce(t *testing.T) {
	t.Parallel()
	plan := mustPlan(t, Inventory{Kinds: []Kind{Repos, Repos, Keys}})
	want := []Step{
		{Action: OfferExport, Kind: Repos, Due: closedAt, Ends: closedAt.Add(wantRetention)},
		{Action: DeleteKeys, Kind: Keys, Due: closedAt},
		{Action: DeleteData, Kind: Repos, Due: closedAt.Add(wantRetention)},
	}
	if !slices.EqualFunc(plan, want, sameStep) {
		t.Errorf("Plan() = %+v, want %+v", plan, want)
	}
}

// TestPlanRefusesUnknownKind is the acceptance test for the refusal: a kind the
// schedule does not know returns an error and no plan, and the error names it.
func TestPlanRefusesUnknownKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		kinds []Kind
	}{
		{"alone", []Kind{"disks"}},
		{"among known kinds", []Kind{Repos, "disks", Keys}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := Plan(closedAt, Inventory{Kinds: tc.kinds})
			if !errors.Is(err, ErrUnknownKind) {
				t.Fatalf("Plan(%q) error = %v, want ErrUnknownKind", tc.kinds, err)
			}
			if plan != nil {
				t.Errorf("Plan(%q) returned steps %+v with the refusal, want none", tc.kinds, plan)
			}
			if !strings.Contains(err.Error(), "disks") {
				t.Errorf("refusal %q does not name the unknown kind", err)
			}
		})
	}
}

// TestDue is the acceptance test for Due: nothing is due before the close, the
// keys are due at the close, the deletions only when 30 days have passed, and a
// second call returns the same steps.
func TestDue(t *testing.T) {
	t.Parallel()
	plan := mustPlan(t, fullInventory())
	ends := closedAt.Add(wantRetention)
	closeSteps := []Step{
		{Action: OfferExport, Kind: Repos, Due: closedAt, Ends: ends},
		{Action: DeleteKeys, Kind: Keys, Due: closedAt},
	}
	deleteSteps := []Step{
		{Action: DeleteKeys, Kind: Keys, Due: closedAt},
		{Action: DeleteData, Kind: Repos, Due: ends},
		{Action: DeleteData, Kind: Tracker, Due: ends},
		{Action: DeleteData, Kind: Caches, Due: ends},
		{Action: DeleteData, Kind: NodePool, Due: ends},
		{Action: DeleteData, Kind: Town, Due: ends},
	}
	cases := []struct {
		name string
		now  time.Time
		want []Step
	}{
		{"before the close", closedAt.Add(-time.Nanosecond), nil},
		{"at the close", closedAt, closeSteps},
		{"a day before the window ends", closedAt.Add(wantRetention - 24*time.Hour), closeSteps},
		{"the hour before the window ends", ends.Add(-time.Hour), closeSteps},
		{"the window ended", ends, deleteSteps},
		{"after the window", ends.Add(time.Second), deleteSteps},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Due(tc.now, plan)
			if !slices.EqualFunc(got, tc.want, sameStep) {
				t.Errorf("Due(%v) = %+v, want %+v", tc.now, got, tc.want)
			}
		})
	}

	t.Run("calling twice returns the same steps", func(t *testing.T) {
		t.Parallel()
		first := Due(closedAt, plan)
		second := Due(closedAt, plan)
		if !slices.EqualFunc(first, second, sameStep) {
			t.Errorf("Due twice at the close = %+v then %+v, want the same steps", first, second)
		}
	})
}
