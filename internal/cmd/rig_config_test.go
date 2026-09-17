package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/wisp"
)

// setupTestRigForConfig creates a minimal Gas Town workspace for rig config testing.
// Returns townRoot and rigName.
func setupTestRigForConfig(t *testing.T) (string, string) {
	t.Helper()

	townRoot := t.TempDir()
	rigName := "testrig"

	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}

	townConfig := &config.TownConfig{
		Type:      "town",
		Version:   config.CurrentTownVersion,
		Name:      "test-town",
		CreatedAt: time.Now().Truncate(time.Second),
	}
	if err := config.SaveTownConfig(filepath.Join(mayorDir, "town.json"), townConfig); err != nil {
		t.Fatalf("save town.json: %v", err)
	}

	rigsConfig := &config.RigsConfig{
		Version: 1,
		Rigs: map[string]config.RigEntry{
			rigName: {
				GitURL:  "git@github.com:test/testrig.git",
				AddedAt: time.Now().Truncate(time.Second),
			},
		},
	}
	if err := config.SaveRigsConfig(filepath.Join(mayorDir, "rigs.json"), rigsConfig); err != nil {
		t.Fatalf("save rigs.json: %v", err)
	}

	rigPath := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}

	rigConfig := config.NewRigConfig(rigName, "git@github.com:test/testrig.git")
	if err := config.SaveRigConfig(filepath.Join(rigPath, "config.json"), rigConfig); err != nil {
		t.Fatalf("save rig config: %v", err)
	}

	oldCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	if err := os.Chdir(townRoot); err != nil {
		t.Fatalf("chdir to town root: %v", err)
	}
	t.Cleanup(func() { os.Chdir(oldCwd) })

	return townRoot, rigName
}

func TestRigConfigSet_WispLayerWarning(t *testing.T) {
	t.Run("warns about ephemeral when writing to wisp layer", func(t *testing.T) {
		townRoot, rigName := setupTestRigForConfig(t)

		rigConfigSetGlobal = false
		rigConfigSetBlock = false

		stderrOut := captureStderr(t, func() {
			err := runRigConfigSet(rigConfigSetCmd, []string{rigName, "max_polecats", "5"})
			if err != nil {
				t.Fatalf("runRigConfigSet: %v", err)
			}
		})

		if !strings.Contains(stderrOut, "ephemeral") {
			t.Errorf("expected ephemeral warning on stderr, got: %q", stderrOut)
		}
		if !strings.Contains(stderrOut, "--global") {
			t.Errorf("expected --global hint on stderr, got: %q", stderrOut)
		}

		// Verify value was actually stored in wisp layer
		wispCfg := wisp.NewConfig(townRoot, rigName)
		val := wispCfg.Get("max_polecats")
		if val == nil {
			t.Error("expected max_polecats to be set in wisp layer")
		}
	})

	t.Run("warns for string values in wisp layer", func(t *testing.T) {
		_, rigName := setupTestRigForConfig(t)

		rigConfigSetGlobal = false
		rigConfigSetBlock = false

		stderrOut := captureStderr(t, func() {
			err := runRigConfigSet(rigConfigSetCmd, []string{rigName, "default_formula", "mol-custom"})
			if err != nil {
				t.Fatalf("runRigConfigSet: %v", err)
			}
		})

		if !strings.Contains(stderrOut, "ephemeral") {
			t.Errorf("expected ephemeral warning on stderr for string value, got: %q", stderrOut)
		}
	})

	t.Run("warns for boolean values in wisp layer", func(t *testing.T) {
		_, rigName := setupTestRigForConfig(t)

		rigConfigSetGlobal = false
		rigConfigSetBlock = false

		stderrOut := captureStderr(t, func() {
			err := runRigConfigSet(rigConfigSetCmd, []string{rigName, "auto_restart", "false"})
			if err != nil {
				t.Fatalf("runRigConfigSet: %v", err)
			}
		})

		if !strings.Contains(stderrOut, "ephemeral") {
			t.Errorf("expected ephemeral warning on stderr for boolean value, got: %q", stderrOut)
		}
	})

	t.Run("no ephemeral warning when using --block flag", func(t *testing.T) {
		_, rigName := setupTestRigForConfig(t)

		rigConfigSetGlobal = false
		rigConfigSetBlock = true
		t.Cleanup(func() { rigConfigSetBlock = false })

		stderrOut := captureStderr(t, func() {
			err := runRigConfigSet(rigConfigSetCmd, []string{rigName, "auto_restart"})
			if err != nil {
				t.Fatalf("runRigConfigSet with --block: %v", err)
			}
		})

		// --block also writes to wisp but has different UX semantics; no ephemeral warning expected
		if strings.Contains(stderrOut, "ephemeral") {
			t.Errorf("unexpected ephemeral warning for --block operation, got: %q", stderrOut)
		}
	})
}

// showRow extracts the value column of the `gt rig config show` row for key.
func showRow(t *testing.T, out, key string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == key {
			return strings.Join(fields[1:], " ")
		}
	}
	t.Fatalf("no row for %q in output:\n%s", key, out)
	return ""
}

// setWispValue runs `gt rig config set <rig> <key> <value>` into the wisp layer
// and returns the value as it was stored on disk (after the JSON round-trip).
func setWispValue(t *testing.T, townRoot, rigName, key, value string) interface{} {
	t.Helper()

	rigConfigSetGlobal = false
	rigConfigSetBlock = false
	t.Cleanup(func() {
		rigConfigSetGlobal = false
		rigConfigSetBlock = false
	})

	if err := runRigConfigSet(rigConfigSetCmd, []string{rigName, key, value}); err != nil {
		t.Fatalf("runRigConfigSet %s=%s: %v", key, value, err)
	}

	return wisp.NewConfig(townRoot, rigName).Get(key)
}

// TestRigConfigSet_IntegerKeyStoresNumber is the regression from gt-8yx2:
// `gt rig config set <rig> max_polecats 1` used to store boolean true, because
// "1" parses as a bool before it parses as an int. An integer key then read that
// bool back as 0, so a cap of exactly one was impossible to express and showed up
// in `gt rig config show` as "true".
func TestRigConfigSet_IntegerKeyStoresNumber(t *testing.T) {
	tests := []struct {
		key     string
		value   string
		wantInt int
	}{
		{"max_polecats", "1", 1},
		{"max_polecats", "0", 0},
		{"max_polecats", "4", 4},
		{"priority_adjustment", "10", 10},
	}

	for _, tc := range tests {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			townRoot, rigName := setupTestRigForConfig(t)
			stored := setWispValue(t, townRoot, rigName, tc.key, tc.value)

			if _, isBool := stored.(bool); isBool {
				t.Errorf("%s=%s stored as bool %v; integer keys must never get a bool",
					tc.key, tc.value, stored)
			}
			if got := rig.CoerceInt(stored); got != tc.wantInt {
				t.Errorf("%s stored as %v (%T), reads back as %d, want %d",
					tc.key, stored, stored, got, tc.wantInt)
			}
		})
	}
}

// TestRigConfigSet_BoolKeyStoresBool covers the other half of the typing rule:
// boolean keys must keep accepting 1/0 as spellings of true/false, and must never
// be stored as numbers, which the daemon's auto_restart check would ignore.
func TestRigConfigSet_BoolKeyStoresBool(t *testing.T) {
	tests := []struct {
		key      string
		value    string
		wantBool bool
	}{
		{"auto_restart", "false", false},
		{"auto_restart", "0", false},
		{"auto_restart", "true", true},
		{"auto_restart", "1", true},
		{"auto_start_on_up", "1", true},
		{"dnd", "false", false},
	}

	for _, tc := range tests {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			townRoot, rigName := setupTestRigForConfig(t)
			stored := setWispValue(t, townRoot, rigName, tc.key, tc.value)

			if _, isNum := stored.(float64); isNum {
				t.Errorf("%s=%s stored as a number (%v); boolean keys must store bools",
					tc.key, tc.value, stored)
			}
			if got := rig.CoerceBool(stored); got != tc.wantBool {
				t.Errorf("%s stored as %v (%T), reads back as %v, want %v",
					tc.key, stored, stored, got, tc.wantBool)
			}
		})
	}
}

// TestRigConfigSet_AutoRestartZeroDisablesRestart checks auto_restart=0 the way
// its consumer does - the daemon treats an explicitly false value as "do not
// restart", so the stored value has to read false through rig.CoerceBool rather
// than being ignored by a `val.(bool)` type assertion.
func TestRigConfigSet_AutoRestartZeroDisablesRestart(t *testing.T) {
	townRoot, rigName := setupTestRigForConfig(t)
	setWispValue(t, townRoot, rigName, "auto_restart", "0")

	_, r, err := getRig(rigName)
	if err != nil {
		t.Fatalf("getRig: %v", err)
	}

	result := r.GetConfigWithSource("auto_restart")
	if result.Source != rig.SourceWisp {
		t.Errorf("auto_restart source = %s, want wisp", result.Source)
	}
	if r.GetBoolConfig("auto_restart") {
		t.Error("auto_restart=0 should read as disabled")
	}
}

// TestRigConfigSet_RejectsWrongTypeForKnownKey verifies that a value which cannot
// be interpreted as the key's type is rejected at write time instead of being
// stored in a form the key's readers silently treat as zero.
func TestRigConfigSet_RejectsWrongTypeForKnownKey(t *testing.T) {
	tests := []struct {
		key    string
		value  string
		errHas string
	}{
		{"max_polecats", "many", "whole number"},
		{"max_polecats", "", "whole number"},
		{"auto_restart", "maybe", "boolean"},
	}

	for _, tc := range tests {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			townRoot, rigName := setupTestRigForConfig(t)

			rigConfigSetGlobal = false
			rigConfigSetBlock = false

			err := runRigConfigSet(rigConfigSetCmd, []string{rigName, tc.key, tc.value})
			if err == nil {
				t.Fatalf("expected %s=%q to be rejected", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.errHas) {
				t.Errorf("error %q should mention %q", err.Error(), tc.errHas)
			}
			if stored := wisp.NewConfig(townRoot, rigName).Get(tc.key); stored != nil {
				t.Errorf("rejected value was written anyway: %s=%v", tc.key, stored)
			}
		})
	}
}

// TestRigConfigSet_UnknownKeyGuesses checks that keys with no declared type still
// accept numbers and strings; only declared keys get strict parsing.
func TestRigConfigSet_UnknownKeyGuesses(t *testing.T) {
	tests := []struct {
		value string
		want  interface{}
	}{
		{"1", float64(1)},
		{"custom", "custom"},
		{"true", true},
	}

	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			townRoot, rigName := setupTestRigForConfig(t)
			if got := setWispValue(t, townRoot, rigName, "custom_key", tc.value); got != tc.want {
				t.Errorf("custom_key=%s stored as %v (%T), want %v", tc.value, got, got, tc.want)
			}
		})
	}
}

// TestRigConfigShow_DisplaysTypedValue covers the reporting symptom: a cap of one
// must display as 1, including for a legacy wisp file that already holds the bool
// the old inference wrote.
func TestRigConfigShow_DisplaysTypedValue(t *testing.T) {
	t.Run("set through the CLI", func(t *testing.T) {
		townRoot, rigName := setupTestRigForConfig(t)
		setWispValue(t, townRoot, rigName, "max_polecats", "1")

		out := captureStdout(t, func() {
			if err := runRigConfigShow(rigConfigShowCmd, []string{rigName}); err != nil {
				t.Fatalf("runRigConfigShow: %v", err)
			}
		})

		if got := showRow(t, out, "max_polecats"); got != "1" {
			t.Errorf("show displayed max_polecats as %q, want \"1\"", got)
		}
	})

	t.Run("legacy bool value on disk", func(t *testing.T) {
		townRoot, rigName := setupTestRigForConfig(t)

		// What the old inference wrote for `max_polecats 1`.
		wispDir := filepath.Join(townRoot, wisp.WispConfigDir, wisp.ConfigSubdir)
		if err := os.MkdirAll(wispDir, 0755); err != nil {
			t.Fatal(err)
		}
		contents := `{"rig":"` + rigName + `","values":{"max_polecats":true,"auto_restart":0},"blocked":[]}`
		if err := os.WriteFile(filepath.Join(wispDir, rigName+".json"), []byte(contents), 0644); err != nil {
			t.Fatal(err)
		}

		out := captureStdout(t, func() {
			if err := runRigConfigShow(rigConfigShowCmd, []string{rigName}); err != nil {
				t.Fatalf("runRigConfigShow: %v", err)
			}
		})

		if got := showRow(t, out, "max_polecats"); got != "1" {
			t.Errorf("show displayed a legacy max_polecats=true as %q, want \"1\"", got)
		}
		if got := showRow(t, out, "auto_restart"); got != "false" {
			t.Errorf("show displayed a legacy auto_restart=0 as %q, want \"false\"", got)
		}
	})
}
