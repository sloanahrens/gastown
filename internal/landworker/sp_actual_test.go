package landworker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/storypoints"
)

// spLanded is the commit the fake lander reports and the landing record holds,
// so writeSPActual finds the record for the landing it just made.
const spLanded = "cccccccccc"

// spStore is the bead store a test swaps in from inside the landing, so a
// failure reaches only the sp-actual write and read that follow the landing.
type spStore struct {
	*beadsfake.Fake
	showErr   error
	updateErr error
}

func (s spStore) Show(id string) (*beads.Issue, error) {
	if s.showErr != nil {
		return nil, s.showErr
	}
	return s.Fake.Show(id)
}

func (s spStore) Update(id string, opts beads.UpdateOptions) error {
	if s.updateErr != nil && len(opts.AddLabels) > 0 && strings.HasPrefix(opts.AddLabels[0], "sp-actual:") {
		return s.updateErr
	}
	return s.Fake.Update(id, opts)
}

// seedLabelled seeds the ready bead the landing takes, with the extra labels
// it carries (its estimate, or a stale sp-actual).
func (h *harness) seedLabelled(t *testing.T, id string, labels ...string) {
	t.Helper()
	w := land.Work{Branch: branch, Head: noteHead, Target: "main", Worker: "opal"}
	h.bd.Seed(beads.Issue{ID: id, Title: "work", Status: "hooked", Type: "task", Assignee: "gastown/polecats/opal",
		Labels: append([]string{land.LabelReadyToLand}, labels...), Notes: land.FormatReadyNote(w), UpdatedAt: seededRowAt})
}

// landingRecords makes the fake lander do what the production lander does
// before it returns: write the landing record, then report the landing.
func (h *harness) landingRecords(rec land.LandingRecord) {
	h.lander.fn = func(_ int, w land.Work) (land.Result, error) {
		rec.BeadID, rec.LandedCommit, rec.Target = w.BeadID, spLanded, "main"
		h.files.recs = append(h.files.recs, rec)
		return land.Result{LandedCommit: spLanded, PatchID: "pppppppppp"}, nil
	}
}

// captureLogs points the worker's log at a slice the test can read back.
func (h *harness) captureLogs() *[]string {
	var logs []string
	h.w.Logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	return &logs
}

// labelsOf reads the bead's labels from the fake store.
func (h *harness) labelsOf(t *testing.T, id string) []string {
	t.Helper()
	is, err := h.bd.Show(id)
	if err != nil {
		t.Fatal(err)
	}
	return is.Labels
}

// actualLabelsOf returns the sp-actual labels the bead carries.
func actualLabelsOf(labels []string) []string {
	var out []string
	for _, l := range labels {
		if strings.HasPrefix(l, "sp-actual:") {
			out = append(out, l)
		}
	}
	return out
}

func TestPassWritesSPActualFromTheLandingTime(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLabelled(t, "gt-abc", "sp:5")
	h.landingRecords(land.LandingRecord{PolecatSecs: 600})
	rep := h.w.Pass(context.Background())
	if rep.Landed != 1 {
		t.Fatalf("report %v; want one landing", rep)
	}
	// Ten minutes is on the 8-point edge (up to 10.5 minutes).
	if got := actualLabelsOf(h.labelsOf(t, "gt-abc")); len(got) != 1 || got[0] != "sp-actual:8" {
		t.Fatalf("sp-actual labels %v; want [sp-actual:8] for ten minutes", got)
	}
}

// TestPassSPActualSkipsUnmeasuredOrUnestimatedBeads: each row is a landing the
// measure cannot judge, so no sp-actual label is written, one log line names
// the reason, and the landing stands.
func TestPassSPActualSkipsUnmeasuredOrUnestimatedBeads(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		rec    land.LandingRecord
		labels []string
		reason string
	}{
		{"absent time", land.LandingRecord{}, []string{"sp:5"}, "polecat time absent"},
		{"lower bound", land.LandingRecord{PolecatSecs: 600, PolecatLowerBound: true}, []string{"sp:5"}, "polecat time is a lower bound"},
		{"upper bound", land.LandingRecord{PolecatSecs: 600, PolecatUpperBound: true}, []string{"sp:5"}, "polecat time is an upper bound"},
		// The seat ran (attempts present) but the time the worker reads is
		// zero, so the skip must come from FromMinutes' error, not an absent record.
		{"zero seconds", land.LandingRecord{PolecatSecs: 0, PolecatAttempts: []land.PolecatAttempt{{Seat: "opal", Secs: 0}}}, []string{"sp:5"}, storypoints.ErrNoMinutes.Error()},
		{"no sp label", land.LandingRecord{PolecatSecs: 600}, nil, "no sp: estimate label"},
		{"sp label off the scale", land.LandingRecord{PolecatSecs: 600}, []string{"sp:4"}, "no sp: estimate label"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.seedLabelled(t, "gt-abc", tc.labels...)
			logs := h.captureLogs()
			h.landingRecords(tc.rec)
			rep := h.w.Pass(context.Background())
			if rep.Landed != 1 || rep.Failed != 0 {
				t.Fatalf("report %v; want one landing and no failure", rep)
			}
			if got := actualLabelsOf(h.labelsOf(t, "gt-abc")); len(got) != 0 {
				t.Fatalf("sp-actual labels %v written for an unmeasured or unestimated bead; want none", got)
			}
			skips := spSkipLines(*logs)
			if len(skips) != 1 || !strings.Contains(skips[0], tc.reason) {
				t.Fatalf("sp-actual skip lines %q; want one line naming %q", skips, tc.reason)
			}
		})
	}
}

// TestPassSPActualIgnoresAnotherLandingsRecord: the record the worker reads
// must be the one this landing wrote, never a stale one for the bead.
func TestPassSPActualIgnoresAnotherLandingsRecord(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLabelled(t, "gt-abc", "sp:5")
	h.lander.fn = func(_ int, w land.Work) (land.Result, error) {
		h.files.recs = append(h.files.recs, land.LandingRecord{BeadID: w.BeadID, LandedCommit: "dddddddddd", PolecatSecs: 600})
		return land.Result{LandedCommit: spLanded, PatchID: "pppppppppp"}, nil
	}
	if rep := h.w.Pass(context.Background()); rep.Landed != 1 {
		t.Fatalf("report %v; want one landing", rep)
	}
	if got := actualLabelsOf(h.labelsOf(t, "gt-abc")); len(got) != 0 {
		t.Fatalf("sp-actual labels %v read from another landing's record; want none", got)
	}
}

// TestPassSPActualWritesNothingWithNoRecordOfTheLanding: a landing the store
// holds no record of has no time to measure, so no label is written.
func TestPassSPActualWritesNothingWithNoRecordOfTheLanding(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLabelled(t, "gt-abc", "sp:5")
	h.lander.fn = func(_ int, _ land.Work) (land.Result, error) {
		return land.Result{LandedCommit: spLanded, PatchID: "pppppppppp"}, nil
	}
	if rep := h.w.Pass(context.Background()); rep.Landed != 1 || rep.Failed != 0 {
		t.Fatalf("report %v; want one landing and no failure", rep)
	}
	if got := actualLabelsOf(h.labelsOf(t, "gt-abc")); len(got) != 0 {
		t.Fatalf("sp-actual labels %v with no landing record; want none", got)
	}
}

// TestPassSPActualReplacesAStaleLabel: a second landing of the bead writes its
// own measure and takes the earlier one off, so the bead carries one.
func TestPassSPActualReplacesAStaleLabel(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLabelled(t, "gt-abc", "sp:5", "sp-actual:3")
	h.landingRecords(land.LandingRecord{PolecatSecs: 600})
	if rep := h.w.Pass(context.Background()); rep.Landed != 1 {
		t.Fatalf("report %v; want one landing", rep)
	}
	if got := actualLabelsOf(h.labelsOf(t, "gt-abc")); len(got) != 1 || got[0] != "sp-actual:8" {
		t.Fatalf("sp-actual labels %v; want only [sp-actual:8]", got)
	}
}

// TestPassSPActualLabelsTheRecordARepairCompletes: a landing whose record was
// left incomplete is labelled by the repair pass that completes it, not by the
// pass that landed it.
func TestPassSPActualLabelsTheRecordARepairCompletes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLabelled(t, "gt-abc", "sp:5")
	h.lander.fn = func(n int, w land.Work) (land.Result, error) {
		if n == 1 {
			h.files.recs = append(h.files.recs, land.LandingRecord{BeadID: w.BeadID, LandedCommit: spLanded, Target: "main"})
			res := land.Result{LandedCommit: spLanded}
			return res, &land.RecordError{Result: res, Err: errors.New("close failed")}
		}
		// The repair pass completes the record with the seat's time.
		h.files.recs[len(h.files.recs)-1].PolecatSecs = 600
		return land.Result{LandedCommit: spLanded}, nil
	}
	if rep := h.w.Pass(context.Background()); rep.Landed != 1 {
		t.Fatalf("first pass %v; want the landing counted", rep)
	}
	if got := actualLabelsOf(h.labelsOf(t, "gt-abc")); len(got) != 0 {
		t.Fatalf("sp-actual labels %v written before the record was complete; want none", got)
	}
	if err := h.bd.Update("gt-abc", beads.UpdateOptions{RemoveLabels: []string{land.LabelReadyToLand}}); err != nil {
		t.Fatal(err)
	}
	h.remote.tips[branch] = "3333333333333333333333333333333333333333"
	if rep := h.w.Pass(context.Background()); rep.Repaired != 1 {
		t.Fatalf("second pass %v; want one repair", rep)
	}
	if got := actualLabelsOf(h.labelsOf(t, "gt-abc")); len(got) != 1 || got[0] != "sp-actual:8" {
		t.Fatalf("sp-actual labels %v after the repair; want [sp-actual:8]", got)
	}
}

// TestPassSPActualWriteFailureIsLoggedAndLandingUnchanged: a failed label write
// is logged and the landing stands: counted, cleared and announced as before.
func TestPassSPActualWriteFailureIsLoggedAndLandingUnchanged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLabelled(t, "gt-abc", "sp:5")
	logs := h.captureLogs()
	h.landingRecords(land.LandingRecord{PolecatSecs: 600})
	inner := h.lander.fn
	h.lander.fn = func(n int, w land.Work) (land.Result, error) {
		res, err := inner(n, w)
		h.w.Beads = spStore{Fake: h.bd, updateErr: errors.New("bd update: database locked")}
		return res, err
	}
	rep := h.w.Pass(context.Background())
	if rep.Landed != 1 || rep.Failed != 0 || rep.Rejected != 0 {
		t.Fatalf("report %v; want the landing counted and nothing failed", rep)
	}
	if len(h.cleared) != 1 || len(h.afterLand) != 1 {
		t.Fatalf("cleared %v, after-landing %v; want the landing's own steps to run", h.cleared, h.afterLand)
	}
	if got := actualLabelsOf(h.labelsOf(t, "gt-abc")); len(got) != 0 {
		t.Fatalf("sp-actual labels %v; the write failed so want none", got)
	}
	if !spLogged(*logs, "writing sp-actual:8") {
		t.Fatalf("logs %q; want the failed sp-actual write logged", *logs)
	}
}

// TestPassSPActualReadFailureIsLoggedAndLandingUnchanged: an unreadable landing
// record is logged and writes nothing; it is not read as an unmeasured bead.
func TestPassSPActualReadFailureIsLoggedAndLandingUnchanged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLabelled(t, "gt-abc", "sp:5")
	logs := h.captureLogs()
	h.landingRecords(land.LandingRecord{PolecatSecs: 600})
	inner := h.lander.fn
	h.lander.fn = func(n int, w land.Work) (land.Result, error) {
		res, err := inner(n, w)
		h.files.err = errors.New("landings file unreadable")
		return res, err
	}
	rep := h.w.Pass(context.Background())
	if rep.Landed != 1 || rep.Failed != 0 {
		t.Fatalf("report %v; want the landing counted and nothing failed", rep)
	}
	if got := actualLabelsOf(h.labelsOf(t, "gt-abc")); len(got) != 0 {
		t.Fatalf("sp-actual labels %v; the record was unreadable so want none", got)
	}
	if !spLogged(*logs, "reading the landing record for sp-actual") {
		t.Fatalf("logs %q; want the unreadable record logged", *logs)
	}
}

// TestPassSPActualBeadReadFailureIsLoggedAndLandingUnchanged: a bead the worker
// cannot read after the landing is logged and gets no label.
func TestPassSPActualBeadReadFailureIsLoggedAndLandingUnchanged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLabelled(t, "gt-abc", "sp:5")
	logs := h.captureLogs()
	h.landingRecords(land.LandingRecord{PolecatSecs: 600})
	inner := h.lander.fn
	h.lander.fn = func(n int, w land.Work) (land.Result, error) {
		res, err := inner(n, w)
		h.w.Beads = spStore{Fake: h.bd, showErr: errors.New("bd show: timeout")}
		return res, err
	}
	rep := h.w.Pass(context.Background())
	if rep.Landed != 1 || rep.Failed != 0 {
		t.Fatalf("report %v; want the landing counted and nothing failed", rep)
	}
	if !spLogged(*logs, "reading the bead for sp-actual") {
		t.Fatalf("logs %q; want the unreadable bead logged", *logs)
	}
}

// spSkipLines returns the log lines that record an sp-actual skip.
func spSkipLines(logs []string) []string {
	var out []string
	for _, l := range logs {
		if strings.Contains(l, "sp-actual skipped") {
			out = append(out, l)
		}
	}
	return out
}

func spLogged(logs []string, want string) bool {
	for _, l := range logs {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}
