package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// overrunSpec is a bead in the overrun box's shape as far as the bead itself
// goes: a shaped task with exactly one label (gt-b9j3o.4). The seat and the
// rig's threshold, the box's other two facts, are the fixture's.
func overrunSpec(id string) specdispatch.Spec {
	s := cleanSpec(id, 1, "2026-09-29T10:00:00Z")
	s.Labels = []string{"handoff"}
	return s
}

// overrunSeats is one seat on the model family the box was measured on.
func overrunSeats() []specdispatch.Seat {
	return []specdispatch.Seat{{Agent: "claude-haiku", Cap: 2}}
}

// overrunNotes is the overrun flags a bead wears.
func overrunNotes(f *fakeSpecTown, id string) []string {
	var out []string
	for _, n := range f.notes[id] {
		if strings.HasPrefix(n, specdispatch.OverrunNoteKey) {
			out = append(out, n)
		}
	}
	return out
}

// A bead in the box is flagged before its sling and dispatched as it would be
// otherwise: the flag is a note, not a hold. A bead outside the box on any one
// fact — the seat's model, a second label, the rig's threshold — is dispatched
// with no note.
func TestSpecDispatchFlagsTheOverrunBox(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		agent     string
		labels    []string
		threshold float64
		want      bool
	}{
		{"the box", "claude-haiku", []string{"handoff"}, 0.70, true},
		{"the bottom of the threshold band", "claude-haiku", []string{"handoff"}, 0.60, true},
		{"a sonnet seat", "claude-sonnet", []string{"handoff"}, 0.70, false},
		{"a second label", "claude-haiku", []string{"handoff", "rework"}, 0.70, false},
		{"a threshold above the band", "claude-haiku", []string{"handoff"}, 0.90, false},
		{"a rig with no threshold", "claude-haiku", []string{"handoff"}, 0, false},
	} {
		spec := overrunSpec("gt-box")
		spec.Labels = tc.labels
		f := newFakeSpecTown(spec)
		f.seats = []specdispatch.Seat{{Agent: tc.agent, Cap: 1}}
		if tc.threshold != 0 {
			f.omThreshold["gastown"] = tc.threshold
		}
		env := f.env()
		env.PerTick = 1
		report := runSpecDispatchCycle(env)

		if len(f.slung) != 1 {
			t.Fatalf("%s: slung %v, want the bead dispatched whatever the flag says", tc.name, f.slung)
		}
		notes := overrunNotes(f, "gt-box")
		if got := len(notes) > 0; got != tc.want {
			t.Errorf("%s: flagged = %v, want %v (notes %v)", tc.name, got, tc.want, notes)
		}
		line := ""
		if len(report.Dispatched) > 0 {
			line = report.Dispatched[0].Line
		}
		if got := strings.Contains(line, specdispatch.OverrunNoteKey); got != tc.want {
			t.Errorf("%s: the tick line = %q, want the flag %v", tc.name, line, tc.want)
		}
	}
}

// A dry run prints the flag and writes nothing: no note reaches the bead, and
// no seat is spent.
func TestSpecDispatchDryRunPrintsTheOverrunFlag(t *testing.T) {
	t.Parallel()

	f := newFakeSpecTown(overrunSpec("gt-box"))
	f.seats = overrunSeats()
	f.omThreshold["gastown"] = 0.70
	env := f.env()
	env.PerTick = 1
	env.DryRun = true
	report := runSpecDispatchCycle(env)

	if len(f.slung) != 0 {
		t.Fatalf("the dry run slung %v", f.slung)
	}
	if notes := overrunNotes(f, "gt-box"); len(notes) != 0 {
		t.Errorf("the dry run wrote the flag: %v", notes)
	}
	if len(report.Dispatched) != 1 || !strings.Contains(report.Dispatched[0].Line, specdispatch.OverrunNoteKey) {
		t.Errorf("the dry run did not print the flag: %+v", report.Dispatched)
	}
}

// A sling's flag rides the sling: a background job carries the note and writes
// it only when the sling dispatches, and a failed sling writes none.
func TestSpecDispatchOverrunFlagFollowsTheSling(t *testing.T) {
	t.Parallel()

	f := newFakeSpecTown(overrunSpec("gt-box"))
	f.seats = overrunSeats()
	f.omThreshold["gastown"] = 0.70
	env := f.env()
	env.PerTick = 1
	env.Background = true
	report := runSpecDispatchCycle(env)

	if len(report.Pending) != 1 {
		t.Fatalf("pending = %+v, want one sling", report.Pending)
	}
	job := report.Pending[0]
	if !strings.HasPrefix(job.OverrunNote, specdispatch.OverrunNoteKey) {
		t.Fatalf("the job carries no flag: %+v", job)
	}
	if notes := overrunNotes(f, "gt-box"); len(notes) != 0 {
		t.Errorf("the tick wrote the flag of a sling it did not run: %v", notes)
	}

	slingReport := runSpecSlingJob(env, job)
	if len(f.slung) != 1 {
		t.Fatalf("the job slung %v", f.slung)
	}
	if notes := overrunNotes(f, "gt-box"); len(notes) != 1 {
		t.Errorf("the dispatched sling wrote %d flag(s), want 1: %v", len(notes), notes)
	}
	if len(slingReport.Dispatched) != 1 || !strings.Contains(slingReport.Dispatched[0].Line, specdispatch.OverrunNoteKey) {
		t.Errorf("the sling's report does not carry the flag: %+v", slingReport.Dispatched)
	}

	// A sling that fails dispatched nothing, so its bead carries no flag.
	failed := newFakeSpecTown(overrunSpec("gt-box"))
	failed.seats = overrunSeats()
	failed.omThreshold["gastown"] = 0.70
	failed.slingErrs["gt-box"] = []error{errors.New("sling broke")}
	failedEnv := failed.env()
	failedEnv.PerTick = 1
	runSpecDispatchCycle(failedEnv)
	if notes := overrunNotes(failed, "gt-box"); len(notes) != 0 {
		t.Errorf("a failed sling flagged its bead: %v", notes)
	}
}

// An unreadable threshold is not a rig with no rubric: the tick reports it, the
// bead is not flagged on a guess, and the bead is slung as it would be. The
// read is the rig's, so it is made and reported once however many candidates
// the tick has.
func TestSpecDispatchReportsAnUnreadableThresholdOnce(t *testing.T) {
	t.Parallel()

	f := newFakeSpecTown(
		overrunSpec("gt-one"),
		overrunSpec("gt-two"),
	)
	f.seats = overrunSeats()
	f.omThresholdErr["gastown"] = errors.New("no such file")
	env := f.env()
	env.PerTick = 2
	report := runSpecDispatchCycle(env)

	if len(f.slung) != 2 {
		t.Fatalf("slung %v, want both beads dispatched", f.slung)
	}
	if notes := overrunNotes(f, "gt-one"); len(notes) != 0 {
		t.Errorf("a bead was flagged on a threshold that could not be read: %v", notes)
	}
	if reads := f.omThresholdReads["gastown"]; reads != 1 {
		t.Errorf("the threshold was read %d time(s), want once for the rig", reads)
	}
	thresholdErrs := 0
	for _, e := range report.Errors {
		if strings.Contains(e, "threshold") {
			thresholdErrs++
		}
	}
	if thresholdErrs != 1 {
		t.Errorf("the failed read was reported %d time(s), want 1: %v", thresholdErrs, report.Errors)
	}
}

// The rig's threshold is read once per tick, however many candidates ask for
// it: the box asks it for each candidate that fits it, and the file does not
// change inside a tick.
func TestSpecDispatchReadsTheThresholdOncePerRig(t *testing.T) {
	t.Parallel()

	f := newFakeSpecTown(overrunSpec("gt-one"), overrunSpec("gt-two"))
	f.seats = overrunSeats()
	f.omThreshold["gastown"] = 0.70
	env := f.env()
	env.PerTick = 2
	runSpecDispatchCycle(env)

	if reads := f.omThresholdReads["gastown"]; reads != 1 {
		t.Errorf("the threshold was read %d time(s), want once for the rig", reads)
	}
	for _, id := range []string{"gt-one", "gt-two"} {
		if notes := overrunNotes(f, id); len(notes) != 1 {
			t.Errorf("%s carries %d flag(s), want 1: %v", id, len(notes), notes)
		}
	}
}

// A tick built with no threshold reader is a construction bug, not a rig with
// no rubric: it says so in the report instead of unflagging every bead in
// silence.
func TestSpecDispatchReportsATickWithNoThresholdReader(t *testing.T) {
	t.Parallel()

	f := newFakeSpecTown(overrunSpec("gt-box"))
	f.seats = overrunSeats()
	env := f.env()
	env.PerTick = 1
	env.OMThreshold = nil
	report := runSpecDispatchCycle(env)

	if len(f.slung) != 1 {
		t.Fatalf("slung %v, want the bead dispatched", f.slung)
	}
	reported := false
	for _, e := range report.Errors {
		if strings.Contains(e, "review threshold") {
			reported = true
		}
	}
	if !reported {
		t.Errorf("a tick with no reader reported nothing: %v", report.Errors)
	}
	if notes := overrunNotes(f, "gt-box"); len(notes) != 0 {
		t.Errorf("a bead was flagged with no threshold to read: %v", notes)
	}
}

// The flag names the facts the bead does not show: the rig's threshold and the
// seat's model family.
func TestSpecDispatchOverrunFlagNamesTheSeatAndThreshold(t *testing.T) {
	t.Parallel()

	f := newFakeSpecTown(overrunSpec("gt-box"))
	f.seats = overrunSeats()
	f.omThreshold["gastown"] = 0.68
	env := f.env()
	env.PerTick = 1
	runSpecDispatchCycle(env)

	notes := overrunNotes(f, "gt-box")
	if len(notes) != 1 {
		t.Fatalf("notes = %v, want one flag", notes)
	}
	for _, want := range []string{"0.68", "haiku"} {
		if !strings.Contains(notes[0], want) {
			t.Errorf("the flag does not name %q: %s", want, notes[0])
		}
	}
}
