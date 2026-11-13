package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/rig"
)

// retireMayorTown builds a town under t.TempDir() with one rig, gastown, whose
// mayor/rig is set up to retire: the beads have moved to <rig>/.beads.
func retireMayorTown(t *testing.T) (townRoot, from, to string) {
	t.Helper()
	townRoot = t.TempDir()
	rigPath := filepath.Join(townRoot, "gastown")
	from = constants.RigMayorPath(rigPath)
	to = from + ".retired-20261113"
	for _, dir := range []string{from, filepath.Join(rigPath, constants.DirBeads)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(rigPath, constants.DirBeads, "metadata.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return townRoot, from, to
}

// retireMayorClock is a fixed clock at noon local time, so the destination date
// is the same in every time zone.
func retireMayorClock() time.Time {
	return time.Date(2026, time.November, 13, 12, 0, 0, 0, time.Local)
}

func retireMayorExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatal(err)
	return false
}

func TestRigRetireMayorDryRunChangesNothing(t *testing.T) {
	t.Parallel()
	townRoot, from, to := retireMayorTown(t)
	var out bytes.Buffer

	if err := retireMayorRig(&out, rig.OSRetireFS{}, inTown(townRoot), "gastown", false, retireMayorClock); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !retireMayorExists(t, from) || retireMayorExists(t, to) {
		t.Errorf("dry run moved the directory: %s exists=%v, %s exists=%v", from, retireMayorExists(t, from), to, retireMayorExists(t, to))
	}
	if want := "mv " + to + " " + from; !strings.Contains(out.String(), want) {
		t.Errorf("output %q does not print the undoing %q", out.String(), want)
	}
	if !strings.Contains(out.String(), "Dry run") {
		t.Errorf("output %q does not say it is a dry run", out.String())
	}
}

func TestRigRetireMayorApplyRenamesOnce(t *testing.T) {
	t.Parallel()
	townRoot, from, to := retireMayorTown(t)
	var out bytes.Buffer

	if err := retireMayorRig(&out, rig.OSRetireFS{}, inTown(townRoot), "gastown", true, retireMayorClock); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if retireMayorExists(t, from) || !retireMayorExists(t, to) {
		t.Errorf("apply did not rename: %s exists=%v, %s exists=%v", from, retireMayorExists(t, from), to, retireMayorExists(t, to))
	}
	if want := "mv " + to + " " + from; !strings.Contains(out.String(), want) {
		t.Errorf("output %q does not print the undoing %q", out.String(), want)
	}
}

func TestRigRetireMayorRefusalNamesPathAndRenamesNothing(t *testing.T) {
	t.Parallel()
	townRoot, from, to := retireMayorTown(t)
	routes := filepath.Join(townRoot, constants.DirBeads, "routes.jsonl")
	if err := os.MkdirAll(filepath.Dir(routes), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(routes, []byte("{\"path\":\"gastown/mayor/rig\",\"prefix\":\"gt-\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	err := retireMayorRig(&out, rig.OSRetireFS{}, inTown(townRoot), "gastown", true, retireMayorClock)
	if err == nil {
		t.Fatal("apply with a routes refusal = nil, want an error")
	}
	if !strings.Contains(out.String(), routes+":1") {
		t.Errorf("output %q does not name the routes line", out.String())
	}
	if !retireMayorExists(t, from) || retireMayorExists(t, to) {
		t.Errorf("refused apply renamed the directory")
	}
}

func TestRigRetireMayorAlreadyRetiredIsNotAnError(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	var out bytes.Buffer

	if err := retireMayorRig(&out, rig.OSRetireFS{}, inTown(townRoot), "gastown", true, retireMayorClock); err != nil {
		t.Fatalf("already retired: %v", err)
	}
	if !strings.Contains(out.String(), "already retired") {
		t.Errorf("output %q does not say the rig is already retired", out.String())
	}
}

// inTown is the town finder for a town at townRoot.
func inTown(townRoot string) func() (string, error) {
	return func() (string, error) { return townRoot, nil }
}

// renameRefusingFS is a RetireFS whose rename always fails.
type renameRefusingFS struct{ rig.OSRetireFS }

func (renameRefusingFS) Rename(string, string) error {
	return errors.New("rename refused")
}

func TestRigRetireMayorOutsideATownIsAnError(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	outside := func() (string, error) { return "", errors.New("no town here") }

	err := retireMayorRig(&out, rig.OSRetireFS{}, outside, "gastown", false, retireMayorClock)
	if err == nil || !strings.Contains(err.Error(), "not in a Gas Town workspace") {
		t.Fatalf("error = %v, want the not-in-a-workspace refusal", err)
	}
}

func TestRigRetireMayorInvalidRigNameIsAnError(t *testing.T) {
	t.Parallel()
	townRoot, from, _ := retireMayorTown(t)
	var out bytes.Buffer

	err := retireMayorRig(&out, rig.OSRetireFS{}, inTown(townRoot), "../gastown", true, retireMayorClock)
	if err == nil || !strings.Contains(err.Error(), "planning retire-mayor") {
		t.Fatalf("error = %v, want the planning refusal", err)
	}
	if !retireMayorExists(t, from) {
		t.Error("an invalid rig name renamed a directory")
	}
}

func TestRigRetireMayorRenameFailureIsAnError(t *testing.T) {
	t.Parallel()
	townRoot, from, to := retireMayorTown(t)
	var out bytes.Buffer

	err := retireMayorRig(&out, renameRefusingFS{}, inTown(townRoot), "gastown", true, retireMayorClock)
	if err == nil || !strings.Contains(err.Error(), "rename refused") {
		t.Fatalf("error = %v, want the rename failure", err)
	}
	if !retireMayorExists(t, from) || retireMayorExists(t, to) {
		t.Error("a failed rename moved the directory")
	}
}

func TestRigRetireMayorIsRegisteredUnderRig(t *testing.T) {
	t.Parallel()
	var found bool
	for _, sub := range rigCmd.Commands() {
		if sub.Name() != "retire-mayor" {
			continue
		}
		found = true
		if !strings.Contains(sub.Short, "dry run") {
			t.Errorf("retire-mayor help %q does not say it is a dry run", sub.Short)
		}
		if sub.Flags().Lookup("apply") == nil {
			t.Error("retire-mayor has no --apply flag")
		}
	}
	if !found {
		t.Error("gt rig has no retire-mayor subcommand")
	}
}
