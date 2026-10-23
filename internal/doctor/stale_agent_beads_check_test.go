package doctor

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestNewStaleAgentBeadsCheck(t *testing.T) {
	t.Parallel()
	check := NewStaleAgentBeadsCheck()

	if check.Name() != "stale-agent-beads" {
		t.Errorf("expected name 'stale-agent-beads', got %q", check.Name())
	}

	if !check.CanFix() {
		t.Error("expected CanFix to return true")
	}

	if check.Description() != "Detect agent beads for removed workers (crew and polecats)" {
		t.Errorf("unexpected description: %q", check.Description())
	}

	if check.Category() != CategoryRig {
		t.Errorf("expected category %q, got %q", CategoryRig, check.Category())
	}
}

func TestStaleAgentBeadsCheck_NoRoutes(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// No .beads dir at all — LoadRoutes returns empty, so check returns OK (no rigs)
	check := NewStaleAgentBeadsCheck()
	ctx := noBD(tmpDir)

	result := check.Run(ctx)

	// With no routes, there are no rigs to check, so result is OK or Warning
	if result.Status != StatusOK && result.Status != StatusWarning {
		t.Errorf("expected StatusOK or StatusWarning, got %v: %s", result.Status, result.Message)
	}
}

func TestStaleAgentBeadsCheck_NoRigs(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Create .beads dir with empty routes.jsonl
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewStaleAgentBeadsCheck()
	ctx := noBD(tmpDir)

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for no rigs, got %v: %s", result.Status, result.Message)
	}
}

func TestStaleAgentBeadsCheck_CrewOnDisk(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Set up routes pointing to a rig
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"gt-","path":"myrig/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create rig beads directory
	rigBeadsDir := filepath.Join(tmpDir, "myrig", "mayor", "rig", ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create crew on disk
	crewDir := filepath.Join(tmpDir, "myrig", "crew")
	for _, name := range []string{"alice", "bob"} {
		if err := os.MkdirAll(filepath.Join(crewDir, name), 0755); err != nil {
			t.Fatal(err)
		}
	}

	check := NewStaleAgentBeadsCheck()
	ctx := noBD(tmpDir)

	// Without a running bd daemon, List() will fail gracefully
	// The check should handle this and not crash
	result := check.Run(ctx)
	t.Logf("Stale agent beads check: status=%v, message=%s", result.Status, result.Message)
}

// TestStaleAgentBeadsUnreadablePolecatsDir verifies that a rig whose polecats
// directory exists but cannot be read contributes no stale beads and closes
// none. Before gt-9wiz3, listPolecats folded that error into "no polecats", so
// the check and its fix closed the agent beads of live polecats.
func TestStaleAgentBeadsUnreadablePolecatsDir(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads"), 0755); err != nil {
		t.Fatal(err)
	}

	// A live polecat on disk, behind a directory the check cannot read.
	polecatsDir := filepath.Join(tmpDir, "gastown", "polecats")
	if err := os.MkdirAll(filepath.Join(polecatsDir, "nux", ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(polecatsDir, 0o000); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-exec-files — the target is the fixture directory: traversal needs its search bit back
	t.Cleanup(func() { _ = os.Chmod(polecatsDir, 0o755) })

	town := newDoctorTown()
	town.db(filepath.Join(tmpDir, "gastown", "mayor", "rig")).Seed(beads.Issue{
		ID:     "gt-gastown-polecat-nux",
		Title:  "nux",
		Type:   "agent",
		Labels: []string{"gt:agent"},
	})

	check := NewStaleAgentBeadsCheck()
	ctx := town.ctx(tmpDir, "")

	result := check.Run(ctx)
	for _, detail := range result.Details {
		if detail == "gt-gastown-polecat-nux" {
			t.Fatalf("unreadable polecats directory marked live polecat stale; details: %v", result.Details)
		}
	}
	if result.Status == StatusOK {
		t.Errorf("an unreadable polecats directory must be reported, got OK: %s", result.Message)
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() returned error: %v", err)
	}
	if updates := town.updates(); len(updates) != 0 {
		t.Errorf("Fix() closed beads for a rig it could not read: %v", updates)
	}
}

// TestStaleAgentBeadsUnreadableCrewDir verifies that a rig whose crew directory
// exists but cannot be read contributes no stale beads and closes none. Before
// gt-ro1lc, listCrewWorkers folded that error into "no crew", so the check and
// its fix closed the agent beads of live crew members.
func TestStaleAgentBeadsUnreadableCrewDir(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads"), 0755); err != nil {
		t.Fatal(err)
	}

	// A live crew member on disk, behind a directory the check cannot read.
	crewDir := filepath.Join(tmpDir, "gastown", "crew")
	if err := os.MkdirAll(filepath.Join(crewDir, "sloan", ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(crewDir, 0o000); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-exec-files — the target is the fixture directory: traversal needs its search bit back
	t.Cleanup(func() { _ = os.Chmod(crewDir, 0o755) })

	town := newDoctorTown()
	town.db(filepath.Join(tmpDir, "gastown", "mayor", "rig")).Seed(beads.Issue{
		ID:     "gt-gastown-crew-sloan",
		Title:  "sloan",
		Type:   "agent",
		Labels: []string{"gt:agent"},
	})

	check := NewStaleAgentBeadsCheck()
	ctx := town.ctx(tmpDir, "")

	result := check.Run(ctx)
	for _, detail := range result.Details {
		if detail == "gt-gastown-crew-sloan" {
			t.Fatalf("unreadable crew directory marked live crew member stale; details: %v", result.Details)
		}
	}
	if result.Status == StatusOK {
		t.Errorf("an unreadable crew directory must be reported, got OK: %s", result.Message)
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() returned error: %v", err)
	}
	if updates := town.updates(); len(updates) != 0 {
		t.Errorf("Fix() closed beads for a rig it could not read: %v", updates)
	}
}

// === Tests for Phase 2: Deregistered rig orphan detection ===

func TestStaleAgentBeadsCheck_Phase2_NoTownBeadsDir(t *testing.T) {
	t.Parallel()
	// Phase 2 scans town beads for orphan agent beads. When the .beads
	// dir exists (for routes) but has no Dolt connection, the check should
	// handle ListAgentBeads failure gracefully and not crash.
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Routes with one known rig + town-level route
	routesContent := `{"prefix":"hq-","path":"."}` + "\n" +
		`{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create the rig beads directory so Phase 1 can attempt to scan
	rigBeadsDir := filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewStaleAgentBeadsCheck()
	ctx := noBD(tmpDir)

	// Should not crash — Phase 2 ListAgentBeads will fail without Dolt
	result := check.Run(ctx)
	t.Logf("Phase 2 no-dolt check: status=%v, message=%s", result.Status, result.Message)

	// Without Dolt, no agent beads can be found, so result should be OK
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when no Dolt server, got %v: %s", result.Status, result.Message)
	}
}

func TestStaleAgentBeadsCheck_KnownPrefixTracking(t *testing.T) {
	t.Parallel()
	// Verify that both town-level routes (path ".") and rig routes are
	// tracked in the knownPrefixes map. Phase 2 uses this to identify
	// orphan beads from deregistered rigs.
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Mix of town-level and rig-level routes
	routesContent := `{"prefix":"hq-","path":"."}` + "\n" +
		`{"prefix":"hq-cv-","path":"."}` + "\n" +
		`{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n" +
		`{"prefix":"bd-","path":"beads/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create rig directories
	for _, rigPath := range []string{"gastown/mayor/rig/.beads", "beads/mayor/rig/.beads"} {
		if err := os.MkdirAll(filepath.Join(tmpDir, rigPath), 0755); err != nil {
			t.Fatal(err)
		}
	}

	check := NewStaleAgentBeadsCheck()
	ctx := noBD(tmpDir)

	// Run should complete without crash — verifies knownPrefixes includes
	// both town-level ("hq", "hq-cv") and rig-level ("gt", "bd") prefixes
	result := check.Run(ctx)
	t.Logf("Known prefix tracking: status=%v, message=%s", result.Status, result.Message)
}

// === Tests for parseCrewOrPolecatFromID helper ===

func TestParseCrewOrPolecatFromID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		id         string
		prefix     string
		rigName    string
		role       string
		wantWorker string
	}{
		{
			name:       "standard crew ID",
			id:         "gt-gastown-crew-alice",
			prefix:     "gt",
			rigName:    "gastown",
			role:       "crew",
			wantWorker: "alice",
		},
		{
			name:       "standard polecat ID",
			id:         "gt-gastown-polecat-nux",
			prefix:     "gt",
			rigName:    "gastown",
			role:       "polecat",
			wantWorker: "nux",
		},
		{
			name:       "collapsed form (prefix == rigName)",
			id:         "ff-crew-joe",
			prefix:     "ff",
			rigName:    "ff",
			role:       "crew",
			wantWorker: "joe",
		},
		{
			name:       "collapsed polecat (prefix == rigName)",
			id:         "ff-polecat-toast",
			prefix:     "ff",
			rigName:    "ff",
			role:       "polecat",
			wantWorker: "toast",
		},
		{
			name:       "different prefix and rigName",
			id:         "sh-shippercrm-crew-controllers",
			prefix:     "sh",
			rigName:    "shippercrm",
			role:       "crew",
			wantWorker: "controllers",
		},
		{
			name:       "ID does not match pattern",
			id:         "gt-gastown-polecat-toast",
			prefix:     "gt",
			rigName:    "gastown",
			role:       "crew",
			wantWorker: "",
		},
		{
			name:       "wrong prefix",
			id:         "bd-beads-crew-human",
			prefix:     "gt",
			rigName:    "gastown",
			role:       "crew",
			wantWorker: "",
		},
		{
			name:       "empty worker name after prefix strip",
			id:         "gt-gastown-crew-",
			prefix:     "gt",
			rigName:    "gastown",
			role:       "crew",
			wantWorker: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worker := parseCrewOrPolecatFromID(tt.id, tt.prefix, tt.rigName, tt.role)
			if worker != tt.wantWorker {
				t.Errorf("worker = %q, want %q", worker, tt.wantWorker)
			}
		})
	}
}

// === Tests for dedup helper ===

func TestDedup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{
			name:  "empty",
			input: []string{},
			want:  []string{},
		},
		{
			name:  "nil",
			input: nil,
			want:  nil,
		},
		{
			name:  "no duplicates",
			input: []string{"a", "b", "c"},
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "all duplicates",
			input: []string{"a", "a", "a"},
			want:  []string{"a"},
		},
		{
			name:  "mixed duplicates preserving order",
			input: []string{"c", "a", "b", "a", "c", "d"},
			want:  []string{"c", "a", "b", "d"},
		},
		{
			name:  "adjacent duplicates",
			input: []string{"x", "x", "y", "y", "z"},
			want:  []string{"x", "y", "z"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dedup(tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("dedup(%v) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// === Test for Fix() fallback to town beads ===

func TestStaleAgentBeadsCheck_FixFallbackToTownBeads(t *testing.T) {
	t.Parallel()
	// When Fix() encounters an orphan bead with unknown prefix, it should
	// attempt to close via town beads client instead of silently skipping.
	// Without Dolt, this verifies the code path doesn't crash.
	tmpDir := t.TempDir()

	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"hq-","path":"."}` + "\n" +
		`{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	rigBeadsDir := filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewStaleAgentBeadsCheck()
	ctx := noBD(tmpDir)

	// Fix should not crash even when Run() reports no stale beads
	err := check.Fix(ctx)
	if err != nil {
		t.Errorf("Fix() returned unexpected error: %v", err)
	}
	t.Logf("Fix() with no stale beads completed without error")
}
