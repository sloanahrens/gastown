package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNewPrefixMismatchCheck(t *testing.T) {
	check := NewPrefixMismatchCheck()

	if check.Name() != "prefix-mismatch" {
		t.Errorf("expected name 'prefix-mismatch', got %q", check.Name())
	}

	if !check.CanFix() {
		t.Error("expected CanFix to return true")
	}
}

func TestPrefixMismatchCheck_NoRoutes(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewPrefixMismatchCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for no routes, got %v", result.Status)
	}
}

func TestPrefixMismatchCheck_NoRigsJson(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create routes.jsonl
	routesPath := filepath.Join(beadsDir, "routes.jsonl")
	routesContent := `{"prefix":"gt-","path":"gastown/mayor/rig"}`
	if err := os.WriteFile(routesPath, []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewPrefixMismatchCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK when no rigs.json, got %v", result.Status)
	}
}

func TestPrefixMismatchCheck_Matching(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	mayorDir := filepath.Join(tmpDir, "mayor")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create routes.jsonl with gt- prefix
	routesPath := filepath.Join(beadsDir, "routes.jsonl")
	routesContent := `{"prefix":"gt-","path":"gastown/mayor/rig"}`
	if err := os.WriteFile(routesPath, []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create rigs.json with matching gt prefix
	rigsPath := filepath.Join(mayorDir, "rigs.json")
	rigsContent := `{
		"version": 1,
		"rigs": {
			"gastown": {
				"git_url": "https://github.com/example/gastown",
				"beads": {
					"prefix": "gt"
				}
			}
		}
	}`
	if err := os.WriteFile(rigsPath, []byte(rigsContent), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewPrefixMismatchCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for matching prefixes, got %v: %s", result.Status, result.Message)
	}
}

func TestPrefixMismatchCheck_Mismatch(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	mayorDir := filepath.Join(tmpDir, "mayor")
	// Create rig's mayor/rig/.beads directory and redirect so ResolveBeadsDir returns the mayor/rig path
	rigBeadsDir := filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads")
	rigRootBeadsDir := filepath.Join(tmpDir, "gastown", ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigRootBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Create redirect file so ResolveBeadsDir follows it
	if err := os.WriteFile(filepath.Join(rigRootBeadsDir, "redirect"), []byte("mayor/rig/.beads\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create routes.jsonl with gt- prefix
	routesPath := filepath.Join(beadsDir, "routes.jsonl")
	routesContent := `{"prefix":"gt-","path":"gastown/mayor/rig"}`
	if err := os.WriteFile(routesPath, []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create rigs.json with WRONG prefix (ga instead of gt)
	rigsPath := filepath.Join(mayorDir, "rigs.json")
	rigsContent := `{
		"version": 1,
		"rigs": {
			"gastown": {
				"git_url": "https://github.com/example/gastown",
				"beads": {
					"prefix": "ga"
				}
			}
		}
	}`
	if err := os.WriteFile(rigsPath, []byte(rigsContent), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewPrefixMismatchCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning for prefix mismatch, got %v: %s", result.Status, result.Message)
	}

	if len(result.Details) != 1 {
		t.Errorf("expected 1 detail, got %d", len(result.Details))
	}
}

func TestPrefixMismatchCheck_Fix(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	mayorDir := filepath.Join(tmpDir, "mayor")
	// Create rig's mayor/rig/.beads directory and redirect so ResolveBeadsDir returns the mayor/rig path
	rigBeadsDir := filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads")
	rigRootBeadsDir := filepath.Join(tmpDir, "gastown", ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigRootBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Create redirect file so ResolveBeadsDir follows it
	if err := os.WriteFile(filepath.Join(rigRootBeadsDir, "redirect"), []byte("mayor/rig/.beads\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create routes.jsonl with gt- prefix
	routesPath := filepath.Join(beadsDir, "routes.jsonl")
	routesContent := `{"prefix":"gt-","path":"gastown/mayor/rig"}`
	if err := os.WriteFile(routesPath, []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create rigs.json with WRONG prefix (ga instead of gt)
	rigsPath := filepath.Join(mayorDir, "rigs.json")
	rigsContent := `{
		"version": 1,
		"rigs": {
			"gastown": {
				"git_url": "https://github.com/example/gastown",
				"beads": {
					"prefix": "ga"
				}
			}
		}
	}`
	if err := os.WriteFile(rigsPath, []byte(rigsContent), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewPrefixMismatchCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	// First verify there's a mismatch
	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected mismatch before fix, got %v", result.Status)
	}

	// Fix it
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() failed: %v", err)
	}

	// Verify it's now fixed
	result = check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK after fix, got %v: %s", result.Status, result.Message)
	}

	// Verify rigs.json was updated
	data, err := os.ReadFile(rigsPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadRigsConfig(rigsPath)
	if err != nil {
		t.Fatalf("failed to load fixed rigs.json: %v (content: %s)", err, data)
	}
	if cfg.Rigs["gastown"].BeadsConfig.Prefix != "gt" {
		t.Errorf("expected prefix 'gt' after fix, got %q", cfg.Rigs["gastown"].BeadsConfig.Prefix)
	}
}

func TestNewDatabasePrefixCheck(t *testing.T) {
	check := NewDatabasePrefixCheck()

	if check.Name() != "database-prefix" {
		t.Errorf("expected name 'database-prefix', got %q", check.Name())
	}

	if !check.CanFix() {
		t.Error("expected CanFix to return true")
	}
}

func TestDatabasePrefixCheck_NoRoutes(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := NewDatabasePrefixCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for no routes, got %v", result.Status)
	}
}

func TestDatabasePrefixCheck_EmptyRoutes(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create empty routes.jsonl
	routesPath := filepath.Join(beadsDir, "routes.jsonl")
	if err := os.WriteFile(routesPath, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewDatabasePrefixCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for empty routes, got %v", result.Status)
	}
}

func TestDatabasePrefixCheck_NoBeadsDir(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create routes.jsonl with a route to a non-existent beads directory
	routesPath := filepath.Join(beadsDir, "routes.jsonl")
	routesContent := `{"prefix":"gt-","path":"gastown/mayor/rig"}`
	if err := os.WriteFile(routesPath, []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewDatabasePrefixCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	// No beads dir for the rig means zero prefix comparisons were made —
	// that's unverified, not confirmed-matching (gt-whvu).
	if result.Status != StatusSkipped {
		t.Errorf("expected StatusSkipped when rig beads dir doesn't exist, got %v", result.Status)
	}
}

// mockDBPrefixGetter returns canned prefixes by directory for testing.
type mockDBPrefixGetter struct {
	prefixes map[string]string // rigPath -> prefix
	setCalls []prefixSetCall   // recorded Fix calls (not used by getter, but handy for the mock)
}

type prefixSetCall struct {
	rigPath string
	prefix  string
}

func (m *mockDBPrefixGetter) GetDBPrefix(rigPath string) (string, error) {
	if p, ok := m.prefixes[rigPath]; ok {
		return p, nil
	}
	return "", fmt.Errorf("no prefix configured")
}

func TestDatabasePrefixCheck_SkipsRigRedirectingToTownDB(t *testing.T) {
	// Layout:
	//   <town>/.beads/           <- town root beads (prefix "hq")
	//   <town>/site_manager/.beads/redirect -> "../.beads"  (shares town DB)
	//   routes.jsonl has both {"prefix":"hq-","path":"."} and {"prefix":"sm-","path":"site_manager"}
	//
	// Before the fix, the check would see site_manager's DB prefix is "hq" (because
	// it shares the town DB), flag it as a mismatch with "sm", and --fix would
	// overwrite the shared DB's prefix to "sm", breaking the town.

	tmpDir := t.TempDir()

	// Town-level .beads
	townBeads := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(townBeads, 0755); err != nil {
		t.Fatal(err)
	}

	// routes.jsonl: town root + site_manager
	routesContent := `{"prefix":"hq-","path":"."}
{"prefix":"sm-","path":"site_manager"}`
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	// site_manager/.beads/redirect -> ../.beads (shares town DB)
	smBeads := filepath.Join(tmpDir, "site_manager", ".beads")
	if err := os.MkdirAll(smBeads, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(smBeads, "redirect"), []byte("../.beads\n"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := &mockDBPrefixGetter{
		prefixes: map[string]string{
			filepath.Join(tmpDir, "site_manager"): "hq",
		},
	}

	check := NewDatabasePrefixCheck()
	check.prefixGetter = mock
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	// The redirect rig is correctly excluded from comparison (never flagged
	// as a mismatch), but that also means zero prefixes were actually
	// verified — report skipped, not a confirmed pass (gt-whvu).
	if result.Status != StatusSkipped {
		t.Errorf("expected StatusSkipped (redirect rig excluded, nothing verified), got %v: %s\nDetails: %v",
			result.Status, result.Message, result.Details)
	}
	if len(check.mismatches) != 0 {
		t.Errorf("expected 0 mismatches, got %d: %+v", len(check.mismatches), check.mismatches)
	}
}

func TestDatabasePrefixCheck_DetectsMismatchForOwnDB(t *testing.T) {
	// A rig with its own .beads (no redirect) that has a wrong prefix
	// should still be detected as a mismatch.

	tmpDir := t.TempDir()

	townBeads := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(townBeads, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix":"hq-","path":"."}
{"prefix":"mm-","path":"mission_manager"}`
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	// mission_manager has its own .beads (no redirect)
	mmBeads := filepath.Join(tmpDir, "mission_manager", ".beads")
	if err := os.MkdirAll(mmBeads, 0755); err != nil {
		t.Fatal(err)
	}

	mock := &mockDBPrefixGetter{
		prefixes: map[string]string{
			filepath.Join(tmpDir, "mission_manager"): "wrong",
		},
	}

	check := NewDatabasePrefixCheck()
	check.prefixGetter = mock
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning for prefix mismatch, got %v: %s", result.Status, result.Message)
	}
	if len(check.mismatches) != 1 {
		t.Fatalf("expected 1 mismatch, got %d", len(check.mismatches))
	}
	m := check.mismatches[0]
	if m.routesPrefix != "mm" || m.dbPrefix != "wrong" {
		t.Errorf("unexpected mismatch data: routes=%q db=%q", m.routesPrefix, m.dbPrefix)
	}
}

func TestDatabasePrefixCheck_UsesMetadataDatabaseEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake bd stub is shell-specific")
	}

	tmpDir := t.TempDir()
	townBeads := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(townBeads, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte("{\"prefix\":\"gt-\",\"path\":\"gastown/mayor/rig\"}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(tmpDir, "gastown", "mayor", "rig")
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"dolt_mode":"server","dolt_database":"gastown"}`), 0644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	script := `#!/usr/bin/env bash
if [ "$1 $2 $3" = "config get issue_prefix" ]; then
  if [ "$BEADS_DIR" = "$EXPECT_BEADS_DIR" ] && [ "$BEADS_DOLT_SERVER_DATABASE" = "gastown" ]; then
    printf 'gt\n'
  else
    printf 'hq\n'
  fi
  exit 0
fi
exit 1
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("EXPECT_BEADS_DIR", beadsDir)
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "stale")
	t.Setenv("BEADS_DIR", filepath.Join(tmpDir, "wrong", ".beads"))

	check := NewDatabasePrefixCheck()
	result := check.Run(&CheckContext{TownRoot: tmpDir})
	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK with metadata-selected database, got %v: %s details=%v", result.Status, result.Message, result.Details)
	}
}

// TestDatabasePrefixCheck_FixUsesMetadataDatabase verifies Fix resolves the
// Dolt database name from metadata.json (not the rig name or ambient env) and
// hands it to the prefix setter. The setter (doltserver.SetRigIssuePrefix)
// owns env scoping now that Fix no longer shells out to bd — bd 1.2+ refuses
// `bd config set issue_prefix` (gt-8po).
func TestDatabasePrefixCheck_FixUsesMetadataDatabase(t *testing.T) {
	tmpDir := t.TempDir()
	rigPath := filepath.Join(tmpDir, "gastown", "mayor", "rig")
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"dolt_mode":"server","dolt_database":"gastown"}`), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "stale")
	t.Setenv("BEADS_DIR", filepath.Join(tmpDir, "wrong", ".beads"))

	var gotDatabase, gotBeadsDir string
	check := NewDatabasePrefixCheck()
	check.mismatches = []databasePrefixMismatch{{rigPath: "gastown/mayor/rig", routesPrefix: "gt", dbPrefix: "hq"}}
	check.prefixSetter = func(_, beadsDir, database, _ string) error {
		gotBeadsDir = beadsDir
		gotDatabase = database
		return nil
	}
	if err := check.Fix(&CheckContext{TownRoot: tmpDir}); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	if gotDatabase != "gastown" {
		t.Fatalf("database = %q, want %q (from metadata.json, not env %q)", gotDatabase, "gastown", "stale")
	}
	if gotBeadsDir != beadsDir {
		t.Fatalf("beadsDir = %q, want %q (not ambient BEADS_DIR)", gotBeadsDir, beadsDir)
	}
}

func TestDatabasePrefixCheck_MultipleRedirectsSameDB(t *testing.T) {
	// Multiple rigs all redirect to the town DB. None should be flagged.

	tmpDir := t.TempDir()

	townBeads := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(townBeads, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix":"hq-","path":"."}
{"prefix":"sm-","path":"site_manager"}
{"prefix":"cr-","path":"camera_relay"}
{"prefix":"au-","path":"autostart"}`
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	// All three rigs redirect to town root
	for _, rig := range []string{"site_manager", "camera_relay", "autostart"} {
		rigBeads := filepath.Join(tmpDir, rig, ".beads")
		if err := os.MkdirAll(rigBeads, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rigBeads, "redirect"), []byte("../.beads\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	mock := &mockDBPrefixGetter{
		prefixes: map[string]string{
			filepath.Join(tmpDir, "site_manager"): "hq",
			filepath.Join(tmpDir, "camera_relay"): "hq",
			filepath.Join(tmpDir, "autostart"):    "hq",
		},
	}

	check := NewDatabasePrefixCheck()
	check.prefixGetter = mock
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	// All three routes redirect to the shared town DB, so zero prefix
	// comparisons actually happen — that must surface as skipped, not as a
	// verified pass (gt-whvu).
	if result.Status != StatusSkipped {
		t.Errorf("expected StatusSkipped (all redirect rigs skipped, nothing verified), got %v: %s\nDetails: %v",
			result.Status, result.Message, result.Details)
	}
}

func TestDatabasePrefixCheck_MixedOwnAndRedirect(t *testing.T) {
	// Mix of rigs: some redirect to town DB (should be skipped), one has
	// its own DB with a wrong prefix (should be flagged).

	tmpDir := t.TempDir()

	townBeads := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(townBeads, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix":"hq-","path":"."}
{"prefix":"sm-","path":"site_manager"}
{"prefix":"mm-","path":"mission_manager"}`
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	// site_manager redirects to town DB
	smBeads := filepath.Join(tmpDir, "site_manager", ".beads")
	if err := os.MkdirAll(smBeads, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(smBeads, "redirect"), []byte("../.beads\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// mission_manager has its own DB (no redirect) with wrong prefix
	mmBeads := filepath.Join(tmpDir, "mission_manager", ".beads")
	if err := os.MkdirAll(mmBeads, 0755); err != nil {
		t.Fatal(err)
	}

	mock := &mockDBPrefixGetter{
		prefixes: map[string]string{
			filepath.Join(tmpDir, "site_manager"):    "hq",
			filepath.Join(tmpDir, "mission_manager"): "wrong",
		},
	}

	check := NewDatabasePrefixCheck()
	check.prefixGetter = mock
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning, got %v: %s", result.Status, result.Message)
	}
	if len(check.mismatches) != 1 {
		t.Fatalf("expected 1 mismatch (only mission_manager), got %d: %+v",
			len(check.mismatches), check.mismatches)
	}
	if check.mismatches[0].rigPath != "mission_manager" {
		t.Errorf("expected mismatch for mission_manager, got %s", check.mismatches[0].rigPath)
	}
}

// TestDatabasePrefixCheck_FixUsesStoreSetter verifies Fix persists the
// routes.jsonl prefix through the injected setter (production: the SDK store —
// bd 1.2+ refuses `bd config set issue_prefix`, and legacy databases with
// issue_prefix unset block every bd create in that rig; gt-8po).
func TestDatabasePrefixCheck_FixUsesStoreSetter(t *testing.T) {
	tmpDir := t.TempDir()

	// The beads rig on upgraded towns: DB named "beads", route prefix "be".
	rigBeads := filepath.Join(tmpDir, "beads", "mayor", "rig", ".beads")
	if err := os.MkdirAll(rigBeads, 0755); err != nil {
		t.Fatal(err)
	}
	metadata := `{"backend":"dolt","dolt_database":"beads","dolt_mode":"server"}`
	if err := os.WriteFile(filepath.Join(rigBeads, "metadata.json"), []byte(metadata), 0644); err != nil {
		t.Fatal(err)
	}

	type call struct {
		townRoot, beadsDir, database, prefix string
	}
	var calls []call

	check := NewDatabasePrefixCheck()
	check.mismatches = []databasePrefixMismatch{
		{rigPath: "beads/mayor/rig", routesPrefix: "be", dbPrefix: "issue_prefix (not set)"},
	}
	check.prefixSetter = func(townRoot, beadsDir, database, prefix string) error {
		calls = append(calls, call{townRoot, beadsDir, database, prefix})
		return nil
	}

	ctx := &CheckContext{TownRoot: tmpDir}
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() returned error: %v", err)
	}

	if len(calls) != 1 {
		t.Fatalf("expected 1 setter call, got %d: %+v", len(calls), calls)
	}
	got := calls[0]
	if got.townRoot != tmpDir {
		t.Errorf("townRoot = %q, want %q", got.townRoot, tmpDir)
	}
	if got.beadsDir != rigBeads {
		t.Errorf("beadsDir = %q, want %q", got.beadsDir, rigBeads)
	}
	// Database name must come from metadata.json (dolt_database), NOT the rig
	// name — they differ on upgraded towns.
	if got.database != "beads" {
		t.Errorf("database = %q, want %q (from metadata.json dolt_database)", got.database, "beads")
	}
	if got.prefix != "be" {
		t.Errorf("prefix = %q, want %q", got.prefix, "be")
	}
}

// TestDatabasePrefixCheck_FixFallsBackToRigNameDatabase verifies the database
// name falls back to the rig path's first component when metadata.json is
// absent.
func TestDatabasePrefixCheck_FixFallsBackToRigNameDatabase(t *testing.T) {
	tmpDir := t.TempDir()
	rigBeads := filepath.Join(tmpDir, "gastown", "mayor", "rig", ".beads")
	if err := os.MkdirAll(rigBeads, 0755); err != nil {
		t.Fatal(err)
	}

	var gotDatabase string
	check := NewDatabasePrefixCheck()
	check.mismatches = []databasePrefixMismatch{
		{rigPath: "gastown/mayor/rig", routesPrefix: "gt", dbPrefix: "wrong"},
	}
	check.prefixSetter = func(_, _, database, _ string) error {
		gotDatabase = database
		return nil
	}

	if err := check.Fix(&CheckContext{TownRoot: tmpDir}); err != nil {
		t.Fatalf("Fix() returned error: %v", err)
	}
	if gotDatabase != "gastown" {
		t.Errorf("database = %q, want fallback %q", gotDatabase, "gastown")
	}
}
