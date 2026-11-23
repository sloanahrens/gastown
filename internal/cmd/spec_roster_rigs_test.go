package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/session"
)

// TestTickEffRigsWithPolecatDirs: polecats in two of five rigs, an empty polecat
// directory in one and none in two, answer those two in the list's order (the
// list is out of name order); a directory that cannot be listed keeps its rig
// and returns its error.
func TestTickEffRigsWithPolecatDirs(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	for _, dir := range []string{
		"gastown/polecats/nux", "gastown/polecats/furiosa",
		"beaver/polecats/chip", "empty/polecats",
		"broken",
	} {
		if err := os.MkdirAll(filepath.Join(town, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A file where a polecat directory would be cannot be listed.
	if err := os.WriteFile(filepath.Join(town, "broken", "polecats"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// "quiet" and "gone" hold no polecat directory.
	got, err := rigsWithPolecatDirs(town, []string{"gone", "beaver", "quiet", "gastown", "empty", "broken"})
	if err == nil {
		t.Fatalf("rigsWithPolecatDirs error = nil, want the broken rig's listing error")
	}
	if want := []string{"beaver", "gastown", "broken"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rigsWithPolecatDirs = %v, want %v", got, want)
	}
}

// TestTickEffRosterRigsKeepsRecordsAndErrors is gt-qrm3vj: a rig with no polecat
// directory is still read when a live sandbox record names it, a rig with
// neither is dropped, and a rig whose directory cannot be listed is kept and
// its error reported.
func TestTickEffRosterRigsKeepsRecordsAndErrors(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "arig", "boxrig", "brokenrig", "quietrig")
	writeTownPolecatDir(t, town, "arig", "p1")
	// boxrig's seat lives in a sandbox: no polecat directory, only the record.
	if err := session.WriteSandboxRecord(town, session.SandboxRecord{Session: "gt-boxrig-seat", Rig: "boxrig", Polecat: "p1"}); err != nil {
		t.Fatalf("WriteSandboxRecord: %v", err)
	}
	// A file where a polecat directory would be cannot be listed.
	if err := os.WriteFile(filepath.Join(town, "brokenrig", "polecats"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := rosterRigNames(town, seatPool("deepseek-flash", 10))
	if err == nil || !strings.Contains(err.Error(), "brokenrig") {
		t.Fatalf("rosterRigNames error = %v, want the listing error for brokenrig", err)
	}
	if want := []string{"arig", "boxrig", "brokenrig"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rosterRigNames = %v, want %v", got, want)
	}
}

// TestTickEffRosterRigsRefusesUnreadableRecord is gt-qrm3vj: a record the
// roster cannot read is an error, never a town with no sandbox seat.
func TestTickEffRosterRigsRefusesUnreadableRecord(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "arig")
	writeTownPolecatDir(t, town, "arig", "p1")
	dir := session.SandboxRegistryDir(town)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gt-bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := rosterRigNames(town, seatPool("deepseek-flash", 10)); err == nil || !strings.Contains(err.Error(), "listing live sandbox records") {
		t.Fatalf("rosterRigNames error = %v, want the unreadable record's error", err)
	}
}
