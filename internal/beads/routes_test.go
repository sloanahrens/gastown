package beads

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

func TestGetPrefixForRig(t *testing.T) {
	// Create a temporary directory with routes.jsonl
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix": "gt-", "path": "gastown/mayor/rig"}
{"prefix": "bd-", "path": "beads/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		rig      string
		expected string
	}{
		{"gastown", "gt"},
		{"beads", "bd"},
		{"unknown", "gt"}, // default
		{"", "gt"},        // empty rig -> default
	}

	for _, tc := range tests {
		t.Run(tc.rig, func(t *testing.T) {
			result := GetPrefixForRig(tmpDir, tc.rig)
			if result != tc.expected {
				t.Errorf("GetPrefixForRig(%q, %q) = %q, want %q", tmpDir, tc.rig, result, tc.expected)
			}
		})
	}
}

func TestGetPrefixForRig_NoRoutesFile(t *testing.T) {
	tmpDir := t.TempDir()
	// No routes.jsonl file

	result := GetPrefixForRig(tmpDir, "anything")
	if result != "gt" {
		t.Errorf("Expected default 'gt' when no routes file, got %q", result)
	}
}

func TestGetPrefixForRig_RigsConfigFallback(t *testing.T) {
	tmpDir := t.TempDir()

	// Write rigs.json with a non-gt prefix
	rigsPath := filepath.Join(tmpDir, "mayor", "rigs.json")
	if err := os.MkdirAll(filepath.Dir(rigsPath), 0755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.RigsConfig{
		Version: config.CurrentRigsVersion,
		Rigs: map[string]config.RigEntry{
			"project_ideas": {
				BeadsConfig: &config.BeadsConfig{Prefix: "pi"},
			},
		},
	}
	if err := config.SaveRigsConfig(rigsPath, cfg); err != nil {
		t.Fatalf("SaveRigsConfig: %v", err)
	}

	result := GetPrefixForRig(tmpDir, "project_ideas")
	if result != "pi" {
		t.Errorf("Expected prefix from rigs config, got %q", result)
	}
}

// GetPrefixForRig keeps its "gt" default when rigs.json is unreadable: its
// callers build ids from the result without an empty check (gt-71dgi).
func TestGetPrefixForRig_CorruptRigsJSONKeepsDefault(t *testing.T) {
	tmpDir := t.TempDir()
	rigsPath := filepath.Join(tmpDir, "mayor", "rigs.json")
	if err := os.MkdirAll(filepath.Dir(rigsPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rigsPath, []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}

	if got := GetPrefixForRig(tmpDir, "gastown"); got != "gt" {
		t.Errorf("GetPrefixForRig with a corrupt rigs.json = %q, want the gt default", got)
	}
}

func TestExtractPrefix(t *testing.T) {
	tests := []struct {
		beadID   string
		expected string
	}{
		{"ap-qtsup.16", "ap-"},
		{"hq-cv-abc", "hq-"},
		{"gt-mol-xyz", "gt-"},
		{"bd-123", "bd-"},
		{"", ""},
		{"nohyphen", ""},
		{"-startswithhyphen", ""}, // Leading hyphen = invalid prefix
		{"-", ""},                 // Just hyphen = invalid
		{"a-", "a-"},              // Trailing hyphen is valid
	}

	for _, tc := range tests {
		t.Run(tc.beadID, func(t *testing.T) {
			result := ExtractPrefix(tc.beadID)
			if result != tc.expected {
				t.Errorf("ExtractPrefix(%q) = %q, want %q", tc.beadID, result, tc.expected)
			}
		})
	}
}

func TestGetRigPathForPrefix(t *testing.T) {
	// Create a temporary directory with routes.jsonl
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix": "ap-", "path": "ai_platform/mayor/rig"}
{"prefix": "gt-", "path": "gastown/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		prefix   string
		expected string
	}{
		{"ap-", filepath.Join(tmpDir, "ai_platform/mayor/rig")},
		{"gt-", filepath.Join(tmpDir, "gastown/mayor/rig")},
		{"hq-", tmpDir},  // Town-level beads return townRoot
		{"unknown-", ""}, // Unknown prefix returns empty
		{"", ""},         // Empty prefix returns empty
	}

	for _, tc := range tests {
		t.Run(tc.prefix, func(t *testing.T) {
			result := GetRigPathForPrefix(tmpDir, tc.prefix)
			if result != tc.expected {
				t.Errorf("GetRigPathForPrefix(%q, %q) = %q, want %q", tmpDir, tc.prefix, result, tc.expected)
			}
		})
	}
}

func TestGetRigPathForPrefix_NoRoutesFile(t *testing.T) {
	tmpDir := t.TempDir()
	// No routes.jsonl file

	result := GetRigPathForPrefix(tmpDir, "ap-")
	if result != "" {
		t.Errorf("Expected empty string when no routes file, got %q", result)
	}
}

func TestResolveHookDir(t *testing.T) {
	// Create a temporary directory with routes.jsonl
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix": "ap-", "path": "ai_platform/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		beadID      string
		hookWorkDir string
		expected    string
	}{
		{
			name:        "prefix resolution takes precedence over hookWorkDir",
			beadID:      "ap-test",
			hookWorkDir: "/custom/path",
			expected:    filepath.Join(tmpDir, "ai_platform/mayor/rig"),
		},
		{
			name:        "resolves rig path from prefix",
			beadID:      "ap-test",
			hookWorkDir: "",
			expected:    filepath.Join(tmpDir, "ai_platform/mayor/rig"),
		},
		{
			name:        "town-level bead returns townRoot",
			beadID:      "hq-test",
			hookWorkDir: "",
			expected:    tmpDir,
		},
		{
			name:        "unknown prefix uses hookWorkDir as fallback",
			beadID:      "xx-unknown",
			hookWorkDir: "/fallback/path",
			expected:    "/fallback/path",
		},
		{
			name:        "unknown prefix without hookWorkDir falls back to townRoot",
			beadID:      "xx-unknown",
			hookWorkDir: "",
			expected:    tmpDir,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := ResolveHookDir(tmpDir, tc.beadID, tc.hookWorkDir)
			if result != tc.expected {
				t.Errorf("ResolveHookDir(%q, %q, %q) = %q, want %q",
					tmpDir, tc.beadID, tc.hookWorkDir, result, tc.expected)
			}
		})
	}
}

func TestResolveBeadsDirForID(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create rig beads directory for gt- prefix
	rigBeadsDir := filepath.Join(tmpDir, "gastown/mayor/rig/.beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix": "gt-", "path": "gastown/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		beadID   string
		expected string
	}{
		{
			name:     "town-level bead returns currentBeadsDir",
			beadID:   "hq-test123",
			expected: beadsDir,
		},
		{
			name:     "rig-prefixed bead resolves to rig beadsDir",
			beadID:   "gt-abc",
			expected: rigBeadsDir,
		},
		{
			name:     "unknown prefix returns currentBeadsDir",
			beadID:   "xx-unknown",
			expected: beadsDir,
		},
		{
			name:     "empty bead ID returns currentBeadsDir",
			beadID:   "",
			expected: beadsDir,
		},
		{
			name:     "no hyphen returns currentBeadsDir",
			beadID:   "nohyphen",
			expected: beadsDir,
		},
		{
			name:     "wisp bead (hq-wisp-xxx) resolves to town beads",
			beadID:   "hq-wisp-abc123",
			expected: beadsDir,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := ResolveBeadsDirForID(beadsDir, tc.beadID)
			if result != tc.expected {
				t.Errorf("ResolveBeadsDirForID(%q, %q) = %q, want %q",
					beadsDir, tc.beadID, result, tc.expected)
			}
		})
	}
}

func TestResolveBeadsDirForID_NoRoutes(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	// No routes.jsonl — should always return currentBeadsDir
	result := ResolveBeadsDirForID(beadsDir, "gt-abc")
	if result != beadsDir {
		t.Errorf("expected %q, got %q", beadsDir, result)
	}
}

func TestResolveBeadsDirForID_UsesTownRoutesFromWorktreeBeadsDir(t *testing.T) {
	tmpDir := t.TempDir()
	townBeadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(townBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	worktreeBeadsDir := filepath.Join(tmpDir, "gastown", "polecats", "chrome", "gastown", ".beads")
	if err := os.MkdirAll(worktreeBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	rigBeadsDir := filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "mayor", "town.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix": "gt-", "path": "gastown/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(townBeadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	result := ResolveBeadsDirForID(worktreeBeadsDir, "gt-abc")
	if result != rigBeadsDir {
		t.Fatalf("ResolveBeadsDirForID(%q, %q) = %q, want %q", worktreeBeadsDir, "gt-abc", result, rigBeadsDir)
	}
	result = ResolveBeadsDirForID(worktreeBeadsDir, "hq-wisp-abc")
	if result != townBeadsDir {
		t.Fatalf("ResolveBeadsDirForID(%q, %q) = %q, want %q", worktreeBeadsDir, "hq-wisp-abc", result, townBeadsDir)
	}
}

func TestGetRigNameForPrefix(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix": "gt-", "path": "gastown/mayor/rig"}
{"prefix": "bd-", "path": "beads/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		prefix   string
		expected string
	}{
		{"gt-", "gastown"},
		{"bd-", "beads"},
		{"hq-", ""},      // Town-level, no specific rig
		{"unknown-", ""}, // Not in routes
		{"", ""},         // Empty prefix
	}

	for _, tc := range tests {
		t.Run(tc.prefix, func(t *testing.T) {
			result := GetRigNameForPrefix(tmpDir, tc.prefix)
			if result != tc.expected {
				t.Errorf("GetRigNameForPrefix(%q, %q) = %q, want %q", tmpDir, tc.prefix, result, tc.expected)
			}
		})
	}
}

func TestGetRigDirForName(t *testing.T) {
	// pathWithin resolves symlinks on the root but the rig dirs never exist,
	// so a symlinked TempDir (macOS /var -> /private/var) would make every
	// existing-root/nonexistent-path comparison fail. Canonicalize up front.
	tmpDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix": "ga-", "path": "gantry"}
{"prefix": "al-", "path": "algoanki/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		rigName  string
		expected string
	}{
		{"gantry", filepath.Join(tmpDir, "gantry")},
		{"algoanki", filepath.Join(tmpDir, "algoanki/mayor/rig")},
		{"unknown", ""}, // Not in routes
		{"", ""},        // Empty rig name
	}

	for _, tc := range tests {
		t.Run(tc.rigName, func(t *testing.T) {
			result := GetRigDirForName(tmpDir, tc.rigName)
			if result != tc.expected {
				t.Errorf("GetRigDirForName(%q, %q) = %q, want %q", tmpDir, tc.rigName, result, tc.expected)
			}
		})
	}
}

func TestGetRigDirForName_TownLevelNotReturned(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}
	// Town-level rig (path=".") should not be returned — it has no rig dir.
	result := GetRigDirForName(tmpDir, "hq")
	if result != "" {
		t.Errorf("GetRigDirForName for town-level path = %q, want empty string", result)
	}
}

func TestResolveRepoAliasBeadsDir(t *testing.T) {
	townRoot := t.TempDir()
	townBeads := filepath.Join(townRoot, ".beads")
	rigRoot := filepath.Join(townRoot, "gastown")
	canonicalRig := filepath.Join(rigRoot, "mayor", "rig")
	canonicalBeads := filepath.Join(canonicalRig, ".beads")
	decoyBeads := filepath.Join(rigRoot, ".beads")
	escapeRig := filepath.Join(townRoot, "escape", "mayor", "rig")
	escapeBeads := filepath.Join(escapeRig, ".beads")
	escapeTarget := filepath.Join(townRoot, "..", "outside", ".beads")

	for _, dir := range []string{townBeads, canonicalBeads, decoyBeads, escapeBeads, escapeTarget} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(
		`{"prefix":"hq-","path":"."}`+"\n"+
			`{"prefix":"gt-","path":"gastown/mayor/rig"}`+"\n"+
			`{"prefix":"es-","path":"escape/mayor/rig"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonicalBeads, "metadata.json"), []byte(`{"dolt_database":"gastown"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoyBeads, "metadata.json"), []byte(`{"dolt_database":"hq"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(escapeBeads, "redirect"), []byte(filepath.Join("..", "..", "..", "..", "outside", ".beads")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		repo     string
		expected string
		ok       bool
	}{
		{"rig alias uses canonical route", "gastown", canonicalBeads, true},
		{"hq alias uses town beads", "hq", townBeads, true},
		{"town alias uses town beads", "town", townBeads, true},
		{"path-like remains unresolved", "gastown/mayor/rig", "", false},
		{"unknown bare repo remains unresolved", "unknown", "", false},
		{"redirect outside town rejected", "escape", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ResolveRepoAliasBeadsDir(townRoot, tc.repo)
			if ok != tc.ok {
				t.Fatalf("ResolveRepoAliasBeadsDir ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.expected {
				t.Fatalf("ResolveRepoAliasBeadsDir dir = %q, want %q", got, tc.expected)
			}
		})
	}
}

// seatTownWithRoutes builds the town a sandbox seat gets (gt-dmtmv): a routes
// table naming rigs a and b and the town, the town's and rig a's workspaces on
// disk, and the registry that names both rigs' databases. Rig b's workspace is
// the one the skeleton leaves out.
func seatTownWithRoutes(t *testing.T) (townRoot string) {
	t.Helper()
	// pathWithin compares resolved roots, so a symlinked temp dir (macOS
	// /var -> /private/var) must be resolved before the town is built.
	townRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	townBeads := filepath.Join(townRoot, ".beads")
	rigABeads := filepath.Join(townRoot, "a", "mayor", "rig", ".beads")
	for _, dir := range []string{filepath.Join(townRoot, "mayor"), townBeads, rigABeads} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(
		`{"prefix":"aa-","path":"a/mayor/rig"}`+"\n"+
			`{"prefix":"bb-","path":"b/mayor/rig"}`+"\n"+
			`{"prefix":"hq-","path":"."}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{
  "type": "town",
  "version": 2,
  "name": "seat",
  "registry": {"version": 1, "rigs": {
    "a": {"beads": {"prefix": "aa"}, "dolt_database": "aa"},
    "b": {"beads": {"prefix": "bb"}, "dolt_database": "bbdb"}
  }}
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigABeads, "metadata.json"), []byte(`{"dolt_database":"aa"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

// TestRepoAliasSkeletonTown: a seat's town holds one rig's tree while its
// routes table names every rig, and a polecat must still be able to name
// another rig. Alias b resolves to the beads directory its route names, which
// the resolution does not create.
func TestRepoAliasSkeletonTown(t *testing.T) {
	t.Parallel()
	townRoot := seatTownWithRoutes(t)

	got, ok := ResolveRepoAliasBeadsDir(townRoot, "b")
	if !ok {
		t.Fatalf("ResolveRepoAliasBeadsDir(%q, \"b\") did not resolve", townRoot)
	}
	want := filepath.Join(townRoot, "b", "mayor", "rig", ".beads")
	if got != want {
		t.Fatalf("ResolveRepoAliasBeadsDir = %q, want %q", got, want)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("resolving alias b touched %s (stat err = %v); resolution creates nothing", want, err)
	}

	// The rig whose workspace is on disk resolves as it always did.
	if got, ok := ResolveRepoAliasBeadsDir(townRoot, "a"); !ok || got != filepath.Join(townRoot, "a", "mayor", "rig", ".beads") {
		t.Fatalf("ResolveRepoAliasBeadsDir(%q) = %q, %v; want rig a's workspace", got, got, ok)
	}
}

// TestRepoAliasUnknown: an alias the routes table does not name is refused by
// the create path, not guessed.
func TestRepoAliasUnknown(t *testing.T) {
	t.Parallel()
	townRoot := seatTownWithRoutes(t)
	r := newRecorder(nil)
	b := newBeads(beadsFields{workDir: filepath.Join(townRoot, "a", "mayor", "rig"), baseEnv: []string{"PATH=/usr/bin"}, exec: r.exec})

	_, err := b.Create(CreateOptions{Title: "filed nowhere", Rig: "nowhere"})
	if err == nil || !strings.Contains(err.Error(), `unknown repo/rig alias "nowhere"`) {
		t.Fatalf("Create(--rig=nowhere) error = %v, want the unknown repo/rig alias refusal", err)
	}
	if calls := r.calls(); len(calls) != 0 {
		t.Fatalf("bd ran %d time(s) for an unresolved alias: %v", len(calls), r.argvs())
	}
	if _, ok := ResolveRepoAliasBeadsDir(townRoot, "nowhere"); ok {
		t.Error("ResolveRepoAliasBeadsDir resolved an alias the routes table does not name")
	}
}

// TestRepoAliasRigTheTownCannotPlace: a rig the routes name but the town
// registry gives no database is refused rather than left to bd's fallback,
// which is the town's own database.
func TestRepoAliasRigTheTownCannotPlace(t *testing.T) {
	t.Parallel()
	townRoot := seatTownWithRoutes(t)
	// Drop rig b from the registry, keeping its route.
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{
  "type": "town",
  "version": 2,
  "name": "seat",
  "registry": {"version": 1, "rigs": {
    "a": {"beads": {"prefix": "aa"}, "dolt_database": "aa"}
  }}
}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if got, ok := ResolveRepoAliasBeadsDir(townRoot, "b"); ok {
		t.Fatalf("ResolveRepoAliasBeadsDir resolved rig b to %q with no database in the registry", got)
	}
	if _, err := os.Stat(filepath.Join(townRoot, "b")); !os.IsNotExist(err) {
		t.Fatalf("rig b's tree exists (stat err = %v); the test needs it absent", err)
	}
}

// TestRepoAliasCreatePinsAnAbsentWorkspace: creating in a rig whose workspace
// the seat does not hold pins bd to that rig's database by name, and writes
// nothing: no workspace directory, no config.yaml, and never the town's
// database.
func TestRepoAliasCreatePinsAnAbsentWorkspace(t *testing.T) {
	t.Parallel()
	townRoot := seatTownWithRoutes(t)
	r := newRecorder(func([]string) reply {
		return reply{stdout: `{"id":"bb-1","title":"found in b","status":"open"}`}
	})
	b := newBeads(beadsFields{workDir: filepath.Join(townRoot, "a", "mayor", "rig"), baseEnv: []string{"PATH=/usr/bin"}, exec: r.exec})

	if _, err := b.Create(CreateOptions{Title: "found in b", Rig: "b"}); err != nil {
		t.Fatalf("Create(--rig=b): %v", err)
	}

	calls := r.calls()
	if len(calls) != 1 {
		t.Fatalf("bd calls = %d, want one create: %v", len(calls), r.argvs())
	}
	wantDir := filepath.Join(townRoot, "b", "mayor", "rig", ".beads")
	if got, _ := lastEnvValue(calls[0].env, "BEADS_DIR"); got != wantDir {
		t.Errorf("BEADS_DIR = %q, want rig b's workspace %q", got, wantDir)
	}
	if got, _ := lastEnvValue(calls[0].env, "BEADS_DOLT_SERVER_DATABASE"); got != "bbdb" {
		t.Errorf("BEADS_DOLT_SERVER_DATABASE = %q, want bbdb (rig b's database)", got)
	}
	if _, err := os.Stat(wantDir); !os.IsNotExist(err) {
		t.Errorf("create made rig b's workspace (stat err = %v); it must not", err)
	}
}

// TestRepoAliasCreateRefusesAnUnwritableWorkspaceConfig: the create stops when
// a workspace it resolved cannot take its config.yaml, rather than running bd
// against a workspace the render could not prepare.
func TestRepoAliasCreateRefusesAnUnwritableWorkspaceConfig(t *testing.T) {
	t.Parallel()
	townRoot := seatTownWithRoutes(t)
	// config.yaml as a directory: EnsureConfigYAML cannot read or write it.
	if err := os.Mkdir(filepath.Join(townRoot, "a", "mayor", "rig", ".beads", "config.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newRecorder(nil)
	b := newBeads(beadsFields{workDir: filepath.Join(townRoot, "a", "mayor", "rig"), baseEnv: []string{"PATH=/usr/bin"}, exec: r.exec})

	_, err := b.Create(CreateOptions{Title: "filed in a", Rig: "a"})
	if err == nil || !strings.Contains(err.Error(), `ensuring beads config for rig "a"`) {
		t.Fatalf("Create(--rig=a) error = %v, want the config refusal", err)
	}
	if calls := r.calls(); len(calls) != 0 {
		t.Fatalf("bd ran %d time(s) for an unprepared workspace: %v", len(calls), r.argvs())
	}
}

// TestEnsureCreateConfigYAML: the create writes config.yaml into a workspace
// that exists, leaves an absent one absent, and refuses a path that is not a
// workspace it can prepare. The last two outcomes are unreachable from Create
// (resolution already refused them), so they are driven here.
func TestEnsureCreateConfigYAML(t *testing.T) {
	t.Parallel()
	townRoot := seatTownWithRoutes(t)

	present := filepath.Join(townRoot, "a", "mayor", "rig", ".beads")
	if err := ensureCreateConfigYAML(present, townRoot, "a"); err != nil {
		t.Fatalf("ensureCreateConfigYAML(present workspace): %v", err)
	}
	config, err := os.ReadFile(filepath.Join(present, "config.yaml"))
	if err != nil || !strings.Contains(string(config), "issue-prefix: aa") {
		t.Fatalf("config.yaml = %q, %v; want the rig's prefix written", config, err)
	}

	absent := filepath.Join(townRoot, "b", "mayor", "rig", ".beads")
	if err := ensureCreateConfigYAML(absent, townRoot, "b"); err != nil {
		t.Fatalf("ensureCreateConfigYAML(absent workspace): %v", err)
	}
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Fatalf("an absent workspace was created (stat err = %v)", err)
	}

	// A path that is a file, and one whose parent is a file: neither is a
	// workspace to write into.
	filePath := filepath.Join(townRoot, "filerig", "mayor", "rig", ".beads")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte("not a workspace"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureCreateConfigYAML(filePath, townRoot, "filerig"); err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("ensureCreateConfigYAML(file path) error = %v, want the not-a-directory refusal", err)
	}

	underFile := filepath.Join(townRoot, "blob", "mayor", "rig", ".beads")
	if err := os.WriteFile(filepath.Join(townRoot, "blob"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureCreateConfigYAML(underFile, townRoot, "blob"); err == nil || !strings.Contains(err.Error(), "inspecting beads workspace") {
		t.Fatalf("ensureCreateConfigYAML(uninspectable path) error = %v, want the inspection refusal", err)
	}
}

// TestLoadRoutesRejectsAnUnreadableFile: a routes.jsonl that cannot be read is
// an error, not an empty table (a town whose routes vanished must not read as
// having none).
func TestLoadRoutesRejectsAnUnreadableFile(t *testing.T) {
	t.Parallel()
	beadsDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(beadsDir, RoutesFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRoutes(beadsDir); err == nil {
		t.Fatal("LoadRoutes = nil error for a routes.jsonl that cannot be read")
	}
}

// TestParseRoutesSkipsMalformedLines: a malformed line is reported and skipped,
// and the routes around it survive.
func TestParseRoutesSkipsMalformedLines(t *testing.T) {
	t.Parallel()
	doc := []byte("# a comment\n" +
		`{"prefix":"aa-","path":"a/mayor/rig"}` + "\n" +
		"not json\n" +
		"\n" +
		`{"prefix":"bb-","path":"b/mayor/rig"}` + "\n")

	var warned []int
	routes := ParseRoutes(doc, func(line int, err error) {
		if err == nil {
			t.Error("ParseRoutes warned with a nil error")
		}
		warned = append(warned, line)
	})
	if len(warned) != 1 || warned[0] != 3 {
		t.Errorf("warned lines = %v, want just line 3", warned)
	}
	if len(routes) != 2 || routes[0].Prefix != "aa-" || routes[1].Prefix != "bb-" {
		t.Errorf("routes = %+v, want the two valid lines", routes)
	}
}

func TestRewriteBDCreateRepoAlias(t *testing.T) {
	townRoot := t.TempDir()
	townBeads := filepath.Join(townRoot, ".beads")
	canonicalBeads := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	decoyBeads := filepath.Join(townRoot, "gastown", ".beads")
	for _, dir := range []string{townBeads, canonicalBeads, decoyBeads} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(
		`{"prefix":"hq-","path":"."}`+"\n"+
			`{"prefix":"gt-","path":"gastown/mayor/rig"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		argv    []string
		want    []string
		wantDir string
	}{
		{
			name:    "global flags before create",
			argv:    []string{"bd", "--allow-stale", "create", "--repo", "gastown", "--title", "x"},
			want:    []string{"bd", "--allow-stale", "create", "--title", "x"},
			wantDir: canonicalBeads,
		},
		{
			name:    "equals form",
			argv:    []string{"bd", "--json", "create", "--repo=gastown"},
			want:    []string{"bd", "--json", "create"},
			wantDir: canonicalBeads,
		},
		{
			name:    "unknown bare repo unchanged",
			argv:    []string{"bd", "create", "--repo", "unknown", "--title", "x"},
			want:    []string{"bd", "create", "--repo", "unknown", "--title", "x"},
			wantDir: "",
		},
		{
			name:    "path-like repo unchanged",
			argv:    []string{"bd", "create", "--repo", "gastown/mayor/rig", "--title", "x"},
			want:    []string{"bd", "create", "--repo", "gastown/mayor/rig", "--title", "x"},
			wantDir: "",
		},
		{
			name:    "duplicate repo unchanged",
			argv:    []string{"bd", "create", "--repo", "gastown", "--repo", "/tmp/other"},
			want:    []string{"bd", "create", "--repo", "gastown", "--repo", "/tmp/other"},
			wantDir: "",
		},
		{
			name:    "repo after sentinel unchanged",
			argv:    []string{"bd", "create", "--", "--repo", "gastown"},
			want:    []string{"bd", "create", "--", "--repo", "gastown"},
			wantDir: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, gotDir := RewriteBDCreateRepoAlias(townRoot, tc.argv)
			if gotDir != tc.wantDir {
				t.Fatalf("beads dir = %q, want %q", gotDir, tc.wantDir)
			}
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("argv = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestCheckPrefixAvailable(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix": "gt-", "path": "gastown/mayor/rig"}
{"prefix": "bd-", "path": "beads/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		prefix  string
		newPath string
		wantErr bool
	}{
		{
			name:    "new prefix is available",
			prefix:  "cr-",
			newPath: "crucible",
			wantErr: false,
		},
		{
			name:    "same rig re-registering same prefix",
			prefix:  "gt-",
			newPath: "gastown",
			wantErr: false,
		},
		{
			name:    "same rig different path variant",
			prefix:  "gt-",
			newPath: "gastown/mayor/rig",
			wantErr: false,
		},
		{
			name:    "collision with different rig",
			prefix:  "gt-",
			newPath: "getresearch",
			wantErr: true,
		},
		{
			name:    "collision with beads prefix",
			prefix:  "bd-",
			newPath: "boardgame",
			wantErr: true,
		},
		{
			name:    "town-level prefix not blocked",
			prefix:  "hq-",
			newPath: "headquarters",
			wantErr: true, // "." rig name conflicts with "headquarters"
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckPrefixAvailable(tmpDir, tc.prefix, tc.newPath)
			if (err != nil) != tc.wantErr {
				t.Errorf("CheckPrefixAvailable(%q, %q) error = %v, wantErr %v",
					tc.prefix, tc.newPath, err, tc.wantErr)
			}
		})
	}
}

func TestCheckPrefixAvailable_NoRoutes(t *testing.T) {
	tmpDir := t.TempDir()
	// No .beads directory — all prefixes should be available
	err := CheckPrefixAvailable(tmpDir, "gt-", "gastown")
	if err != nil {
		t.Errorf("expected no error with no routes file, got: %v", err)
	}
}

// TestCheckPrefixAvailableRejectsExistingRigName is the regression test for
// gt-1h1ea: rig add drops the databases named like the new prefix, so a
// prefix naming a registered rig's database has to be refused even when no
// route carries it.
func TestCheckPrefixAvailableRejectsExistingRigName(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix": "gt-", "path": "gastown/mayor/rig"}
{"prefix": "bd-", "path": "beads/mayor/rig"}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}
	// beads_ab is registered but routed nowhere, which is the state the drop
	// sees when a rig is added from a config the route file predates. It
	// records no database of its own, so its name stands in for one.
	registry := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{
		"gastown":  {BeadsConfig: &config.BeadsConfig{Prefix: "gt"}, DoltDatabase: "gt"},
		"beads":    {BeadsConfig: &config.BeadsConfig{Prefix: "bd"}, DoltDatabase: "beads"},
		"beads_ab": {BeadsConfig: &config.BeadsConfig{Prefix: "ba"}},
		"mango":    {BeadsConfig: &config.BeadsConfig{Prefix: "mg"}, DoltDatabase: "beads_ma"},
	}}
	if err := os.MkdirAll(filepath.Join(tmpDir, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveRigsConfig(filepath.Join(tmpDir, "mayor", "rigs.json"), registry); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		prefix  string
		newPath string
		wantErr bool
	}{
		{"prefix names a registered rig's database", "beads", "foo", true},
		{"prefix names a rig, not that rig's database", "gastown", "foo", false},
		{"prefix names beads_<registered rig>", "ab", "alpha_bravo", true},
		{"prefix names another rig's beads_<prefix> database", "ma", "mallow", true},
		{"prefix names the new rig itself", "foo", "foo", false},
		{"prefix names nothing registered", "co", "coolthing", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckPrefixAvailable(tmpDir, tc.prefix, tc.newPath)
			if (err != nil) != tc.wantErr {
				t.Errorf("CheckPrefixAvailable(%q, %q) error = %v, wantErr %v",
					tc.prefix, tc.newPath, err, tc.wantErr)
			}
		})
	}
}

func TestAgentBeadIDsWithPrefix(t *testing.T) {
	tests := []struct {
		name     string
		fn       func() string
		expected string
	}{
		{"PolecatBeadIDWithPrefix bd beads obsidian",
			func() string { return PolecatBeadIDWithPrefix("bd", "beads", "obsidian") },
			"bd-beads-polecat-obsidian"},
		{"PolecatBeadIDWithPrefix gt gastown Toast",
			func() string { return PolecatBeadIDWithPrefix("gt", "gastown", "Toast") },
			"gt-gastown-polecat-Toast"},
		{"CrewBeadIDWithPrefix bd beads max",
			func() string { return CrewBeadIDWithPrefix("bd", "beads", "max") },
			"bd-beads-crew-max"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.fn()
			if result != tc.expected {
				t.Errorf("got %q, want %q", result, tc.expected)
			}
		})
	}
}

// TestValidateRigPrefix verifies the post-creation prefix guard (gt-gpy).
func TestValidateRigPrefix(t *testing.T) {
	// Set up a town root with routes.jsonl.
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix": "gt-", "path": "gastown/mayor/rig"}
{"prefix": "bd-", "path": "beads/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		rigName string
		beadID  string
		wantErr bool
	}{
		{
			name:    "same-rig bead: no error",
			rigName: "gastown",
			beadID:  "gt-wisp-abc",
			wantErr: false,
		},
		{
			name:    "cross-rig: hq- bead on gastown rig returns error",
			rigName: "gastown",
			beadID:  "hq-wisp-xyz",
			wantErr: true,
		},
		{
			name:    "bd- bead on beads rig: no error",
			rigName: "beads",
			beadID:  "bd-wisp-123",
			wantErr: false,
		},
		{
			name:    "empty bead ID: no error (can't determine prefix)",
			rigName: "gastown",
			beadID:  "",
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRigPrefix(tmpDir, tc.rigName, tc.beadID)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateRigPrefix(%q, %q) error = %v, wantErr %v", tc.rigName, tc.beadID, err, tc.wantErr)
			}
		})
	}
}
