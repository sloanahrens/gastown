package land

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/polecat/spans"
)

// seedFirstSight writes the host's first-sight record for bead's submission at
// head: what the landing worker wrote from its own clock the first time it saw
// the submission, and the only end a landing trusts on a seat's behalf.
func seedFirstSight(t *testing.T, town, bead, head string, seen time.Time) {
	t.Helper()
	if err := battery.Append(SubmissionsSeenPath(town), SubmissionRecord{SeenAt: seen, Bead: bead, Head: head}); err != nil {
		t.Fatalf("seeding the first-sight record: %v", err)
	}
}

// tornTail writes raw onto the end of f's file with no trailing newline: the
// tail an append leaves when it stops mid-record.
func tornTail(t *testing.T, f *LandingsFile, raw string) {
	t.Helper()
	fh, err := os.OpenFile(f.Path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write([]byte(raw)); err != nil {
		_ = fh.Close()
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLandingsFileAppendsJSONLines(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(town, ".runtime", "landings", "gastown.jsonl"); f.Path != want {
		t.Fatalf("path = %s, want %s", f.Path, want)
	}
	for _, c := range []string{"aaa", "bbb"} {
		if err := f.Append(LandingRecord{BeadID: "gt-" + c, LandedCommit: c}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	var rec LandingRecord
	if err := json.Unmarshal([]byte(lines[1]), &rec); err != nil || rec.LandedCommit != "bbb" {
		t.Fatalf("second line = %q (%v)", lines[1], err)
	}
	if info, _ := os.Stat(f.Path); info.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v", info.Mode().Perm())
	}
	if info, _ := os.Stat(filepath.Dir(f.Path)); info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", info.Mode().Perm())
	}
}

func TestLandingsFileRefusesUnsafeDirAndNames(t *testing.T) {
	t.Parallel()
	if _, err := RigLandingsFile(t.TempDir(), "../evil"); err == nil {
		t.Error("rig name with a path separator accepted")
	}
	town := t.TempDir()
	dir := filepath.Join(town, ".runtime", "landings")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-exec-files — the target is a directory, not a file: the test needs a world-writable landings dir
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Append(LandingRecord{}); err == nil {
		t.Error("world-writable landings dir accepted")
	}
	town2 := t.TempDir()
	dir2 := filepath.Join(town2, ".runtime", "landings")
	if err := os.MkdirAll(dir2, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(town2, "elsewhere"), filepath.Join(dir2, "gastown.jsonl")); err != nil {
		t.Fatal(err)
	}
	f2, _ := RigLandingsFile(town2, "gastown")
	if err := f2.Append(LandingRecord{}); err == nil {
		t.Error("symlinked landings file accepted")
	}
}

func TestLandingsFileFind(t *testing.T) {
	t.Parallel()
	f, _ := RigLandingsFile(t.TempDir(), "gastown")
	if _, ok, err := f.Find("gt-a", "h1"); ok || err != nil {
		t.Fatalf("Find on a missing file = %v, %v", ok, err)
	}
	for _, r := range []LandingRecord{{BeadID: "gt-a", Head: "h0", LandedCommit: "c0"}, {BeadID: "gt-a", Head: "h1", LandedCommit: "c1"}, {BeadID: "gt-b", Head: "h1", LandedCommit: "c2"}} {
		if err := f.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := f.Find("gt-a", "h1")
	if err != nil || !ok || got.LandedCommit != "c1" {
		t.Fatalf("Find = %+v, %v, %v", got, ok, err)
	}
}

func TestLandingsLatestForBeadAndRecent(t *testing.T) {
	t.Parallel()
	f, err := RigLandingsFile(t.TempDir(), "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.LatestForBead("gt-a"); ok || err != nil {
		t.Fatalf("absent file: ok=%v err=%v", ok, err)
	}
	for _, r := range []LandingRecord{
		{BeadID: "gt-a", Head: "h1", LandedCommit: "l1"},
		{BeadID: "gt-b", Head: "h2", LandedCommit: "l2"},
		{BeadID: "gt-a", Head: "h3", LandedCommit: "l3"},
	} {
		if err := f.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	rec, ok, err := f.LatestForBead("gt-a")
	if err != nil || !ok || rec.Head != "h3" {
		t.Fatalf("LatestForBead = %+v %v %v; want the h3 record", rec, ok, err)
	}
	recent, err := f.Recent(2)
	if err != nil || len(recent) != 2 || recent[0].BeadID != "gt-b" || recent[1].Head != "h3" {
		t.Fatalf("Recent(2) = %+v %v", recent, err)
	}
	// Find keeps its exact bead+head match.
	if rec, ok, _ := f.Find("gt-a", "h1"); !ok || rec.LandedCommit != "l1" {
		t.Fatalf("Find(gt-a, h1) = %+v %v", rec, ok)
	}
}

// TestLandingsFileReadsPastATornLine: an append that stopped mid-record left
// one unparsable line, and the reader failed on it — Find errored, Land turned
// that into an InfraError, and every landing on the rig backed off forever
// (gt-jr90x).
func TestLandingsFileReadsPastATornLine(t *testing.T) {
	t.Parallel()
	f, err := RigLandingsFile(t.TempDir(), "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	// The torn line sits between two whole records, which is the shape the
	// repair in Append leaves behind.
	body := strings.Join([]string{
		`{"bead":"gt-a","rig":"gastown","head":"h1","landed_commit":"c1"}`,
		`{"bead":"gt-tor`,
		`{"bead":"gt-b","rig":"gastown","head":"h2","landed_commit":"c2"}`,
		"",
	}, "\n")
	if err := os.WriteFile(f.Path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok, err := f.Find("gt-a", "h1")
	if err != nil || !ok || got.LandedCommit != "c1" {
		t.Fatalf("Find over a torn line = %+v, %v, %v; want the c1 record", got, ok, err)
	}
	latest, ok, err := f.LatestForBead("gt-b")
	if err != nil || !ok || latest.LandedCommit != "c2" {
		t.Fatalf("LatestForBead over a torn line = %+v, %v, %v; want the c2 record", latest, ok, err)
	}
}

// TestLandingsFileWarnsOnceAboutATornLine: the landing worker reads this file
// on every pass, so a line that stays torn is reported on the read that meets
// it and not again.
func TestLandingsFileWarnsOnceAboutATornLine(t *testing.T) {
	t.Parallel()
	f, err := RigLandingsFile(t.TempDir(), "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Append(LandingRecord{BeadID: "gt-a", Head: "h1", LandedCommit: "c1"}); err != nil {
		t.Fatal(err)
	}
	tornTail(t, f, `{"bead":"gt-tor`)
	var warnings []string
	f.Logf = func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}
	if _, ok, err := f.Find("gt-a", "h1"); err != nil || !ok {
		t.Fatalf("Find = %v, %v", ok, err)
	}
	if _, ok, err := f.LatestForBead("gt-a"); err != nil || !ok {
		t.Fatalf("LatestForBead = %v, %v", ok, err)
	}
	if len(warnings) != 1 {
		t.Fatalf("logged %d warning(s) %q; want one for the torn line", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "unparsable") || !strings.Contains(warnings[0], "gt-tor") {
		t.Fatalf("warning %q does not name the torn line", warnings[0])
	}
	// A second torn line is a different set of damage, so it is not swallowed
	// by the first warning.
	tornTail(t, f, "\n{\"bead\":\"gt-tor2")
	if _, ok, err := f.Find("gt-a", "h1"); err != nil || !ok {
		t.Fatalf("Find = %v, %v", ok, err)
	}
	if len(warnings) != 2 {
		t.Fatalf("logged %d warning(s) %q; want a second for the new torn line", len(warnings), warnings)
	}
}

// TestLandingsFileAppendStartsAFreshLineAfterATornTail: appending to a file
// whose tail is torn glued the new record onto the partial one, so a reader
// could parse neither (gt-jr90x).
func TestLandingsFileAppendStartsAFreshLineAfterATornTail(t *testing.T) {
	t.Parallel()
	f, err := RigLandingsFile(t.TempDir(), "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Append(LandingRecord{BeadID: "gt-a", Head: "h1", LandedCommit: "c1"}); err != nil {
		t.Fatal(err)
	}
	tornTail(t, f, `{"bead":"gt-tor`)
	rec := LandingRecord{BeadID: "gt-b", Head: "h2", LandedCommit: "c2"}
	if err := f.Append(rec); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n"+string(line)+"\n") {
		t.Fatalf("the record was glued to the torn tail:\n%q", data)
	}
	got, ok, err := f.LatestForBead("gt-b")
	if err != nil || !ok || got.LandedCommit != "c2" {
		t.Fatalf("LatestForBead after the repair = %+v, %v, %v", got, ok, err)
	}
}

// TestLandingsFileAppendRollsBackAShortWrite: a full disk part-wrote the
// record, and leaving the part behind is what tore the file in the first
// place, so the append puts the file back the way it found it (gt-jr90x).
func TestLandingsFileAppendRollsBackAShortWrite(t *testing.T) {
	t.Parallel()
	f, err := RigLandingsFile(t.TempDir(), "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Append(LandingRecord{BeadID: "gt-a", Head: "h1", LandedCommit: "c1"}); err != nil {
		t.Fatal(err)
	}
	tornTail(t, f, `{"bead":"gt-tor`)
	before, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	f.write = func(fh *os.File, b []byte) (int, error) {
		n, _ := fh.Write(b[:len(b)/2])
		return n, errors.New("no space left on device")
	}
	err = f.Append(LandingRecord{BeadID: "gt-b", Head: "h2", LandedCommit: "c2"})
	if err == nil {
		t.Fatal("a short write was reported as a successful append")
	}
	after, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("the short write stayed in the file:\n got %q\nwant %q", after, before)
	}
	// The file takes the next record, so the rolled-back append left no
	// partial line to glue it onto.
	f.write = nil
	rec := LandingRecord{BeadID: "gt-b", Head: "h2", LandedCommit: "c2"}
	if err := f.Append(rec); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := f.LatestForBead("gt-b"); err != nil || !ok || got.LandedCommit != "c2" {
		t.Fatalf("LatestForBead after the rollback = %+v, %v, %v", got, ok, err)
	}
}

// TestRepairRecordSurvivesATornLanding: repairRecord could not look at
// anything while one line was torn — it returned an InfraError, which the
// landing worker backs off from, so the rig stopped landing (gt-jr90x).
func TestRepairRecordSurvivesATornLanding(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	if err := l.Landings.Append(LandingRecord{BeadID: "gt-other", Head: "h9", LandedCommit: "c9"}); err != nil {
		t.Fatal(err)
	}
	tornTail(t, l.Landings, `{"bead":"gt-tor`)
	res, err := l.repairRecord(l.open(f.repo), f.work)
	if err != nil {
		t.Fatalf("repairRecord with a torn line in the file: %v", err)
	}
	if res != nil {
		t.Fatalf("repairRecord = %+v; want nothing to repair", res)
	}
}

// spanEpoch is the fixed time the polecat-span fixtures' clocks read, so an
// attempt's length is the number the test wrote and not the run's speed.
var spanEpoch = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

// spanClock is a fixed spans.Clock.
type spanClock struct{ at time.Time }

func (c *spanClock) Now() time.Time { return c.at }

// lastLine reads the record Append wrote last, both as a record and as the
// JSON keys it carried, so a test can tell an omitted field from a zero one.
func lastLine(t *testing.T, f *LandingsFile) (LandingRecord, map[string]json.RawMessage) {
	t.Helper()
	data, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	raw := []byte(lines[len(lines)-1])
	var rec LandingRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("last line %q: %v", raw, err)
	}
	var keyed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keyed); err != nil {
		t.Fatal(err)
	}
	return rec, keyed
}

// spansFor builds a span store whose two attempts on bead close after secs
// each, the shape a polecat landing's log has.
func spansFor(t *testing.T, town, bead string, attempts ...time.Duration) *spans.Store {
	t.Helper()
	store := spans.New(spans.DefaultDir(town), &spanClock{at: spanEpoch})
	for i, d := range attempts {
		seat := fmt.Sprintf("gastown/seat%d", i)
		if err := store.Start(bead, seat); err != nil {
			t.Fatal(err)
		}
		if err := store.End(bead, seat, spans.ReasonDone, spanEpoch.Add(d)); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

// TestLandingsFileAppendFillsPolecatTime: a polecat landing whose bead has two
// closed spans gets their sum and one entry per span (gt-yxl22).
func TestLandingsFileAppendFillsPolecatTime(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	f.Spans = spansFor(t, town, "gt-abc", 60*time.Second, 90*time.Second)
	if err := f.Append(LandingRecord{BeadID: "gt-abc", LandedCommit: "c1"}); err != nil {
		t.Fatal(err)
	}
	rec, keyed := lastLine(t, f)
	if rec.PolecatSecs != 150 {
		t.Errorf("polecat_secs = %v, want 150", rec.PolecatSecs)
	}
	want := []PolecatAttempt{
		{Seat: "gastown/seat0", Secs: 60},
		{Seat: "gastown/seat1", Secs: 90},
	}
	if !reflect.DeepEqual(rec.PolecatAttempts, want) {
		t.Errorf("polecat_attempts = %+v, want %+v", rec.PolecatAttempts, want)
	}
	if rec.PolecatLowerBound {
		t.Error("polecat_lower_bound set on two closed spans")
	}
	for _, key := range []string{"polecat_secs", "polecat_attempts"} {
		if _, ok := keyed[key]; !ok {
			t.Errorf("%s missing from %v", key, keyed)
		}
	}
	// The lower bound is false, so it is left out rather than written false.
	if _, ok := keyed["polecat_lower_bound"]; ok {
		t.Errorf("polecat_lower_bound written false: %v", keyed["polecat_lower_bound"])
	}
}

// TestLandingsFileAppendMarksAnOpenSpanLowerBound: a span its seat never ended
// contributes no time, and the record says the total is a floor (gt-yxl22).
func TestLandingsFileAppendMarksAnOpenSpanLowerBound(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	store := spans.New(spans.DefaultDir(town), &spanClock{at: spanEpoch})
	if err := store.Start("gt-abc", "gastown/opal"); err != nil {
		t.Fatal(err)
	}
	f.Spans = store
	if err := f.Append(LandingRecord{BeadID: "gt-abc", LandedCommit: "c1"}); err != nil {
		t.Fatal(err)
	}
	rec, keyed := lastLine(t, f)
	if !rec.PolecatLowerBound {
		t.Error("polecat_lower_bound unset on an open span")
	}
	want := []PolecatAttempt{{Seat: "gastown/opal", Secs: 0, LowerBound: true}}
	if !reflect.DeepEqual(rec.PolecatAttempts, want) {
		t.Errorf("polecat_attempts = %+v, want %+v", rec.PolecatAttempts, want)
	}
	// An open attempt adds no time, so the sum stays zero and is omitted.
	if _, ok := keyed["polecat_secs"]; ok {
		t.Errorf("polecat_secs written for an open span: %v", keyed["polecat_secs"])
	}
}

// TestLandingsFileAppendSubmittedClosesTheSubmittingSeatsOpenSpan: a sandboxed
// seat cannot write this town's span log, so its attempt stays open until the
// landing closes it at the submission the seat reported and the host saw. The
// record then carries the seat's real seconds instead of a dash (gt-8yk78).
func TestLandingsFileAppendSubmittedClosesTheSubmittingSeatsOpenSpan(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	store := spans.New(spans.DefaultDir(town), &spanClock{at: spanEpoch})
	f.Spans = store
	if err := store.Start("gt-abc", "gastown/opal"); err != nil {
		t.Fatal(err)
	}
	seedFirstSight(t, town, "gt-abc", "h1", spanEpoch.Add(150*time.Second))

	submitted := spanEpoch.Add(120 * time.Second)
	rec0 := LandingRecord{BeadID: "gt-abc", Head: "h1", LandedCommit: "c1", LandedAt: spanEpoch.Add(180 * time.Second)}
	if err := f.AppendSubmitted(rec0, "gastown/opal", submitted); err != nil {
		t.Fatal(err)
	}

	rec, _ := lastLine(t, f)
	if rec.PolecatSecs != 120 {
		t.Errorf("polecat_secs = %v, want the seat's 120 seconds", rec.PolecatSecs)
	}
	if rec.PolecatLowerBound {
		t.Error("polecat_lower_bound set on an attempt the landing closed")
	}
	want := []PolecatAttempt{{Seat: "gastown/opal", Secs: 120}}
	if !reflect.DeepEqual(rec.PolecatAttempts, want) {
		t.Errorf("polecat_attempts = %+v, want %+v", rec.PolecatAttempts, want)
	}
	// The close is a log write, not a record rewrite: the seat's own attempt
	// reads as ended from the log afterwards.
	sum, err := store.Summarize("gt-abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Attempts) != 1 || sum.Attempts[0].Open {
		t.Errorf("summary = %+v, want the attempt closed", sum)
	}
}

// TestLandingsFileAppendSubmittedLeavesAnotherSeatsAttemptOpen: the landing
// closes the attempt of the seat that submitted, and one an earlier seat left
// open stays open — its end is unknown, so the record keeps the lower bound
// rather than counting the time to someone else's submission as work (gt-8yk78).
func TestLandingsFileAppendSubmittedLeavesAnotherSeatsAttemptOpen(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	store := spans.New(spans.DefaultDir(town), &spanClock{at: spanEpoch})
	f.Spans = store
	for _, seat := range []string{"gastown/opal", "gastown/nux"} {
		if err := store.Start("gt-abc", seat); err != nil {
			t.Fatal(err)
		}
	}
	seedFirstSight(t, town, "gt-abc", "h1", spanEpoch.Add(2*time.Minute))

	rec0 := LandingRecord{BeadID: "gt-abc", Head: "h1", LandedCommit: "c1", LandedAt: spanEpoch.Add(2 * time.Minute)}
	if err := f.AppendSubmitted(rec0, "gastown/nux", spanEpoch.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}

	rec, _ := lastLine(t, f)
	if rec.PolecatSecs != 90 {
		t.Errorf("polecat_secs = %v, want the submitting seat's 90 alone", rec.PolecatSecs)
	}
	if !rec.PolecatLowerBound {
		t.Error("polecat_lower_bound unset while an attempt is still open")
	}
	want := []PolecatAttempt{
		{Seat: "gastown/opal", Secs: 0, LowerBound: true},
		{Seat: "gastown/nux", Secs: 90},
	}
	if !reflect.DeepEqual(rec.PolecatAttempts, want) {
		t.Errorf("polecat_attempts = %+v, want %+v", rec.PolecatAttempts, want)
	}
}

// TestLandingsFileAppendSubmittedCutsAStampPastTheHostsFirstSight: the
// submission stamp is the seat's own word, and a seat must not be able to write
// time into a landing record it did not work. A stamp later than the host's
// first sight of the submission is cut back to that sight, so the queue wait
// after the turn ended is never credited as seat time (gt-8yk78).
func TestLandingsFileAppendSubmittedCutsAStampPastTheHostsFirstSight(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	store := spans.New(spans.DefaultDir(town), &spanClock{at: spanEpoch})
	f.Spans = store
	if err := store.Start("gt-abc", "gastown/opal"); err != nil {
		t.Fatal(err)
	}
	seedFirstSight(t, town, "gt-abc", "h1", spanEpoch.Add(100*time.Second))

	rec0 := LandingRecord{BeadID: "gt-abc", Head: "h1", LandedCommit: "c1", LandedAt: spanEpoch.Add(200 * time.Second)}
	if err := f.AppendSubmitted(rec0, "gastown/opal", spanEpoch.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	rec, _ := lastLine(t, f)
	if rec.PolecatSecs != 100 {
		t.Errorf("polecat_secs = %v, want the host's first-sight 100, not the seat's hour", rec.PolecatSecs)
	}
}

// TestLandingsFileAppendSubmittedBoundsAgainstItsClock: a record that carries
// no landing stamp of its own is bounded by the file's clock, so the end is
// never written past the landing in either shape (gt-8yk78).
func TestLandingsFileAppendSubmittedBoundsAgainstItsClock(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	f.now = func() time.Time { return spanEpoch.Add(100 * time.Second) }
	store := spans.New(spans.DefaultDir(town), &spanClock{at: spanEpoch})
	f.Spans = store
	if err := store.Start("gt-abc", "gastown/opal"); err != nil {
		t.Fatal(err)
	}
	// A first sight the host clock cannot have read yet: only the landing's own
	// clock can bound this end.
	seedFirstSight(t, town, "gt-abc", "h1", spanEpoch.Add(200*time.Second))

	if err := f.AppendSubmitted(LandingRecord{BeadID: "gt-abc", Head: "h1", LandedCommit: "c1"}, "gastown/opal", spanEpoch.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	rec, _ := lastLine(t, f)
	if rec.PolecatSecs != 100 {
		t.Errorf("polecat_secs = %v, want the file clock's 100, not the seat's hour", rec.PolecatSecs)
	}
}

// TestLandingsFileAppendSubmittedLeavesTheAttemptOpenWithoutAFirstSight: the
// seat's stamp alone is not evidence the seat worked the time it claims. With
// no first-sight record the host saw no submission, so no end is written and
// the record keeps the lower bound (gt-8yk78).
func TestLandingsFileAppendSubmittedLeavesTheAttemptOpenWithoutAFirstSight(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	f.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	store := spans.New(spans.DefaultDir(town), &spanClock{at: spanEpoch})
	f.Spans = store
	if err := store.Start("gt-abc", "gastown/opal"); err != nil {
		t.Fatal(err)
	}

	rec0 := LandingRecord{BeadID: "gt-abc", Head: "h1", LandedCommit: "c1", LandedAt: spanEpoch.Add(2 * time.Minute)}
	if err := f.AppendSubmitted(rec0, "gastown/opal", spanEpoch.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	rec, _ := lastLine(t, f)
	if rec.PolecatSecs != 0 || !rec.PolecatLowerBound {
		t.Errorf("polecat time = %v (lower bound %v), want none and a lower bound: the seat's word is not a measurement",
			rec.PolecatSecs, rec.PolecatLowerBound)
	}
	sum, err := store.Summarize("gt-abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Attempts) != 1 || !sum.Attempts[0].Open {
		t.Errorf("summary = %+v, want the attempt left open", sum)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "first-sight") {
		t.Errorf("logged = %q, want one line saying the first-sight record is missing", logged)
	}
}

// TestLandingsFileAppendSubmittedWritesTheLandingWhenTheFirstSightCannotBeRead:
// the first-sight log is the bound the end needs, so a log that cannot be read
// leaves the attempt open rather than closing it on the seat's word, and the
// landing is written anyway with a lower bound (gt-8yk78).
func TestLandingsFileAppendSubmittedWritesTheLandingWhenTheFirstSightCannotBeRead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	f.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	// A directory where the first-sight log belongs: the store the landing
	// built reads it through FirstSeenIn, and every open of it fails.
	if err := os.MkdirAll(SubmissionsSeenPath(town), 0o700); err != nil {
		t.Fatal(err)
	}
	store := spans.New(spans.DefaultDir(town), &spanClock{at: spanEpoch})
	f.Spans = store
	if err := store.Start("gt-abc", "gastown/opal"); err != nil {
		t.Fatal(err)
	}

	rec0 := LandingRecord{BeadID: "gt-abc", Head: "h1", LandedCommit: "c1", LandedAt: spanEpoch.Add(2 * time.Minute)}
	if err := f.AppendSubmitted(rec0, "gastown/opal", spanEpoch.Add(time.Minute)); err != nil {
		t.Fatalf("AppendSubmitted with an unreadable first-sight log: %v", err)
	}

	rec, _ := lastLine(t, f)
	if rec.PolecatSecs != 0 || !rec.PolecatLowerBound {
		t.Errorf("polecat time = %v (lower bound %v), want none and a lower bound", rec.PolecatSecs, rec.PolecatLowerBound)
	}
	sum, err := store.Summarize("gt-abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Attempts) != 1 || !sum.Attempts[0].Open {
		t.Errorf("summary = %+v, want the attempt left open", sum)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "first-sight") {
		t.Errorf("logged = %q, want one line saying the first-sight log could not be read", logged)
	}
}

// TestLandingsFileAppendSubmittedWritesTheLandingWhenTheCloseFails: the span is
// a measurement, so a log that cannot be written costs the time and not the
// landing record (gt-yxl22).
func TestLandingsFileAppendSubmittedWritesTheLandingWhenTheCloseFails(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	f.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	f.Spans = &unreadableSpans{err: errors.New("permission denied")}
	seedFirstSight(t, town, "gt-abc", "h1", spanEpoch.Add(90*time.Second))

	if err := f.AppendSubmitted(LandingRecord{BeadID: "gt-abc", Head: "h1", LandedCommit: "c1"}, "gastown/opal", spanEpoch.Add(time.Minute)); err != nil {
		t.Fatalf("AppendSubmitted with an unwritable span log: %v; the landing must still be written", err)
	}
	if rec, _ := lastLine(t, f); rec.LandedCommit != "c1" {
		t.Errorf("record = %+v, want the landing written", rec)
	}
	if len(logged) != 2 {
		t.Errorf("logged = %q, want one line for the failed close and one for the unreadable log", logged)
	}
}

// TestLandingsFileAppendOmitsPolecatTimeWithoutSpans: a crew landing has no
// seat and so no log, and a record for a bead with no spans carries none of
// the three fields (gt-yxl22).
func TestLandingsFileAppendOmitsPolecatTimeWithoutSpans(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		spans SpanSummarizer
	}{
		{"crew landing", spans.New(spans.DefaultDir(t.TempDir()), nil)},
		{"no span store", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f, err := RigLandingsFile(t.TempDir(), "gastown")
			if err != nil {
				t.Fatal(err)
			}
			f.Spans = c.spans
			if err := f.Append(LandingRecord{BeadID: "gt-crew", LandedCommit: "c1"}); err != nil {
				t.Fatal(err)
			}
			rec, keyed := lastLine(t, f)
			if rec.LandedCommit != "c1" {
				t.Fatalf("record = %+v; the landing must still be written", rec)
			}
			for key := range keyed {
				if strings.HasPrefix(key, "polecat_") {
					t.Errorf("a landing without spans wrote %s", key)
				}
			}
		})
	}
}

// unreadableSpans is a SpanSummarizer whose every read and write fails, the
// shape of a span log that exists but the worker cannot open.
type unreadableSpans struct {
	err  error
	read []string // every bead Summarize was asked for
}

func (u *unreadableSpans) Summarize(bead string) (spans.Summary, error) {
	u.read = append(u.read, bead)
	return spans.Summary{}, u.err
}

func (u *unreadableSpans) EndOpen(string, string, spans.Reason, time.Time) (bool, error) {
	return false, u.err
}

// TestLandingsFileAppendMarksPolecatTimeLowerBoundWhenTheLogCannotBeRead: the
// time is not worth the landing, so the record is still written; the time is
// left out (there is none to report), but the lower bound tells a reader the
// log existed and the total is a floor rather than the crew landing's silence
// (gt-yxl22).
func TestLandingsFileAppendMarksPolecatTimeLowerBoundWhenTheLogCannotBeRead(t *testing.T) {
	t.Parallel()
	f, err := RigLandingsFile(t.TempDir(), "gastown")
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	f.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	store := &unreadableSpans{err: errors.New("permission denied")}
	f.Spans = store
	if err := f.Append(LandingRecord{BeadID: "gt-abc", LandedCommit: "c1"}); err != nil {
		t.Fatalf("Append with an unreadable span log: %v; the landing must still be written", err)
	}
	rec, keyed := lastLine(t, f)
	if rec.LandedCommit != "c1" || rec.PolecatSecs != 0 || rec.PolecatAttempts != nil || !rec.PolecatLowerBound {
		t.Fatalf("record = %+v; want the landing with the time as a lower bound", rec)
	}
	for _, key := range []string{"polecat_secs", "polecat_attempts"} {
		if _, ok := keyed[key]; ok {
			t.Errorf("%s written from an unreadable log: %v", key, keyed[key])
		}
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "gt-abc") {
		t.Errorf("logged = %q; want one line naming the bead", logged)
	}
	if len(store.read) != 1 || store.read[0] != "gt-abc" {
		t.Errorf("Summarize calls = %q; want one for gt-abc", store.read)
	}
}

// TestLandingsFileAppendDoesNotHandAnUnsafeBeadIDToTheSpanStore: the span store
// names one file per bead, so a record whose bead id is not a single path
// component gets no time and never reaches the store or a path (gt-yxl22).
func TestLandingsFileAppendDoesNotHandAnUnsafeBeadIDToTheSpanStore(t *testing.T) {
	t.Parallel()
	for _, bead := range []string{"../escape", "gt/abc", `gt\abc`, "..", ""} {
		t.Run(bead, func(t *testing.T) {
			t.Parallel()
			f, err := RigLandingsFile(t.TempDir(), "gastown")
			if err != nil {
				t.Fatal(err)
			}
			store := &unreadableSpans{err: errors.New("must not be read")}
			f.Spans = store
			if err := f.Append(LandingRecord{BeadID: bead, LandedCommit: "c1"}); err != nil {
				t.Fatalf("Append(%q): %v; the landing must still be written", bead, err)
			}
			if len(store.read) != 0 {
				t.Errorf("Summarize(%q) called with a bead id that is not a path component", bead)
			}
			rec, keyed := lastLine(t, f)
			if rec.LandedCommit != "c1" || rec.PolecatLowerBound {
				t.Fatalf("record = %+v; want the landing with no polecat time", rec)
			}
			for key := range keyed {
				if strings.HasPrefix(key, "polecat_") {
					t.Errorf("%q wrote %s", bead, key)
				}
			}
		})
	}
}

// TestLandingsFileAppendKeepsCallerSuppliedPolecatTime: a caller that sets the
// polecat fields itself keeps them; Append fills them only when the record
// carries none (gt-yxl22).
func TestLandingsFileAppendKeepsCallerSuppliedPolecatTime(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	store := &unreadableSpans{err: errors.New("must not be read")}
	f.Spans = store
	rec := LandingRecord{
		BeadID: "gt-abc", LandedCommit: "c1",
		PolecatSecs:       7,
		PolecatAttempts:   []PolecatAttempt{{Seat: "gastown/opal", Secs: 7}},
		PolecatLowerBound: true,
	}
	if err := f.Append(rec); err != nil {
		t.Fatal(err)
	}
	got, _ := lastLine(t, f)
	if !reflect.DeepEqual(got.PolecatAttempts, rec.PolecatAttempts) || got.PolecatSecs != 7 || !got.PolecatLowerBound {
		t.Errorf("polecat time = %+v; want the caller's, untouched", got)
	}
	if len(store.read) != 0 {
		t.Errorf("Summarize called for a record that already carried its time: %q", store.read)
	}
}

// fixedSpans is a SpanSummarizer that answers every read with one summary, the
// shape of a bead whose seats are one closed attempt and one still hooked.
type fixedSpans struct {
	sum spans.Summary
}

func (f *fixedSpans) Summarize(string) (spans.Summary, error) { return f.sum, nil }

func (f *fixedSpans) EndOpen(string, string, spans.Reason, time.Time) (bool, error) {
	return false, nil
}

// TestLandingsFilePolecatTimeAsOfCountsAnOpenAttemptToThatTime: a landed record
// counts an attempt its seat never ended as nothing, but a landing still
// running reads the time so far: the open attempt counts up to the as-of time,
// and that reading is a lower bound because the attempt is still open. The
// summary's own lower-bound flag is left off, as a store that only knows the
// closed attempts would, so the running reading's floor can only come from the
// open attempt itself (gt-kmp7c).
func TestLandingsFilePolecatTimeAsOfCountsAnOpenAttemptToThatTime(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 26, 13, 50, 0, 0, time.UTC)
	asOf := start.Add(10 * time.Minute)
	f := &LandingsFile{Spans: &fixedSpans{sum: spans.Summary{
		Secs: 600,
		Attempts: []spans.Attempt{
			{Seat: "gastown/opal", Secs: 600},
			{Seat: "gastown/onyx", StartedAt: start, Open: true},
		},
	}}}

	for _, tc := range []struct {
		name      string
		asOf      time.Time
		wantSecs  float64
		wantOpen  float64
		wantLower bool
	}{
		{name: "a landed record adds no time for the open attempt", asOf: time.Time{}, wantSecs: 600, wantOpen: 0},
		{name: "a running landing counts the open attempt up to now", asOf: asOf, wantSecs: 1200, wantOpen: 600, wantLower: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secs, attempts, lower := f.PolecatTime("gt-abc", tc.asOf)
			if secs != tc.wantSecs {
				t.Errorf("secs = %v, want %v", secs, tc.wantSecs)
			}
			if len(attempts) != 2 || attempts[1].Secs != tc.wantOpen {
				t.Errorf("attempts = %+v, want the open attempt at %v secs", attempts, tc.wantOpen)
			}
			if lower != tc.wantLower {
				t.Errorf("lower bound = %v, want %v: a running read counts an attempt that is still open", lower, tc.wantLower)
			}
		})
	}
}

// seedSling writes the town's event log line for bead's dispatch at at: the
// sling start a record falls back on when the seat wrote no span start
// (gt-v7ptp). The seat it names is the landing's own "gastown/opal" in the
// log's "gastown/polecats/opal" spelling.
func seedSling(t *testing.T, town, bead string, at time.Time) {
	t.Helper()
	seedSlingTo(t, town, bead, "gastown/polecats/opal", at)
}

// seedSlingTo is seedSling with the seat the sling names chosen by the caller.
func seedSlingTo(t *testing.T, town, bead, seat string, at time.Time) {
	t.Helper()
	line, err := json.Marshal(events.Event{
		Timestamp: at.UTC().Format(time.RFC3339), Source: "gt", Type: events.TypeSling,
		Actor: events.ActorDaemon, Payload: events.SlingPayload(bead, seat),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(town, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(town, events.EventsFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestLandingsFileAppendFloorsTheTimeASeatWithNoStartLeft: a landed bead whose
// seat wrote no start event at all has no clock in the span log, so the record
// is timed from the bead's dispatch to the end the landing trusts, and named a
// dispatch interval rather than the seat's own measurement. It is an upper
// bound, not a floor: the session starts after the sling, so the interval can
// only overstate the seat's time (gt-v7ptp).
func TestLandingsFileAppendFloorsTheTimeASeatWithNoStartLeft(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	f.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	// A span store the seat never wrote to: gt-p85pw.92's shape.
	f.Spans = spans.New(spans.DefaultDir(town), nil)
	seedSling(t, town, "gt-abc", spanEpoch)
	seedFirstSight(t, town, "gt-abc", "h1", spanEpoch.Add(30*time.Minute))

	rec0 := LandingRecord{BeadID: "gt-abc", Head: "h1", LandedCommit: "c1", LandedAt: spanEpoch.Add(40 * time.Minute)}
	if err := f.AppendSubmitted(rec0, "gastown/opal", spanEpoch.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}

	rec, keyed := lastLine(t, f)
	if want := 20 * 60.0; rec.PolecatSecs != want {
		t.Errorf("polecat_secs = %v, want the %v from the sling to the submission", rec.PolecatSecs, want)
	}
	if !rec.PolecatUpperBound {
		t.Error("polecat_upper_bound unset on a time read from the dispatch rather than the seat")
	}
	if rec.PolecatLowerBound {
		t.Error("polecat_lower_bound set on a dispatch interval, which can only overstate the seat's time")
	}
	want := []PolecatAttempt{{Seat: "gastown/opal", Secs: 20 * 60, UpperBound: true, From: PolecatFromSling}}
	if !reflect.DeepEqual(rec.PolecatAttempts, want) {
		t.Errorf("polecat_attempts = %+v, want %+v", rec.PolecatAttempts, want)
	}
	for _, key := range []string{"polecat_secs", "polecat_attempts", "polecat_upper_bound"} {
		if _, ok := keyed[key]; !ok {
			t.Errorf("%s missing from %v", key, keyed)
		}
	}
	if _, ok := keyed["polecat_lower_bound"]; ok {
		t.Errorf("a dispatch interval wrote polecat_lower_bound: %v", keyed)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "wrote no span start") {
		t.Errorf("logged = %q, want one line naming the missing span start", logged)
	}
}

// TestLandingsFileAppendFloorsNothingWithoutASling: a bead the event log never
// dispatched has no start either, so the record keeps its silence rather than
// inventing one (gt-v7ptp).
func TestLandingsFileAppendFloorsNothingWithoutASling(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	f.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	f.Spans = spans.New(spans.DefaultDir(town), nil)
	seedFirstSight(t, town, "gt-abc", "h1", spanEpoch.Add(30*time.Minute))

	rec0 := LandingRecord{BeadID: "gt-abc", Head: "h1", LandedCommit: "c1", LandedAt: spanEpoch.Add(40 * time.Minute)}
	if err := f.AppendSubmitted(rec0, "gastown/opal", spanEpoch.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}

	rec, keyed := lastLine(t, f)
	if rec.PolecatSecs != 0 || rec.PolecatAttempts != nil || rec.PolecatLowerBound {
		t.Errorf("record = %+v; want no polecat time for a bead with neither a start nor a sling", rec)
	}
	for key := range keyed {
		if strings.HasPrefix(key, "polecat_") {
			t.Errorf("a bead with no start and no sling wrote %s", key)
		}
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "no sling") {
		t.Errorf("logged = %q, want one line saying the event log holds no sling", logged)
	}
}

// TestLandingsFileAppendFloorsNothingWithoutAFirstSight: the end of a dispatch
// floor is the host's, not the seat's, so a submission the host never saw ends
// nothing and the record keeps its silence (gt-8yk78, gt-v7ptp).
func TestLandingsFileAppendFloorsNothingWithoutAFirstSight(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		firstSee func(*LandingsFile)
		want     string
	}{
		{"the log holds no record for the submission", func(*LandingsFile) {}, "no first-sight record"},
		{"the landing has no first-sight reader", func(f *LandingsFile) { f.FirstSeen = nil }, "no first-sight reader"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			f, err := RigLandingsFile(town, "gastown")
			if err != nil {
				t.Fatal(err)
			}
			var logged []string
			f.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
			f.Spans = spans.New(spans.DefaultDir(town), nil)
			c.firstSee(f)
			seedSling(t, town, "gt-abc", spanEpoch)

			rec0 := LandingRecord{BeadID: "gt-abc", Head: "h1", LandedCommit: "c1", LandedAt: spanEpoch.Add(40 * time.Minute)}
			if err := f.AppendSubmitted(rec0, "gastown/opal", spanEpoch.Add(20*time.Minute)); err != nil {
				t.Fatal(err)
			}

			rec, _ := lastLine(t, f)
			if rec.PolecatSecs != 0 || rec.PolecatAttempts != nil {
				t.Errorf("record = %+v; want no time ended on the seat's word alone", rec)
			}
			if len(logged) != 1 || !strings.Contains(logged[0], c.want) {
				t.Errorf("logged = %q, want one line saying the submission has no trusted end (%q)", logged, c.want)
			}
		})
	}
}

// TestLandingsFileAppendFloorsNothingFromASlingAfterTheEnd: a start the reader
// dates at or after the end is not the start of the run that submitted, so it
// times nothing (gt-v7ptp).
func TestLandingsFileAppendFloorsNothingFromASlingAfterTheEnd(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	f.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	f.Spans = spans.New(spans.DefaultDir(town), nil)
	seedFirstSight(t, town, "gt-abc", "h1", spanEpoch.Add(30*time.Minute))
	// A reader that dates the sling at the end itself: SlungAtIn reads only
	// slings before the end, so the guard this drives is the one between a
	// seam's answer and the record.
	f.SlungAt = func(string, time.Time) (time.Time, string, error) {
		return spanEpoch.Add(20 * time.Minute), "gastown/polecats/opal", nil
	}

	rec0 := LandingRecord{BeadID: "gt-abc", Head: "h1", LandedCommit: "c1", LandedAt: spanEpoch.Add(40 * time.Minute)}
	if err := f.AppendSubmitted(rec0, "gastown/opal", spanEpoch.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}

	rec, _ := lastLine(t, f)
	if rec.PolecatSecs != 0 || rec.PolecatAttempts != nil {
		t.Errorf("record = %+v; want no time from a sling after the submission", rec)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "not before the end") {
		t.Errorf("logged = %q, want one line saying the sling is not before the end", logged)
	}
}

// TestLandingsFileAppendFloorsNothingFromAnotherSeatsDispatch: the dispatch
// clock belongs to the run of the seat the sling named. When that is not the
// landing's seat — another polecat, or no seat at all — the interval is not
// this seat's time and is not attributed to it (gt-v7ptp).
func TestLandingsFileAppendFloorsNothingFromAnotherSeatsDispatch(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		target string
		want   string
	}{
		{name: "dispatched to another polecat", target: "gastown/polecats/morsov", want: "dispatched to"},
		{name: "the sling names no seat", target: "", want: "names no seat"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			f, err := RigLandingsFile(town, "gastown")
			if err != nil {
				t.Fatal(err)
			}
			var logged []string
			f.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
			f.Spans = spans.New(spans.DefaultDir(town), nil)
			seedSlingTo(t, town, "gt-abc", tt.target, spanEpoch)
			seedFirstSight(t, town, "gt-abc", "h1", spanEpoch.Add(30*time.Minute))

			rec0 := LandingRecord{BeadID: "gt-abc", Head: "h1", LandedCommit: "c1", LandedAt: spanEpoch.Add(40 * time.Minute)}
			if err := f.AppendSubmitted(rec0, "gastown/opal", spanEpoch.Add(20*time.Minute)); err != nil {
				t.Fatal(err)
			}

			rec, _ := lastLine(t, f)
			if rec.PolecatSecs != 0 || rec.PolecatAttempts != nil || rec.PolecatUpperBound {
				t.Errorf("record = %+v; want no time from a dispatch that is not this seat's", rec)
			}
			if len(logged) != 1 || !strings.Contains(logged[0], tt.want) {
				t.Errorf("logged = %q, want one line containing %q", logged, tt.want)
			}
		})
	}
}

// TestLandingsFileAppendReadsTheFirstSightOnce: the close of the seat's attempt
// and a dispatch floor end at the same trusted time, so one landing reads the
// first-sight record once rather than per clock (gt-v7ptp).
func TestLandingsFileAppendReadsTheFirstSightOnce(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	f.Spans = spans.New(spans.DefaultDir(town), nil)
	seedSling(t, town, "gt-abc", spanEpoch)
	reads := 0
	f.FirstSeen = func(bead, head string) (time.Time, error) {
		reads++
		return spanEpoch.Add(30 * time.Minute), nil
	}

	rec0 := LandingRecord{BeadID: "gt-abc", Head: "h1", LandedCommit: "c1", LandedAt: spanEpoch.Add(40 * time.Minute)}
	if err := f.AppendSubmitted(rec0, "gastown/opal", spanEpoch.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if rec, _ := lastLine(t, f); rec.PolecatSecs != 20*60 {
		t.Errorf("polecat_secs = %v, want the 1200s the floor reads", rec.PolecatSecs)
	}
	if reads != 1 {
		t.Errorf("first-sight reads = %d, want one for the landing", reads)
	}
}

// TestLandingsFileAppendFloorsNothingFromALogPastTheReadBound: the dispatch
// clock is read from a bounded window of the event log. A log larger than it
// may start inside the run, where a repeat sling would date the bead later than
// its dispatch and the interval would come out short, so such a log times
// nothing rather than an interval that may understate the seat's time
// (gt-v7ptp).
func TestLandingsFileAppendFloorsNothingFromALogPastTheReadBound(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	f.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	f.Spans = spans.New(spans.DefaultDir(town), nil)
	padEventLog(t, town, dispatchLogBytes)
	// The sling sits at the very end of a log too large to be read whole.
	seedSling(t, town, "gt-abc", spanEpoch)
	seedFirstSight(t, town, "gt-abc", "h1", spanEpoch.Add(30*time.Minute))

	rec0 := LandingRecord{BeadID: "gt-abc", Head: "h1", LandedCommit: "c1", LandedAt: spanEpoch.Add(40 * time.Minute)}
	if err := f.AppendSubmitted(rec0, "gastown/opal", spanEpoch.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}

	rec, _ := lastLine(t, f)
	if rec.PolecatSecs != 0 || rec.PolecatAttempts != nil {
		t.Errorf("record = %+v; want no time from a log read in part", rec)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "larger than") {
		t.Errorf("logged = %q, want one line saying the log is larger than the read", logged)
	}
}

// padEventLog grows the town's event log past size bytes with well-formed
// lines, so a reader's bound is what decides how much of it is seen.
func padEventLog(t *testing.T, town string, size int64) {
	t.Helper()
	if err := os.MkdirAll(town, 0o700); err != nil {
		t.Fatal(err)
	}
	fh, err := os.OpenFile(filepath.Join(town, events.EventsFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(events.Event{
		Timestamp: spanEpoch.Add(-time.Hour).UTC().Format(time.RFC3339), Source: "gt",
		Type: events.TypeNudge, Payload: events.HookPayload("gt-pad"),
	})
	if err != nil {
		_ = fh.Close()
		t.Fatal(err)
	}
	block := bytes.Repeat(append(line, '\n'), 512)
	for written := int64(0); written < size; written += int64(len(block)) {
		if _, err := fh.Write(block); err != nil {
			_ = fh.Close()
			t.Fatal(err)
		}
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestLandingsFileAppendFloorsNothingWhenTheEventLogCannotBeRead: the dispatch
// is a read like any other, so a log the landing cannot read costs the time and
// the landing is written anyway, the way an unreadable span log does.
func TestLandingsFileAppendFloorsNothingWhenTheEventLogCannotBeRead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	f.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	f.Spans = spans.New(spans.DefaultDir(town), nil)
	// A directory where the event log belongs: opening it succeeds and reading
	// it fails, the shape of a log that is there but cannot be read.
	if err := os.MkdirAll(filepath.Join(town, events.EventsFile), 0o700); err != nil {
		t.Fatal(err)
	}
	seedFirstSight(t, town, "gt-abc", "h1", spanEpoch.Add(30*time.Minute))

	rec0 := LandingRecord{BeadID: "gt-abc", Head: "h1", LandedCommit: "c1", LandedAt: spanEpoch.Add(40 * time.Minute)}
	if err := f.AppendSubmitted(rec0, "gastown/opal", spanEpoch.Add(20*time.Minute)); err != nil {
		t.Fatalf("AppendSubmitted with an unreadable event log: %v; the landing must still be written", err)
	}

	rec, _ := lastLine(t, f)
	if rec.LandedCommit != "c1" || rec.PolecatSecs != 0 || rec.PolecatAttempts != nil {
		t.Errorf("record = %+v; want the landing with no dispatch floor", rec)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "no dispatch floor") {
		t.Errorf("logged = %q, want one line saying the floor could not be read", logged)
	}
}
