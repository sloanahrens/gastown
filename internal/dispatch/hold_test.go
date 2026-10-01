package dispatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperatorHold_NoHoldFile_AllowsDispatch(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if reason := operatorHold(townRoot, noEnv); reason != "" {
		t.Fatalf("OperatorHold on a town with no hold = %q, want \"\"", reason)
	}
}

func TestOperatorHold_HoldFilePresent_RefusesAndNamesTheFile(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := filepath.Join(townRoot, "seat-refill.hold")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatalf("write hold: %v", err)
	}
	reason := operatorHold(townRoot, noEnv)
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
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(townRoot, "seat-refill.hold"), 0755); err != nil {
		t.Fatalf("mkdir hold: %v", err)
	}
	if operatorHold(townRoot, noEnv) == "" {
		t.Fatal("a hold path that exists as a directory must still hold, as `[ -e ]` does in run.sh")
	}
}

func TestOperatorHold_TownEstop_Refuses(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, "ESTOP"), []byte("manual\t2026-09-24T00:00:00Z\ttest\n"), 0644); err != nil {
		t.Fatalf("write ESTOP: %v", err)
	}
	reason := operatorHold(townRoot, noEnv)
	if !strings.Contains(reason, "ESTOP") {
		t.Fatalf("OperatorHold under a town ESTOP = %q, want a reason naming ESTOP", reason)
	}
}

// A hold path that cannot be stat'ed for a reason other than not existing is
// a hold that cannot be ruled out; the hand brake fails closed rather than
// letting an automatic dispatcher through. A town root that is a regular file
// makes the stat fail with ENOTDIR, which is not "does not exist".
func TestOperatorHold_UnstatableHoldPath_FailsClosed(t *testing.T) {
	t.Parallel()
	townRoot := filepath.Join(t.TempDir(), "town")
	if err := os.WriteFile(townRoot, nil, 0644); err != nil {
		t.Fatalf("write town file: %v", err)
	}
	if reason := operatorHold(townRoot, noEnv); !strings.Contains(reason, "unreadable") {
		t.Fatalf("OperatorHold could not stat the hold path and returned %q, want an unreadable-hold refusal", reason)
	}
}

func TestOperatorHold_EmptyTownRoot_AllowsDispatch(t *testing.T) {
	t.Parallel()
	if reason := operatorHold("", noEnv); reason != "" {
		t.Fatalf("OperatorHold(\"\") = %q; with no town there is no hold to read", reason)
	}
}

// The hold file is the seat-refill plugin's; this pins that Go and the shell
// read the same path, so neither can drift to a name the other ignores.
func TestHoldFileName_MatchesSeatRefillPlugin(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	townRoot := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "custom.hold")
	env := envMap{HoldFileEnv: elsewhere}.get

	// The default path is no longer the hold.
	if err := os.WriteFile(filepath.Join(townRoot, HoldFileName), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if reason := operatorHold(townRoot, env); reason != "" {
		t.Errorf("with %s set, the default path still held: %q", HoldFileEnv, reason)
	}
	if err := os.WriteFile(elsewhere, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if reason := operatorHold(townRoot, env); !strings.Contains(reason, elsewhere) {
		t.Errorf("OperatorHold = %q, want it to name the overridden hold %s", reason, elsewhere)
	}
}

func TestOperatorHold_EmptyEnvOverrideUsesDefault(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	env := envMap{HoldFileEnv: ""}.get
	if err := os.WriteFile(filepath.Join(townRoot, HoldFileName), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if operatorHold(townRoot, env) == "" {
		t.Error("an empty override must fall back to <town>/seat-refill.hold, as ${VAR:-default} does")
	}
}

func TestRigHold_RigEstopHoldsOnlyThatRig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, "ESTOP.gastown"), []byte("manual\t2026-09-24T00:00:00Z\tt\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if reason := rigHold(townRoot, "gastown", noEnv); !strings.Contains(reason, "ESTOP.gastown") {
		t.Errorf("RigHold(gastown) = %q, want a reason naming ESTOP.gastown", reason)
	}
	if reason := rigHold(townRoot, "om", noEnv); reason != "" {
		t.Errorf("RigHold(om) = %q; another rig's ESTOP must not hold it", reason)
	}
	if reason := rigHold(townRoot, "", noEnv); reason != "" {
		t.Errorf("RigHold with no rig = %q, want the town answer (none)", reason)
	}
}

// An ESTOP sentinel whose presence cannot be determined holds dispatch, as
// the supervisor's e-stop check refuses (gt-e7lqk: the old stat failed open).
// The hold file is relocated so only the sentinel's stat fails: a town root
// that is a regular file makes it ENOTDIR, which is not "does not exist".
func TestRigHold_UnstatableEstop_FailsClosed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	townRoot := filepath.Join(dir, "town")
	if err := os.WriteFile(townRoot, nil, 0644); err != nil {
		t.Fatal(err)
	}
	env := envMap{HoldFileEnv: filepath.Join(dir, "no-hold")}.get
	for _, rig := range []string{"", "gastown"} {
		if reason := rigHold(townRoot, rig, env); !strings.Contains(reason, "ESTOP unreadable") {
			t.Errorf("rigHold(%q) with an unstatable ESTOP = %q, want an unreadable-ESTOP hold", rig, reason)
		}
	}
}

func TestRigHold_TownHoldHoldsEveryRig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, HoldFileName), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if rigHold(townRoot, "om", noEnv) == "" {
		t.Error("the town hold must hold every rig")
	}
}

func TestHoldLatch_ReportsOnlyTransitions(t *testing.T) {
	t.Parallel()
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

// envMap is a process environment for a test; missing keys read as "".
type envMap map[string]string

func (m envMap) get(k string) string { return m[k] }

// noEnv is an environment with nothing set.
func noEnv(string) string { return "" }

// The exported entry points are the injected ones reading the process
// environment.
func TestOperatorHold_ExportedWrappersReadProcessEnv(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, "ESTOP.om"), []byte("manual\t2026-09-24T00:00:00Z\tt\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got, want := HoldFilePath(townRoot), holdFilePath(townRoot, os.Getenv); got != want {
		t.Errorf("HoldFilePath = %q, want %q", got, want)
	}
	if got, want := OperatorHold(townRoot), operatorHold(townRoot, os.Getenv); got != want {
		t.Errorf("OperatorHold = %q, want %q", got, want)
	}
	if got, want := RigHold(townRoot, "om"), rigHold(townRoot, "om", os.Getenv); got != want {
		t.Errorf("RigHold(om) = %q, want %q", got, want)
	}
}
