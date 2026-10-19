package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
)

// townWithRigs is a town root with the named rigs registered and their
// directories on disk, plus a mayor/ directory that is no rig.
func townWithRigs(t *testing.T, rigs ...string) string {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	for _, name := range rigs {
		if err := os.MkdirAll(filepath.Join(townRoot, name), 0o755); err != nil {
			t.Fatal(err)
		}
		cfg.Rigs[name] = config.RigEntry{}
	}
	if err := config.SaveRigsConfig(constants.MayorRigsPath(townRoot), cfg); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

// A cwd under a town-level directory (mayor/) names no rig, so GT_RIG decides;
// the first path component is no rig just because it is the first component.
// findCurrentRig reads cwd and the environment, findCurrentRigWith takes them.
func TestFindCurrentRigGTRigUnderTownDir(t *testing.T) {
	t.Parallel()
	townRoot := townWithRigs(t, "gastown", "beads")
	getenv := func(k string) string {
		if k == "GT_RIG" {
			return "gastown"
		}
		return ""
	}

	name, r, err := findCurrentRigWith(getenv, filepath.Join(townRoot, "mayor"), townRoot)
	if err != nil {
		t.Fatalf("findCurrentRigWith: %v", err)
	}
	if name != "gastown" || r == nil || r.Name != "gastown" {
		t.Errorf("findCurrentRigWith = %q, %+v, want gastown", name, r)
	}
}

// A cwd inside a registered rig wins over GT_RIG, and an unregistered
// component with no GT_RIG still reports the component it looked up.
func TestFindCurrentRigCwdAndErrors(t *testing.T) {
	t.Parallel()
	townRoot := townWithRigs(t, "gastown", "beads")
	env := func(rig string) func(string) string {
		return func(k string) string {
			if k == "GT_RIG" {
				return rig
			}
			return ""
		}
	}

	t.Run("registered cwd component beats GT_RIG", func(t *testing.T) {
		t.Parallel()
		name, _, err := findCurrentRigWith(env("gastown"), filepath.Join(townRoot, "beads", "crew"), townRoot)
		if err != nil || name != "beads" {
			t.Errorf("findCurrentRigWith = %q, %v, want beads", name, err)
		}
	})
	t.Run("unregistered component without GT_RIG names it", func(t *testing.T) {
		t.Parallel()
		_, _, err := findCurrentRigWith(env(""), filepath.Join(townRoot, "mayor"), townRoot)
		if err == nil || err.Error() != "rig 'mayor' not found" {
			t.Errorf("err = %v, want rig 'mayor' not found", err)
		}
	})
	t.Run("town root with no GT_RIG is not in a rig", func(t *testing.T) {
		t.Parallel()
		_, _, err := findCurrentRigWith(env(""), townRoot, townRoot)
		if err == nil {
			t.Error("want an error at the town root with no GT_RIG")
		}
	})
}
