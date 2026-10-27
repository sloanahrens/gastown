package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// editInTown runs the edit command's body in a temp town whose only
// registered rig is "myrig", with the editor stubbed to a no-op.
func editInTown(t *testing.T, rig string) (townRoot string, opened []string, err error) {
	t.Helper()
	townRoot = t.TempDir()
	writeTestRigsConfig(t, townRoot, "myrig")
	err = runDirectiveEditIn(&bytes.Buffer{}, townRoot, townRoot, rig, "polecat", false, func(path string) error {
		opened = append(opened, path)
		return nil
	})
	return townRoot, opened, err
}

func TestDirectiveEdit_UnknownRigCreatesNothing(t *testing.T) {
	t.Parallel()

	townRoot, opened, err := editInTown(t, "gastwon")
	if err == nil {
		t.Fatal("expected an error for an unregistered rig")
	}
	if !strings.Contains(err.Error(), "gastwon") {
		t.Errorf("error should name the rig, got: %v", err)
	}
	if len(opened) != 0 {
		t.Errorf("editor opened for a rejected rig: %v", opened)
	}
	if _, statErr := os.Stat(filepath.Join(townRoot, "gastwon")); !os.IsNotExist(statErr) {
		t.Errorf("a directory was created for the unknown rig (stat err: %v)", statErr)
	}
}

func TestDirectiveEdit_RigPathIsRejected(t *testing.T) {
	t.Parallel()

	for _, rig := range []string{"../x", "..", "a/b", `a\b`, "my..rig"} {
		townRoot, opened, err := editInTown(t, rig)
		if err == nil {
			t.Errorf("--rig %q: expected an error", rig)
			continue
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%q", rig)) {
			t.Errorf("--rig %q: error should name the rig, got: %v", rig, err)
		}
		if len(opened) != 0 {
			t.Errorf("--rig %q: editor opened for a rejected rig: %v", rig, opened)
		}
		if _, statErr := os.Stat(filepath.Join(filepath.Dir(townRoot), "x")); !os.IsNotExist(statErr) {
			t.Errorf("--rig %q: a path outside the town was written (stat err: %v)", rig, statErr)
		}
	}
}

func TestDirectiveEdit_RegisteredRigStillWorks(t *testing.T) {
	t.Parallel()

	townRoot, opened, err := editInTown(t, "myrig")
	if err != nil {
		t.Fatalf("registered rig: %v", err)
	}
	// An edit writes the new path of the rig's layer: the rig's canonical checkout
	// (gt-80rnu).
	want := filepath.Join(townRoot, "myrig", "mayor", "rig", ".gastown", "directives", "polecat.md")
	if len(opened) != 1 || opened[0] != want {
		t.Errorf("editor opened %v, want [%s]", opened, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("directive file not created: %v", err)
	}
}

func TestDirectiveShow_RigValidation(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	writeTestRigsConfig(t, townRoot, "myrig")

	for _, rig := range []string{"gastwon", "../x", "a/b"} {
		if _, err := resolveDirectiveRigIn(townRoot, rig, townRoot); err == nil {
			t.Errorf("--rig %q: expected an error", rig)
		}
	}

	if rigName, err := resolveDirectiveRigIn(townRoot, "myrig", townRoot); err != nil || rigName != "myrig" {
		t.Errorf("registered rig: got %q, %v", rigName, err)
	}
	// No explicit rig: cwd detection is unchanged (no rig under the town root).
	if rigName, err := resolveDirectiveRigIn(townRoot, "", townRoot); err != nil || rigName != "" {
		t.Errorf("no explicit rig at town root: got %q, %v", rigName, err)
	}
}
