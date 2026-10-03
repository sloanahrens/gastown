package intent

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var polecat = Seat{Rig: "gastown", Role: "polecat", Name: "flint"}

func TestSeatPathMatchesPauseMarkerConvention(t *testing.T) {
	t.Parallel()
	cases := []struct {
		seat Seat
		want string
	}{
		{polecat, "/town/.runtime/agents/gastown/polecat.flint.json"},
		{Seat{Rig: "gastown", Role: "witness"}, "/town/.runtime/agents/gastown/witness.json"},
		{Seat{Role: "deacon"}, "/town/.runtime/agents/deacon.json"},
		{Seat{Role: "dog", Name: "rex"}, "/town/.runtime/agents/dog.rex.json"},
	}
	for _, c := range cases {
		if got := c.seat.Path("/town"); got != c.want {
			t.Errorf("%v.Path = %q, want %q", c.seat, got, c.want)
		}
	}
}

func TestReadAbsentIsRunAndNotHeld(t *testing.T) {
	t.Parallel()
	rec, err := Read(t.TempDir(), polecat)
	if err != nil {
		t.Fatalf("Read absent: %v", err)
	}
	if rec.Held() || rec.EffectiveDesired() != DesiredRun {
		t.Fatalf("absent record = %+v, want run and not held", rec)
	}
}

func TestUpdateRoundTrip(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	_, err := Update(town, polecat, func(r *Record) error {
		r.Desired = DesiredRun
		r.WorkBead = "gt-abc"
		r.IncarnationID = "inc-1"
		r.Restarts = append(r.Restarts, now)
		r.Actor = "daemon"
		r.UpdatedAt = now
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	rec, err := Read(town, polecat)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if rec.WorkBead != "gt-abc" || rec.IncarnationID != "inc-1" || len(rec.Restarts) != 1 || rec.Actor != "daemon" || rec.Version != Version {
		t.Fatalf("round trip = %+v", rec)
	}
}

func TestMalformedAndUnreadableReadAsHeld(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := polecat.Path(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := Read(town, polecat)
	if err == nil || !rec.Held() || !rec.Frozen {
		t.Fatalf("malformed: rec=%+v err=%v, want held+frozen and an error", rec, err)
	}

	// A directory where the file should be cannot be read as a file.
	other := Seat{Rig: "gastown", Role: "polecat", Name: "jade"}
	if err := os.MkdirAll(other.Path(town), 0o755); err != nil {
		t.Fatal(err)
	}
	rec, err = Read(town, other)
	if err == nil || !rec.Held() {
		t.Fatalf("unreadable: rec=%+v err=%v, want held and an error", rec, err)
	}
}

func TestUpdateOnMalformedStartsFromHeldRecord(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := polecat.Path(town)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte("garbage"), 0o644)

	var seen Record
	if _, err := Update(town, polecat, func(r *Record) error { seen = *r; return nil }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !seen.Held() {
		t.Fatalf("mutator saw %+v, want the fail-closed held record", seen)
	}
	rec, err := Read(town, polecat)
	if err != nil || !rec.Held() {
		t.Fatalf("after update: %+v %v, want a parseable held record", rec, err)
	}
}

func TestPausedIsAlwaysSerializedAndEqualsHeld(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	check := func(want bool) {
		t.Helper()
		data, err := os.ReadFile(polecat.Path(town))
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		v, ok := raw["paused"]
		if !ok {
			t.Fatalf("paused missing from %s (the shell dog reads null as paused)", data)
		}
		if v != want {
			t.Fatalf("paused = %v, want %v in %s", v, want, data)
		}
	}
	_, _ = Update(town, polecat, func(r *Record) error { r.Desired = DesiredRun; return nil })
	check(false)
	_, _ = Update(town, polecat, func(r *Record) error { r.Frozen = true; return nil })
	check(true)
	_, _ = Update(town, polecat, func(r *Record) error { r.Frozen = false; r.Desired = DesiredPark; return nil })
	check(true)
	_, _ = Update(town, polecat, func(r *Record) error { r.Desired = DesiredStop; return nil })
	check(false)
}

func TestConcurrentUpdatesLoseNothing(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Update(town, polecat, func(r *Record) error {
				r.Restarts = append(r.Restarts, time.Now())
				return nil
			}); err != nil {
				t.Errorf("Update: %v", err)
			}
		}()
	}
	wg.Wait()
	rec, err := Read(town, polecat)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Restarts) != 8 {
		t.Fatalf("restarts = %d, want 8 (a concurrent update was lost)", len(rec.Restarts))
	}
}

func TestAgentsDirHoldsOnlyTheRecord(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if _, err := Update(town, polecat, func(r *Record) error { return nil }); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(polecat.Path(town)))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "polecat.flint.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("agents dir = %v, want only polecat.flint.json", names)
	}
}

func TestRestartsSince(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	r := Record{Restarts: []time.Time{now.Add(-2 * time.Hour), now.Add(-50 * time.Minute), now.Add(-time.Minute)}}
	if got := r.RestartsSince(now.Add(-time.Hour)); got != 2 {
		t.Fatalf("RestartsSince = %d, want 2", got)
	}
}

func TestHoldReason(t *testing.T) {
	t.Parallel()
	if got := (Record{Frozen: true, Reason: "budget"}).HoldReason(); !strings.Contains(got, "budget") {
		t.Fatalf("HoldReason = %q", got)
	}
	if got := (Record{}).HoldReason(); got != "" {
		t.Fatalf("HoldReason of a free record = %q, want empty", got)
	}
}

// TestLegacyPauseMarkerReadsAsPark: a marker written before the intent record
// existed carries only the pause fields. It must read as held, and a later
// update that resumes the seat must be able to clear it.
func TestLegacyPauseMarkerReadsAsPark(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := polecat.Path(town)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	legacy := `{"paused": true, "reason": "inspecting", "paused_at": "2026-09-29T10:00:00Z", "paused_by": "human"}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := Read(town, polecat)
	if err != nil || !rec.Held() || rec.EffectiveDesired() != DesiredPark || rec.Reason != "inspecting" {
		t.Fatalf("legacy marker = %+v, %v; want held park with its reason", rec, err)
	}
	rec, err = Update(town, polecat, func(r *Record) error { r.Desired = DesiredRun; return nil })
	if err != nil || rec.Held() || rec.Paused {
		t.Fatalf("resume of legacy marker = %+v, %v; want free", rec, err)
	}
}

// TestNoDoltDependency: the record is plain file I/O. The package imports no
// process execution and nothing that reaches the store, so it works when
// Dolt is down.
func TestNoDoltDependency(t *testing.T) {
	t.Parallel()
	assertNoStoreImports(t, "intent.go")
}

func assertNoStoreImports(t *testing.T, file string) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if path == "os/exec" || strings.Contains(path, "internal/beads") || strings.Contains(path, "internal/doltserver") {
			t.Errorf("%s imports %s", file, path)
		}
	}
}

// gt-obbx2: submitted is a desired state, not a hold. Nothing may restart the
// seat, but Kill still works, so it must not read as parked or frozen (the
// shell dog reads the paused field, and gt agent resume owns parks).
func TestMarkSubmittedIsNotAHold(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

	if err := MarkSubmitted(town, polecat, "gt-abc", "gt done", now); err != nil {
		t.Fatal(err)
	}
	rec, err := Read(town, polecat)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Submitted() || rec.WorkBead != "gt-abc" || rec.Desired != DesiredSubmitted {
		t.Fatalf("record = %+v, want submitted for gt-abc", rec)
	}
	if rec.Held() || rec.Paused {
		t.Fatalf("a submitted seat reads as held: %+v", rec)
	}

	if err := ClearSubmitted(town, polecat, "gt sling", now); err != nil {
		t.Fatal(err)
	}
	rec, _ = Read(town, polecat)
	if rec.Submitted() || rec.WorkBead != "" || rec.EffectiveDesired() != DesiredRun {
		t.Fatalf("record after ClearSubmitted = %+v, want run with no work bead", rec)
	}
}

// A park or freeze outranks a submission, and ClearSubmitted never lifts one.
func TestSubmittedNeverOverridesAHold(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	if _, err := Update(town, polecat, func(r *Record) error {
		r.Desired, r.Reason = DesiredPark, "operator inspecting"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := MarkSubmitted(town, polecat, "gt-abc", "gt done", now); err != nil {
		t.Fatal(err)
	}
	rec, _ := Read(town, polecat)
	if rec.Desired != DesiredPark || rec.Submitted() {
		t.Fatalf("MarkSubmitted overrode a park: %+v", rec)
	}

	if err := ClearSubmitted(town, polecat, "gt sling", now); err != nil {
		t.Fatal(err)
	}
	if rec, _ = Read(town, polecat); rec.Desired != DesiredPark {
		t.Fatalf("ClearSubmitted lifted a park: %+v", rec)
	}
}

// ClearSubmitted on a seat with no record must not create one.
func TestClearSubmittedWithoutRecordWritesNothing(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := ClearSubmitted(town, polecat, "gt sling", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(polecat.Path(town)); !os.IsNotExist(err) {
		t.Fatalf("ClearSubmitted created a record (stat err %v)", err)
	}
}

func TestClearLandedStopsTheSubmittedSeat(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	if err := MarkSubmitted(town, polecat, "gt-abc", "gt done", now); err != nil {
		t.Fatal(err)
	}
	// A different bead leaves the seat alone.
	if changed, err := ClearLanded(town, polecat, "gt-other", "landing worker", now); err != nil || changed {
		t.Fatalf("ClearLanded(other) = %v, %v; want no change", changed, err)
	}
	changed, err := ClearLanded(town, polecat, "gt-abc", "landing worker", now)
	if err != nil || !changed {
		t.Fatalf("ClearLanded = %v, %v; want a change", changed, err)
	}
	rec, err := Read(town, polecat)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Submitted() || rec.EffectiveDesired() != DesiredStop || rec.WorkBead != "" || rec.Actor != "landing worker" {
		t.Fatalf("record after ClearLanded: %+v", rec)
	}
	// A second call is a no-op.
	if changed, err := ClearLanded(town, polecat, "gt-abc", "landing worker", now); err != nil || changed {
		t.Fatalf("second ClearLanded = %v, %v; want no change", changed, err)
	}
}

func TestClearLandedLeavesAParkedSeat(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	if _, err := Update(town, polecat, func(r *Record) error { r.Desired = DesiredPark; return nil }); err != nil {
		t.Fatal(err)
	}
	if changed, err := ClearLanded(town, polecat, "gt-abc", "landing worker", now); err != nil || changed {
		t.Fatalf("ClearLanded on a parked seat = %v, %v; want no change", changed, err)
	}
	if rec, _ := Read(town, polecat); rec.EffectiveDesired() != DesiredPark {
		t.Fatalf("parked seat became %s", rec.EffectiveDesired())
	}
}

// A seat whose session ended without submitting and which holds no work is
// retired to stop, so no reader keeps looking for a session nothing will
// start (gt-613vw).
func TestMarkIdleStopsTheSeatAndDropsItsEvidence(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	if _, err := Update(town, polecat, func(r *Record) error {
		r.Desired = DesiredRun
		r.WorkBead = "gt-abc"
		r.Progress = &Progress{SampledAt: now, DeadSamples: 796}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	changed, err := MarkIdle(town, polecat, "daemon/patrol-scan", now)
	if err != nil || !changed {
		t.Fatalf("MarkIdle = %v, %v; want a change", changed, err)
	}
	rec, err := Read(town, polecat)
	if err != nil {
		t.Fatal(err)
	}
	if rec.EffectiveDesired() != DesiredStop || rec.WorkBead != "" || rec.Progress != nil || rec.Actor != "daemon/patrol-scan" {
		t.Fatalf("record after MarkIdle: %+v", rec)
	}
	if changed, err := MarkIdle(town, polecat, "daemon/patrol-scan", now); err != nil || changed {
		t.Fatalf("second MarkIdle = %v, %v; want no change", changed, err)
	}
}

// A hold outranks idleness: the operator's park is not overwritten by a
// supervisor retiring an idle seat.
func TestMarkIdleLeavesHeldAndSubmittedSeatsAlone(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	held := []struct {
		name string
		set  Desired
	}{
		{"parked", DesiredPark},
		{"submitted", DesiredSubmitted},
	}
	for _, h := range held {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if _, err := Update(town, polecat, func(r *Record) error {
				r.Desired = h.set
				r.WorkBead = "gt-abc"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if changed, err := MarkIdle(town, polecat, "daemon/patrol-scan", now); err != nil || changed {
				t.Fatalf("MarkIdle on a %s seat = %v, %v; want no change", h.name, changed, err)
			}
			if rec, _ := Read(town, polecat); rec.EffectiveDesired() != h.set || rec.WorkBead != "gt-abc" {
				t.Fatalf("seat became %s with work %q", rec.EffectiveDesired(), rec.WorkBead)
			}
		})
	}
}

// Remove deletes the record, so the readers that walk the agents directory
// stop seeing a seat that is gone (gt-u7voe). It tolerates a record that was
// never written.
func TestRemoveDeletesTheRecordAndToleratesAnAbsentOne(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if _, err := Update(town, polecat, func(r *Record) error {
		r.Desired = DesiredPark
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if err := Remove(town, polecat); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(polecat.Path(town)); !os.IsNotExist(err) {
		t.Fatalf("record still on disk after Remove, stat err=%v", err)
	}
	rec, err := Read(town, polecat)
	if err != nil || rec.Held() || rec.EffectiveDesired() != DesiredRun {
		t.Fatalf("Read after Remove = %+v, %v; want the absent record (run, not held)", rec, err)
	}
	if err := Remove(town, polecat); err != nil {
		t.Fatalf("Remove with no record: %v", err)
	}
}
