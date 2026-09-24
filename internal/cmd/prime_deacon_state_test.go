package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/deacon"
)

// writePrimeDeaconState writes raw JSON as the deacon's state.json under
// townRoot and returns its path.
func writePrimeDeaconState(t *testing.T, townRoot, contents string) string {
	t.Helper()
	dir := deacon.DeaconStateDir(townRoot)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestPrimeResetsDeaconPatrolState_FreshSessionOnly mirrors the witness's
// former fresh-session predicate (primeResetsWitnessPatrolState, superseded
// by gt-oabl's patrol-report-time bounding once the witness's counter became
// purely informational): only a hook-mode SessionStart with source "startup"
// or "clear", for RoleDeacon, counts as a fresh session. Resume and
// compaction continue the session that owns the counter, and a bare `gt
// prime` (no --hook) is a context read, not a session start.
func TestPrimeResetsDeaconPatrolState_FreshSessionOnly(t *testing.T) {
	cases := []struct {
		name     string
		role     Role
		hookMode bool
		source   string
		want     bool
	}{
		{"fresh startup", RoleDeacon, true, "startup", true},
		{"fresh clear", RoleDeacon, true, "clear", true},
		{"resume does not reset", RoleDeacon, true, "resume", false},
		{"compact does not reset", RoleDeacon, true, "compact", false},
		{"no hook mode does not reset", RoleDeacon, false, "startup", false},
		{"witness role does not reset via deacon path", RoleWitness, true, "startup", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := primeResetsDeaconPatrolState(tc.role, tc.hookMode, tc.source); got != tc.want {
				t.Errorf("primeResetsDeaconPatrolState(%v, %v, %q) = %v, want %v",
					tc.role, tc.hookMode, tc.source, got, tc.want)
			}
		})
	}
}

func TestResetDeaconPatrolState_FreshSessionResetsCounterAndPreservesOtherFields(t *testing.T) {
	townRoot := t.TempDir()
	path := writePrimeDeaconState(t, townRoot, `{
  "patrol_count": 29,
  "extraordinary_action": false,
  "last_patrol": "2026-09-24T20:20:00Z"
}`)

	origHookMode, origSource := primeHookMode, primeHookSource
	primeHookMode, primeHookSource = true, "startup"
	t.Cleanup(func() { primeHookMode, primeHookSource = origHookMode, origSource })

	ctx := RoleContext{Role: RoleDeacon, TownRoot: townRoot}
	msg := resetDeaconPatrolState(ctx)
	if msg == "" {
		t.Fatal("resetDeaconPatrolState returned no status line for a changed counter")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("parse back: %v", err)
	}
	var count int
	if err := json.Unmarshal(fields["patrol_count"], &count); err != nil {
		t.Fatalf("parse patrol_count: %v", err)
	}
	if count != 0 {
		t.Errorf("patrol_count = %d, want 0", count)
	}
	for _, key := range []string{"extraordinary_action", "last_patrol"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("field %q was dropped by the reset", key)
		}
	}
}

func TestResetDeaconPatrolState_ResumeAndCompactionDoNotReset(t *testing.T) {
	for _, source := range []string{"resume", "compact"} {
		t.Run(source, func(t *testing.T) {
			townRoot := t.TempDir()
			path := writePrimeDeaconState(t, townRoot, `{"patrol_count": 29}`)

			origHookMode, origSource := primeHookMode, primeHookSource
			primeHookMode, primeHookSource = true, source
			t.Cleanup(func() { primeHookMode, primeHookSource = origHookMode, origSource })

			ctx := RoleContext{Role: RoleDeacon, TownRoot: townRoot}
			if msg := resetDeaconPatrolState(ctx); msg != "" {
				t.Errorf("resetDeaconPatrolState on source %q returned %q, want no-op", source, msg)
			}

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatalf("parse back: %v", err)
			}
			var count int
			if err := json.Unmarshal(fields["patrol_count"], &count); err != nil {
				t.Fatalf("parse patrol_count: %v", err)
			}
			if count != 29 {
				t.Errorf("patrol_count = %d, want unchanged 29 on source %q", count, source)
			}
		})
	}
}

func TestResetDeaconPatrolState_DryRunDoesNotWrite(t *testing.T) {
	townRoot := t.TempDir()
	path := writePrimeDeaconState(t, townRoot, `{"patrol_count": 29}`)

	origHookMode, origSource, origDryRun := primeHookMode, primeHookSource, primeDryRun
	primeHookMode, primeHookSource, primeDryRun = true, "startup", true
	t.Cleanup(func() { primeHookMode, primeHookSource, primeDryRun = origHookMode, origSource, origDryRun })

	ctx := RoleContext{Role: RoleDeacon, TownRoot: townRoot}
	if msg := resetDeaconPatrolState(ctx); msg != "" {
		t.Errorf("resetDeaconPatrolState under --dry-run returned %q, want no-op", msg)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("parse back: %v", err)
	}
	var count int
	if err := json.Unmarshal(fields["patrol_count"], &count); err != nil {
		t.Fatalf("parse patrol_count: %v", err)
	}
	if count != 29 {
		t.Errorf("patrol_count = %d, want unchanged 29 under --dry-run", count)
	}
}
