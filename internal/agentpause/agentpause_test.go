package agentpause

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPauseIsPausedRoundTrip(t *testing.T) {
	town := t.TempDir()
	role, name := "polecat", "flint"

	if got, st, err := IsPaused(town, "gastown", role, name); err != nil || got || st != nil {
		t.Fatalf("pre-pause: got (%v, %v, %v), want (false, nil, nil)", got, st, err)
	}

	if err := Pause(town, "gastown", role, name, "misbehaving", "mayor", "working"); err != nil {
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
	if st.PriorAgentState != "working" {
		t.Errorf("priorAgentState = %q, want %q", st.PriorAgentState, "working")
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

	if err := Pause(town, "gastown", "polecat", "jade", "test", "human", ""); err != nil {
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

// TestPauseGateIsFileLayerOnly pins the simplified design (gt-ahik): the
// marker file is the ONLY source of truth. PauseGate has no bead to consult,
// no reader argument, nothing to disagree with.
func TestPauseGateIsFileLayerOnly(t *testing.T) {
	town := t.TempDir()

	// No marker → not paused.
	got, st, err := PauseGate(town, "gastown", "polecat", "flint")
	if err != nil || got || st != nil {
		t.Fatalf("no marker: got (%v, %v, %v)", got, st, err)
	}

	if err := Pause(town, "gastown", "polecat", "flint", "frozen", "human", ""); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	got, st, err = PauseGate(town, "gastown", "polecat", "flint")
	if err != nil || !got {
		t.Fatalf("file-layer pause: got (%v, %v, %v), want (true, state, nil)", got, st, err)
	}
	if st.Reason != "frozen" {
		t.Errorf("reason = %q, want %q", st.Reason, "frozen")
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

	paused, st, err := PauseGate(town, "gastown", "polecat", "flint")
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
	if err := Pause(town, "gastown", "polecat", "flint", "first", "human", ""); err != nil {
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
		if err := Pause(town, "gastown", "polecat", "flint", fmt.Sprintf("reason %d", i), "human", ""); err != nil {
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
	if err := Pause(town, "gastown", "polecat", "flint", "x", "human", ""); err != nil {
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

	if err := Pause(town, "gastown", "polecat", "flint", "the real reason", "mayor", ""); err != nil {
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

	if err := Pause(town, "gastown", "polecat", "flint", "looping", "mayor", ""); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := Pause(town, "gastown", "witness", "", "parked", "human", ""); err != nil {
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
