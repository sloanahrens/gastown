package doctor

import (
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// seedRetiredConvoys gives town's store the shapes the retired feeder left
// behind: a labeled convoy, a legacy typed one, a bead wearing both markers,
// an already-closed convoy, and an ordinary open task.
func seedRetiredConvoys(town *doctorTown) {
	town.db("/town").Seed(
		beads.Issue{ID: "hq-cv-label", Status: "open", Labels: []string{convoyLabel}},
		beads.Issue{ID: "hq-cv-type", Status: "open", Type: convoyIssueType},
		beads.Issue{ID: "hq-cv-both", Status: "open", Type: convoyIssueType, Labels: []string{convoyLabel}},
		beads.Issue{ID: "hq-cv-closed", Status: "closed", Type: convoyIssueType, Labels: []string{convoyLabel}},
		beads.Issue{ID: "gt-work", Status: "open"},
	)
}

// TestConvoyRetirementRunFindsOpenConvoys: both markers identify a convoy, a
// bead wearing both is one convoy, and nothing that is not an open convoy is
// named.
func TestConvoyRetirementRunFindsOpenConvoys(t *testing.T) {
	t.Parallel()
	town := newDoctorTown()
	seedRetiredConvoys(town)

	res := NewConvoyRetirementCheck().Run(town.ctx("/town", ""))

	if res.Status != StatusWarning {
		t.Fatalf("Run status = %v (%s), want Warning", res.Status, res.Message)
	}
	got := append([]string(nil), res.Details...)
	sort.Strings(got)
	want := []string{"hq-cv-both", "hq-cv-label", "hq-cv-type"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Run details = %v, want %v", got, want)
	}
}

// TestConvoyRetirementRunOKWhenNoneAreOpen: a town with only closed convoys
// and ordinary work passes, so the check stops once the one-shot has run.
func TestConvoyRetirementRunOKWhenNoneAreOpen(t *testing.T) {
	t.Parallel()
	town := newDoctorTown()
	town.db("/town").Create(beads.CreateOptions{Title: "an ordinary task", Priority: -1})

	res := NewConvoyRetirementCheck().Run(town.ctx("/town", ""))

	if res.Status != StatusOK {
		t.Errorf("Run status = %v (%s), want OK", res.Status, res.Message)
	}
}

// TestConvoyRetirementFixClosesTheConvoysOnly: the fix closes every open
// convoy with the retirement reason and leaves the work they tracked alone.
func TestConvoyRetirementFixClosesTheConvoysOnly(t *testing.T) {
	t.Parallel()
	town := newDoctorTown()
	seedRetiredConvoys(town)
	ctx := town.ctx("/town", "")

	check := NewConvoyRetirementCheck()
	if res := check.Run(ctx); res.Status != StatusWarning {
		t.Fatalf("Run status = %v (%s), want Warning", res.Status, res.Message)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	for _, id := range []string{"hq-cv-label", "hq-cv-type", "hq-cv-both"} {
		issue, err := town.db("/town").Show(id)
		if err != nil {
			t.Fatalf("Show(%s): %v", id, err)
		}
		if issue.Status != string(beads.StatusClosed) {
			t.Errorf("%s status = %q, want closed", id, issue.Status)
		}
		if issue.CloseReason != convoyRetiredReason {
			t.Errorf("%s close reason = %q, want %q", id, issue.CloseReason, convoyRetiredReason)
		}
	}

	work, err := town.db("/town").Show("gt-work")
	if err != nil {
		t.Fatalf("Show(gt-work): %v", err)
	}
	if work.Status != string(beads.StatusOpen) {
		t.Errorf("tracked work bead status = %q, want open", work.Status)
	}

	if res := check.Run(ctx); res.Status != StatusOK {
		t.Errorf("Run after Fix = %v (%s), want OK", res.Status, res.Message)
	}
}

// TestConvoyRetirementFixThroughTheDoctor: the fix is destructive, and the
// run/fix/re-run loop reports it fixed.
func TestConvoyRetirementFixThroughTheDoctor(t *testing.T) {
	t.Parallel()
	town := newDoctorTown()
	seedRetiredConvoys(town)
	ctx := town.ctx("/town", "")

	check := NewConvoyRetirementCheck()
	if !IsDestructiveFix(check) {
		t.Fatal("the convoy retirement fix closes beads; it must report itself destructive")
	}

	d := NewDoctor()
	d.Register(check)
	authorized := false
	res, err := d.FixOne(ctx, check.Name(), nil, func(Check) error {
		authorized = true
		return nil
	})
	if err != nil {
		t.Fatalf("FixOne: %v", err)
	}
	if !authorized {
		t.Error("FixOne did not ask for authorization before the destructive fix")
	}
	if !res.Fixed {
		t.Errorf("FixOne result = %v (%s), want Fixed", res.Status, res.Message)
	}
}

// TestConvoyRetirementRunSkippedWhenTheStoreWillNotAnswer: an unreadable store
// proves nothing, so the check reports UNKNOWN rather than a clean town.
func TestConvoyRetirementRunSkippedWhenTheStoreWillNotAnswer(t *testing.T) {
	t.Parallel()

	res := NewConvoyRetirementCheck().Run(noBD("/town"))

	if res.Status != StatusSkipped {
		t.Errorf("Run status = %v (%s), want Skipped", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, errNoDatabase.Error()) {
		t.Errorf("Run message %q does not name the store's error %v", res.Message, errNoDatabase)
	}
}
