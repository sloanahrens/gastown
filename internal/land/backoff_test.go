package land

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBackoffFileRoundTrip: the snapshot the landing worker writes reads back
// as it was written, beside the rig's landings file and readable by its
// owner alone (gt-fn9e6.44).
func TestBackoffFileRoundTrip(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f, err := RigBackoffFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	landings, err := RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(f.Path) != filepath.Dir(landings.Path) {
		t.Fatalf("backoff file %s is not beside the landings file %s", f.Path, landings.Path)
	}
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	want := BackoffState{Rig: "gastown", At: at, Beads: []BackoffRecord{{
		BeadID: "gt-abc", Stage: "push", Failures: 3,
		NextTry: at.Add(4 * time.Minute), Error: "the pre-push hook refused land/gt-abc",
	}}}
	if err := f.Write(want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", info.Mode().Perm())
	}
	b, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	var got BackoffState
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Rig != want.Rig || !got.At.Equal(want.At) || len(got.Beads) != 1 || got.Beads[0] != want.Beads[0] {
		t.Fatalf("read back %+v, want %+v", got, want)
	}

	// A later snapshot replaces the file rather than appending to it: the
	// reader must never see two passes at once.
	if err := f.Write(BackoffState{Rig: "gastown", At: at.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Beads) != 0 {
		t.Fatalf("read back %+v; want the replaced snapshot with nothing failing", got)
	}
}

func TestRigBackoffFileRefusesARigThatLeavesTheDirectory(t *testing.T) {
	t.Parallel()
	for _, rig := range []string{"", "..", "a/b", `a\b`} {
		if _, err := RigBackoffFile(t.TempDir(), rig); err == nil {
			t.Errorf("RigBackoffFile(%q) was accepted", rig)
		}
	}
}
