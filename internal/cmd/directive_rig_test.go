package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// directiveTown builds a temp town with one registered rig, "myrig", and
// makes it the working directory. The editor is stubbed with `true`, which
// exits 0 and leaves the file as the command created it.
func directiveTown(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	writeTestRigsConfig(t, townRoot, "myrig")
	t.Chdir(townRoot)
	t.Setenv("EDITOR", "true")
	return townRoot
}

func runDirectiveEditCmd(t *testing.T, role, rig string) error {
	t.Helper()
	cmd := directiveEditCmd
	directiveEditRig, directiveEditTown = rig, false
	t.Cleanup(func() { directiveEditRig, directiveEditTown = "", false })
	cmd.SetOut(&bytes.Buffer{})
	return runDirectiveEdit(cmd, []string{role})
}

func TestDirectiveEdit_UnknownRigCreatesNothing(t *testing.T) {
	townRoot := directiveTown(t)

	err := runDirectiveEditCmd(t, "polecat", "gastwon")
	if err == nil {
		t.Fatal("expected an error for an unregistered rig")
	}
	if !strings.Contains(err.Error(), "gastwon") {
		t.Errorf("error should name the rig, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(townRoot, "gastwon")); !os.IsNotExist(statErr) {
		t.Errorf("a directory was created for the unknown rig (stat err: %v)", statErr)
	}
}

func TestDirectiveEdit_RigPathIsRejected(t *testing.T) {
	townRoot := directiveTown(t)
	outside := filepath.Join(filepath.Dir(townRoot), "x")

	for _, rig := range []string{"../x", "..", "a/b", `a\b`, "my..rig"} {
		err := runDirectiveEditCmd(t, "polecat", rig)
		if err == nil {
			t.Errorf("--rig %q: expected an error", rig)
			continue
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%q", rig)) {
			t.Errorf("--rig %q: error should name the rig, got: %v", rig, err)
		}
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Errorf("a path outside the town was written (stat err: %v)", err)
	}
}

func TestDirectiveEdit_RegisteredRigStillWorks(t *testing.T) {
	townRoot := directiveTown(t)

	if err := runDirectiveEditCmd(t, "polecat", "myrig"); err != nil {
		t.Fatalf("registered rig: %v", err)
	}
	if _, err := os.Stat(filepath.Join(townRoot, "myrig", "directives", "polecat.md")); err != nil {
		t.Errorf("directive file not created: %v", err)
	}
}

func TestDirectiveShow_RigValidation(t *testing.T) {
	directiveTown(t)

	for _, rig := range []string{"gastwon", "../x", "a/b"} {
		if _, _, err := resolveDirectiveContext(rig); err == nil {
			t.Errorf("--rig %q: expected an error", rig)
		}
	}

	_, rigName, err := resolveDirectiveContext("myrig")
	if err != nil || rigName != "myrig" {
		t.Errorf("registered rig: got %q, %v", rigName, err)
	}
}
