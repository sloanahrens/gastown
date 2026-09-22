package agentpause

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestPauseIsPausedRoundTrip(t *testing.T) {
	town := t.TempDir()
	role, name := "polecat", "flint"

	if got, st, err := IsPaused(town, "gastown", role, name); err != nil || got || st != nil {
		t.Fatalf("pre-pause: got (%v, %v, %v), want (false, nil, nil)", got, st, err)
	}

	if err := Pause(town, "gastown", role, name, "misbehaving", "mayor"); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	got, st, err := IsPaused(town, "gastown", role, name)
	if err != nil {
		t.Fatalf("IsPaused: %v", err)
	}
	if !got {
		t.Fatal("want paused=true after Pause")
	}
	if st.Reason != "misbehaving" {
		t.Errorf("reason = %q, want %q", st.Reason, "misbehaving")
	}
	if st.PausedBy != "mayor" {
		t.Errorf("pausedBy = %q, want %q", st.PausedBy, "mayor")
	}
	if time.Since(st.PausedAt) > time.Minute {
		t.Errorf("pausedAt = %v, want recent", st.PausedAt)
	}
	if st.Address != "gastown/flint" {
		t.Errorf("address = %q, want %q", st.Address, "gastown/flint")
	}
}

func TestResumeClearsMarker(t *testing.T) {
	town := t.TempDir()

	if err := Pause(town, "gastown", "polecat", "jade", "test", "human"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := Resume(town, "gastown", "polecat", "jade"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got, _, err := IsPaused(town, "gastown", "polecat", "jade"); err != nil || got {
		t.Fatalf("post-resume: got (%v, %v), want (false, nil)", got, err)
	}
	// Resume on a non-paused agent is a no-op, not an error.
	if err := Resume(town, "gastown", "polecat", "jade"); err != nil {
		t.Fatalf("Resume on non-paused: %v", err)
	}
}

func TestFilePathSingleton(t *testing.T) {
	if got := FilePath("/t", "gastown", "witness", ""); got != filepath.Join("/t", ".runtime", "agents", "gastown", "witness.json") {
		t.Errorf("FilePath singleton = %q", got)
	}
	if got := FilePath("/t", "gastown", "polecat", "flint"); got != filepath.Join("/t", ".runtime", "agents", "gastown", "polecat.flint.json") {
		t.Errorf("FilePath named = %q", got)
	}
}

func TestPauseGateFileLayer(t *testing.T) {
	town := t.TempDir()

	// No marker → not paused, nil beadsClient is fine.
	got, st, err := PauseGate(town, "gastown", "polecat", "flint", nil)
	if err != nil || got || st != nil {
		t.Fatalf("no marker: got (%v, %v, %v)", got, st, err)
	}

	if err := Pause(town, "gastown", "polecat", "flint", "frozen", "human"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	got, st, err = PauseGate(town, "gastown", "polecat", "flint", nil)
	if err != nil || !got {
		t.Fatalf("file-layer pause: got (%v, %v, %v), want (true, state, nil)", got, st, err)
	}
	if st.Reason != "frozen" {
		t.Errorf("reason = %q, want %q", st.Reason, "frozen")
	}
}

// TestPauseGateEmptyRigShortCircuits pins the guard, not the bead layer:
// with an empty rig there is no agent bead ID to derive, so PauseGate must
// return "not paused" without touching the client. Town-level agents reach
// the bead layer through PauseGateBeadID instead.
func TestPauseGateEmptyRigShortCircuits(t *testing.T) {
	town := t.TempDir()
	reader := &fakeBeadReader{}

	got, _, err := PauseGate(town, "", "polecat", "flint", reader)
	if err != nil || got {
		t.Fatalf("empty rig: got (%v, %v), want (false, nil)", got, err)
	}
	if len(reader.asked) != 0 {
		t.Errorf("empty rig: bead layer consulted (%v), want short-circuit", reader.asked)
	}
}

// fakeBeadReader serves canned agent beads, so the bead-layer fallback is
// covered without a Dolt fixture (gt-wisp-6ajo).
type fakeBeadReader struct {
	issue  *beads.Issue
	fields *beads.AgentFields
	err    error
	asked  []string
}

func (f *fakeBeadReader) GetAgentBead(id string) (*beads.Issue, *beads.AgentFields, error) {
	f.asked = append(f.asked, id)
	return f.issue, f.fields, f.err
}

// pausedBead returns a reader serving an agent bead with the given state.
func pausedBead(state beads.AgentState, updatedAt string) *fakeBeadReader {
	return &fakeBeadReader{
		issue:  &beads.Issue{ID: "gt-flint", UpdatedAt: updatedAt},
		fields: &beads.AgentFields{AgentState: string(state)},
	}
}

func TestBeadPaused(t *testing.T) {
	cases := []struct {
		name    string
		reader  BeadReader
		beadID  string
		want    bool
		wantErr bool
	}{
		{"nil reader", nil, "gt-flint", false, false},
		{"no bead id", pausedBead(beads.AgentStatePaused, ""), "", false, false},
		{"paused", pausedBead(beads.AgentStatePaused, "2026-09-22T10:00:00Z"), "gt-flint", true, false},
		{"idle", pausedBead(beads.AgentStateIdle, ""), "gt-flint", false, false},
		{"working", pausedBead(beads.AgentStateWorking, ""), "gt-flint", false, false},
		{"read error", &fakeBeadReader{err: errors.New("dolt down")}, "gt-flint", false, true},
		{
			"no agent bead",
			&fakeBeadReader{},
			"gt-flint", false, false,
		},
		{
			"bead without agent fields",
			&fakeBeadReader{issue: &beads.Issue{ID: "gt-flint"}},
			"gt-flint", false, false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, st, err := BeadPaused(tc.reader, tc.beadID)
			if (err != nil) != tc.wantErr {
				t.Fatalf("BeadPaused err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("BeadPaused = %v, want %v", got, tc.want)
			}
			if tc.want && (st == nil || !st.Paused) {
				t.Errorf("BeadPaused state = %+v, want paused state", st)
			}
			if tc.want && tc.name == "paused" && st.PausedAt.IsZero() {
				t.Errorf("BeadPaused did not carry UpdatedAt as PausedAt: %+v", st)
			}
		})
	}
}

// TestPauseGateBeadLayerFallback is the case the two-layer design exists for:
// the marker file is gone (lost, cleaned, someone removed it) but the agent
// bead still carries agent_state=paused. The gate must honor it.
func TestPauseGateBeadLayerFallback(t *testing.T) {
	town := t.TempDir()
	reader := pausedBead(beads.AgentStatePaused, "")

	got, st, err := PauseGate(town, "gastown", "polecat", "flint", reader)
	if err != nil {
		t.Fatalf("PauseGate: %v", err)
	}
	if !got {
		t.Fatal("bead-layer-only pause not detected — PauseGate fallback is dead")
	}
	if st == nil || !st.Paused {
		t.Fatalf("PauseGate state = %+v, want paused state", st)
	}
	wantID := beads.AgentBeadIDWithPrefix(beads.GetPrefixForRig(town, "gastown"), "gastown", "polecat", "flint")
	if len(reader.asked) != 1 || reader.asked[0] != wantID {
		t.Errorf("bead layer asked for %v, want [%s]", reader.asked, wantID)
	}
}

// TestPauseGateFileLayerWins pins the layer precedence: a marker file is
// authoritative, so its reason is what scanners log and gt status shows.
//
// ReadLayers reads both layers even when the file layer already settles it
// (resume needs both, and one reader serves both callers); the hot-loop
// caller passes a nil reader and pays no read at all.
func TestPauseGateFileLayerWins(t *testing.T) {
	town := t.TempDir()
	if err := Pause(town, "gastown", "polecat", "flint", "filesystem scan", "mayor"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// The bead layer disagrees (idle): the file layer must still decide.
	reader := pausedBead(beads.AgentStateIdle, "")

	got, st, err := PauseGate(town, "gastown", "polecat", "flint", reader)
	if err != nil || !got {
		t.Fatalf("PauseGate = (%v, %+v, %v), want paused", got, st, err)
	}
	if st.Reason != "filesystem scan" || st.PausedBy != "mayor" {
		t.Errorf("state = %+v, want the marker's reason and actor", st)
	}
}

// TestReadLayers is the coverage for the resume path's question (gt-wisp-6ajo):
// a pause held in the bead layer ALONE — the marker-file loss the fallback
// exists for — must be visible and clearable, and a layer that cannot be read
// must never be reported as "not paused".
func TestReadLayers(t *testing.T) {
	// No marker anywhere: the file layer is empty for every case.
	town := t.TempDir()

	// Bead layer only: the marker file is gone, the agent bead still says
	// paused. This is the case resume must be able to clear.
	beadOnly := ReadLayers(town, "gastown", "polecat", "flint", "gt-flint", pausedBead(beads.AgentStatePaused, ""))
	if !beadOnly.Paused() {
		t.Fatal("bead-layer-only pause reported as not paused — resume would refuse to clear it")
	}
	if beadOnly.FilePaused || beadOnly.FileState != nil {
		t.Errorf("file layer should be empty: %+v", beadOnly)
	}
	if st := beadOnly.State(); st == nil || !st.Paused {
		t.Errorf("State() = %+v, want the bead layer's pause", st)
	}

	// Unreadable bead, no marker: unknown, so still "paused" to a caller that
	// only asks Paused(), with the error available for the warning.
	unreadable := ReadLayers(town, "gastown", "polecat", "flint", "gt-flint", &fakeBeadReader{err: errors.New("dolt down")})
	if !unreadable.Paused() {
		t.Error("a failed bead read was reported as not paused — resume would skip clearing")
	}
	if unreadable.Err() == nil {
		t.Error("Err() hid the bead read failure")
	}

	// Neither layer, both readable: the only case resume may call "not paused".
	clean := ReadLayers(town, "gastown", "polecat", "jade", "gt-jade", pausedBead(beads.AgentStateIdle, ""))
	if clean.Paused() || clean.Err() != nil {
		t.Errorf("clean idle agent = %+v err=%v, want not paused, no error", clean, clean.Err())
	}

	// With a marker file present the file layer settles it, and a failed bead
	// read is not reported as a check failure.
	if err := Pause(town, "gastown", "polecat", "flint", "filesystem scan", "mayor"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	both := ReadLayers(town, "gastown", "polecat", "flint", "gt-flint", pausedBead(beads.AgentStatePaused, ""))
	if !both.FilePaused || !both.BeadPaused {
		t.Errorf("both layers should report paused: %+v", both)
	}
	if both.Err() != nil {
		t.Errorf("Err() = %v, want nil when nothing failed", both.Err())
	}
	if st := both.State(); st == nil || st.Reason != "filesystem scan" {
		t.Errorf("State() = %+v, want the marker's reason to win", st)
	}
	decided := ReadLayers(town, "gastown", "polecat", "flint", "gt-flint", &fakeBeadReader{err: errors.New("dolt down")})
	if !decided.Paused() || decided.Err() != nil {
		t.Errorf("file-layer decision should not report a bead-layer error: %+v err=%v", decided, decided.Err())
	}
	if st := decided.State(); st == nil || st.Reason != "filesystem scan" {
		t.Errorf("State() = %+v, want the marker's reason", st)
	}
}

// TestPauseGateFailsClosedOnBeadReadError pins the invariant the restart
// choke point depends on: an error never comes back with paused=false, so a
// caller that ignores the error still refuses to touch the agent.
func TestPauseGateFailsClosedOnBeadReadError(t *testing.T) {
	town := t.TempDir()
	reader := &fakeBeadReader{err: errors.New("dolt down")}

	paused, _, err := PauseGate(town, "gastown", "polecat", "flint", reader)
	if err == nil {
		t.Fatal("PauseGate hid a bead read error")
	}
	if !paused {
		t.Fatal("PauseGate returned paused=false with an error — caller that checks only paused would restart a possibly-paused agent")
	}
}

// TestPauseGateFailsClosedOnBrokenMarker covers the file layer: a marker that
// exists but cannot be parsed is an intentional-freeze signal as far as the
// gate is concerned, error or not.
func TestPauseGateFailsClosedOnBrokenMarker(t *testing.T) {
	town := t.TempDir()
	path := FilePath(town, "gastown", "polecat", "flint")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"paused": tr`), 0o644); err != nil {
		t.Fatal(err)
	}

	paused, st, err := PauseGate(town, "gastown", "polecat", "flint", nil)
	if err == nil {
		t.Error("PauseGate hid the malformed-marker error")
	}
	if !paused {
		t.Fatal("malformed marker read as NOT paused — the gate fails open")
	}
	if st == nil || st.Reason == "" || st.Address != "gastown/flint" {
		t.Errorf("state = %+v, want a reason naming the problem and the derived address", st)
	}
}

// TestPauseGateBeadIDTownLevel covers the resume path for agents with no rig
// segment (mayor, deacon): PauseGateBeadID takes the already-resolved bead ID,
// so their bead-layer pause is reachable at all.
func TestPauseGateBeadIDTownLevel(t *testing.T) {
	town := t.TempDir()
	reader := pausedBead(beads.AgentStatePaused, "")

	got, st, err := PauseGateBeadID(town, "", "mayor", "", "hq-mayor", reader)
	if err != nil || !got || st == nil {
		t.Fatalf("town-level bead pause = (%v, %+v, %v), want paused", got, st, err)
	}

	// And the town-level marker file layer works through the same call.
	if err := Pause(town, "", "mayor", "", "operator hold", "human"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	got, st, err = PauseGateBeadID(town, "", "mayor", "", "hq-mayor", reader)
	if err != nil || !got || st.Reason != "operator hold" {
		t.Fatalf("town-level marker pause = (%v, %+v, %v), want the marker reason", got, st, err)
	}
}

// TestAddressForMatchesMarkerPath pins the invariant that lets `gt agent
// pause`/`resume` and the gt status banner name one agent one way: the
// address built from the marker coordinates is exactly the address derived
// from the marker path, so pause output and the status banner agree
// (gt-wisp-6ajo).
func TestAddressForMatchesMarkerPath(t *testing.T) {
	town := "/town"
	cases := []struct {
		rig, role, name, want string
	}{
		{"gastown", "polecat", "flint", "gastown/flint"},
		{"gastown", "witness", "", "gastown/witness"},
		{"gastown", "refinery", "", "gastown/refinery"},
		{"gastown", "crew", "opal", "gastown/crew/opal"},
		{"beads", "polecat", "jade", "beads/jade"},
		{"", "mayor", "", "mayor"},
		{"", "deacon", "", "deacon"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := AddressFor(tc.rig, tc.role, tc.name)
			if got != tc.want {
				t.Errorf("AddressFor(%q, %q, %q) = %q, want %q", tc.rig, tc.role, tc.name, got, tc.want)
			}
			path := FilePath(town, tc.rig, tc.role, tc.name)
			if fromPath := AddressFromMarkerPath(path); fromPath != got {
				t.Errorf("AddressFromMarkerPath(%s) = %q, but AddressFor said %q — the two must agree", path, fromPath, got)
			}
		})
	}
}

// TestPauseWriteIsAtomic hammers Pause while readers read the marker, and
// fails if any read observes a partial file. An in-place rewrite truncates
// first, so a reader (or a crash) can see an empty or half-written marker —
// which reads as paused and strands the agent with a bogus reason
// (gt-wisp-6ajo).
func TestPauseWriteIsAtomic(t *testing.T) {
	town := t.TempDir()
	path := FilePath(town, "gastown", "polecat", "flint")
	if err := Pause(town, "gastown", "polecat", "flint", "first", "human"); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	done := make(chan struct{})
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		partial []string
	)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				data, err := os.ReadFile(path)
				if err != nil {
					continue
				}
				var st State
				if err := json.Unmarshal(data, &st); err != nil {
					mu.Lock()
					partial = append(partial, fmt.Sprintf("%d bytes: %v", len(data), err))
					mu.Unlock()
					return
				}
				if !st.Paused {
					mu.Lock()
					partial = append(partial, fmt.Sprintf("%d bytes: parsed but paused=false", len(data)))
					mu.Unlock()
					return
				}
			}
		}()
	}
	// Enough writes to interleave with the readers, few enough that the
	// per-write fsync does not dominate the suite (this test is ~1.5s).
	for i := 0; i < 50; i++ {
		if err := Pause(town, "gastown", "polecat", "flint", fmt.Sprintf("reason %d", i), "human"); err != nil {
			t.Fatalf("Pause: %v", err)
		}
	}
	close(done)
	wg.Wait()

	if len(partial) > 0 {
		t.Errorf("readers observed %d partial marker writes (first: %s) — write is not atomic", len(partial), partial[0])
	}
}

// TestPauseLeavesNoTempFiles guards the temp-file half of the atomic write.
func TestPauseLeavesNoTempFiles(t *testing.T) {
	town := t.TempDir()
	if err := Pause(town, "gastown", "polecat", "flint", "x", "human"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(FilePath(town, "gastown", "polecat", "flint")))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "polecat.flint.json" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("marker dir = %v, want exactly [polecat.flint.json]", names)
	}
}

// TestPauseRepairsBrokenMarker: `gt agent pause` on an agent whose marker is
// unreadable must rewrite it (with the operator's reason), not report
// "already paused". IsPaused fails closed, so the CLI relies on the error to
// tell the two apart.
func TestPauseRepairsBrokenMarker(t *testing.T) {
	town := t.TempDir()
	path := FilePath(town, "gastown", "polecat", "flint")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("paused\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := IsPaused(town, "gastown", "polecat", "flint"); err == nil {
		t.Error("IsPaused returned no error for an unparseable marker; the CLI cannot detect the repair case")
	}

	if err := Pause(town, "gastown", "polecat", "flint", "the real reason", "mayor"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	paused, st, err := IsPaused(town, "gastown", "polecat", "flint")
	if err != nil || !paused || st.Reason != "the real reason" {
		t.Fatalf("after repair: (%v, %+v, %v), want the operator's reason", paused, st, err)
	}
}

// TestBrokenMarkerSurfacesEverywhere: a marker that cannot be read must not
// vanish from the surfaces that tell the operator what is going on, or a
// parked agent shows up as untouched in gt status while the gate refuses to
// restart it.
func TestBrokenMarkerSurfacesEverywhere(t *testing.T) {
	town := t.TempDir()
	path := FilePath(town, "gastown", "polecat", "flint")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"paused": tr`), 0o644); err != nil {
		t.Fatal(err)
	}

	if st := PausedByState(town, "gastown", "polecat", "flint"); st == nil {
		t.Error("PausedByState hid a broken marker from gt status")
	} else if !strings.Contains(st.Reason, "malformed") || st.Address != "gastown/flint" {
		t.Errorf("PausedByState = %+v, want a reason naming the problem and the agent address", st)
	}

	found := false
	for _, st := range ListPaused(town) {
		if st.Address == "gastown/flint" {
			found = true
		}
	}
	if !found {
		t.Error("ListPaused skipped a broken marker — the PAUSED banner would omit the agent")
	}
}

// TestUnpausedMarkerValueIsHonored: a marker that parses and says
// paused=false is an explicit "not paused", not a broken read. Fail-closed
// applies to unreadable state, not to readable state we dislike.
func TestUnpausedMarkerValueIsHonored(t *testing.T) {
	town := t.TempDir()
	path := FilePath(town, "gastown", "polecat", "flint")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"paused":false}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	paused, _, err := IsPaused(town, "gastown", "polecat", "flint")
	if err != nil || paused {
		t.Fatalf("IsPaused = (%v, %v), want (false, nil)", paused, err)
	}
	if st := PausedByState(town, "gastown", "polecat", "flint"); st != nil {
		t.Errorf("PausedByState = %+v, want nil", st)
	}
}

// TestAddressFromMarkerPath covers the address derivation gt status uses to
// name a paused agent in the PAUSED banner (gt-ahik). A banner that says
// "PAUSED" without naming which agent is not actionable.
func TestAddressFromMarkerPath(t *testing.T) {
	town := "/town"
	cases := []struct {
		name string
		path string
		want string
	}{
		{"polecat", FilePath(town, "gastown", "polecat", "flint"), "gastown/flint"},
		{"witness", FilePath(town, "gastown", "witness", ""), "gastown/witness"},
		{"refinery", FilePath(town, "gastown", "refinery", ""), "gastown/refinery"},
		{"crew", FilePath(town, "gastown", "crew", "opal"), "gastown/crew/opal"},
		{"second rig", FilePath(town, "beads", "polecat", "jade"), "beads/jade"},
		{"town-level singleton", filepath.Join(town, ".runtime", "agents", "deacon.json"), "deacon"},
		{"not a marker path", "/tmp/random.json", ""},
		{"no json suffix", filepath.Join(town, ".runtime", "agents", "gastown", "polecat.flint"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AddressFromMarkerPath(tc.path); got != tc.want {
				t.Errorf("AddressFromMarkerPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestReason(t *testing.T) {
	if got := Reason(nil); got != "(no reason given)" {
		t.Errorf("Reason(nil) = %q", got)
	}
	if got := Reason(&State{}); got != "(no reason given)" {
		t.Errorf("Reason(empty) = %q", got)
	}
	if got := Reason(&State{Reason: "  "}); got != "(no reason given)" {
		t.Errorf("Reason(whitespace) = %q", got)
	}
	if got := Reason(&State{Reason: "looping"}); got != "looping" {
		t.Errorf("Reason = %q", got)
	}
}

func TestListPaused(t *testing.T) {
	town := t.TempDir()

	if got := ListPaused(town); len(got) != 0 {
		t.Fatalf("empty town: got %d paused, want 0", len(got))
	}

	if err := Pause(town, "gastown", "polecat", "flint", "looping", "mayor"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := Pause(town, "gastown", "witness", "", "parked", "human"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// A non-paused marker is ignored by ListPaused.
	path := FilePath(town, "gastown", "polecat", "jade")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"paused":false}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ListPaused(town)
	if len(got) != 2 {
		t.Fatalf("ListPaused = %d entries, want 2", len(got))
	}
	// Each entry must name its agent so gt status can print which agent
	// is paused, not just that something is (gt-ahik).
	byAddress := map[string]string{}
	for _, st := range got {
		byAddress[st.Address] = st.Reason
	}
	want := map[string]string{"gastown/flint": "looping", "gastown/witness": "parked"}
	for addr, reason := range want {
		if byAddress[addr] != reason {
			t.Errorf("ListPaused[%q] reason = %q, want %q (got: %v)",
				addr, byAddress[addr], reason, byAddress)
		}
	}
}