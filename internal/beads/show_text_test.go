package beads

import (
	"path/filepath"
	"slices"
	"testing"
)

// TestShowTextHandsBdTheFlagsUnchanged is gt show's promise: bd gets the argv
// the operator typed, and its text comes back as it printed it.
func TestShowTextHandsBdTheFlagsUnchanged(t *testing.T) {
	t.Parallel()
	const printed = "◇ gt-abc · title   [P2 · OPEN]\n"
	r := newRecorder(func([]string) reply { return reply{stdout: printed} })
	b := newRecordedBeads(t.TempDir(), r)

	args := []string{"gt-abc", "--json", "-v"}
	text, err := b.ShowText("gt-abc", args)
	if err != nil {
		t.Fatalf("ShowText: %v", err)
	}
	if text != printed {
		t.Errorf("ShowText = %q, want bd's stdout unchanged", text)
	}
	if got, want := r.argvs(), []string{"show gt-abc --json -v"}; !slices.Equal(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
	calls := r.calls()
	if _, ok := lastEnvValue(calls[0].env, "BD_MACHINE"); ok {
		t.Error("BD_MACHINE must be absent: gt show prints bd's prose, not the envelope")
	}
	if v, _ := lastEnvValue(calls[0].env, "BD_READONLY"); v != "true" {
		t.Errorf("BD_READONLY = %q, want true: gt show never writes", v)
	}
}

// TestShowTextRoutesTheIDToItsDatabase keeps the pinning gt show had when it
// exec'd bd itself: a rig-prefixed id, read from the town, is answered by the
// rig's database and not by the town's.
func TestShowTextRoutesTheIDToItsDatabase(t *testing.T) {
	t.Parallel()
	townRoot, _ := townWithRoutes(t)
	r := newRecorder(nil)
	b := newBeads(beadsFields{workDir: townRoot, baseEnv: []string{"PATH=/usr/bin"}, exec: r.exec})

	if _, err := b.ShowText("gt-abc", []string{"gt-abc"}); err != nil {
		t.Fatalf("ShowText: %v", err)
	}
	calls := r.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want one", len(calls))
	}
	rigDir := filepath.Join(townRoot, "gastown", "mayor", "rig")
	if got, want := calls[0].dir, rigDir; got != want {
		t.Errorf("dir = %q, want %q", got, want)
	}
	beadsDir, _ := lastEnvValue(calls[0].env, "BEADS_DIR")
	if want := filepath.Join(rigDir, ".beads"); beadsDir != want {
		t.Errorf("BEADS_DIR = %q, want %q", beadsDir, want)
	}
	if db, _ := lastEnvValue(calls[0].env, "BEADS_DOLT_SERVER_DATABASE"); db != "gt" {
		t.Errorf("BEADS_DOLT_SERVER_DATABASE = %q, want gt (the rig's database)", db)
	}
}
