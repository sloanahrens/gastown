package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// parkTown is a town whose registry holds testrig and otherrig.
func parkTown(t *testing.T) string {
	t.Helper()
	entry := `{"git_url":"x","added_at":"2026-09-07T00:00:00Z","beads":{"repo":"","prefix":"%s"}}`
	return writeTownFiles(t, map[string]string{
		"mayor/town.json": `{"type":"town","version":2,"name":"t"}`,
		"mayor/rigs.json": `{"version":1,"rigs":{"testrig":` + strings.Replace(entry, "%s", "tr", 1) +
			`,"otherrig":` + strings.Replace(entry, "%s", "or", 1) + `}}`,
	})
}

func TestParkAndUnparkRigs(t *testing.T) {
	t.Parallel()
	town := parkTown(t)
	var out bytes.Buffer
	rec := config.RigParked{Since: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), By: "sloan", Reason: "rework"}
	if err := parkRigs(&out, town, []string{"testrig"}, rec); err != nil {
		t.Fatalf("parkRigs = %v\n%s", err, out.String())
	}
	if !IsRigParked(town, "testrig") || IsRigParked(town, "otherrig") {
		t.Fatal("park did not land on exactly testrig")
	}
	if blocked, reason := IsRigParkedOrDocked(town, "testrig"); !blocked || reason != "parked" {
		t.Errorf("IsRigParkedOrDocked = %v, %q", blocked, reason)
	}
	if !strings.Contains(out.String(), "rework") {
		t.Errorf("park output = %q, want the reason", out.String())
	}

	out.Reset()
	if err := unparkRigs(&out, town, []string{"testrig"}); err != nil {
		t.Fatalf("unparkRigs = %v", err)
	}
	if IsRigParked(town, "testrig") {
		t.Error("testrig still parked after unpark")
	}

	out.Reset()
	if err := parkRigs(&out, town, []string{"testrig", "nope"}, rec); err == nil {
		t.Error("parking an unregistered rig succeeded")
	}
	if !IsRigParked(town, "testrig") {
		t.Error("the registered rig in a partly failing park was not parked")
	}
}

// TestIsRigParked_FailsClosed: a town whose registry does not load reads
// every rig as parked, so dispatch refuses rather than guesses.
func TestIsRigParked_FailsClosed(t *testing.T) {
	t.Parallel()
	town := parkTown(t)
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !IsRigParked(town, "testrig") {
		t.Error("an unreadable registry read as unparked")
	}
	if blocked, reason := IsRigParkedOrDocked(town, "testrig"); !blocked || reason != "parked" {
		t.Errorf("IsRigParkedOrDocked = %v, %q; want parked", blocked, reason)
	}
}

// TestMigrateParkedRigs: a rig parked by an older gt reads parked before the
// migration and after it, and the migration names it.
func TestMigrateParkedRigs(t *testing.T) {
	t.Parallel()
	town := parkTown(t)
	legacy := filepath.Join(town, ".beads-wisp", "config", "testrig.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte(`{"rig":"testrig","values":{"status":"parked"},"blocked":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !IsRigParked(town, "testrig") {
		t.Fatal("legacy-parked rig read as unparked before migration")
	}
	var out bytes.Buffer
	if err := migrateParkedRigs(&out, town, "test"); err != nil {
		t.Fatalf("migrateParkedRigs = %v", err)
	}
	if !strings.Contains(out.String(), "testrig") {
		t.Errorf("migration output = %q", out.String())
	}
	if !IsRigParked(town, "testrig") || IsRigParked(town, "otherrig") {
		t.Error("park state changed across migration")
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy wisp file still present: %v", err)
	}
}

func TestRigConfigRefusesStatusKey(t *testing.T) {
	t.Parallel()
	if err := refuseStatusKey("status"); err == nil || !strings.Contains(err.Error(), "gt rig park") {
		t.Errorf("refuseStatusKey(status) = %v", err)
	}
	if err := refuseStatusKey("max_polecats"); err != nil {
		t.Errorf("refuseStatusKey(max_polecats) = %v", err)
	}
}

func TestRigDockedLabel(t *testing.T) {
	t.Parallel()
	if RigDockedLabel != "status:docked" {
		t.Errorf("expected RigDockedLabel to be 'status:docked', got %q", RigDockedLabel)
	}
}
