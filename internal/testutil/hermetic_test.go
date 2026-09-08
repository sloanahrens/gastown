package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/workspace"
)

// makeFakeTown builds a minimal "live town" fixture: marker file, rigs.json
// with one known rig, watched subdirectories, and an events log.
func makeFakeTown(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "mayor", "town.json"), `{"type":"town","version":2,"name":"fake"}`)
	writeFile(t, filepath.Join(root, "mayor", "rigs.json"), `{"version":1,"rigs":{"gastown":{}}}`)
	for _, sub := range watchedSubdirs {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, ".events.jsonl"),
		`{"ts":"2026-09-08T00:00:00Z","source":"gt","type":"boot","actor":"mayor","visibility":"feed"}`+"\n")
	return root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTripwire_CleanTownReportsNoLeaks(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)
	if leaks := snap.diff(); len(leaks) != 0 {
		t.Errorf("clean town reported leaks: %v", leaks)
	}
}

func TestTripwire_DetectsNewFilesAndDatabases(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)

	// Orphan test database (the hq-det/hq-4zrq incident shape).
	if err := os.MkdirAll(filepath.Join(town, ".dolt-data", "testdb_abc123"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Stray file dropped at the town root.
	writeFile(t, filepath.Join(town, "scratch.txt"), "oops")
	// Lock churn must be ignored — legitimate concurrent agents create these.
	writeFile(t, filepath.Join(town, ".events.jsonl.lock"), "")

	leaks := snap.diff()
	if len(leaks) != 2 {
		t.Fatalf("expected 2 leaks, got %d: %v", len(leaks), leaks)
	}
	joined := strings.Join(leaks, "\n")
	if !strings.Contains(joined, ".dolt-data/testdb_abc123") {
		t.Errorf("orphan database not detected: %v", leaks)
	}
	if !strings.Contains(joined, "scratch.txt") {
		t.Errorf("stray root file not detected: %v", leaks)
	}
}

func TestTripwire_FlagsFixtureActorEvents(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)

	f, err := os.OpenFile(filepath.Join(town, ".events.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// One event from a known rig (concurrent legitimate traffic) and one from
	// a fixture rig that does not exist in rigs.json (the gt-x9o incident).
	_, _ = f.WriteString(`{"ts":"2026-09-08T00:01:00Z","source":"gt","type":"done","actor":"gastown/witness","visibility":"feed"}` + "\n")
	_, _ = f.WriteString(`{"ts":"2026-09-08T00:01:01Z","source":"gt","type":"spawn","actor":"myr/mycat","visibility":"feed"}` + "\n")
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	leaks := snap.diff()
	if len(leaks) != 1 {
		t.Fatalf("expected exactly 1 leak (fixture actor), got %d: %v", len(leaks), leaks)
	}
	if !strings.Contains(leaks[0], "myr/mycat") {
		t.Errorf("fixture actor not flagged: %v", leaks)
	}
}

func TestTripwire_ToleratesUnknownActor(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)

	f, err := os.OpenFile(filepath.Join(town, ".events.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// detectActor() (internal/cmd/sling_helpers.go) legitimately logs actor
	// "unknown" when GetRole() can't resolve an identity (e.g. a sling run
	// outside an agent session). That must not trip the leak detector (gt-ro0).
	_, _ = f.WriteString(`{"ts":"2026-09-08T00:01:00Z","source":"gt","type":"sling","actor":"unknown","visibility":"feed"}` + "\n")
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if leaks := snap.diff(); len(leaks) != 0 {
		t.Errorf("legitimate unknown-actor sling event flagged as leak: %v", leaks)
	}
}

func TestHermeticTest_ScrubsAndRedirects(t *testing.T) {
	t.Setenv("GT_ROLE", "gastown/polecats/flint")
	t.Setenv("BD_ACTOR", "someone")
	t.Setenv("BEADS_DB", "gt")
	origHome := os.Getenv("HOME")

	town := HermeticTest(t)

	for _, v := range []string{"GT_ROLE", "BD_ACTOR", "BEADS_DB"} {
		if got := os.Getenv(v); got != "" {
			t.Errorf("%s survived the scrub: %q", v, got)
		}
	}
	if home := os.Getenv("HOME"); home == origHome || home == "" {
		t.Errorf("HOME not redirected: %q", home)
	}
	if got := os.Getenv("GT_DOLT_PORT"); got != poisonDoltPort {
		t.Errorf("GT_DOLT_PORT = %q, want poisoned %q", got, poisonDoltPort)
	}
	if got := os.Getenv(HermeticEnvVar); got != "1" {
		t.Errorf("%s = %q, want 1", HermeticEnvVar, got)
	}
	if got := os.Getenv("GT_TOWN_ROOT"); got != town {
		t.Errorf("GT_TOWN_ROOT = %q, want sandbox town %q", got, town)
	}
	if ok, _ := workspace.IsWorkspace(town); !ok {
		t.Errorf("sandbox town %q is not a valid workspace", town)
	}
}

func TestScratchTown_CwdResolvesToScratch(t *testing.T) {
	town := ScratchTown(t)

	root, err := workspace.FindFromCwd()
	if err != nil {
		t.Fatalf("FindFromCwd: %v", err)
	}
	// Resolve symlinks on both sides (macOS /var -> /private/var).
	wantReal, _ := filepath.EvalSymlinks(town)
	gotReal, _ := filepath.EvalSymlinks(root)
	if gotReal != wantReal {
		t.Errorf("FindFromCwd = %q, want scratch town %q", root, town)
	}
}
