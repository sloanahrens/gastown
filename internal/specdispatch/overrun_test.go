package specdispatch

import (
	"errors"
	"strings"
	"testing"
)

// overrunFact is the bare fact set the table starts each case from: every
// field in the box, so a case names only the fact it changes.
func overrunFact(threshold float64) OverrunFacts {
	return OverrunFacts{
		Type: "task", Priority: 1, Shaped: true, Labels: 1, Model: "haiku",
		Threshold: func() (float64, error) { return threshold, nil },
	}
}

func TestInOverrunBoxReadsTheBox(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		change func(*OverrunFacts)
		want   bool
	}{
		{"the box itself", func(*OverrunFacts) {}, true},
		{"a bug", func(f *OverrunFacts) { f.Type = "bug" }, true},
		{"a P0 bead", func(f *OverrunFacts) { f.Priority = 0 }, true},
		{"a P2 bead", func(f *OverrunFacts) { f.Priority = 2 }, true},
		{"the type in capitals and spaces", func(f *OverrunFacts) { f.Type = " Bug " }, true},
		{"the model in capitals", func(f *OverrunFacts) { f.Model = "Haiku" }, true},
		{"a feature bead", func(f *OverrunFacts) { f.Type = "feature" }, false},
		{"a chore bead", func(f *OverrunFacts) { f.Type = "chore" }, false},
		{"a P3 bead", func(f *OverrunFacts) { f.Priority = 3 }, false},
		{"an unscored bead", func(f *OverrunFacts) { f.Priority = -1 }, false},
		{"an unshaped bead", func(f *OverrunFacts) { f.Shaped = false }, false},
		{"a bead with two labels", func(f *OverrunFacts) { f.Labels = 2 }, false},
		{"a bead with no label", func(f *OverrunFacts) { f.Labels = 0 }, false},
		{"a sonnet seat", func(f *OverrunFacts) { f.Model = "sonnet" }, false},
		{"a seat that names no family", func(f *OverrunFacts) { f.Model = "" }, false},
	} {
		facts := overrunFact(0.70)
		tc.change(&facts)
		got, err := InOverrunBox(facts)
		if err != nil {
			t.Errorf("%s: InOverrunBox: %v", tc.name, err)
			continue
		}
		if got.Match != tc.want {
			t.Errorf("%s: Match = %v, want %v", tc.name, got.Match, tc.want)
		}
	}
}

// The band ends are in the box and the step past either is out of it. A rig
// that sets no threshold reads as 0, which is outside: the finding was measured
// on rigs whose reviewer scores in the band.
func TestInOverrunBoxBandsTheThreshold(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		threshold float64
		want      bool
	}{
		{"the bottom of the band", 0.60, true},
		{"the middle of the band", 0.68, true},
		{"the top of the band", 0.75, true},
		{"below the band", 0.55, false},
		{"above the band", 0.80, false},
		{"no threshold at all", 0, false},
	} {
		got, err := InOverrunBox(overrunFact(tc.threshold))
		if err != nil {
			t.Errorf("%s: InOverrunBox: %v", tc.name, err)
			continue
		}
		if got.Match != tc.want {
			t.Errorf("%s (%.2f): Match = %v, want %v", tc.name, tc.threshold, got.Match, tc.want)
		}
		if got.Threshold != tc.threshold {
			t.Errorf("%s: Threshold = %v, want %v", tc.name, got.Threshold, tc.threshold)
		}
	}
}

// The threshold is the box's one read, so a fact that already leaves the box
// must not spend it. The count is what keeps a later refactor from hoisting the
// read above the cheap facts.
func TestInOverrunBoxAsksTheThresholdLast(t *testing.T) {
	t.Parallel()

	asked := 0
	facts := overrunFact(0.70)
	facts.Type = "feature"
	facts.Threshold = func() (float64, error) { asked++; return 0.70, nil }
	got, err := InOverrunBox(facts)
	if err != nil {
		t.Fatalf("InOverrunBox: %v", err)
	}
	if got.Match {
		t.Error("a feature bead matched the box")
	}
	if asked != 0 {
		t.Errorf("the threshold was read %d time(s) for a bead outside the box", asked)
	}

	asked = 0
	inside := overrunFact(0.70)
	inside.Threshold = func() (float64, error) { asked++; return 0.70, nil }
	if got, err := InOverrunBox(inside); err != nil {
		t.Fatalf("InOverrunBox: %v", err)
	} else if !got.Match {
		t.Fatal("a bead inside the box did not match")
	}
	if asked != 1 {
		t.Errorf("a bead inside the box read the threshold %d time(s), want 1", asked)
	}
}

// A threshold that cannot be read is an error, not a fact outside the box: the
// caller reports the read, and the bead is not flagged on a guess.
func TestInOverrunBoxReportsAnUnreadableThreshold(t *testing.T) {
	t.Parallel()

	facts := overrunFact(0.70)
	want := errors.New("unreadable")
	facts.Threshold = func() (float64, error) { return 0, want }
	got, err := InOverrunBox(facts)
	if !errors.Is(err, want) {
		t.Fatalf("InOverrunBox error = %v, want %v", err, want)
	}
	if got.Match {
		t.Error("a bead was flagged on a threshold that could not be read")
	}
}

// The box has to be told where the threshold comes from: a facts value with no
// reader is a construction error, not a rig with no rubric.
func TestInOverrunBoxNeedsAThresholdReader(t *testing.T) {
	t.Parallel()

	facts := overrunFact(0.70)
	facts.Threshold = nil
	if _, err := InOverrunBox(facts); err == nil {
		t.Fatal("the box answered with no threshold reader")
	}
}

// The note names the facts an operator cannot read off the bead, and says the
// dispatch is unchanged.
func TestOverrunNoteNamesTheFlaggedFacts(t *testing.T) {
	t.Parallel()

	note := OverrunNote("haiku", 0.70)
	for _, want := range []string{OverrunNoteKey, "0.70", "haiku", "not a hold"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note does not name %q: %s", want, note)
		}
	}
}
