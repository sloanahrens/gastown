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

// The unexported-seam tests above hand findCurrentRigWith its cwd; this one
// runs the production wrapper, which reads the process cwd and GT_RIG itself.
// It changes the process cwd, so it is not parallel.
//
//testpolicy:allow parallel — findCurrentRig reads the process working directory and GT_RIG, both process-wide
func TestFindCurrentRigReadsCwdAndEnvironment(t *testing.T) {
	townRoot := townWithRigs(t, "gastown", "beads")
	// t.TempDir may sit behind a symlink (/var -> /private/var on macOS) that
	// the process cwd resolves, which would put cwd outside townRoot.
	townRoot, err := filepath.EvalSymlinks(townRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "beads", "crew"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("registered cwd component beats GT_RIG", func(t *testing.T) {
		//testpolicy:allow no-chdir — the cwd is the input under test: findCurrentRig reads the working directory
		t.Chdir(filepath.Join(townRoot, "beads", "crew"))
		//testpolicy:allow no-env — GT_RIG is the input under test: findCurrentRig reads it from the process environment
		t.Setenv("GT_RIG", "gastown")
		name, r, err := findCurrentRig(townRoot)
		if err != nil || name != "beads" || r == nil || r.Name != "beads" {
			t.Errorf("findCurrentRig = %q, %+v, %v, want beads", name, r, err)
		}
	})
	t.Run("town-level directory falls back to GT_RIG", func(t *testing.T) {
		//testpolicy:allow no-chdir — the cwd is the input under test: findCurrentRig reads the working directory
		t.Chdir(filepath.Join(townRoot, "mayor"))
		//testpolicy:allow no-env — GT_RIG is the input under test: findCurrentRig reads it from the process environment
		t.Setenv("GT_RIG", "gastown")
		name, r, err := findCurrentRig(townRoot)
		if err != nil || name != "gastown" || r == nil || r.Name != "gastown" {
			t.Errorf("findCurrentRig = %q, %+v, %v, want gastown", name, r, err)
		}
	})
}

// A registry that cannot be read says nothing about whether the cwd rig is
// registered, so GT_RIG must not take its place: the caller gets the read
// failure for the rig the cwd names.
func TestFindCurrentRigRegistryUnreadableKeepsCwdRig(t *testing.T) {
	t.Parallel()
	townRoot := townWithRigs(t, "gastown", "beads")
	if err := os.WriteFile(constants.MayorRigsPath(townRoot), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		if k == "GT_RIG" {
			return "gastown"
		}
		return ""
	}

	name, _, err := findCurrentRigWith(getenv, filepath.Join(townRoot, "beads", "crew"), townRoot)
	if err == nil {
		t.Fatalf("findCurrentRigWith = %q, want an error from the unreadable registry", name)
	}
	if name == "gastown" {
		t.Errorf("GT_RIG replaced the cwd rig %q over an unreadable registry", "beads")
	}
}
