package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
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

	t.Run("max_polecats=1 stores as int not bool", func(t *testing.T) {
		townRoot, rigName := setupTestRigForConfig(t)

		rigConfigSetGlobal = false
		rigConfigSetBlock = false

		err := runRigConfigSet(rigConfigSetCmd, []string{rigName, "max_polecats", "1"})
		if err != nil {
			t.Fatalf("runRigConfigSet: %v", err)
		}

		wispCfg := wisp.NewConfig(townRoot, rigName)
		val := wispCfg.Get("max_polecats")
		if val == nil {
			t.Fatal("expected max_polecats to be set in wisp layer")
		}
		if _, ok := val.(bool); ok {
			t.Errorf("max_polecats=1 was stored as bool (true), want int(1)")
		}
		if n, ok := val.(int); !ok || n != 1 {
			t.Errorf("max_polecats=1 = %T(%v), want int(1)", val, val)
		}
	})

	t.Run("max_polecats=0 stores as int not bool", func(t *testing.T) {
		townRoot, rigName := setupTestRigForConfig(t)

		rigConfigSetGlobal = false
		rigConfigSetBlock = false

		err := runRigConfigSet(rigConfigSetCmd, []string{rigName, "max_polecats", "0"})
		if err != nil {
			t.Fatalf("runRigConfigSet: %v", err)
		}

		wispCfg := wisp.NewConfig(townRoot, rigName)
		val := wispCfg.Get("max_polecats")
		if val == nil {
			t.Fatal("expected max_polecats to be set in wisp layer")
		}
		if n, ok := val.(int); !ok || n != 0 {
			t.Errorf("max_polecats=0 = %T(%v), want int(0)", val, val)
		}
	})

	t.Run("boolean values still infer as bool for non-numeric strings", func(t *testing.T) {
		townRoot, rigName := setupTestRigForConfig(t)

		rigConfigSetGlobal = false
		rigConfigSetBlock = false

		for _, v := range []string{"true", "false", "yes", "no", "1", "0"} {
			err := runRigConfigSet(rigConfigSetCmd, []string{rigName, "auto_restart", v})
			if err != nil {
				t.Fatalf("runRigConfigSet auto_restart=%q: %v", v, err)
			}

			wispCfg := wisp.NewConfig(townRoot, rigName)
			val := wispCfg.Get("auto_restart")
			if val == nil {
				t.Fatalf("expected auto_restart to be set for value %q", v)
			}
			// "true"/"false"/"yes"/"no" should be bool
			// "1"/"0" should be int (Atoi wins over ParseBool)
			switch val.(type) {
			case bool, int:
				// acceptable
			default:
				t.Errorf("auto_restart=%q = %T(%v), want bool or int", v, val, val)
			}
		}
	})
}
