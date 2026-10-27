//go:build unix

package spans

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var testEpoch = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

type fixedClock struct {
	at time.Time
}

func (c fixedClock) Now() time.Time { return c.at }

// stepClock moves the start time between attempts.
type stepClock struct {
	at time.Time
}

func (c *stepClock) Now() time.Time { return c.at }

// writeLog plants a log the production writers did not write, so a fixture can
// put lines in an order Start and End would not produce.
func writeLog(t *testing.T, dir, bead, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, bead+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// logLines encodes events as one JSONL body in the order given.
func logLines(t *testing.T, evs ...event) string {
	t.Helper()
	var b strings.Builder
	for _, ev := range evs {
		line, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimSuffix(string(data), "\n")
	if body == "" {
		return nil
	}
	return strings.Split(body, "\n")
}

func TestStartEnd_OneCleanAttempt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := New(dir, fixedClock{at: testEpoch})

	if err := st.Start("gt-abc", "organic"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := st.End("gt-abc", "organic", ReasonDone, testEpoch.Add(90*time.Second)); err != nil {
		t.Fatalf("End: %v", err)
	}

	sum, err := st.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Secs != 90 {
		t.Errorf("Secs = %v, want 90", sum.Secs)
	}
	if len(sum.Attempts) != 1 || sum.Attempts[0].Seat != "organic" ||
		sum.Attempts[0].Secs != 90 || sum.Attempts[0].LowerBound {
		t.Errorf("Attempts = %+v, want one closed 90s attempt by organic", sum.Attempts)
	}
	if sum.LowerBound || sum.Skipped != 0 || sum.Corrupt != 0 {
		t.Errorf("LowerBound/Skipped/Corrupt = %v/%d/%d, want false/0/0",
			sum.LowerBound, sum.Skipped, sum.Corrupt)
	}

	lines := readLines(t, filepath.Join(dir, "gt-abc.jsonl"))
	if len(lines) != 2 {
		t.Fatalf("log has %d lines, want 2", len(lines))
	}
	var start, end event
	if err := json.Unmarshal([]byte(lines[0]), &start); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &end); err != nil {
		t.Fatal(err)
	}
	if start.Event != EventStart || end.Event != EventEnd {
		t.Errorf("events = %q, %q; want start, end", start.Event, end.Event)
	}
	if start.At != "2026-10-25T12:00:00Z" || end.At != "2026-10-25T12:01:30Z" {
		t.Errorf("at = %q, %q; want RFC3339 UTC", start.At, end.At)
	}
	if end.Reason != string(ReasonDone) || end.LowerBound {
		t.Errorf("end reason/lower_bound = %q/%v, want done/false", end.Reason, end.LowerBound)
	}
}

func TestStart_FormatsAtInUTC(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	zone := time.FixedZone("UTC+2", 2*60*60)
	st := New(dir, fixedClock{at: testEpoch.In(zone)})

	if err := st.Start("gt-abc", "organic"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	lines := readLines(t, filepath.Join(dir, "gt-abc.jsonl"))
	if len(lines) != 1 {
		t.Fatalf("log has %d lines, want 1", len(lines))
	}
	var ev event
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.At != "2026-10-25T12:00:00Z" {
		t.Errorf("at = %q, want the UTC spelling 2026-10-25T12:00:00Z", ev.At)
	}
}

func TestSummarize_TwoAttemptsSummed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	clk := &stepClock{at: testEpoch}
	st := New(dir, clk)

	if err := st.Start("gt-abc", "organic"); err != nil {
		t.Fatal(err)
	}
	if err := st.End("gt-abc", "organic", ReasonEscalated, testEpoch.Add(60*time.Second)); err != nil {
		t.Fatal(err)
	}
	// The rework round starts where the first attempt left off.
	clk.at = testEpoch.Add(60 * time.Second)
	if err := st.Start("gt-abc", "dipper"); err != nil {
		t.Fatal(err)
	}
	if err := st.End("gt-abc", "dipper", ReasonDone, testEpoch.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}

	sum, err := st.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Secs != 90 {
		t.Errorf("Secs = %v, want the 60s and 30s attempts summed to 90", sum.Secs)
	}
	want := []Attempt{
		{Seat: "organic", StartedAt: testEpoch, Secs: 60},
		{Seat: "dipper", StartedAt: testEpoch.Add(60 * time.Second), Secs: 30},
	}
	if len(sum.Attempts) != len(want) {
		t.Fatalf("Attempts = %+v, want %+v", sum.Attempts, want)
	}
	for i := range want {
		if sum.Attempts[i] != want[i] {
			t.Errorf("Attempts[%d] = %+v, want %+v", i, sum.Attempts[i], want[i])
		}
	}
	if sum.LowerBound {
		t.Error("LowerBound = true, want false for two closed attempts")
	}
}

func TestSummarize_OpenAttemptIsLowerBound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := New(dir, fixedClock{at: testEpoch})

	if err := st.Start("gt-abc", "organic"); err != nil {
		t.Fatal(err)
	}

	sum, err := st.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Secs != 0 {
		t.Errorf("Secs = %v, want 0 for an attempt with no end", sum.Secs)
	}
	if len(sum.Attempts) != 1 {
		t.Fatalf("Attempts = %+v, want the open attempt listed", sum.Attempts)
	}
	if a := sum.Attempts[0]; a.Seat != "organic" || a.Secs != 0 || !a.LowerBound {
		t.Errorf("Attempts[0] = %+v, want organic with zero Secs and LowerBound", a)
	}
	if !sum.LowerBound {
		t.Error("LowerBound = false, want true while an attempt is open")
	}
}

// TestSummarize_ClosedAndOpenSpans: a bead whose last attempt is still open
// keeps the lower bound, and its Secs are the closed attempts' alone — the open
// one adds no time rather than the time to now, which is not work (gt-8yk78).
func TestSummarize_ClosedAndOpenSpans(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	clk := &stepClock{at: testEpoch}
	st := New(dir, clk)

	if err := st.Start("gt-abc", "organic"); err != nil {
		t.Fatal(err)
	}
	if err := st.End("gt-abc", "organic", ReasonDone, testEpoch.Add(45*time.Second)); err != nil {
		t.Fatal(err)
	}
	clk.at = testEpoch.Add(45 * time.Second)
	if err := st.Start("gt-abc", "dipper"); err != nil {
		t.Fatal(err)
	}

	sum, err := st.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Secs != 45 {
		t.Errorf("Secs = %v, want the closed attempt's 45", sum.Secs)
	}
	if !sum.LowerBound {
		t.Error("LowerBound = false, want true while the last attempt is open")
	}
	want := []Attempt{
		{Seat: "organic", StartedAt: testEpoch, Secs: 45},
		{Seat: "dipper", StartedAt: testEpoch.Add(45 * time.Second), Open: true, LowerBound: true},
	}
	if len(sum.Attempts) != len(want) {
		t.Fatalf("Attempts = %+v, want %+v", sum.Attempts, want)
	}
	for i := range want {
		if sum.Attempts[i] != want[i] {
			t.Errorf("Attempts[%d] = %+v, want %+v", i, sum.Attempts[i], want[i])
		}
	}
}

// TestEndOpen_ClosesTheSeatsOpenAttempt: the paths that end an attempt for a
// seat that cannot end it itself (an overrun stop, a confirmed death) write
// the end through EndOpen, which reports the attempt it found.
func TestEndOpen_ClosesTheSeatsOpenAttempt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := New(dir, fixedClock{at: testEpoch})
	if err := st.Start("gt-abc", "organic"); err != nil {
		t.Fatal(err)
	}

	closed, err := st.EndOpen("gt-abc", "organic", ReasonOverrun, testEpoch.Add(90*time.Second))
	if err != nil {
		t.Fatalf("EndOpen: %v", err)
	}
	if !closed {
		t.Fatal("EndOpen = false, want the open attempt found and closed")
	}

	sum, err := st.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Secs != 90 || sum.Skipped != 0 || sum.LowerBound {
		t.Errorf("Secs/Skipped/LowerBound = %v/%d/%v, want 90/0/false", sum.Secs, sum.Skipped, sum.LowerBound)
	}
	if len(sum.Attempts) != 1 || sum.Attempts[0].Open {
		t.Errorf("Attempts = %+v, want the one attempt closed", sum.Attempts)
	}
}

// TestEndOpen_NoOpenAttemptWritesNothing: an attempt a seat already ended (a
// repeated stop, a gt done that got there first) is not closed twice, and a
// bead with no attempt at all gains no log.
func TestEndOpen_NoOpenAttemptWritesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := New(dir, fixedClock{at: testEpoch})
	if err := st.Start("gt-abc", "organic"); err != nil {
		t.Fatal(err)
	}
	if err := st.End("gt-abc", "organic", ReasonDone, testEpoch.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}

	for _, bead := range []string{"gt-abc", "gt-never-started"} {
		closed, err := st.EndOpen(bead, "organic", ReasonDead, testEpoch.Add(time.Hour))
		if err != nil {
			t.Fatalf("EndOpen(%q): %v", bead, err)
		}
		if closed {
			t.Errorf("EndOpen(%q) = true, want no open attempt to close", bead)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "gt-never-started.jsonl")); !os.IsNotExist(err) {
		t.Errorf("a bead with no attempt gained a log: %v", err)
	}
	sum, err := st.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Secs != 30 || sum.Skipped != 0 {
		t.Errorf("Secs/Skipped = %v/%d, want the single 30s attempt and no skipped end", sum.Secs, sum.Skipped)
	}
}

// TestEndOpen_DatesAnOldEndAtTheAttemptStart: the time EndOpen is given can be
// second-hand evidence from before the attempt began (a stale liveness record
// from an earlier attempt). An end dated there would be skipped on read, so it
// is moved to the start and the attempt still closes as a lower bound.
func TestEndOpen_DatesAnOldEndAtTheAttemptStart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := New(dir, fixedClock{at: testEpoch})
	if err := st.Start("gt-abc", "organic"); err != nil {
		t.Fatal(err)
	}

	closed, err := st.EndOpen("gt-abc", "organic", ReasonDead, testEpoch.Add(-5*time.Minute))
	if err != nil {
		t.Fatalf("EndOpen: %v", err)
	}
	if !closed {
		t.Fatal("EndOpen = false, want the open attempt closed")
	}

	sum, err := st.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Secs != 0 || sum.Skipped != 0 || !sum.LowerBound {
		t.Errorf("Secs/Skipped/LowerBound = %v/%d/%v, want 0/0/true", sum.Secs, sum.Skipped, sum.LowerBound)
	}
	if len(sum.Attempts) != 1 || sum.Attempts[0].Open {
		t.Errorf("Attempts = %+v, want the attempt closed rather than the end skipped", sum.Attempts)
	}
}

// TestEndOpen_RefusesABeadThatIsNotAPathComponent: the bead id reaches a
// filename only here, so the end path validates it as the start path does.
func TestEndOpen_RefusesABeadThatIsNotAPathComponent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := New(dir, fixedClock{at: testEpoch})

	if _, err := st.EndOpen("../escape", "organic", ReasonDead, testEpoch); err == nil {
		t.Fatal("EndOpen accepted a bead id that is not a path component")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("EndOpen wrote under %s: entries %v, err %v", dir, entries, err)
	}
}

// TestEndSeatOpen_ClosesTheAttemptTheSeatLeftOnItsBead: a stop knows a
// polecat's name, not the bead it held, and must not ask Dolt — so the store
// finds the bead in its own logs and ends the attempt there. The close is a
// lower bound, because a resumed seat continues it (gt-8yk78).
func TestEndSeatOpen_ClosesTheAttemptTheSeatLeftOnItsBead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := New(dir, fixedClock{at: testEpoch})
	if err := st.Start("gt-abc", "gastown/onyx"); err != nil {
		t.Fatal(err)
	}
	if err := st.Start("gt-abc", "gastown/other"); err != nil {
		t.Fatal(err)
	}

	bead, ended, err := st.EndSeatOpen("gastown/onyx", ReasonStopped, testEpoch.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("EndSeatOpen: %v", err)
	}
	if !ended || bead != "gt-abc" {
		t.Fatalf("EndSeatOpen = %q, %v; want gt-abc closed", bead, ended)
	}

	sum, err := st.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(sum.Attempts) != 2 {
		t.Fatalf("Attempts = %+v, want both seats' attempts", sum.Attempts)
	}
	for _, a := range sum.Attempts {
		switch a.Seat {
		case "gastown/onyx":
			if a.Open || !a.LowerBound || a.Secs != 120 {
				t.Errorf("stopped attempt = %+v, want 120s closed as a lower bound", a)
			}
		case "gastown/other":
			if !a.Open {
				t.Errorf("attempt = %+v, want another seat's attempt left open", a)
			}
		}
	}
}

// TestEndSeatOpen_ClosesOnlyTheNewestAttempt: a seat works one bead at a time,
// so an older attempt still open on another bead is stale from an earlier turn.
// Closing it at this stop would credit it with the time since, so it stays open
// and only the newest attempt ends (gt-8yk78).
func TestEndSeatOpen_ClosesOnlyTheNewestAttempt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	clock := &stepClock{at: testEpoch}
	st := New(dir, clock)
	if err := st.Start("gt-old", "gastown/onyx"); err != nil {
		t.Fatal(err)
	}
	clock.at = testEpoch.Add(10 * time.Minute)
	if err := st.Start("gt-new", "gastown/onyx"); err != nil {
		t.Fatal(err)
	}

	bead, ended, err := st.EndSeatOpen("gastown/onyx", ReasonStopped, testEpoch.Add(20*time.Minute))
	if err != nil {
		t.Fatalf("EndSeatOpen: %v", err)
	}
	if !ended || bead != "gt-new" {
		t.Fatalf("EndSeatOpen = %q, %v; want the newest attempt gt-new closed", bead, ended)
	}

	old, err := st.Summarize("gt-old")
	if err != nil {
		t.Fatalf("Summarize(gt-old): %v", err)
	}
	if len(old.Attempts) != 1 || !old.Attempts[0].Open || old.Attempts[0].Secs != 0 {
		t.Errorf("gt-old = %+v, want its stale attempt left open", old.Attempts)
	}
	newer, err := st.Summarize("gt-new")
	if err != nil {
		t.Fatalf("Summarize(gt-new): %v", err)
	}
	if len(newer.Attempts) != 1 || newer.Attempts[0].Open || newer.Attempts[0].Secs != 600 {
		t.Errorf("gt-new = %+v, want 600s closed", newer.Attempts)
	}
}

// TestEndSeatOpen_NoOpenAttemptWritesNothing: a seat that already ended its
// attempt, or a store with no log for it, gains no line and no error.
func TestEndSeatOpen_NoOpenAttemptWritesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := New(dir, fixedClock{at: testEpoch})
	if err := st.Start("gt-abc", "gastown/onyx"); err != nil {
		t.Fatal(err)
	}
	if err := st.End("gt-abc", "gastown/onyx", ReasonDone, testEpoch.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}

	bead, ended, err := st.EndSeatOpen("gastown/onyx", ReasonStopped, testEpoch.Add(time.Hour))
	if err != nil {
		t.Fatalf("EndSeatOpen: %v", err)
	}
	if ended || bead != "" {
		t.Errorf("EndSeatOpen = %q, %v; want nothing to close", bead, ended)
	}
	if _, _, err := New(filepath.Join(dir, "absent"), fixedClock{at: testEpoch}).
		EndSeatOpen("gastown/onyx", ReasonStopped, testEpoch); err != nil {
		t.Errorf("EndSeatOpen on a store with no directory: %v, want no error", err)
	}
	sum, err := st.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Secs != 30 || sum.Skipped != 0 {
		t.Errorf("Secs/Skipped = %v/%d, want the one 30s attempt and no second end", sum.Secs, sum.Skipped)
	}
}

// TestEndSeatOpen_ClosesNothingWhileALogCannotBeRead: a log that cannot be read
// could hold the seat's newest attempt, so closing the newest attempt of the
// logs that can be read would credit a stale one with time the seat did not
// hold. The read failure goes back to the caller, the readable attempt stays
// open for the landing's close, and a later stop closes it once the log reads
// (gt-8yk78).
func TestEndSeatOpen_ClosesNothingWhileALogCannotBeRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := New(dir, fixedClock{at: testEpoch})
	if err := st.Start("gt-abc", "gastown/onyx"); err != nil {
		t.Fatal(err)
	}
	// A dangling symlink is present in the store but resolves to nothing, so
	// its bead cannot be read.
	broken := filepath.Join(dir, "gt-broken.jsonl")
	if err := os.Symlink(filepath.Join(dir, "gone"), broken); err != nil {
		t.Fatal(err)
	}

	bead, ended, err := st.EndSeatOpen("gastown/onyx", ReasonStopped, testEpoch.Add(time.Minute))
	if err == nil {
		t.Fatal("EndSeatOpen swallowed a log it could not read")
	}
	if ended || bead != "" {
		t.Fatalf("EndSeatOpen = %q, %v; want nothing closed while gt-broken cannot be read", bead, ended)
	}
	sum, err := st.Summarize("gt-abc")
	if err != nil {
		t.Fatal(err)
	}
	if !sum.HasOpen("gastown/onyx") {
		t.Error("gt-abc's attempt was closed although a log that could hold a newer one was unreadable")
	}

	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	bead, ended, err = st.EndSeatOpen("gastown/onyx", ReasonStopped, testEpoch.Add(2*time.Minute))
	if err != nil || !ended || bead != "gt-abc" {
		t.Fatalf("EndSeatOpen after the log reads again = %q, %v, %v; want gt-abc closed", bead, ended, err)
	}
}

// TestEndSeatOpen_ReportsAStoreItCannotRead: a store path that cannot be
// listed is a read that failed, not an absent store. It goes back to the
// caller instead of reading as "this seat holds no attempt" (gt-8yk78).
func TestEndSeatOpen_ReportsAStoreItCannotRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	storePath := filepath.Join(dir, "polecat-spans")
	if err := os.WriteFile(storePath, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := New(storePath, fixedClock{at: testEpoch}).
		EndSeatOpen("gastown/onyx", ReasonStopped, testEpoch); err == nil {
		t.Fatal("EndSeatOpen read a store it could not list as an empty one")
	}
}

func TestEndSeatOpen_RefusesAnEmptySeat(t *testing.T) {
	t.Parallel()
	if _, _, err := New(t.TempDir(), nil).EndSeatOpen("", ReasonStopped, testEpoch); err == nil {
		t.Fatal("EndSeatOpen accepted an empty seat")
	}
}

// TestHasOpen_TracksTheSeatUntilItsEnd: a submission closes the attempt its
// own seat opened, and only that one.
func TestHasOpen_TracksTheSeatUntilItsEnd(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	clock := &stepClock{at: testEpoch}
	st := New(dir, clock)

	if err := st.Start("gt-abc", "gastown/onyx"); err != nil {
		t.Fatal(err)
	}
	sum, err := st.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if !sum.HasOpen("gastown/onyx") {
		t.Error("HasOpen = false for a seat that started and has not ended")
	}
	if sum.HasOpen("gastown/other") {
		t.Error("HasOpen = true for a seat with no attempt on the bead")
	}

	clock.at = testEpoch.Add(time.Minute)
	if err := st.End("gt-abc", "gastown/onyx", ReasonDone, clock.at); err != nil {
		t.Fatal(err)
	}
	sum, err = st.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.HasOpen("gastown/onyx") {
		t.Error("HasOpen = true after the seat's end")
	}
}

func TestSummarize_DeadSeatCloseIsLowerBound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := New(dir, fixedClock{at: testEpoch})

	if err := st.Start("gt-abc", "organic"); err != nil {
		t.Fatal(err)
	}
	if err := st.End("gt-abc", "organic", ReasonDead, testEpoch.Add(45*time.Second)); err != nil {
		t.Fatal(err)
	}

	sum, err := st.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Secs != 45 {
		t.Errorf("Secs = %v, want the dead close's 45s counted", sum.Secs)
	}
	if len(sum.Attempts) != 1 {
		t.Fatalf("Attempts = %+v, want one attempt", sum.Attempts)
	}
	if a := sum.Attempts[0]; a.Secs != 45 || !a.LowerBound {
		t.Errorf("Attempts[0] = %+v, want 45s marked LowerBound", a)
	}
	if !sum.LowerBound {
		t.Error("LowerBound = false, want true for a dead-seat close")
	}

	lines := readLines(t, filepath.Join(dir, "gt-abc.jsonl"))
	var end event
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &end); err != nil {
		t.Fatal(err)
	}
	if end.Reason != string(ReasonDead) || !end.LowerBound {
		t.Errorf("end reason/lower_bound = %q/%v, want dead/true", end.Reason, end.LowerBound)
	}
}

func TestSummarize_SkipsAndCountsUnpairedEvents(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bead := "gt-abc"
	writeLog(t, dir, bead, logLines(t,
		// An end with no start at all.
		event{Bead: bead, Seat: "ghost", Event: EventEnd, At: formatTime(testEpoch)},
		event{Bead: bead, Seat: "a", Event: EventStart, At: formatTime(testEpoch)},
		// A second start while a's attempt is still open.
		event{Bead: bead, Seat: "a", Event: EventStart, At: formatTime(testEpoch.Add(5 * time.Second))},
		event{Bead: bead, Seat: "a", Event: EventEnd, At: formatTime(testEpoch.Add(20 * time.Second))},
		// The pair is already closed, so this end matches nothing.
		event{Bead: bead, Seat: "a", Event: EventEnd, At: formatTime(testEpoch.Add(30 * time.Second))},
		event{Bead: bead, Seat: "b", Event: EventStart, At: formatTime(testEpoch)},
		// Out of order: an end before its own start.
		event{Bead: bead, Seat: "b", Event: EventEnd, At: formatTime(testEpoch.Add(-time.Second))},
	))

	sum, err := New(dir, fixedClock{at: testEpoch}).Summarize(bead)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Skipped != 4 {
		t.Errorf("Skipped = %d, want 4 (end-without-start, duplicate start, duplicate end, out-of-order end)", sum.Skipped)
	}
	if sum.Corrupt != 0 {
		t.Errorf("Corrupt = %d, want 0", sum.Corrupt)
	}
	if sum.Secs != 20 {
		t.Errorf("Secs = %v, want a's 20s", sum.Secs)
	}
	want := []Attempt{
		{Seat: "a", StartedAt: testEpoch, Secs: 20},
		{Seat: "b", StartedAt: testEpoch, Open: true, LowerBound: true},
	}
	if len(sum.Attempts) != len(want) {
		t.Fatalf("Attempts = %+v, want %+v", sum.Attempts, want)
	}
	for i := range want {
		if sum.Attempts[i] != want[i] {
			t.Errorf("Attempts[%d] = %+v, want %+v", i, sum.Attempts[i], want[i])
		}
	}
	if !sum.LowerBound {
		t.Error("LowerBound = false, want true while b's attempt is open")
	}
}

func TestSummarize_ReportsCorruptLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bead := "gt-abc"
	writeLog(t, dir, bead, logLines(t,
		event{Bead: bead, Seat: "a", Event: EventStart, At: formatTime(testEpoch)},
	)+"{\"bead\":\n"+
		logLines(t,
			event{Bead: "gt-other", Seat: "a", Event: EventEnd, At: formatTime(testEpoch.Add(time.Second))},
			event{Bead: bead, Seat: "a", Event: "pause", At: formatTime(testEpoch.Add(2 * time.Second))},
			event{Bead: bead, Seat: "a", Event: EventEnd, At: "yesterday"},
		)+
		logLines(t, event{Bead: bead, Seat: "a", Event: EventEnd, At: formatTime(testEpoch.Add(30 * time.Second))}))

	sum, err := New(dir, fixedClock{at: testEpoch}).Summarize(bead)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Corrupt != 4 {
		t.Errorf("Corrupt = %d, want 4 (broken JSON, other bead, unknown event, bad timestamp)", sum.Corrupt)
	}
	if sum.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0", sum.Skipped)
	}
	if sum.Secs != 30 || len(sum.Attempts) != 1 || sum.Attempts[0].Secs != 30 {
		t.Errorf("summary = %+v, want the one valid 30s attempt", sum)
	}
}

// TestAppend_SerializesOnThePerBeadLock: a writer waits for the bead's lock
// file, so the whole-line guarantee does not rest on what O_APPEND promises on
// this platform.
func TestAppend_SerializesOnThePerBeadLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := New(dir, fixedClock{at: testEpoch})
	if err := st.Start("gt-abc", "organic"); err != nil {
		t.Fatal(err)
	}

	held, err := lockLog(filepath.Join(dir, "gt-abc.jsonl.lock"))
	if err != nil {
		t.Fatalf("locking the span log: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- st.End("gt-abc", "organic", ReasonDone, testEpoch.Add(time.Second)) }()
	select {
	case err := <-done:
		held.unlock()
		t.Fatalf("End completed while another holder had the log's lock: %v", err)
	//testpolicy:allow no-sleep — a lock the writer must wait on has no event to select on
	case <-time.After(50 * time.Millisecond):
	}
	held.unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("End after the lock was released: %v", err)
		}
	//testpolicy:allow no-sleep — bound the wait for a blocked writer, which fails rather than hangs
	case <-time.After(10 * time.Second):
		t.Fatal("End did not complete after the lock was released")
	}
}

func TestAppend_ConcurrentSeatsNeverInterleave(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bead := "gt-abc"
	st := New(dir, fixedClock{at: testEpoch})

	const perSeat = 50
	var wg sync.WaitGroup
	for _, seat := range []string{"organic", "dipper"} {
		wg.Add(1)
		go func(seat string) {
			defer wg.Done()
			for i := 1; i <= perSeat; i++ {
				if err := st.Start(bead, seat); err != nil {
					t.Errorf("%s Start %d: %v", seat, i, err)
					return
				}
				if err := st.End(bead, seat, ReasonDone, testEpoch.Add(time.Duration(i)*time.Second)); err != nil {
					t.Errorf("%s End %d: %v", seat, i, err)
					return
				}
			}
		}(seat)
	}
	wg.Wait()

	lines := readLines(t, filepath.Join(dir, bead+".jsonl"))
	if len(lines) != 2*2*perSeat {
		t.Fatalf("log has %d lines, want %d", len(lines), 2*2*perSeat)
	}
	for i, line := range lines {
		var ev event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d is not one whole record: %v\n%s", i, err, line)
		}
		if ev.Bead != bead || ev.Seat == "" || (ev.Event != EventStart && ev.Event != EventEnd) {
			t.Fatalf("line %d is torn or foreign: %+v", i, ev)
		}
	}

	sum, err := st.Summarize(bead)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(sum.Attempts) != 2*perSeat || sum.Skipped != 0 || sum.Corrupt != 0 {
		t.Errorf("attempts/skipped/corrupt = %d/%d/%d, want %d/0/0",
			len(sum.Attempts), sum.Skipped, sum.Corrupt, 2*perSeat)
	}
}

func TestBeadID_RefusedWhenNotAPathComponent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := New(dir, fixedClock{at: testEpoch})

	bad := []string{"", ".", "..", "gt/abc", `gt\abc`, "../escape", "a/../../b"}
	for _, bead := range bad {
		if err := st.Start(bead, "organic"); err == nil {
			t.Errorf("Start(%q) = nil, want an error", bead)
		}
		if err := st.End(bead, "organic", ReasonDone, testEpoch); err == nil {
			t.Errorf("End(%q) = nil, want an error", bead)
		}
		if _, err := st.Summarize(bead); err == nil {
			t.Errorf("Summarize(%q) = nil error, want an error", bead)
		}
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("store wrote %d entries (err %v), want none for refused ids", len(entries), err)
	}
}

// TestSummarize_FinalLineWithoutNewlineIsRead: a writer killed mid-append or a
// hand-edited file can leave the last record without its newline; the record
// still closes its attempt.
func TestSummarize_FinalLineWithoutNewlineIsRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bead := "gt-abc"
	body := logLines(t, event{Bead: bead, Seat: "organic", Event: EventStart, At: formatTime(testEpoch)})
	body += strings.TrimSuffix(logLines(t,
		event{Bead: bead, Seat: "organic", Event: EventEnd, At: formatTime(testEpoch.Add(90 * time.Second))}), "\n")
	writeLog(t, dir, bead, body)

	sum, err := New(dir, fixedClock{at: testEpoch}).Summarize(bead)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Secs != 90 || sum.Corrupt != 0 || sum.Skipped != 0 || sum.LowerBound {
		t.Errorf("Secs/Corrupt/Skipped/LowerBound = %v/%d/%d/%v, want 90/0/0/false",
			sum.Secs, sum.Corrupt, sum.Skipped, sum.LowerBound)
	}
	if len(sum.Attempts) != 1 || sum.Attempts[0].Open || sum.Attempts[0].Secs != 90 {
		t.Errorf("Attempts = %+v, want the unterminated end to close the one 90s attempt", sum.Attempts)
	}
}

// TestSummarize_TornFinalLineIsCountedNotFatal: half a record at the end is
// damage the read counts, and the whole records before it still summarize.
func TestSummarize_TornFinalLineIsCountedNotFatal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bead := "gt-abc"
	body := logLines(t, event{Bead: bead, Seat: "organic", Event: EventStart, At: formatTime(testEpoch)})
	body += `{"bead":"gt-abc","seat":"organic","event":"end"`
	writeLog(t, dir, bead, body)

	sum, err := New(dir, fixedClock{at: testEpoch}).Summarize(bead)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Corrupt != 1 {
		t.Errorf("Corrupt = %d, want 1 for the torn final line", sum.Corrupt)
	}
	if len(sum.Attempts) != 1 || !sum.Attempts[0].Open || !sum.LowerBound {
		t.Errorf("summary = %+v, want the start left open and lower bound", sum)
	}
}

// TestSummarize_PresentButUnreadableStoreIsNotAnEmptyRead: a store entry that
// is there and cannot be read is an error, not the zero summary a bead with no
// log returns.
func TestSummarize_PresentButUnreadableStoreIsNotAnEmptyRead(t *testing.T) {
	t.Parallel()

	plant := map[string]func(t *testing.T, dir, path string){
		"directory": func(t *testing.T, _, path string) {
			t.Helper()
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"dangling symlink": func(t *testing.T, dir, path string) {
			t.Helper()
			if err := os.Symlink(filepath.Join(dir, "gone.jsonl"), path); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, plant := range plant {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			plant(t, dir, filepath.Join(dir, "gt-abc.jsonl"))
			sum, err := New(dir, fixedClock{at: testEpoch}).Summarize("gt-abc")
			if err == nil {
				t.Fatalf("Summarize = %+v, nil error; want an unreadable store to fail the read", sum)
			}
		})
	}
}

func TestSummarize_MissingLogIsZeroAttempts(t *testing.T) {
	t.Parallel()
	st := New(t.TempDir(), fixedClock{at: testEpoch})

	sum, err := st.Summarize("gt-never-started")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Secs != 0 || len(sum.Attempts) != 0 || sum.LowerBound || sum.Skipped != 0 || sum.Corrupt != 0 {
		t.Errorf("summary = %+v, want the zero value", sum)
	}
}

// TestStartEnd_RejectASeatThatIsNotOneToken: a new attempt names its seat as one
// token, since the seat keys the start/end pairing and is shown to operators, so
// whitespace and control characters are refused as a bead id that is not a path
// component is. A close refuses only an empty seat: a log written before this
// rule can hold an attempt under a seat with a space in it, and that attempt
// must still be closable (gt-rm0ab).
func TestStartEnd_RejectASeatThatIsNotOneToken(t *testing.T) {
	t.Parallel()
	st := New(t.TempDir(), fixedClock{at: testEpoch})

	bad := []string{"", " ", "organic\tdipper", "organic\ndipper", "organic dipper", "organic\x00"}
	for _, seat := range bad {
		if err := st.Start("gt-abc", seat); err == nil {
			t.Errorf("Start(%q) = nil, want an error", seat)
		}
	}
	if err := st.End("gt-abc", "", ReasonDone, testEpoch); err == nil {
		t.Errorf("End(\"\") = nil, want an error")
	}
	if _, err := st.EndOpen("gt-abc", "", ReasonDead, testEpoch); err == nil {
		t.Errorf("EndOpen(\"\") = nil error, want an error")
	}
	if err := st.Start("gt-abc", "gastown/polecats/organic"); err != nil {
		t.Errorf("Start rejected the rig/name seat: %v", err)
	}
}

func TestStartEnd_RejectEmptySeatAndUnknownReason(t *testing.T) {
	t.Parallel()
	st := New(t.TempDir(), fixedClock{at: testEpoch})

	if err := st.Start("gt-abc", ""); err == nil {
		t.Error("Start with an empty seat = nil, want an error")
	}
	if err := st.End("gt-abc", "", ReasonDone, testEpoch); err == nil {
		t.Error("End with an empty seat = nil, want an error")
	}
	if err := st.End("gt-abc", "organic", Reason("finished"), testEpoch); err == nil {
		t.Error("End with an unknown reason = nil, want an error")
	}
}

// DefaultDir is spelled out rather than called: a helper that drifts off
// .runtime/polecat-spans has to fail a test, not move it.
func TestDefaultDir(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	want := filepath.Join(townRoot, ".runtime", "polecat-spans")
	if got := DefaultDir(townRoot); got != want {
		t.Errorf("DefaultDir = %q, want %q", got, want)
	}
}

// A log written before the whitespace rule still reads as it did: a seat with a
// space in it is an attempt, not a corrupt line, so a tightened rule does not
// change the totals of spans already recorded (gt-rm0ab).
func TestSummarize_ReadsALegacySeatWithAWhitespaceAsItDid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bead := "gt-abc"
	writeLog(t, dir, bead, logLines(t,
		event{Bead: bead, Seat: "old seat", Event: EventStart, At: formatTime(testEpoch)},
		event{Bead: bead, Seat: "old seat", Event: EventEnd, At: formatTime(testEpoch.Add(30 * time.Second))},
	))

	sum, err := New(dir, fixedClock{at: testEpoch}).Summarize(bead)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Corrupt != 0 {
		t.Errorf("Corrupt = %d, want the legacy seat read, not counted corrupt", sum.Corrupt)
	}
	if sum.Secs != 30 || len(sum.Attempts) != 1 {
		t.Errorf("summary = %+v, want the one 30s attempt", sum)
	}
}

// An attempt a log holds under a seat with a space in it can still be closed:
// the close refuses only an empty seat, so the attempt does not stay open
// forever behind the whitespace rule (gt-rm0ab).
func TestEndOpen_ClosesALegacySeatWithAWhitespace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bead := "gt-abc"
	writeLog(t, dir, bead, logLines(t,
		event{Bead: bead, Seat: "old seat", Event: EventStart, At: formatTime(testEpoch)},
	))
	st := New(dir, fixedClock{at: testEpoch.Add(time.Minute)})

	closed, err := st.EndOpen(bead, "old seat", ReasonDone, testEpoch.Add(30*time.Second))
	if err != nil {
		t.Fatalf("EndOpen on a legacy seat: %v", err)
	}
	if !closed {
		t.Fatal("EndOpen found no open attempt under the legacy seat")
	}
	sum, err := st.Summarize(bead)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Secs != 30 || len(sum.Attempts) != 1 || sum.Attempts[0].Open {
		t.Errorf("summary = %+v, want the legacy attempt closed at 30s", sum)
	}
}
