package dispatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperatorHold_NoHoldFile_AllowsDispatch(t *testing.T) {
	t.Setenv(HoldFileEnv, "")
	townRoot := t.TempDir()
	if reason := OperatorHold(townRoot); reason != "" {
		t.Fatalf("OperatorHold on a town with no hold = %q, want \"\"", reason)
	}
}

func TestOperatorHold_HoldFilePresent_RefusesAndNamesTheFile(t *testing.T) {
	t.Setenv(HoldFileEnv, "")
	townRoot := t.TempDir()
	path := filepath.Join(townRoot, "seat-refill.hold")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatalf("write hold: %v", err)
	}
	reason := OperatorHold(townRoot)
	if reason == "" {
		t.Fatal("OperatorHold ignored the operator hold file")
	}
	if !strings.Contains(reason, path) {
		t.Errorf("reason %q does not name the hold file %s, so an operator cannot tell what to remove", reason, path)
	}
}

// An empty file is enough: the operator's gesture is `touch`, and the shell
// plugin tests only existence.
func TestOperatorHold_HoldDirectoryAlsoCounts(t *testing.T) {
	t.Setenv(HoldFileEnv, "")
	townRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(townRoot, "seat-refill.hold"), 0755); err != nil {
		t.Fatalf("mkdir hold: %v", err)
	}
	if OperatorHold(townRoot) == "" {
		t.Fatal("a hold path that exists as a directory must still hold, as `[ -e ]` does in run.sh")
	}
}

func TestOperatorHold_TownEstop_Refuses(t *testing.T) {
	t.Setenv(HoldFileEnv, "")
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, "ESTOP"), []byte("manual\t2026-09-24T00:00:00Z\ttest\n"), 0644); err != nil {
		t.Fatalf("write ESTOP: %v", err)
	}
	reason := OperatorHold(townRoot)
	if !strings.Contains(reason, "ESTOP") {
		t.Fatalf("OperatorHold under a town ESTOP = %q, want a reason naming ESTOP", reason)
	}
}

// An unreadable town root is a hold that cannot be ruled out; the hand brake
// fails closed rather than letting an automatic dispatcher through.
func TestOperatorHold_UnstatableHoldPath_FailsClosed(t *testing.T) {
	t.Setenv(HoldFileEnv, "")
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	parent := t.TempDir()
	townRoot := filepath.Join(parent, "town")
	if err := os.Mkdir(townRoot, 0755); err != nil {
		t.Fatalf("mkdir town: %v", err)
	}
	if err := os.Chmod(townRoot, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(townRoot, 0755) })

	if reason := OperatorHold(townRoot); reason == "" {
		t.Fatal("OperatorHold could not stat the hold path and still allowed dispatch")
	}
}

func TestOperatorHold_EmptyTownRoot_AllowsDispatch(t *testing.T) {
	if reason := OperatorHold(""); reason != "" {
		t.Fatalf("OperatorHold(\"\") = %q; with no town there is no hold to read", reason)
	}
}

// The hold file is the seat-refill plugin's; this pins that Go and the shell
// read the same path, so neither can drift to a name the other ignores.
func TestHoldFileName_MatchesSeatRefillPlugin(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "plugins", "seat-refill", "run.sh"))
	if err != nil {
		t.Fatalf("read seat-refill run.sh: %v", err)
	}
	want := `$TOWN_ROOT/` + HoldFileName
	if !strings.Contains(string(data), want) {
		t.Errorf("plugins/seat-refill/run.sh does not default its hold file to %s", want)
	}
}

// GT_SEAT_REFILL_HOLD relocates the hold file for run.sh (run.sh:60); Go
// honors the same override so one hold means one file everywhere.
func TestOperatorHold_EnvOverrideRelocatesHoldFile(t *testing.T) {
	townRoot := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "custom.hold")
	t.Setenv(HoldFileEnv, elsewhere)

	// The default path is no longer the hold.
	if err := os.WriteFile(filepath.Join(townRoot, HoldFileName), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if reason := OperatorHold(townRoot); reason != "" {
		t.Errorf("with %s set, the default path still held: %q", HoldFileEnv, reason)
	}
	if err := os.WriteFile(elsewhere, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if reason := OperatorHold(townRoot); !strings.Contains(reason, elsewhere) {
		t.Errorf("OperatorHold = %q, want it to name the overridden hold %s", reason, elsewhere)
	}
}

func TestOperatorHold_EmptyEnvOverrideUsesDefault(t *testing.T) {
	townRoot := t.TempDir()
	t.Setenv(HoldFileEnv, "")
	if err := os.WriteFile(filepath.Join(townRoot, HoldFileName), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if OperatorHold(townRoot) == "" {
		t.Error("an empty override must fall back to <town>/seat-refill.hold, as ${VAR:-default} does")
	}
}

func TestRigHold_RigEstopHoldsOnlyThatRig(t *testing.T) {
	townRoot := t.TempDir()
	t.Setenv(HoldFileEnv, "")
	if err := os.WriteFile(filepath.Join(townRoot, "ESTOP.gastown"), []byte("manual\t2026-09-24T00:00:00Z\tt\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if reason := RigHold(townRoot, "gastown"); !strings.Contains(reason, "ESTOP.gastown") {
		t.Errorf("RigHold(gastown) = %q, want a reason naming ESTOP.gastown", reason)
	}
	if reason := RigHold(townRoot, "om"); reason != "" {
		t.Errorf("RigHold(om) = %q; another rig's ESTOP must not hold it", reason)
	}
	if reason := RigHold(townRoot, ""); reason != "" {
		t.Errorf("RigHold with no rig = %q, want the town answer (none)", reason)
	}
}

func TestRigHold_TownHoldHoldsEveryRig(t *testing.T) {
	townRoot := t.TempDir()
	t.Setenv(HoldFileEnv, "")
	if err := os.WriteFile(filepath.Join(townRoot, HoldFileName), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if RigHold(townRoot, "om") == "" {
		t.Error("the town hold must hold every rig")
	}
}

func TestHoldLatch_ReportsOnlyTransitions(t *testing.T) {
	var l HoldLatch
	steps := []struct {
		reason string
		want   bool
	}{
		{"", false},      // never held: nothing to say
		{"held A", true}, // hold appears
		{"held A", false},
		{"held A", false},
		{"held B", true}, // reason changed
		{"", true},       // hold lifted
		{"", false},
	}
	for i, s := range steps {
		if got := l.Changed(s.reason); got != s.want {
			t.Errorf("step %d Changed(%q) = %v, want %v", i, s.reason, got, s.want)
		}
	}
}
