package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/reworkstats"
)

// The tests drive the step's run with a fake record source, so no ledger, review
// log or clock outside the test decides a figure.

// reworkProgramNow is the run's clock: a Monday noon, which is the week the run
// falls in and is still filling.
func reworkProgramNow() time.Time {
	return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
}

// reworkProgramDay is noon on day, so a record's week is its date's Monday in
// every time zone the record could be read in.
func reworkProgramDay(t *testing.T, day string) time.Time {
	t.Helper()
	at, err := time.Parse(time.DateOnly, day)
	if err != nil {
		t.Fatalf("test day %q: %v", day, err)
	}
	return at.Add(12 * time.Hour)
}

// reworkProgramWeek records n landings landed on day, the first twoRounds of
// them with two om review rounds, each with secs polecat seconds.
func reworkProgramWeek(t *testing.T, prefix, day string, n, twoRounds int, secs float64) ([]landings.Record, []land.ReviewRecord) {
	t.Helper()
	at := reworkProgramDay(t, day)
	recs := make([]landings.Record, 0, n)
	reviews := make([]land.ReviewRecord, 0, n+twoRounds)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("gt-%s-%02d", prefix, i)
		recs = append(recs, landings.Record{Bead: id, Rig: "gastown", LandedAt: at, PolecatSecs: secs})
		reviews = append(reviews, land.ReviewRecord{BeadID: id, Rig: "gastown", Attempt: 1})
		if i < twoRounds {
			reviews = append(reviews, land.ReviewRecord{BeadID: id, Rig: "gastown", Attempt: 2})
		}
	}
	return recs, reviews
}

// reworkProgramFigureWeek is one complete figure week: twenty beads, four with
// two rounds, each timed ten minutes. A lever test adds its landing to it.
func reworkProgramFigureWeek(t *testing.T) ([]landings.Record, []land.ReviewRecord) {
	t.Helper()
	return reworkProgramWeek(t, "base", "2026-11-09", 20, 4, 600)
}

// reworkProgramStep is a fresh town and the step that writes its document at a
// fixed clock.
func reworkProgramStep(t *testing.T) (string, *reworkNumbersStep) {
	t.Helper()
	town := t.TempDir()
	return town, newReworkNumbersStep(town, reworkProgramNow)
}

// reworkProgramOut is the document's path in town.
func reworkProgramOut(t *testing.T, town string) string {
	t.Helper()
	return filepath.Join(town, ".runtime", "rework-numbers", "latest.json")
}

// reworkProgramRunWith runs the step over a fake record source and returns the
// document it wrote.
func reworkProgramRunWith(t *testing.T, town string, step *reworkNumbersStep, in reworkNumbersRecord) reworkNumbersDocument {
	t.Helper()
	step.read = func() (reworkNumbersRecord, error) { return in, nil }
	if err := step.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	data, err := os.ReadFile(reworkProgramOut(t, town))
	if err != nil {
		t.Fatalf("the run wrote no document: %v", err)
	}
	var doc reworkNumbersDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("the document is not JSON: %v", err)
	}
	return doc
}

// reworkProgramLever returns one lever's row.
func reworkProgramLever(t *testing.T, doc reworkNumbersDocument, lever int) reworkNumbersLever {
	t.Helper()
	for _, row := range doc.Levers {
		if row.Lever == lever {
			return row
		}
	}
	t.Fatalf("the document has no row for lever %d", lever)
	return reworkNumbersLever{}
}

// reworkProgramFigure asserts a figure's value, within rounding.
func reworkProgramFigure(t *testing.T, label string, fig reworkNumbersFigure, want float64) {
	t.Helper()
	if fig.Value == nil {
		t.Fatalf("%s: no value (reason %q), want %v", label, fig.Reason, want)
	}
	if math.Abs(*fig.Value-want) > 1e-9 {
		t.Fatalf("%s: %v, want %v", label, *fig.Value, want)
	}
}

// reworkProgramReason asserts a figure has no value and the reason given.
func reworkProgramReason(t *testing.T, label string, fig reworkNumbersFigure, want string) {
	t.Helper()
	if fig.Value != nil {
		t.Fatalf("%s: value %v, want no value with reason %q", label, *fig.Value, want)
	}
	if fig.Reason != want {
		t.Fatalf("%s: reason %q, want %q", label, fig.Reason, want)
	}
}

// TestReworkProgramRunWritesNumbersAndLeverDeltas is the bead's acceptance: one
// run over a fake record source writes the two numbers for the last complete
// week and the lever's before/after deltas, under the document's wire names.
func TestReworkProgramRunWritesNumbersAndLeverDeltas(t *testing.T) {
	t.Parallel()
	before, beforeReviews := reworkProgramWeek(t, "before", "2026-10-26", 20, 10, 1200)
	after, afterReviews := reworkProgramWeek(t, "after", "2026-11-09", 20, 5, 600)
	// The lever lands between the two windows, so both are complete.
	recs := append(before, after...)
	recs = append(recs, landings.Record{Bead: "gt-a4yodt", Rig: "gastown", LandedAt: reworkProgramDay(t, "2026-11-04")})
	reviews := append(beforeReviews, afterReviews...)

	town, step := reworkProgramStep(t)
	doc := reworkProgramRunWith(t, town, step, reworkNumbersRecord{Landings: recs, Reviews: reviews})

	if doc.Numbers.Week != "2026-11-09" {
		t.Fatalf("numbers week %q, want the last complete week 2026-11-09", doc.Numbers.Week)
	}
	reworkProgramFigure(t, "numbers.two_rounds_share", doc.Numbers.TwoRoundsShare, 0.25)
	reworkProgramFigure(t, "numbers.mean_polecat_minutes", doc.Numbers.MeanPolecatMinutes, 10)

	lever := reworkProgramLever(t, doc, 2)
	reworkProgramFigure(t, "lever 2 before two_rounds_share", lever.Before.TwoRoundsShare, 0.5)
	reworkProgramFigure(t, "lever 2 after two_rounds_share", lever.After.TwoRoundsShare, 0.25)
	reworkProgramFigure(t, "lever 2 before mean_polecat_minutes", lever.Before.MeanPolecatMinutes, 20)
	reworkProgramFigure(t, "lever 2 after mean_polecat_minutes", lever.After.MeanPolecatMinutes, 10)
	reworkProgramFigure(t, "lever 2 two_rounds_share_delta", reworkNumbersFigure{Value: lever.TwoRoundsShareDelta}, -0.25)
	reworkProgramFigure(t, "lever 2 mean_polecat_minutes_delta", reworkNumbersFigure{Value: lever.MeanPolecatMinutesDelta}, -10)
}

// TestReworkProgramRunRefusesFailedRead: a record source that fails stops the
// run, and the previous document is not replaced.
func TestReworkProgramRunRefusesFailedRead(t *testing.T) {
	t.Parallel()
	town, step := reworkProgramStep(t)
	step.read = func() (reworkNumbersRecord, error) {
		return reworkNumbersRecord{}, errors.New("the ledger is unreadable")
	}
	err := step.run()
	if err == nil || !strings.Contains(err.Error(), "reading the rework program's records") {
		t.Fatalf("run error %v, want the failed read named", err)
	}
	if _, statErr := os.Stat(reworkProgramOut(t, town)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a failed read wrote a document: %v", statErr)
	}
}

// TestReworkProgramRunRefusesBadWindow: a window the report cannot build is an
// error, not a document with no numbers.
func TestReworkProgramRunRefusesBadWindow(t *testing.T) {
	t.Parallel()
	town, step := reworkProgramStep(t)
	step.weeks = 0
	step.read = func() (reworkNumbersRecord, error) { return reworkNumbersRecord{}, nil }
	err := step.run()
	if err == nil || !strings.Contains(err.Error(), "want at least 1") {
		t.Fatalf("run error %v, want the window refusal", err)
	}
	if _, statErr := os.Stat(reworkProgramOut(t, town)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a refused window wrote a document: %v", statErr)
	}
}

// TestReworkProgramRunRefusesNoCompleteWeek: a one-week window is the week the
// run falls in, which is still filling, so there is no figure week to write.
func TestReworkProgramRunRefusesNoCompleteWeek(t *testing.T) {
	t.Parallel()
	town, step := reworkProgramStep(t)
	step.weeks = 1
	step.read = func() (reworkNumbersRecord, error) { return reworkNumbersRecord{}, nil }
	err := step.run()
	if err == nil || !strings.Contains(err.Error(), "holds no complete week") {
		t.Fatalf("run error %v, want the no-complete-week refusal", err)
	}
	if _, statErr := os.Stat(reworkProgramOut(t, town)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a window with no complete week wrote a document: %v", statErr)
	}
}

// reworkProgramDocumentWithLeverWeek builds the document over a report computed
// with the real week function, then names the lever's landing week with weekOf.
// The report never sees the failing function, so the test does not depend on how
// many times reworkstats.Compute asks for a week.
func reworkProgramDocumentWithLeverWeek(t *testing.T, weekOf func(string) (string, error)) error {
	t.Helper()
	_, step := reworkProgramStep(t)
	recs, reviews := reworkProgramFigureWeek(t)
	recs = append(recs, landings.Record{Bead: "gt-a4yodt", Rig: "gastown", LandedAt: reworkProgramDay(t, "2026-11-04")})
	now := step.now()
	report, err := reworkstats.Compute(recs, reviews, now, step.weeks, metrics.WeekOf)
	if err != nil {
		t.Fatalf("computing the report: %v", err)
	}
	_, err = newReworkNumbersDocument(report, recs, 0, now, weekOf)
	return err
}

// TestReworkProgramDocumentRefusesUnnameableLeverWeek: a week function that fails
// on the lever's landing fails the document: a lever row cannot be left without
// the week its landing names.
func TestReworkProgramDocumentRefusesUnnameableLeverWeek(t *testing.T) {
	t.Parallel()
	err := reworkProgramDocumentWithLeverWeek(t, func(string) (string, error) {
		return "", errors.New("the clock cannot name this week")
	})
	if err == nil || !strings.Contains(err.Error(), "the week of gt-a4yodt's landing") ||
		!strings.Contains(err.Error(), "the clock cannot name this week") {
		t.Fatalf("document error %v, want the unnameable lever week and its cause named", err)
	}
}

// TestReworkProgramDocumentRefusesUnparsableLeverWeek: a week that is not a date
// is a failed document, not a lever row with an empty window.
func TestReworkProgramDocumentRefusesUnparsableLeverWeek(t *testing.T) {
	t.Parallel()
	err := reworkProgramDocumentWithLeverWeek(t, func(string) (string, error) { return "not-a-week", nil })
	if err == nil || !strings.Contains(err.Error(), `the week of gt-a4yodt's landing, "not-a-week"`) {
		t.Fatalf("document error %v, want the unparsable lever week named", err)
	}
}

// TestReworkProgramRunWritesNoDocumentWhenTheWeekFunctionFails: a run whose week
// function fails everywhere returns the error and leaves no document.
func TestReworkProgramRunWritesNoDocumentWhenTheWeekFunctionFails(t *testing.T) {
	t.Parallel()
	town, step := reworkProgramStep(t)
	recs, reviews := reworkProgramFigureWeek(t)
	step.weekOf = func(string) (string, error) { return "", errors.New("the clock cannot name this week") }
	step.read = func() (reworkNumbersRecord, error) {
		return reworkNumbersRecord{Landings: recs, Reviews: reviews}, nil
	}
	if err := step.run(); err == nil {
		t.Fatal("a run whose week function fails returned no error")
	}
	if _, statErr := os.Stat(reworkProgramOut(t, town)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a failed run wrote a document: %v", statErr)
	}
}

// TestReworkProgramLeverWithoutBead: a lever with no shipping bead, and the
// retired lever, carry the reason their row has no deltas.
func TestReworkProgramLeverWithoutBead(t *testing.T) {
	t.Parallel()
	town, step := reworkProgramStep(t)
	recs, reviews := reworkProgramFigureWeek(t)
	recs = append(recs, landings.Record{Bead: "gt-a4yodt", Rig: "gastown", LandedAt: reworkProgramDay(t, "2026-11-04")})
	doc := reworkProgramRunWith(t, town, step, reworkNumbersRecord{Landings: recs, Reviews: reviews})

	for _, lever := range []int{1, 3, 4, 5} {
		row := reworkProgramLever(t, doc, lever)
		if row.Reason != "no shipping bead recorded yet" {
			t.Fatalf("lever %d reason %q, want no shipping bead recorded yet", lever, row.Reason)
		}
		if row.TwoRoundsShareDelta != nil || row.MeanPolecatMinutesDelta != nil {
			t.Fatalf("lever %d has deltas over no shipping bead", lever)
		}
	}
	retired := reworkProgramLever(t, doc, 7)
	if retired.Reason != "retired by gt-dpl81b" {
		t.Fatalf("lever 7 reason %q, want retired by gt-dpl81b", retired.Reason)
	}
	if retired.TwoRoundsShareDelta != nil || retired.MeanPolecatMinutesDelta != nil {
		t.Fatal("the retired lever has deltas")
	}
}

// TestReworkProgramLeverUnlanded: a shipping bead with no landing record leaves
// its lever row a reason, and the other rows stand.
func TestReworkProgramLeverUnlanded(t *testing.T) {
	t.Parallel()
	town, step := reworkProgramStep(t)
	recs, reviews := reworkProgramFigureWeek(t)
	doc := reworkProgramRunWith(t, town, step, reworkNumbersRecord{Landings: recs, Reviews: reviews})

	if row := reworkProgramLever(t, doc, 2); row.Reason != "no landing record for gt-a4yodt" {
		t.Fatalf("lever 2 reason %q, want no landing record for gt-a4yodt", row.Reason)
	}
	if row := reworkProgramLever(t, doc, 6); row.Reason != "no landing record for gt-1zkwsb" {
		t.Fatalf("lever 6 reason %q, want no landing record for gt-1zkwsb", row.Reason)
	}
}

// TestReworkProgramLeverNoLandingTime: a landing record with no landed_at
// cannot date its lever.
func TestReworkProgramLeverNoLandingTime(t *testing.T) {
	t.Parallel()
	town, step := reworkProgramStep(t)
	recs, reviews := reworkProgramFigureWeek(t)
	recs = append(recs, landings.Record{Bead: "gt-a4yodt", Rig: "gastown"})
	doc := reworkProgramRunWith(t, town, step, reworkNumbersRecord{Landings: recs, Reviews: reviews})

	if row := reworkProgramLever(t, doc, 2); row.Reason != "the landing record for gt-a4yodt has no landed_at" {
		t.Fatalf("lever 2 reason %q, want the undated landing record named", row.Reason)
	}
	if doc.Records.NoLandingTime != 1 {
		t.Fatalf("no_landing_time %d, want 1", doc.Records.NoLandingTime)
	}
}

// TestReworkProgramLeverWeekStillFilling: the week after a lever that landed in
// the last complete week is the week the run falls in, so it is no figure week.
func TestReworkProgramLeverWeekStillFilling(t *testing.T) {
	t.Parallel()
	town, step := reworkProgramStep(t)
	recs, reviews := reworkProgramFigureWeek(t)
	recs = append(recs, landings.Record{Bead: "gt-a4yodt", Rig: "gastown", LandedAt: reworkProgramDay(t, "2026-11-10")})
	doc := reworkProgramRunWith(t, town, step, reworkNumbersRecord{Landings: recs, Reviews: reviews})

	row := reworkProgramLever(t, doc, 2)
	if row.After.Week != "2026-11-16" {
		t.Fatalf("lever 2 after week %q, want the week the run falls in", row.After.Week)
	}
	reworkProgramReason(t, "lever 2 after two_rounds_share", row.After.TwoRoundsShare, "the week is still filling, so it is no figure week")
	reworkProgramReason(t, "lever 2 after mean_polecat_minutes", row.After.MeanPolecatMinutes, "the week is still filling, so it is no figure week")
	if row.TwoRoundsShareDelta != nil || row.MeanPolecatMinutesDelta != nil {
		t.Fatal("a lever with a still-filling week has deltas")
	}
}

// TestReworkProgramLeverWeekOutsideWindow: a lever landed in the window's first
// week has its before week outside the report.
func TestReworkProgramLeverWeekOutsideWindow(t *testing.T) {
	t.Parallel()
	town, step := reworkProgramStep(t)
	recs, reviews := reworkProgramFigureWeek(t)
	recs = append(recs, landings.Record{Bead: "gt-a4yodt", Rig: "gastown", LandedAt: reworkProgramDay(t, "2026-09-29")})
	doc := reworkProgramRunWith(t, town, step, reworkNumbersRecord{Landings: recs, Reviews: reviews})

	row := reworkProgramLever(t, doc, 2)
	if row.Before.Week != "2026-09-21" {
		t.Fatalf("lever 2 before week %q, want 2026-09-21", row.Before.Week)
	}
	reworkProgramReason(t, "lever 2 before two_rounds_share", row.Before.TwoRoundsShare, "the week is outside the window")
	reworkProgramReason(t, "lever 2 before mean_polecat_minutes", row.Before.MeanPolecatMinutes, "the week is outside the window")
	if row.TwoRoundsShareDelta != nil || row.MeanPolecatMinutesDelta != nil {
		t.Fatal("a lever with a week outside the window has deltas")
	}
}

// TestReworkProgramLeverWindowNoTimedLanding: a lever window with enough beads
// but no polecat time gives the share and leaves the mean nil with its reason.
func TestReworkProgramLeverWindowNoTimedLanding(t *testing.T) {
	t.Parallel()
	town, step := reworkProgramStep(t)
	untimed, untimedReviews := reworkProgramWeek(t, "untimed", "2026-10-26", 20, 0, 0)
	recs, reviews := reworkProgramFigureWeek(t)
	recs = append(recs, untimed...)
	reviews = append(reviews, untimedReviews...)
	recs = append(recs, landings.Record{Bead: "gt-a4yodt", Rig: "gastown", LandedAt: reworkProgramDay(t, "2026-11-04")})
	doc := reworkProgramRunWith(t, town, step, reworkNumbersRecord{Landings: recs, Reviews: reviews})

	row := reworkProgramLever(t, doc, 2)
	reworkProgramFigure(t, "lever 2 before two_rounds_share", row.Before.TwoRoundsShare, 0)
	reworkProgramReason(t, "lever 2 before mean_polecat_minutes", row.Before.MeanPolecatMinutes, "no landing in the week carries a polecat time")
	if row.MeanPolecatMinutesDelta != nil {
		t.Fatal("a lever over an untimed mean has a mean delta")
	}
}

// TestReworkProgramLeverWeekTooThin: a before week under twenty beads is too
// thin for a figure, so the lever has no delta over it.
func TestReworkProgramLeverWeekTooThin(t *testing.T) {
	t.Parallel()
	town, step := reworkProgramStep(t)
	thin, thinReviews := reworkProgramWeek(t, "thin", "2026-10-26", 19, 4, 600)
	recs, reviews := reworkProgramFigureWeek(t)
	recs = append(recs, thin...)
	reviews = append(reviews, thinReviews...)
	recs = append(recs, landings.Record{Bead: "gt-a4yodt", Rig: "gastown", LandedAt: reworkProgramDay(t, "2026-11-04")})
	doc := reworkProgramRunWith(t, town, step, reworkNumbersRecord{Landings: recs, Reviews: reviews})

	row := reworkProgramLever(t, doc, 2)
	if row.Before.Beads != 19 {
		t.Fatalf("lever 2 before beads %d, want 19", row.Before.Beads)
	}
	reworkProgramReason(t, "lever 2 before two_rounds_share", row.Before.TwoRoundsShare, "too few beads in the week to say")
	if row.TwoRoundsShareDelta != nil || row.MeanPolecatMinutesDelta != nil {
		t.Fatal("a lever over a too-thin week has deltas")
	}
}

// TestReworkProgramNumbersTooThin: the figure week under twenty beads gives a
// reason, never a zero share or a mean over nothing.
func TestReworkProgramNumbersTooThin(t *testing.T) {
	t.Parallel()
	town, step := reworkProgramStep(t)
	recs, reviews := reworkProgramWeek(t, "thin", "2026-11-09", 19, 4, 600)
	doc := reworkProgramRunWith(t, town, step, reworkNumbersRecord{Landings: recs, Reviews: reviews})

	if doc.Numbers.Beads != 19 {
		t.Fatalf("numbers beads %d, want 19", doc.Numbers.Beads)
	}
	reworkProgramReason(t, "numbers.two_rounds_share", doc.Numbers.TwoRoundsShare, "too few beads in the week to say")
	reworkProgramReason(t, "numbers.mean_polecat_minutes", doc.Numbers.MeanPolecatMinutes, "too few beads in the week to say")
}

// TestReworkProgramNumbersNoTimedLanding: a week with enough beads but no
// polecat time gives the share and leaves the mean nil with its reason.
func TestReworkProgramNumbersNoTimedLanding(t *testing.T) {
	t.Parallel()
	town, step := reworkProgramStep(t)
	recs, reviews := reworkProgramWeek(t, "untimed", "2026-11-09", 20, 0, 0)
	doc := reworkProgramRunWith(t, town, step, reworkNumbersRecord{Landings: recs, Reviews: reviews})

	reworkProgramFigure(t, "numbers.two_rounds_share", doc.Numbers.TwoRoundsShare, 0)
	reworkProgramReason(t, "numbers.mean_polecat_minutes", doc.Numbers.MeanPolecatMinutes, "no landing in the week carries a polecat time")
	if doc.Numbers.Timed != 0 || doc.Numbers.Untimed != 20 {
		t.Fatalf("timed %d, untimed %d, want 0 and 20", doc.Numbers.Timed, doc.Numbers.Untimed)
	}
}

// TestReworkProgramReadersCountUnreadable: the town's own readers count a
// landings line and a review line that are not records, and skip a queue line.
func TestReworkProgramReadersCountUnreadable(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	dir := filepath.Join(town, ".runtime", "landings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	landing := `{"bead":"gt-1","rig":"gastown","landed_at":"2026-11-11T12:00:00Z","polecat_secs":600}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gastown.jsonl"), []byte(landing+"not a record\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reviews := strings.Join([]string{
		`{"bead":"gt-1","rig":"gastown","attempt":2}`,
		"not a record",
		`{"followup_queue":{"findings":[]}}`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "reviews.jsonl"), []byte(reviews), 0o644); err != nil {
		t.Fatal(err)
	}

	step := newReworkNumbersStep(town, reworkProgramNow)
	if err := step.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	data, err := os.ReadFile(reworkProgramOut(t, town))
	if err != nil {
		t.Fatal(err)
	}
	var doc reworkNumbersDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("the document is not JSON: %v", err)
	}
	if doc.Records.UnreadableLines != 2 {
		t.Fatalf("unreadable_lines %d, want the landings line and the review line", doc.Records.UnreadableLines)
	}
}

// TestReworkProgramReadersMissingFilesAreEmpty: a town that never landed has no
// records and no error, and the document says why its figures are absent.
func TestReworkProgramReadersMissingFilesAreEmpty(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	step := newReworkNumbersStep(town, reworkProgramNow)
	if err := step.run(); err != nil {
		t.Fatalf("run over a town with no records: %v", err)
	}
	data, err := os.ReadFile(reworkProgramOut(t, town))
	if err != nil {
		t.Fatal(err)
	}
	var doc reworkNumbersDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("the document is not JSON: %v", err)
	}
	if doc.Records.UnreadableLines != 0 || doc.Records.OutsideWindow != 0 || doc.Records.NoLandingTime != 0 {
		t.Fatalf("record counts %+v, want all zero", doc.Records)
	}
	reworkProgramReason(t, "numbers.two_rounds_share", doc.Numbers.TwoRoundsShare, "too few beads in the week to say")
}

// TestReworkProgramReadersUnopenableLanding: a ledger that cannot be opened is
// a failed read the run reports, not an empty rig.
func TestReworkProgramReadersUnopenableLanding(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	dir := filepath.Join(town, ".runtime", "landings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(dir, "gastown.jsonl")
	if err := os.Symlink(ledger, ledger); err != nil {
		t.Fatalf("a self-referential symlink is how this test breaks a read: %v", err)
	}
	err := newReworkNumbersStep(town, reworkProgramNow).run()
	if err == nil || !strings.Contains(err.Error(), "opening landings file") {
		t.Fatalf("run error %v, want the unopenable ledger named", err)
	}
}

// TestReworkProgramReadersUnopenableReview: a review log that cannot be opened
// is a failed read the run reports, not a town with no verdicts.
func TestReworkProgramReadersUnopenableReview(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	dir := filepath.Join(town, ".runtime", "landings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "reviews.jsonl")
	if err := os.Symlink(log, log); err != nil {
		t.Fatalf("a self-referential symlink is how this test breaks a read: %v", err)
	}
	err := newReworkNumbersStep(town, reworkProgramNow).run()
	if err == nil || !strings.Contains(err.Error(), "opening the review records") {
		t.Fatalf("run error %v, want the unopenable review log named", err)
	}
}

// TestReworkProgramReadersUnreadableReview: a review log that opens but cannot
// be read fails the run too.
func TestReworkProgramReadersUnreadableReview(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A directory opens as a file and fails on its first read.
	if err := os.MkdirAll(filepath.Join(town, ".runtime", "landings", "reviews.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := newReworkNumbersStep(town, reworkProgramNow).run()
	if err == nil || !strings.Contains(err.Error(), "reading the review records") {
		t.Fatalf("run error %v, want the unreadable review log named", err)
	}
}

// TestReworkProgramReadersUnreadableDir: a landings path that is not a
// directory is a failed read the run reports, not an empty week.
func TestReworkProgramReadersUnreadableDir(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, ".runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, ".runtime", "landings"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	step := newReworkNumbersStep(town, reworkProgramNow)
	err := step.run()
	if err == nil || !strings.Contains(err.Error(), "reading the landings directory") {
		t.Fatalf("run error %v, want the unreadable landings directory named", err)
	}
}

// TestReworkProgramRunRefusesUnwritableDocument: a document path that cannot be
// a file is the run's error, never a silent success.
func TestReworkProgramRunRefusesUnwritableDocument(t *testing.T) {
	t.Parallel()
	town, step := reworkProgramStep(t)
	if err := os.MkdirAll(filepath.Join(town, ".runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, ".runtime", "rework-numbers"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, reviews := reworkProgramFigureWeek(t)
	step.read = func() (reworkNumbersRecord, error) {
		return reworkNumbersRecord{Landings: recs, Reviews: reviews}, nil
	}
	err := step.run()
	if err == nil || !strings.Contains(err.Error(), "writing the rework program's document") {
		t.Fatalf("run error %v, want the failed write named", err)
	}
}

// reworkProgramRead is the document a run left in town.
func reworkProgramRead(t *testing.T, town string) reworkNumbersDocument {
	t.Helper()
	data, err := os.ReadFile(reworkProgramOut(t, town))
	if err != nil {
		t.Fatalf("the town has no document: %v", err)
	}
	var doc reworkNumbersDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("the document is not JSON: %v", err)
	}
	return doc
}

// reworkNumbersTestDaemon builds a daemon whose rework_numbers patrol is
// enabled, over town.
func reworkNumbersTestDaemon(t *testing.T, town string, logged io.Writer) *Daemon {
	t.Helper()
	return &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logged, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			ReworkNumbers: &agentconfig.ReworkNumbersConfig{Enabled: true},
		}},
	}
}

// reworkProgramSource is the tick's step over a fake record source: the figure
// week, counting each read of it.
func reworkProgramSource(t *testing.T, read *int) reworkNumbersStepFunc {
	t.Helper()
	recs, reviews := reworkProgramFigureWeek(t)
	return func(town string, now func() time.Time) *reworkNumbersStep {
		step := newReworkNumbersStep(town, now)
		step.read = func() (reworkNumbersRecord, error) {
			*read = *read + 1
			return reworkNumbersRecord{Landings: recs, Reviews: reviews}, nil
		}
		return step
	}
}

// TestReworkProgramRunInsideTheWeekWritesNothing is the bead's acceptance: a
// tick in the week the last-run record names does nothing, and it is the town's
// record that decides, so the next tick's step — a fresh one, as a restarted
// daemon builds — decides as the first did.
func TestReworkProgramRunInsideTheWeekWritesNothing(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	read := 0
	d := reworkNumbersTestDaemon(t, town, io.Discard)
	d.reworkNumbersStepFor = reworkProgramSource(t, &read)

	ran, _, err := d.runReworkNumbersTick(reworkProgramNow())
	if err != nil || !ran {
		t.Fatalf("the week's first tick = %v, %v; want a run with no error", ran, err)
	}
	wrote := reworkProgramRead(t, town)
	if !wrote.GeneratedAt.Equal(reworkProgramNow()) {
		t.Fatalf("the first run generated %s, want %s", wrote.GeneratedAt, reworkProgramNow())
	}

	later := reworkProgramNow().Add(6 * time.Hour)
	ran, note, err := d.runReworkNumbersTick(later)
	if err != nil {
		t.Fatalf("the tick inside the week: %v", err)
	}
	if ran {
		t.Fatal("a tick inside the week ran, want it to do nothing")
	}
	if read != 1 {
		t.Errorf("a tick inside the week read the town's records %d times, want the first tick's one", read)
	}
	if !strings.Contains(note, "already ran") {
		t.Errorf("the tick inside the week said %q, want the week named as already run", note)
	}
	if after := reworkProgramRead(t, town); !after.GeneratedAt.Equal(wrote.GeneratedAt) {
		t.Fatalf("the document was rewritten at %s, want the first run's %s", after.GeneratedAt, wrote.GeneratedAt)
	}
}

// TestReworkProgramRunInTheNextWeekWritesAgain: the record holds the week, not
// a countdown, so the next week's first tick runs and replaces the document.
func TestReworkProgramRunInTheNextWeekWritesAgain(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	read := 0
	d := reworkNumbersTestDaemon(t, town, io.Discard)
	d.reworkNumbersStepFor = reworkProgramSource(t, &read)

	if ran, _, err := d.runReworkNumbersTick(reworkProgramNow()); err != nil || !ran {
		t.Fatalf("the first week's tick = %v, %v; want a run with no error", ran, err)
	}

	nextWeek := reworkProgramNow().AddDate(0, 0, 7)
	ran, note, err := d.runReworkNumbersTick(nextWeek)
	if err != nil || !ran {
		t.Fatalf("the next week's tick = %v, %v; want a run with no error", ran, err)
	}
	if !strings.Contains(note, "is due") {
		t.Errorf("the next week's tick said %q, want the new week named as due", note)
	}
	if read != 2 {
		t.Errorf("the two weeks read the town's records %d times, want one read each", read)
	}
	if doc := reworkProgramRead(t, town); !doc.GeneratedAt.Equal(nextWeek) {
		t.Fatalf("the document was generated at %s, want the next week's run at %s", doc.GeneratedAt, nextWeek)
	}
}

// TestReworkProgramUnreadableLastRunRunsTheWeek: a record the town cannot read
// makes the week due and says why, rather than skipping it on a broken file.
func TestReworkProgramUnreadableLastRunRunsTheWeek(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	read := 0
	d := reworkNumbersTestDaemon(t, town, io.Discard)
	d.reworkNumbersStepFor = reworkProgramSource(t, &read)
	record := reworkNumbersLastRunPath(town)
	if err := os.MkdirAll(filepath.Dir(record), 0o755); err != nil {
		t.Fatalf("the last-run directory: %v", err)
	}
	if err := os.WriteFile(record, []byte("{not a record"), 0o644); err != nil {
		t.Fatalf("the broken record: %v", err)
	}

	ran, note, err := d.runReworkNumbersTick(reworkProgramNow())
	if err != nil || !ran {
		t.Fatalf("a week with a broken record = %v, %v; want a run with no error", ran, err)
	}
	if !strings.Contains(note, "cannot be read") {
		t.Errorf("a broken record said %q, want the unreadable record named", note)
	}
	reworkProgramRead(t, town)
}

// TestReworkProgramRefusesUnnameableRunWeek: a clock whose week cannot be named
// stops the tick before it reads anything, and writes no document.
func TestReworkProgramRefusesUnnameableRunWeek(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	read := 0
	d := reworkNumbersTestDaemon(t, town, io.Discard)
	source := reworkProgramSource(t, &read)
	d.reworkNumbersStepFor = func(town string, now func() time.Time) *reworkNumbersStep {
		step := source(town, now)
		step.weekOf = func(string) (string, error) { return "", errors.New("no calendar here") }
		return step
	}

	ran, _, err := d.runReworkNumbersTick(reworkProgramNow())
	if ran || err == nil || !strings.Contains(err.Error(), "the week of") {
		t.Fatalf("an unnameable week = %v, %v; want no run and the week named", ran, err)
	}
	if read != 0 {
		t.Errorf("an unnameable week read the town's records %d times, want none", read)
	}
	if _, statErr := os.Stat(reworkProgramOut(t, town)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("an unnameable week wrote a document: %v", statErr)
	}
}

// TestReworkProgramRunReportsARecordItCannotWrite: the document lands, and the
// run still fails, naming the record: a silent failure would leave the next tick
// running the week again with nothing said about why.
func TestReworkProgramRunReportsARecordItCannotWrite(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	read := 0
	d := reworkNumbersTestDaemon(t, town, io.Discard)
	d.reworkNumbersStepFor = reworkProgramSource(t, &read)
	// The record's path is held by a directory, so no rename can land on it.
	if err := os.MkdirAll(reworkNumbersLastRunPath(town), 0o755); err != nil {
		t.Fatalf("the record's path: %v", err)
	}

	ran, _, err := d.runReworkNumbersTick(reworkProgramNow())
	if !ran || err == nil || !strings.Contains(err.Error(), "recording the rework program's run") {
		t.Fatalf("an unwritable record = %v, %v; want the document written and the record named", ran, err)
	}
	reworkProgramRead(t, town)
}

// TestReworkProgramReworkNumbersPatrolStartsOnlyWhenEnabled is the bead's
// second acceptance: the patrol's own ticker exists only when
// daemon.patrols.rework_numbers.enabled is true, and a town that names no key
// starts nothing.
func TestReworkProgramReworkNumbersPatrolStartsOnlyWhenEnabled(t *testing.T) {
	t.Parallel()
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(io.Discard, "", 0),
	}
	if channel, stop := d.startReworkNumbersTicker(); channel != nil || stop != nil {
		t.Error("a town that names no rework_numbers started its ticker")
	}

	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		ReworkNumbers: &agentconfig.ReworkNumbersConfig{Enabled: false},
	}}
	if channel, stop := d.startReworkNumbersTicker(); channel != nil || stop != nil {
		t.Error("a disabled rework_numbers started its ticker")
	}

	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		ReworkNumbers: &agentconfig.ReworkNumbersConfig{Enabled: true},
	}}
	channel, stop := d.startReworkNumbersTicker()
	if channel == nil || stop == nil {
		t.Fatal("an enabled rework_numbers started no ticker")
	}
	stop()
}

// TestReworkProgramReworkNumbersTickRunsTheStepOnce: one started tick runs the
// step once, logs what it did, and frees the guard; a tick that arrives while a
// run is still going is skipped and logged; and a failed run is logged rather
// than counted as a document.
func TestReworkProgramReworkNumbersTickRunsTheStepOnce(t *testing.T) {
	t.Parallel()

	t.Run("one tick, one run", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		read := 0
		d := reworkNumbersTestDaemon(t, t.TempDir(), &logged)
		d.reworkNumbersStepFor = reworkProgramSource(t, &read)

		d.startReworkNumbersTick()
		d.reworkNumbersCycles.Wait()

		if read != 1 {
			t.Errorf("a tick read the town's records %d times, want once", read)
		}
		if d.reworkNumbersRunning.Load() {
			t.Error("the guard stayed set after the run ended, so no later tick could run")
		}
		if !strings.Contains(logged.String(), "wrote this week's document") {
			t.Errorf("the tick logged %q, want the document it wrote", logged.String())
		}
	})

	t.Run("a tick while one runs is skipped", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		d := reworkNumbersTestDaemon(t, t.TempDir(), &logged)
		reached := make(chan struct{}, 1)
		release := make(chan struct{})
		var reads atomic.Int32
		d.reworkNumbersStepFor = func(town string, now func() time.Time) *reworkNumbersStep {
			step := newReworkNumbersStep(town, now)
			step.read = func() (reworkNumbersRecord, error) {
				reads.Add(1)
				reached <- struct{}{}
				<-release
				return reworkNumbersRecord{}, errors.New("the ledger is unreadable")
			}
			return step
		}

		d.startReworkNumbersTick()
		waitForSignal(t, reached, "the first tick never read the records")
		d.startReworkNumbersTick()
		if !strings.Contains(logged.String(), "previous run still going, skipping") {
			t.Errorf("the second tick was not logged as skipped: %q", logged.String())
		}
		close(release)
		d.reworkNumbersCycles.Wait()
		if got := reads.Load(); got != 1 {
			t.Errorf("two starting ticks read the records %d times, want once", got)
		}
	})

	t.Run("a failed run is logged, not counted", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		d := reworkNumbersTestDaemon(t, t.TempDir(), &logged)
		d.reworkNumbersStepFor = func(town string, now func() time.Time) *reworkNumbersStep {
			step := newReworkNumbersStep(town, now)
			step.read = func() (reworkNumbersRecord, error) {
				return reworkNumbersRecord{}, errors.New("the landing ledger is unreadable")
			}
			return step
		}

		d.startReworkNumbersTick()
		d.reworkNumbersCycles.Wait()

		if !strings.Contains(logged.String(), "the landing ledger is unreadable") {
			t.Errorf("a failed run logged %q, want the error named", logged.String())
		}
		if strings.Contains(logged.String(), "wrote this week's document") {
			t.Errorf("a failed run logged a written document: %q", logged.String())
		}
	})
}

// TestReworkProgramReworkNumbersStopJoinsTheRun: Run's return stops the ticker
// and waits for the run it started, and a run still going at the bound is
// logged and left.
func TestReworkProgramReworkNumbersStopJoinsTheRun(t *testing.T) {
	t.Parallel()

	blocking := func(t *testing.T, logged io.Writer) (*Daemon, chan struct{}, func()) {
		t.Helper()
		reached := make(chan struct{}, 1)
		release := make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		d := reworkNumbersTestDaemon(t, t.TempDir(), logged)
		d.reworkNumbersStepFor = func(town string, now func() time.Time) *reworkNumbersStep {
			step := newReworkNumbersStep(town, now)
			step.read = func() (reworkNumbersRecord, error) {
				reached <- struct{}{}
				<-release
				return reworkNumbersRecord{}, errors.New("the ledger is unreadable")
			}
			return step
		}
		t.Cleanup(func() {
			unblock()
			d.reworkNumbersCycles.Wait()
		})
		return d, reached, unblock
	}

	t.Run("the run the ticker started is joined", func(t *testing.T) {
		t.Parallel()
		d, reached, unblock := blocking(t, io.Discard)
		d.reworkNumbersJoinWait = time.Hour

		d.startReworkNumbersTick()
		waitForSignal(t, reached, "the run never read the records")

		stopped := make(chan struct{})
		stopCalled := make(chan struct{})
		go func() {
			d.stopReworkNumbersPatrol(func() { close(stopCalled) })
			close(stopped)
		}()
		waitForSignal(t, stopCalled, "the stop func never ran")
		select {
		case <-stopped:
			t.Fatal("stopReworkNumbersPatrol returned while the run still went")
		default:
		}
		unblock()
		waitForSignal(t, stopped, "stopReworkNumbersPatrol never returned after the run ended")
		if d.reworkNumbersRunning.Load() {
			t.Error("the join returned with the run's guard still set")
		}
	})

	t.Run("a run past the bound is logged and left", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		d, reached, unblock := blocking(t, &logged)
		d.reworkNumbersJoinWait = 30 * time.Millisecond

		d.startReworkNumbersTick()
		waitForSignal(t, reached, "the run never read the records")

		stopped := make(chan struct{})
		go func() {
			d.stopReworkNumbersPatrol(nil)
			close(stopped)
		}()
		waitForSignal(t, stopped, "stopReworkNumbersPatrol never returned past the bound")
		if !strings.Contains(logged.String(), "still going") {
			t.Errorf("a run left past the bound was not logged: %q", logged.String())
		}
		unblock()
		d.reworkNumbersCycles.Wait()
	})
}
