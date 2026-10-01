package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

const (
	criteriaOneTicked = "- [x] gt done refuses\n- [ ] unit tests pass"
	criteriaMixed     = "  - [x] gt done refuses\n- [ ] tests for refuse and pass paths\n  - [ ] make lint && make gate pass\nplain prose line"
	criteriaAllTicked = "- [x] gt done refuses\n- [x] tests pass"
)

func seedCriteria(h *submitHarness, criteria string) {
	h.bd.Seed(beads.Issue{ID: "bd-source", Title: "the work", Type: "task",
		Status: string(beads.StatusHooked), AcceptanceCriteria: criteria})
}

// wantNothingSubmitted: a refused submit rebased, gated, pushed and marked
// nothing.
func wantNothingSubmitted(t *testing.T, h *submitHarness) {
	t.Helper()
	if len(h.stages) != 0 || len(h.gate.heads) != 0 || len(h.repo.pushes) != 0 {
		t.Errorf("after a refusal: stages %v, gate %v, pushes %v", h.stages, h.gate.heads, h.repo.pushes)
	}
	if issue := h.source(t); beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Errorf("refused submit marked the bead ready to land: labels %v", issue.Labels)
	}
}

// TestSubmitRefusesUncheckedCriteriaNamingEach: the landing worker rejects a
// bead with an unchecked box as policy, after the session has retired; gt
// done says so first, listing each unchecked line, before the rebase, the
// branch checks, the gate and the push.
func TestSubmitRefusesUncheckedCriteriaNamingEach(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	seedCriteria(h, criteriaMixed)
	err := h.submit()
	if err == nil {
		t.Fatal("submit succeeded with unchecked acceptance criteria")
	}
	for _, want := range []string{
		"bd-source has 2 unchecked acceptance criteria",
		"- [ ] tests for refuse and pass paths",
		"- [ ] make lint && make gate pass",
		"bd show bd-source",
		"--acceptance",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "gt done refuses") {
		t.Errorf("refusal %q names a ticked criterion", err)
	}
	wantNothingSubmitted(t, h)
}

// TestSubmitWithTickedOrNoCriteriaSubmits: every box ticked, or no criteria
// at all, submits as before.
func TestSubmitWithTickedOrNoCriteriaSubmits(t *testing.T) {
	t.Parallel()
	for name, criteria := range map[string]string{
		"all ticked": criteriaAllTicked,
		"none":       "",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newSubmitHarness(t)
			seedCriteria(h, criteria)
			if err := h.submit(); err != nil {
				t.Fatalf("submit: %v", err)
			}
			if issue := h.source(t); !beads.HasLabel(issue, land.LabelReadyToLand) {
				t.Errorf("labels %v lack %s", issue.Labels, land.LabelReadyToLand)
			}
		})
	}
}

// TestCrewSubmitRefusesUncheckedCriteria: the crew path refuses the same
// way, before the gate.
func TestCrewSubmitRefusesUncheckedCriteria(t *testing.T) {
	t.Parallel()
	h := newCrewSubmitHarness(t)
	seedCriteria(h, criteriaOneTicked)
	err := submitCrewForLanding(h.r)
	if err == nil || !strings.Contains(err.Error(), "1 unchecked acceptance criteria") || !strings.Contains(err.Error(), "- [ ] unit tests pass") {
		t.Fatalf("crew submit = %v, want a refusal naming the unchecked line", err)
	}
	wantNothingSubmitted(t, h)
}

// TestCrewSubmitWithTickedCriteriaSubmits: a crew bead with every box ticked
// is marked ready to land.
func TestCrewSubmitWithTickedCriteriaSubmits(t *testing.T) {
	t.Parallel()
	h := newCrewSubmitHarness(t)
	seedCriteria(h, criteriaAllTicked)
	if err := submitCrewForLanding(h.r); err != nil {
		t.Fatalf("crew submit: %v", err)
	}
	if issue := h.source(t); !beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Errorf("labels %v lack %s", issue.Labels, land.LabelReadyToLand)
	}
}

// TestRefuseUncheckedCriteriaSharesTheLandingCounter: the refusal lists
// exactly as many lines as the count the landing worker's policy check uses.
func TestRefuseUncheckedCriteriaSharesTheLandingCounter(t *testing.T) {
	t.Parallel()
	issue := &beads.Issue{AcceptanceCriteria: criteriaMixed}
	if got, want := len(beads.UncheckedCriteria(issue)), beads.HasUncheckedCriteria(issue); got != want {
		t.Errorf("UncheckedCriteria lists %d, HasUncheckedCriteria counts %d", got, want)
	}
	if err := refuseUncheckedCriteria("bd-x", nil); err != nil {
		t.Errorf("nil issue refused: %v", err)
	}
}
