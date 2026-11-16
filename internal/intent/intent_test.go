package intent

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
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

// A sling or a sandbox debt on a submitted seat moves UpdatedAt but is not a new
// submission, so SubmittedAt keeps the submission's own time (gt-v5nb1).
func TestSubmittedAtSurvivesOtherMutators(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	submitted := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	if err := MarkSubmitted(town, polecat, "gt-abc", "gt done", submitted); err != nil {
		t.Fatal(err)
	}
	if err := MarkSlung(town, polecat, "claude", "gt sling", submitted.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := MarkSandboxOwed(town, polecat, true, "patrol", submitted.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	rec, err := Read(town, polecat)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.SubmittedAt.Equal(submitted) || rec.UpdatedAt.Equal(submitted) {
		t.Fatalf("record = %+v, want SubmittedAt %s kept while UpdatedAt moved on", rec, submitted)
	}
}

// Each transition in this package that clears WorkBead clears SubmittedAt too, so
// no later record reads the old submission's time (gt-v5nb1).
func TestSubmittedAtIsClearedWithWorkBead(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name  string
		clear func(town string) error
	}{
		{"ClearSubmitted", func(town string) error {
			return ClearSubmitted(town, polecat, "gt sling", now)
		}},
		{"ClearLanded", func(town string) error {
			_, err := ClearLanded(town, polecat, "gt-abc", "landing worker", now)
			return err
		}},
		{"MarkIdle", func(town string) error {
			// MarkIdle leaves a submitted seat alone, so the seat is run first.
			if _, err := Update(town, polecat, func(r *Record) error {
				r.Desired = DesiredRun
				return nil
			}); err != nil {
				return err
			}
			_, err := MarkIdle(town, polecat, "witness", now)
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if err := MarkSubmitted(town, polecat, "gt-abc", "gt done", now); err != nil {
				t.Fatal(err)
			}
			if err := c.clear(town); err != nil {
				t.Fatal(err)
			}
			rec, err := Read(town, polecat)
			if err != nil {
				t.Fatal(err)
			}
			if rec.WorkBead != "" || !rec.SubmittedAt.IsZero() {
				t.Fatalf("record after %s = %+v, want no work bead and no submission time", c.name, rec)
			}
		})
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

// TestMarkSlungRecordsTheAgentTheSeatRuns is gt-2q283: the session's GT_AGENT
// dies with the session, so the record is what a sessionless seat — mid-landing
// or waiting for its supervised restart — is counted by. MarkSlung writes it on
// a seat that has no record yet and overwrites the agent a later sling runs the
// seat on, so a reused seat is never counted on its predecessor's agent.
func TestMarkSlungRecordsTheAgentTheSeatRuns(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 10, 14, 11, 34, 0, 0, time.UTC)

	if err := MarkSlung(town, polecat, "deepseek-flash", "gt sling", now); err != nil {
		t.Fatal(err)
	}
	rec, err := Read(town, polecat)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Agent != "deepseek-flash" {
		t.Fatalf("record = %+v, want agent deepseek-flash", rec)
	}
	if rec.Held() || rec.Paused || rec.Submitted() || rec.EffectiveDesired() != DesiredRun {
		t.Fatalf("recording an agent is not a hold or a submission: %+v", rec)
	}

	// The seat is reused on another agent: the record follows the sling.
	if err := MarkSlung(town, polecat, "claude-sonnet", "gt sling", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if rec, _ = Read(town, polecat); rec.Agent != "claude-sonnet" {
		t.Fatalf("record = %+v, want agent claude-sonnet", rec)
	}
}

// An empty agent is the polecat role default, and is recorded as no agent at
// all, which is what the readers that count a sessionless seat fall back to.
func TestMarkSlungRecordsAnEmptyAgentAsNone(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := MarkSlung(town, polecat, "  ", "gt sling", time.Now()); err != nil {
		t.Fatal(err)
	}
	if rec, _ := Read(town, polecat); rec.Agent != "" {
		t.Fatalf("record = %+v, want no agent", rec)
	}
}

// A park outranks a dispatch, and MarkSlung never lifts one: an operator
// inspecting the seat still owns it.
func TestMarkSlungLeavesAHoldAlone(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if _, err := Update(town, polecat, func(r *Record) error {
		r.Desired, r.Reason = DesiredPark, "operator inspecting"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := MarkSlung(town, polecat, "deepseek-flash", "gt sling", time.Now()); err != nil {
		t.Fatal(err)
	}
	rec, _ := Read(town, polecat)
	if rec.Desired != DesiredPark || rec.Agent != "" {
		t.Fatalf("MarkSlung wrote to a parked seat: %+v", rec)
	}
}

// A sling starts a new session, so the seat's stall evidence (a dead sample of
// the session before) is dropped: left in place it reads as the new session
// being dead on the town pill until the next patrol scan overwrites it. A held
// seat keeps its record as it is, evidence included.
func TestMarkSlungDropsTheEvidenceOfThePreviousSession(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 23, 17, 47, 48, 0, time.UTC)
	dead := &Progress{SampledAt: now.Add(-4 * time.Minute), DeadSamples: 1}

	t.Run("running_seat", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		if _, err := Update(town, polecat, func(r *Record) error {
			r.Desired = DesiredRun
			r.Progress = dead
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := MarkSlung(town, polecat, "deepseek-flash-sandbox", "gt sling", now); err != nil {
			t.Fatal(err)
		}
		rec, err := Read(town, polecat)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Progress != nil || rec.Agent != "deepseek-flash-sandbox" || rec.EffectiveDesired() != DesiredRun {
			t.Fatalf("record after the sling = %+v, want no evidence, the new agent, still run", rec)
		}
	})

	t.Run("held_seat", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		if _, err := Update(town, polecat, func(r *Record) error {
			r.Desired, r.Reason = DesiredPark, "operator inspecting"
			r.Progress = dead
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := MarkSlung(town, polecat, "deepseek-flash-sandbox", "gt sling", now); err != nil {
			t.Fatal(err)
		}
		rec, err := Read(town, polecat)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Desired != DesiredPark || rec.Reason != "operator inspecting" ||
			rec.Progress == nil || rec.Progress.DeadSamples != 1 || rec.Agent != "" {
			t.Fatalf("a held seat was written to: %+v", rec)
		}
	})
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

// recordJSON returns the test seat's record file as the raw JSON object it was
// written as, so a test can assert the exact keys and not only what a typed
// read makes of them.
func recordJSON(t *testing.T, town string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(polecat.Path(town))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("record is not a JSON object: %v\n%s", err, data)
	}
	return raw
}

// gt-chcarg: the finished-turn facts the agent bead carries today (turn state,
// exit type, cleanup status, last work bead) land in the intent record, under
// the keys the design names, so a reader can leave the bead.
func TestAgentBeadRetireMarkTurnEndedRoundTrips(t *testing.T) {
	t.Parallel()
	seeded := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	now := seeded.Add(2 * time.Hour)
	for _, c := range []struct {
		name string
		end  TurnEnd
	}{
		{"completed", TurnEnd{TurnState: "done", ExitType: "COMPLETED", CleanupStatus: "clean", WorkBead: "gt-work"}},
		{"deferred", TurnEnd{TurnState: "stuck", ExitType: "DEFERRED", CleanupStatus: "has_uncommitted", WorkBead: "gt-other"}},
		{"escalated", TurnEnd{TurnState: "awaiting-gate", ExitType: "ESCALATED", CleanupStatus: "has_unpushed", WorkBead: "gt-third"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if _, err := Update(town, polecat, func(r *Record) error {
				r.Desired = DesiredRun
				r.WorkBead = "gt-waiting-to-land"
				r.Actor, r.UpdatedAt = "gt sling", seeded
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			if err := MarkTurnEnded(town, polecat, c.end, "gt done", now); err != nil {
				t.Fatalf("MarkTurnEnded: %v", err)
			}
			rec, err := Read(town, polecat)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if rec.TurnState != c.end.TurnState || rec.ExitType != c.end.ExitType ||
				rec.CleanupStatus != c.end.CleanupStatus || rec.LastWorkBead != c.end.WorkBead {
				t.Fatalf("record after MarkTurnEnded = %+v, want the turn's facts %+v", rec, c.end)
			}
			// WorkBead is the work waiting to land, not the turn's bead, and the
			// write leaves it alone.
			if rec.WorkBead != "gt-waiting-to-land" {
				t.Fatalf("WorkBead = %q, want it left as gt-waiting-to-land", rec.WorkBead)
			}
			if rec.Actor != "gt done" || !rec.UpdatedAt.Equal(now) {
				t.Fatalf("record actor/updated_at = %q/%s, want gt done/%s", rec.Actor, rec.UpdatedAt, now)
			}

			raw := recordJSON(t, town)
			for key, want := range map[string]string{
				"turn_state":     c.end.TurnState,
				"exit_type":      c.end.ExitType,
				"cleanup_status": c.end.CleanupStatus,
				"last_work_bead": c.end.WorkBead,
			} {
				got, isString := raw[key].(string)
				if !isString || got != want {
					t.Errorf("file key %q = %v, want the string %q (in %v)", key, raw[key], want, raw)
				}
			}
		})
	}
}

// The finished-turn facts are observations of what the session did, so a park or
// a freeze does not stop the write; the hold itself is left exactly as it was
// and the record still reads as held.
func TestAgentBeadRetireMarkTurnEndedLeavesAHoldStanding(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 11, 0, 0, 0, time.UTC)
	end := TurnEnd{TurnState: "done", ExitType: "COMPLETED", CleanupStatus: "clean", WorkBead: "gt-work"}
	for _, c := range []struct {
		name   string
		reason string
		apply  func(r *Record)
	}{
		{"parked", "operator inspecting", func(r *Record) { r.Desired, r.Reason = DesiredPark, "operator inspecting" }},
		{"frozen", "budget", func(r *Record) { r.Frozen, r.Reason = true, "budget" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if _, err := Update(town, polecat, func(r *Record) error {
				c.apply(r)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := MarkTurnEnded(town, polecat, end, "gt done", now); err != nil {
				t.Fatalf("MarkTurnEnded: %v", err)
			}
			rec, err := Read(town, polecat)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if !rec.Held() || !rec.Paused || rec.Reason != c.reason {
				t.Fatalf("hold after MarkTurnEnded = %+v, want the %s seat still held with reason %q", rec, c.name, c.reason)
			}
			if rec.TurnState != end.TurnState || rec.ExitType != end.ExitType ||
				rec.CleanupStatus != end.CleanupStatus || rec.LastWorkBead != end.WorkBead {
				t.Fatalf("record after MarkTurnEnded = %+v, want the turn's facts recorded on the held seat", rec)
			}
		})
	}
}

// The fail-closed rule survives the new writer: a record that cannot be parsed
// is still held after MarkTurnEnded, with the facts beside the hold.
func TestAgentBeadRetireMarkTurnEndedKeepsAFailClosedRecordHeld(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := polecat.Path(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	end := TurnEnd{TurnState: "done", ExitType: "COMPLETED", CleanupStatus: "clean", WorkBead: "gt-work"}
	if err := MarkTurnEnded(town, polecat, end, "gt done", time.Date(2026, 11, 15, 11, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MarkTurnEnded: %v", err)
	}
	rec, err := Read(town, polecat)
	if err != nil {
		t.Fatalf("Read after the rewrite: %v", err)
	}
	if !rec.Held() || !rec.Frozen || !rec.Paused {
		t.Fatalf("record after MarkTurnEnded = %+v, want it still held and frozen", rec)
	}
	if rec.TurnState != end.TurnState || rec.LastWorkBead != end.WorkBead {
		t.Fatalf("record = %+v, want the turn's facts beside the hold", rec)
	}
}

// A sling starts a new session, so the previous turn's facts go with it; the
// last work bead is what the seat did, not what it is doing, and stays.
func TestAgentBeadRetireMarkSlungClearsTheTurnFacts(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	if err := MarkTurnEnded(town, polecat, TurnEnd{
		TurnState: "done", ExitType: "COMPLETED", CleanupStatus: "clean", WorkBead: "gt-work",
	}, "gt done", now); err != nil {
		t.Fatal(err)
	}

	if err := MarkSlung(town, polecat, "deepseek-flash", "gt sling", now.Add(time.Hour)); err != nil {
		t.Fatalf("MarkSlung: %v", err)
	}
	rec, err := Read(town, polecat)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if rec.TurnState != "" || rec.ExitType != "" || rec.CleanupStatus != "" {
		t.Fatalf("record after MarkSlung = %+v, want the turn's state, exit type and cleanup status cleared", rec)
	}
	if rec.LastWorkBead != "gt-work" || rec.Agent != "deepseek-flash" {
		t.Fatalf("record after MarkSlung = %+v, want last_work_bead kept and the new agent", rec)
	}
	raw := recordJSON(t, town)
	for _, key := range []string{"turn_state", "exit_type", "cleanup_status"} {
		if _, ok := raw[key]; ok {
			t.Errorf("cleared key %q is still in the file: %v", key, raw)
		}
	}
	if got, isString := raw["last_work_bead"].(string); !isString || got != "gt-work" {
		t.Errorf("last_work_bead = %v, want gt-work kept in %v", raw["last_work_bead"], raw)
	}
}

// A record written before these fields existed carries none of them: it reads
// back unchanged, and a rewrite does not add them.
func TestAgentBeadRetireLegacyRecordReadsAndRewritesUnchanged(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := polecat.Path(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	legacy := `{"version":1,"desired":"run","work_bead":"gt-abc","agent":"deepseek-flash",` +
		`"actor":"gt sling","updated_at":"2026-11-14T08:00:00Z","submitted_at":"2026-11-14T08:00:00Z","paused":false}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	want := Record{
		Version: Version, Desired: DesiredRun, WorkBead: "gt-abc", Agent: "deepseek-flash",
		Actor: "gt sling", SubmittedAt: at, UpdatedAt: at,
	}
	rec, err := Read(town, polecat)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !reflect.DeepEqual(rec, want) {
		t.Fatalf("legacy record read as\n %+v\nwant\n %+v", rec, want)
	}

	if _, err := Update(town, polecat, func(r *Record) error { r.Actor = "rewrite"; return nil }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	raw := recordJSON(t, town)
	for _, key := range []string{"turn_state", "exit_type", "cleanup_status", "last_work_bead"} {
		if _, ok := raw[key]; ok {
			t.Errorf("a rewrite added %q to a record that never had it: %v", key, raw)
		}
	}
	for key, want := range map[string]any{"work_bead": "gt-abc", "agent": "deepseek-flash", "actor": "rewrite"} {
		if got := raw[key]; got != want {
			t.Errorf("rewrite changed %q: %v, want %v (in %v)", key, got, want, raw)
		}
	}
}

// SetCleanupStatus answers a cleanup status found after the turn's facts were
// recorded: it changes that one field and UpdatedAt, and the actor that recorded
// the turn stays.
func TestAgentBeadRetireSetCleanupStatusChangesOnlyCleanupStatus(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	seeded := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	now := seeded.Add(90 * time.Minute)
	if _, err := Update(town, polecat, func(r *Record) error {
		r.Desired = DesiredRun
		r.WorkBead = "gt-live"
		r.Agent = "deepseek-flash"
		r.LastWorkBead = "gt-work"
		r.TurnState, r.ExitType, r.CleanupStatus = "done", "COMPLETED", "clean"
		r.SubmittedAt = seeded.Add(-time.Hour)
		r.Restarts = []time.Time{seeded.Add(-2 * time.Hour)}
		r.Progress = &Progress{SampledAt: seeded, DeadSamples: 2}
		r.Actor, r.UpdatedAt = "gt done", seeded
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := Read(town, polecat)
	if err != nil {
		t.Fatal(err)
	}

	if err := SetCleanupStatus(town, polecat, "has_unpushed", "overrun guard", now); err != nil {
		t.Fatalf("SetCleanupStatus: %v", err)
	}
	after, err := Read(town, polecat)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := before
	want.CleanupStatus = "has_unpushed"
	want.UpdatedAt = now
	if !reflect.DeepEqual(after, want) {
		t.Fatalf("record after SetCleanupStatus =\n %+v\nwant only cleanup_status and updated_at to change, from\n %+v", after, before)
	}
	raw := recordJSON(t, town)
	if got, isString := raw["cleanup_status"].(string); !isString || got != "has_unpushed" {
		t.Fatalf("cleanup_status in the file = %v, want has_unpushed", raw["cleanup_status"])
	}
}
