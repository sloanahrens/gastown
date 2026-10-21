package scorecard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev/failcause"
)

// The judging of a test on gastown's bead gt-abc, at a fixed instant.
var judgedAt = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// failcauseLine is one cause line as the battery writes it, judged at judgedAt.
func failcauseLine(t *testing.T, bead string) []byte {
	t.Helper()
	willRecur := 0.8
	raw, err := json.Marshal(failcause.Line{
		TS:      judgedAt,
		Rig:     "gastown",
		Bead:    bead,
		Package: "example.com/x/render",
		Test:    "TestRenderTable",
		Answers: map[string]failcause.Answer{
			failcause.QWillRecur: {Noul: &willRecur},
			failcause.QCause:     {Choice: "shared_state"},
		},
		Model:           "jev-1.13.0",
		QuestionVersion: failcause.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// ciLedger writes the CI-failure ledger to a fresh file, then sets its last
// write to modified. It returns the path.
func ciLedger(t *testing.T, modified time.Time, recs ...failcause.CIRecord) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ci-failures.jsonl")
	var data []byte
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	return path
}

// ciFailure is one ledger record of the test on a bead, at at.
func ciFailure(bead string, at time.Time) failcause.CIRecord {
	return failcause.CIRecord{Time: at, Rig: "gastown", Bead: bead, Package: "example.com/x/render", Test: "TestRenderTable"}
}

// labelOne parses one cause line through the adapter and labels it at now.
func labelOne(t *testing.T, ledgerPath string, now time.Time, bead string) Labeling {
	t.Helper()
	a := newFailcauseAdapter(filepath.Join(t.TempDir(), "failcause.jsonl"), ledgerPath, func() time.Time { return now })
	it, err := a.Parse(failcauseLine(t, bead))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, err := a.Label(context.Background(), it)
	if err != nil {
		t.Fatalf("Label: %v", err)
	}
	return got
}

// A repeat of the test on a different bead inside the window labels will_recur
// true, and the label does not wait for the window to close.
func TestFailcauseLabelsARepeatOnAnotherBeadPositive(t *testing.T) {
	t.Parallel()
	ledger := ciLedger(t, judgedAt.Add(3*24*time.Hour),
		ciFailure("gt-abc", judgedAt),
		ciFailure("gt-other", judgedAt.Add(5*24*time.Hour)),
	)

	got := labelOne(t, ledger, judgedAt.Add(6*24*time.Hour), "gt-abc")

	if !got.Labeled || got.Labels[qWillRecur] != true || got.Rule != ruleFailcauseRecurred {
		t.Errorf("Labeling = %+v, want will_recur true by %s", got, ruleFailcauseRecurred)
	}
}

// A repeat on the same bead is the same bead failing again, not a recurrence
// of the defect on another landing.
func TestFailcauseIgnoresARepeatOnTheSameBead(t *testing.T) {
	t.Parallel()
	ledger := ciLedger(t, judgedAt.Add(2*24*time.Hour),
		ciFailure("gt-abc", judgedAt),
		ciFailure("gt-abc", judgedAt.Add(2*24*time.Hour)),
	)

	got := labelOne(t, ledger, judgedAt.Add(15*24*time.Hour), "gt-abc")

	if !got.Labeled || got.Labels[qWillRecur] != false || got.Rule != ruleFailcauseQuiet {
		t.Errorf("Labeling = %+v, want will_recur false by %s", got, ruleFailcauseQuiet)
	}
}

// Before the window closes, with no repeat yet, the line is unlabeled.
func TestFailcauseIsUnlabeledBeforeTheWindowCloses(t *testing.T) {
	t.Parallel()
	ledger := ciLedger(t, judgedAt, ciFailure("gt-abc", judgedAt))

	got := labelOne(t, ledger, judgedAt.Add(13*24*time.Hour), "gt-abc")

	if got.Labeled {
		t.Errorf("Labeling = %+v, want unlabeled while the window is open", got)
	}
}

// A ledger written ten days after the judging still holds every record the
// fourteen-day window could hold, so a window closed with no repeat is quiet.
func TestFailcauseLabelsAQuietWindowTheLedgerKept(t *testing.T) {
	t.Parallel()
	ledger := ciLedger(t, judgedAt.Add(10*24*time.Hour), ciFailure("gt-abc", judgedAt))

	got := labelOne(t, ledger, judgedAt.Add(failcause.WillRecurWindow+24*time.Hour), "gt-abc")

	if !got.Labeled || got.Labels[qWillRecur] != false || got.Rule != ruleFailcauseQuiet {
		t.Errorf("Labeling = %+v, want will_recur false by %s", got, ruleFailcauseQuiet)
	}
}

// Past the window with no repeat, the negative label needs proof the ledger
// kept the whole window: its last write must come strictly before the judging
// plus the ledger's retention. A later write may have pruned a repeat.
func TestFailcauseNegativeNeedsALedgerThatKeptTheWindow(t *testing.T) {
	t.Parallel()
	closed := judgedAt.Add(failcause.WillRecurWindow + 24*time.Hour)
	boundary := judgedAt.Add(failcause.CIFailureRetention)

	covered := ciLedger(t, boundary.Add(-time.Second), ciFailure("gt-abc", judgedAt))
	if got := labelOne(t, covered, closed, "gt-abc"); !got.Labeled || got.Labels[qWillRecur] != false {
		t.Errorf("covered ledger: Labeling = %+v, want will_recur false", got)
	}

	// An mtime at the boundary is a proxy, not proof of a write before it: the
	// daemon can queue on the ledger lock and rewrite after taking its clock,
	// and a filesystem can floor the timestamp. The label is withheld.
	atBoundary := ciLedger(t, boundary, ciFailure("gt-abc", judgedAt))
	if got := labelOne(t, atBoundary, closed, "gt-abc"); got.Labeled {
		t.Errorf("a ledger whose last write is at the boundary: Labeling = %+v, want unlabeled", got)
	}

	pruned := ciLedger(t, boundary.Add(time.Hour), ciFailure("gt-abc", judgedAt))
	if got := labelOne(t, pruned, closed, "gt-abc"); got.Labeled {
		t.Errorf("a ledger written after the window's coverage: Labeling = %+v, want unlabeled", got)
	}
}

// A repeat that lands after the window does not count, so the test is quiet
// for the window.
func TestFailcauseRepeatAfterTheWindowDoesNotCount(t *testing.T) {
	t.Parallel()
	ledger := ciLedger(t, judgedAt.Add(6*24*time.Hour),
		ciFailure("gt-abc", judgedAt),
		ciFailure("gt-other", judgedAt.Add(failcause.WillRecurWindow+time.Hour)),
	)

	got := labelOne(t, ledger, judgedAt.Add(failcause.WillRecurWindow+2*24*time.Hour), "gt-abc")

	if !got.Labeled || got.Labels[qWillRecur] != false {
		t.Errorf("Labeling = %+v, want will_recur false: the repeat came after the window", got)
	}
}

// With no ledger at all, a closed window cannot be told from a lost ledger, so
// the line is unlabeled.
func TestFailcauseWithoutALedgerIsUnlabeled(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "ci-failures.jsonl")

	got := labelOne(t, missing, judgedAt.Add(failcause.WillRecurWindow+24*time.Hour), "gt-abc")

	if got.Labeled {
		t.Errorf("Labeling = %+v, want unlabeled with no ledger", got)
	}
}

// A line from another question set cannot be compared with this one, and a
// line with no answers has nothing to score.
func TestFailcauseParseSkipsWhatItCannotCompare(t *testing.T) {
	t.Parallel()
	a := newFailcauseAdapter("", "", time.Now)

	var old failcause.Line
	if err := json.Unmarshal(failcauseLine(t, "gt-abc"), &old); err != nil {
		t.Fatal(err)
	}
	old.QuestionVersion = failcause.Version + 1
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Parse(raw); err == nil {
		t.Error("Parse of another question set succeeded, want an error")
	}

	old.QuestionVersion = failcause.Version
	old.Answers = nil
	raw, err = json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Parse(raw); err == nil {
		t.Error("Parse of a line with no answers succeeded, want an error")
	}
}

// The battery is registered under its own name, so gt jev scorecard can read it.
func TestFailcauseIsRegistered(t *testing.T) {
	t.Parallel()
	a, err := Open(FailcauseBattery, t.TempDir())
	if err != nil {
		t.Fatalf("Open(%s): %v", FailcauseBattery, err)
	}
	if a.Battery() != FailcauseBattery {
		t.Errorf("Battery() = %q, want %q", a.Battery(), FailcauseBattery)
	}
}
