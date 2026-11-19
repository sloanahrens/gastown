package cmd

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// A bead whose contention schedule is spent is held under the contention label
// with the time a retry is owed. Once that time passes the dispatcher retries
// the bead by itself: both labels and the schedule go, and the bead is a
// candidate of the same tick (gt-5udj56).

// contentionResumeMetadata is the schedule a spent contention leaves: the three
// keys this package writes, plus another writer's key the clear must leave.
func contentionResumeMetadata(retryAt time.Time) []byte {
	return []byte(fmt.Sprintf(`{"%s":%q,"%s":5,"%s":%q,"spec_dispatch_unrelated":"keep"}`,
		specdispatch.ContentionFirstKey, specTestNow.Add(-15*time.Minute).UTC().Format(time.RFC3339),
		specdispatch.ContentionAttemptsKey,
		specdispatch.ContentionRetryAtKey, retryAt.UTC().Format(time.RFC3339)))
}

// contentionHeldIssue is the bead the label bead of this chain leaves behind:
// the failure label beside the contention label, and a schedule whose retry is
// dated at retryAt.
func contentionHeldIssue(id string, retryAt time.Time) beads.Issue {
	return beads.Issue{
		ID:       id,
		Type:     "task",
		Status:   "open",
		Priority: 2,
		Labels:   []string{specdispatch.DispatchFailedLabel, specdispatch.ContentionLabel},
		Metadata: contentionResumeMetadata(retryAt),
	}
}

func contentionResumeBoard(retryAt time.Time) func(string) ([]*beads.Issue, error) {
	return func(string) ([]*beads.Issue, error) {
		is := contentionHeldIssue("gt-a", retryAt)
		return []*beads.Issue{&is}, nil
	}
}

// contentionResumeRetry wires the read to the production probe and clear over
// store, so a test drives the decision and the writes the tick would make.
func contentionResumeRetry(store beads.Client) *specTimeoutRetry {
	return &specTimeoutRetry{
		due:   func(string) (specRetryDue, error) { return specTimedOutRetriesFrom(store, specTestNow) },
		clear: func(_, beadID string) error { return specRetryTimedOutSlingFrom(store, beadID) },
	}
}

// Once the pause is over the bead is a candidate of the tick, wearing neither
// label, and its schedule is gone: the next loss starts a fresh run.
func TestSpecContentionResumeRetriesOnceThePauseIsOver(t *testing.T) {
	t.Parallel()
	townRoot := specTown(t)
	db := beadsfake.New()
	db.Seed(contentionHeldIssue("gt-a", specTestNow.Add(-time.Second)))
	store := contentionDropStore{db}

	got := specCandidatesRetrying(townRoot, 2, contentionResumeBoard(specTestNow.Add(-time.Second)), notDocked, contentionResumeRetry(store))

	if len(got.Candidates) != 1 || got.Candidates[0].Spec.ID != "gt-a" {
		t.Fatalf("candidates = %+v, want the retried bead", got.Candidates)
	}
	if len(got.Retried) != 1 ||
		!strings.Contains(got.Retried[0], specdispatch.DispatchFailedLabel) ||
		!strings.Contains(got.Retried[0], specdispatch.ContentionLabel) {
		t.Errorf("Retried = %v, want one line naming both labels", got.Retried)
	}
	if len(got.Errors) != 0 {
		t.Errorf("errors = %v, want none for a schedule that reads cleanly", got.Errors)
	}
	for _, l := range got.Candidates[0].Spec.Labels {
		if l == specdispatch.DispatchFailedLabel || l == specdispatch.ContentionLabel {
			t.Errorf("candidate labels = %v, want both retry labels gone from the row", got.Candidates[0].Spec.Labels)
		}
	}

	issue, err := store.Show("gt-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{specdispatch.DispatchFailedLabel, specdispatch.ContentionLabel} {
		if slices.Contains(issue.Labels, gone) {
			t.Errorf("labels = %v, want %s cleared", issue.Labels, gone)
		}
	}
	if rec, ok := specdispatch.ContentionRecord(issue.Metadata); ok {
		t.Errorf("metadata %q still holds the schedule %+v, want it unset", issue.Metadata, rec)
	}
	if !strings.Contains(string(issue.Metadata), `"spec_dispatch_unrelated":"keep"`) {
		t.Errorf("metadata = %q, want another writer's key left alone", issue.Metadata)
	}
}

// Before the pause ends the bead stays held: the probe does not name it, the
// tick plans no sling, and both labels stay on.
func TestSpecContentionResumeHoldsTheBeadUntilThePauseIsOver(t *testing.T) {
	t.Parallel()
	townRoot := specTown(t)
	db := beadsfake.New()
	db.Seed(contentionHeldIssue("gt-a", specTestNow.Add(time.Minute)))
	store := contentionDropStore{db}

	got := specCandidatesRetrying(townRoot, 2, contentionResumeBoard(specTestNow.Add(time.Minute)), notDocked, contentionResumeRetry(store))

	if len(got.Candidates) != 0 || got.LabeledFailed != 1 {
		t.Fatalf("candidates = %+v, LabeledFailed = %d; want the bead held and counted", got.Candidates, got.LabeledFailed)
	}
	if len(got.Retried) != 0 {
		t.Errorf("Retried = %v, want none before the pause is over", got.Retried)
	}
	issue, err := store.Show("gt-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{specdispatch.DispatchFailedLabel, specdispatch.ContentionLabel} {
		if !slices.Contains(issue.Labels, kept) {
			t.Errorf("labels = %v, want %s kept", issue.Labels, kept)
		}
	}
}

// The probe answers for the contention label, not for a schedule in the
// metadata: a bead wearing only the failed label is not made due, even with a
// retry time already past.
func TestSpecContentionResumeIgnoresABeadWithoutTheContentionLabel(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(beads.Issue{
		ID:       "gt-a",
		Status:   "open",
		Labels:   []string{specdispatch.DispatchFailedLabel},
		Metadata: contentionResumeMetadata(specTestNow.Add(-time.Minute)),
	})

	got, err := specTimedOutRetriesFrom(db, specTestNow)
	if err != nil {
		t.Fatalf("specTimedOutRetriesFrom: %v", err)
	}
	if got.Due["gt-a"] {
		t.Errorf("due = %v, want a bead the contention label does not hold left alone", got.Due)
	}
	if len(got.Errors) != 0 {
		t.Errorf("errors = %v, want none for a bead that does not carry the label", got.Errors)
	}
}

// A contention schedule nothing can read leaves its bead held, and the line
// saying so is the tick's: a run that cannot be trusted is never retried blind.
func TestSpecContentionResumeHoldsABeadWhoseRecordCannotBeRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		metadata []byte
	}{
		{"a retry time that is not RFC 3339", []byte(fmt.Sprintf(`{"%s":"2026-09-30T11:45:00Z","%s":5,"%s":"soon"}`,
			specdispatch.ContentionFirstKey, specdispatch.ContentionAttemptsKey, specdispatch.ContentionRetryAtKey))},
		{"no schedule at all", nil},
	}
	for _, c := range cases {
		db := beadsfake.New()
		db.Seed(beads.Issue{
			ID:       "gt-a",
			Status:   "open",
			Labels:   []string{specdispatch.DispatchFailedLabel, specdispatch.ContentionLabel},
			Metadata: c.metadata,
		})

		got, err := specTimedOutRetriesFrom(db, specTestNow)
		if err != nil {
			t.Fatalf("%s: specTimedOutRetriesFrom: %v", c.name, err)
		}
		if got.Due["gt-a"] {
			t.Errorf("%s: due = %v, want the bead held", c.name, got.Due)
		}
		if len(got.Errors) != 1 || !strings.Contains(got.Errors[0], "gt-a") || !strings.Contains(got.Errors[0], "cannot be read") {
			t.Errorf("%s: errors = %v, want one naming the bead and the unreadable schedule", c.name, got.Errors)
		}
	}
}

// The clear removes the two labels and the three schedule keys and nothing
// else: another label and another writer's metadata stay for their own reader.
func TestSpecContentionResumeClearKeepsTheBeadsOtherState(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	is := contentionHeldIssue("gt-a", specTestNow.Add(-time.Second))
	is.Labels = append(is.Labels, "keep-me")
	db.Seed(is)
	store := contentionDropStore{db}

	if err := specRetryTimedOutSlingFrom(store, "gt-a"); err != nil {
		t.Fatalf("specRetryTimedOutSlingFrom: %v", err)
	}
	issue, err := store.Show("gt-a")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(issue.Labels, "keep-me") {
		t.Errorf("labels = %v, want the unrelated label kept", issue.Labels)
	}
	for _, gone := range []string{specdispatch.DispatchFailedLabel, specdispatch.ContentionLabel} {
		if slices.Contains(issue.Labels, gone) {
			t.Errorf("labels = %v, want %s cleared", issue.Labels, gone)
		}
	}
	if _, ok := specdispatch.ContentionRecord(issue.Metadata); ok {
		t.Errorf("metadata = %q, want the schedule unset", issue.Metadata)
	}
	if !strings.Contains(string(issue.Metadata), `"spec_dispatch_unrelated":"keep"`) {
		t.Errorf("metadata = %q, want another writer's key left alone", issue.Metadata)
	}
}

// failingShower is a store whose reads fail: a clear that cannot read its bead
// has no answer about what to remove, so it reports the error and writes
// nothing.
type failingShower struct {
	beads.Client
	err error
}

func (f failingShower) Show(string) (*beads.Issue, error) { return nil, f.err }

// nilShow is a store that answers a read with no bead and no error.
type nilShow struct{ beads.Client }

func (nilShow) Show(string) (*beads.Issue, error) { return nil, nil }

// A clear that cannot read the bead is an error, never a silent success: the
// bead's labels are left on, so the tick holds it rather than slinging it on a
// read that did not happen.
func TestSpecContentionResumeReportsAnUnreadableBead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		store beads.Client
		want  string
	}{
		{"the read fails", failingShower{err: errors.New("dolt: connection refused")}, "connection refused"},
		{"the store names no bead", nilShow{}, "no bead gt-a"},
	}
	for _, c := range cases {
		err := specRetryTimedOutSlingFrom(c.store, "gt-a")
		if err == nil || !strings.Contains(err.Error(), "reading the bead to retry") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it naming %q", c.name, err, c.want)
		}
	}
}

// A clear whose write fails is reported, and both labels stay: the bead waits
// for the next tick instead of being slung with its labels half removed.
func TestSpecContentionResumeReportsAFailedClear(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(contentionHeldIssue("gt-a", specTestNow.Add(-time.Second)))
	store := failingUpdate{Client: contentionDropStore{db}, err: errors.New("dolt: connection refused")}

	err := specRetryTimedOutSlingFrom(store, "gt-a")
	if err == nil || !strings.Contains(err.Error(), "clearing the contention retry") || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v, want the failed clear named", err)
	}
	issue, err := db.Show("gt-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{specdispatch.DispatchFailedLabel, specdispatch.ContentionLabel} {
		if !slices.Contains(issue.Labels, kept) {
			t.Errorf("labels = %v, want %s kept after a failed clear", issue.Labels, kept)
		}
	}
}

// The respawn gate applies to a contention retry as it does to a bound-kill
// retry: a bead whose budget is spent is reported held and keeps its labels.
func TestSpecContentionResumeRespawnLimitHoldsTheBead(t *testing.T) {
	t.Parallel()
	townRoot := specTown(t)
	db := beadsfake.New()
	db.Seed(contentionHeldIssue("gt-a", specTestNow.Add(-time.Second)))
	store := contentionDropStore{db}
	retry := contentionResumeRetry(store)
	retry.respawnGate = func(string) (bool, []string) { return true, nil }

	got := specCandidatesRetrying(townRoot, 2, contentionResumeBoard(specTestNow.Add(-time.Second)), notDocked, retry)

	if len(got.Candidates) != 0 || got.LabeledFailed != 1 {
		t.Fatalf("candidates = %+v, LabeledFailed = %d; want the bead held", got.Candidates, got.LabeledFailed)
	}
	if len(got.RespawnHeld) != 1 || got.RespawnHeld[0] != "gt-a" {
		t.Errorf("RespawnHeld = %v, want gt-a", got.RespawnHeld)
	}
	if len(got.Retried) != 0 {
		t.Errorf("Retried = %v, want none for a bead at its respawn limit", got.Retried)
	}
	issue, err := store.Show("gt-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{specdispatch.DispatchFailedLabel, specdispatch.ContentionLabel} {
		if !slices.Contains(issue.Labels, kept) {
			t.Errorf("labels = %v, want %s kept", issue.Labels, kept)
		}
	}
}
