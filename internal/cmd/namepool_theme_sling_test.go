package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// themeUnresolvedFake is a polecat manager whose rig style has no theme file:
// its pool refuses every allocation with err, and nothing else is created.
type themeUnresolvedFake struct {
	*namedSlingFake
	err error
}

func (f *themeUnresolvedFake) AllocateAndAdd(polecat.AddOptions) (string, *polecat.Polecat, error) {
	f.allocCalls++
	return "", nil, f.err
}

// missingThemeErr is the error a rig's pool returns when its style names a
// theme file that is not there. The real pool produces it, so the message the
// test checks is the one an operator reads.
func missingThemeErr(t *testing.T) error {
	t.Helper()
	townRoot := t.TempDir()
	pool := polecat.NewNamePoolWithConfig(townRoot, "alpha", "gone", nil, 10)
	pool.SetTownRoot(townRoot)
	_, err := pool.Allocate()
	if !errors.Is(err, polecat.ErrThemeUnresolved) {
		t.Fatalf("Allocate = %v, want ErrThemeUnresolved", err)
	}
	return err
}

// A sling into a rig whose theme file is missing gets no name and no polecat,
// and the refusal is one an automatic dispatcher reads as a deferral, not a
// failure. The pool does not fall back to a built-in name.
func TestAllocatePolecatForSling_MissingThemeRefusesWithoutAFallbackName(t *testing.T) {
	t.Parallel()
	fake := &themeUnresolvedFake{namedSlingFake: newNamedSlingFake(), err: missingThemeErr(t)}

	name, err := allocatePolecatForSling(fake, "alpha", "", polecat.AddOptions{})
	if err == nil {
		t.Fatalf("allocatePolecatForSling = %q, nil; want a refusal", name)
	}
	if name != "" {
		t.Errorf("name = %q with the theme missing, want none", name)
	}
	if !errors.Is(err, polecat.ErrThemeUnresolved) {
		t.Errorf("err = %v; want it to keep ErrThemeUnresolved", err)
	}
	if !strings.Contains(err.Error(), `"gone"`) {
		t.Errorf("refusal %q does not name the missing style", err)
	}
	if !isSpecSlingRefusal(err) {
		t.Errorf("%q is not read as a deferral: the tick would label the bead dispatch-failed", err)
	}
	if len(fake.addNames) != 0 {
		t.Errorf("a named create ran after the pool refused: %v", fake.addNames)
	}
}

// The spec dispatcher takes the refusal as the bead's skip reason: the bead is
// deferred with the theme named, is not counted as failed or dispatched, and
// its seat is handed back.
func TestSpecRunPendingSlingsRecordsAMissingThemeAsASkip(t *testing.T) {
	t.Parallel()
	fake := &themeUnresolvedFake{namedSlingFake: newNamedSlingFake(), err: missingThemeErr(t)}
	_, refusal := allocatePolecatForSling(fake, "alpha", "", polecat.AddOptions{})

	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	f.slingErrs["gt-a"] = []error{refusal}
	budget := specdispatch.Budget{
		Seats: []specdispatch.Seat{{Agent: "deepseek-flash", Cap: 3}},
		Now:   specTestNow,
	}
	budget.Bump("deepseek-flash", specTestNow)
	pending := []specPendingSling{{
		id:    "gt-a",
		c:     specCandidate{Spec: f.specs["gt-a"], Rig: "alpha"},
		seat:  specdispatch.SeatChoice{Agent: "deepseek-flash"},
		entry: specDispatchEntry{Bead: "gt-a", Rig: "alpha", Agent: "deepseek-flash"},
	}}
	var report specDispatchReport
	specRunPendingSlings(f.env(), pending, &report, &budget)

	if len(report.Dispatched) != 0 || len(report.Failed) != 0 {
		t.Fatalf("dispatched = %+v, failed = %+v; want the bead deferred, not dispatched or failed", report.Dispatched, report.Failed)
	}
	if len(report.Skipped) != 1 {
		t.Fatalf("skipped = %+v; want the bead recorded as skipped", report.Skipped)
	}
	line := report.Skipped[0].Line
	if !strings.Contains(line, "sling deferred") || !strings.Contains(line, `"gone"`) {
		t.Errorf("skip reason = %q; want the deferral and the missing style named", line)
	}
	if got := f.labels["gt-a"]; len(got) != 0 {
		t.Errorf("labels = %v, want none: a refusal is not a failure", got)
	}
	if got := budget.Seats[0].Live; got != 0 {
		t.Errorf("seat live = %d, want 0: a refused sling hands its seat back", got)
	}
}
