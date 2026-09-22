package agentpause

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
// with an empty rig there is no agent bead ID to look up, so PauseGate
// must return "not paused" without touching the client. (Real bead-layer
// fallback coverage would need a Dolt fixture; the file layer is the
// authoritative path and is covered above.)
func TestPauseGateEmptyRigShortCircuits(t *testing.T) {
	town := t.TempDir()

	got, _, err := PauseGate(town, "", "polecat", "flint", nil)
	if err != nil || got {
		t.Fatalf("empty rig: got (%v, %v), want (false, nil)", got, err)
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