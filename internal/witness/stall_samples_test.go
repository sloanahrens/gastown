package witness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stallTestWindow is the gt-xb27 comparison window used throughout.
const stallTestWindow = 30 * time.Minute

// stagedActivity builds a live, transcript-dated observation of polecat at
// observedAt, as ObserveRealActivity would report it.
func stagedActivity(polecat string, observedAt, transcriptMtime time.Time, bytes int64, pane string) RealActivity {
	return RealActivity{
		Polecat:         polecat,
		Session:         "gt-" + polecat,
		AgentAlive:      true,
		ObservedAt:      observedAt,
		LastActivity:    transcriptMtime,
		ActivitySource:  ActivitySourceTranscript,
		TranscriptPath:  "/transcripts/" + polecat + ".jsonl",
		TranscriptBytes: bytes,
		PaneSignature:   pane,
	}
}

func checkFor(t *testing.T, checks []StallCheck, polecat string) StallCheck {
	t.Helper()
	for _, c := range checks {
		if c.Polecat == polecat {
			return c
		}
	}
	t.Fatalf("no stall check for %s in %+v", polecat, checks)
	return StallCheck{}
}

func mustTrack(t *testing.T, store *StallSampleStore, observed []RealActivity, now time.Time) []StallCheck {
	t.Helper()
	checks, err := TrackStalls(store, observed, stallTestWindow, now)
	if err != nil {
		t.Fatalf("TrackStalls: %v", err)
	}
	return checks
}

// TestTrackStalls_Sample1SurvivesRespawn is the claude-8w7 step 1 regression:
// sample 1 is recorded by one witness session and compared by the next. A new
// store instance stands in for the respawned witness, which has nothing in its
// context but the file on disk.
func TestTrackStalls_Sample1SurvivesRespawn(t *testing.T) {
	town := t.TempDir()
	t0 := time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)
	mtime := t0.Add(-40 * time.Minute)

	// Witness session 1 takes sample 1, then is respawned.
	first := mustTrack(t, NewStallSampleStore(town, "gastown"),
		[]RealActivity{stagedActivity("opal", t0, mtime, 4096, "sig-frozen")}, t0)
	c := checkFor(t, first, "opal")
	if c.Stalled || !c.Recorded {
		t.Fatalf("first observation must record sample 1 and never judge: %+v", c)
	}
	if _, err := os.Stat(filepath.Join(WitnessStateDir(town, "gastown"), stallSamplesFileName)); err != nil {
		t.Fatalf("sample 1 not persisted: %v", err)
	}

	// Witness session 2 (fresh store, fresh process) takes sample 2 a full
	// window later: nothing changed, so this is a positive stall signal.
	t1 := t0.Add(31 * time.Minute)
	second := mustTrack(t, NewStallSampleStore(town, "gastown"),
		[]RealActivity{stagedActivity("opal", t1, mtime, 4096, "sig-frozen")}, t1)
	c = checkFor(t, second, "opal")
	if !c.Stalled {
		t.Fatalf("sample 2 must compare against the persisted sample 1 and find a stall: %+v", c)
	}
	if c.Recorded {
		t.Errorf("a stall verdict must keep sample 1, not replace it: %+v", c)
	}
	if !c.Baseline.SampledAt.Equal(t0) {
		t.Errorf("baseline sampled_at = %v, want %v", c.Baseline.SampledAt, t0)
	}
}

// TestTrackStalls_TooSoonKeepsSample1 checks that a scan inside the window does
// not overwrite sample 1 — otherwise a witness scanning every 5 minutes would
// never accumulate a 30-minute window.
func TestTrackStalls_TooSoonKeepsSample1(t *testing.T) {
	town := t.TempDir()
	t0 := time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)
	mtime := t0.Add(-40 * time.Minute)

	for i, offset := range []time.Duration{0, 5 * time.Minute, 10 * time.Minute, 20 * time.Minute} {
		now := t0.Add(offset)
		checks := mustTrack(t, NewStallSampleStore(town, "gastown"),
			[]RealActivity{stagedActivity("opal", now, mtime, 4096, "sig")}, now)
		c := checkFor(t, checks, "opal")
		if c.Stalled {
			t.Fatalf("scan %d (%v after sample 1) must not be a stall: %+v", i, offset, c)
		}
		if i > 0 && (c.Recorded || !c.Baseline.SampledAt.Equal(t0)) {
			t.Fatalf("scan %d replaced sample 1: %+v", i, c)
		}
	}

	now := t0.Add(30 * time.Minute)
	c := checkFor(t, mustTrack(t, NewStallSampleStore(town, "gastown"),
		[]RealActivity{stagedActivity("opal", now, mtime, 4096, "sig")}, now), "opal")
	if !c.Stalled {
		t.Fatalf("a full window after the kept sample 1 must be a stall: %+v", c)
	}
}

// TestTrackStalls_WorkResetsSample1 covers the two progress signals: a working
// polecat re-records sample 1 so the window restarts from its latest work.
func TestTrackStalls_WorkResetsSample1(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)
	mtime := t0.Add(-40 * time.Minute)
	t1 := t0.Add(45 * time.Minute)

	cases := map[string]RealActivity{
		"transcript grew":    stagedActivity("opal", t1, mtime, 9999, "sig"),
		"transcript touched": stagedActivity("opal", t1, t0.Add(10*time.Minute), 4096, "sig"),
		"pane changed":       stagedActivity("opal", t1, mtime, 4096, "sig-moved"),
	}
	for name, cur := range cases {
		t.Run(name, func(t *testing.T) {
			town := t.TempDir()
			mustTrack(t, NewStallSampleStore(town, "gastown"),
				[]RealActivity{stagedActivity("opal", t0, mtime, 4096, "sig")}, t0)

			c := checkFor(t, mustTrack(t, NewStallSampleStore(town, "gastown"), []RealActivity{cur}, t1), "opal")
			if c.Stalled {
				t.Fatalf("a working polecat is never stalled: %+v", c)
			}
			if !c.Recorded {
				t.Fatalf("work since sample 1 must re-record it: %+v", c)
			}

			// The window restarts: a scan shortly after is not a stall even
			// though the first sample is now 50 minutes old.
			t2 := t1.Add(5 * time.Minute)
			again := cur
			again.ObservedAt = t2
			c = checkFor(t, mustTrack(t, NewStallSampleStore(town, "gastown"), []RealActivity{again}, t2), "opal")
			if c.Stalled || !c.Baseline.SampledAt.Equal(t1) {
				t.Fatalf("window must restart at the re-recorded sample: %+v", c)
			}
		})
	}
}

// TestTrackStalls_NewSessionStartsOver: a sample written for another tmux
// session or transcript (restarted polecat, reused name) is never compared.
func TestTrackStalls_NewSessionStartsOver(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)
	mtime := t0.Add(-40 * time.Minute)
	t1 := t0.Add(45 * time.Minute)

	for name, mutate := range map[string]func(*RealActivity){
		"session":    func(a *RealActivity) { a.Session = "gt-opal-restarted" },
		"transcript": func(a *RealActivity) { a.TranscriptPath = "/transcripts/opal-2.jsonl" },
	} {
		t.Run(name, func(t *testing.T) {
			town := t.TempDir()
			mustTrack(t, NewStallSampleStore(town, "gastown"),
				[]RealActivity{stagedActivity("opal", t0, mtime, 4096, "sig")}, t0)
			cur := stagedActivity("opal", t1, mtime, 4096, "sig")
			mutate(&cur)
			c := checkFor(t, mustTrack(t, NewStallSampleStore(town, "gastown"), []RealActivity{cur}, t1), "opal")
			if c.Stalled || !c.Recorded {
				t.Fatalf("a sample from another %s must be replaced, not compared: %+v", name, c)
			}
		})
	}
}

// TestTrackStalls_InsufficientEvidenceNeverStalls: a dead agent or a missing
// pane signature on sample 2 keeps sample 1 but never yields a stall.
func TestTrackStalls_InsufficientEvidenceNeverStalls(t *testing.T) {
	t0 := time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)
	mtime := t0.Add(-40 * time.Minute)
	t1 := t0.Add(45 * time.Minute)

	for name, mutate := range map[string]func(*RealActivity){
		"agent dead":    func(a *RealActivity) { a.AgentAlive = false },
		"no pane sig":   func(a *RealActivity) { a.PaneSignature = "" },
		"no transcript": func(a *RealActivity) { a.ActivitySource = ActivitySourceNone; a.LastActivity = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			town := t.TempDir()
			mustTrack(t, NewStallSampleStore(town, "gastown"),
				[]RealActivity{stagedActivity("opal", t0, mtime, 4096, "sig")}, t0)
			cur := stagedActivity("opal", t1, mtime, 4096, "sig")
			mutate(&cur)
			c := checkFor(t, mustTrack(t, NewStallSampleStore(town, "gastown"), []RealActivity{cur}, t1), "opal")
			if c.Stalled {
				t.Fatalf("%s must not be a stall: %+v", name, c)
			}
			if c.Recorded {
				t.Fatalf("%s must keep sample 1 for the next scan: %+v", name, c)
			}
		})
	}
}

// TestTrackStalls_UndatedFirstSampleIsReplaced: a sample 1 without a transcript
// date cannot anchor a comparison, so a later dated observation replaces it.
func TestTrackStalls_UndatedFirstSampleIsReplaced(t *testing.T) {
	town := t.TempDir()
	t0 := time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)
	undated := stagedActivity("opal", t0, time.Time{}, 0, "sig")
	undated.ActivitySource = ActivitySourceNone
	mustTrack(t, NewStallSampleStore(town, "gastown"), []RealActivity{undated}, t0)

	t1 := t0.Add(45 * time.Minute)
	c := checkFor(t, mustTrack(t, NewStallSampleStore(town, "gastown"),
		[]RealActivity{stagedActivity("opal", t1, t0.Add(-time.Hour), 4096, "sig")}, t1), "opal")
	if c.Stalled || !c.Recorded {
		t.Fatalf("an undated sample 1 must be replaced, not compared: %+v", c)
	}
}

// TestTrackStalls_PrunesPolecatsThatWentAway: when a polecat no longer has a
// live session its sample is deleted, so a later session under the same name
// can never be judged against it.
func TestTrackStalls_PrunesPolecatsThatWentAway(t *testing.T) {
	town := t.TempDir()
	t0 := time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)
	mtime := t0.Add(-time.Hour)
	mustTrack(t, NewStallSampleStore(town, "gastown"), []RealActivity{
		stagedActivity("opal", t0, mtime, 1, "a"),
		stagedActivity("lapis", t0, mtime, 1, "b"),
	}, t0)

	t1 := t0.Add(time.Minute)
	mustTrack(t, NewStallSampleStore(town, "gastown"), []RealActivity{stagedActivity("opal", t1, mtime, 1, "a")}, t1)

	samples, err := NewStallSampleStore(town, "gastown").Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := samples["lapis"]; ok {
		t.Errorf("lapis went away but its sample survived: %+v", samples)
	}
	if _, ok := samples["opal"]; !ok {
		t.Errorf("opal is still live but its sample was pruned: %+v", samples)
	}

	// No live polecats at all: the file is emptied, not left stale.
	mustTrack(t, NewStallSampleStore(town, "gastown"), nil, t1.Add(time.Minute))
	samples, err = NewStallSampleStore(town, "gastown").Load()
	if err != nil || len(samples) != 0 {
		t.Fatalf("expected no samples after every polecat went away, got %+v (err %v)", samples, err)
	}
}

// TestStallSampleStore_FileSchema pins the on-disk shape: the fields the design
// names, in a versioned envelope, written by rename (no temp file left behind).
func TestStallSampleStore_FileSchema(t *testing.T) {
	town := t.TempDir()
	t0 := time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)
	mustTrack(t, NewStallSampleStore(town, "gastown"),
		[]RealActivity{stagedActivity("opal", t0, t0.Add(-time.Minute), 77, "sig")}, t0)

	dir := WitnessStateDir(town, "gastown")
	data, err := os.ReadFile(filepath.Join(dir, stallSamplesFileName))
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Version int                               `json:"version"`
		Samples map[string]map[string]interface{} `json:"samples"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("file is not JSON: %v\n%s", err, data)
	}
	if raw.Version != 1 {
		t.Errorf("version = %d, want 1", raw.Version)
	}
	s := raw.Samples["opal"]
	for _, key := range []string{"polecat", "session", "sampled_at", "transcript_path", "transcript_mtime", "transcript_bytes", "pane_signature"} {
		if _, ok := s[key]; !ok {
			t.Errorf("sample is missing %q: %s", key, data)
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// TestStallSampleStore_CorruptFileStartsOver: an unreadable file is reported
// but never blocks the scan; fresh samples replace it.
func TestStallSampleStore_CorruptFileStartsOver(t *testing.T) {
	town := t.TempDir()
	dir := WitnessStateDir(town, "gastown")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stallSamplesFileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)
	checks, err := TrackStalls(NewStallSampleStore(town, "gastown"),
		[]RealActivity{stagedActivity("opal", t0, t0.Add(-time.Hour), 1, "sig")}, stallTestWindow, t0)
	if err == nil {
		t.Error("a corrupt file must be reported")
	}
	if c := checkFor(t, checks, "opal"); !c.Recorded || c.Stalled {
		t.Fatalf("a corrupt file must leave a fresh sample 1: %+v", c)
	}
	samples, loadErr := NewStallSampleStore(town, "gastown").Load()
	if loadErr != nil || samples["opal"].PaneSignature != "sig" {
		t.Fatalf("corrupt file must be replaced by fresh samples, got %+v (err %v)", samples, loadErr)
	}
}

// TestStallSampleStore_MissingFileIsEmpty: no file means no samples, no error.
func TestStallSampleStore_MissingFileIsEmpty(t *testing.T) {
	samples, err := NewStallSampleStore(t.TempDir(), "gastown").Load()
	if err != nil || len(samples) != 0 {
		t.Fatalf("missing file: samples=%v err=%v", samples, err)
	}
}
