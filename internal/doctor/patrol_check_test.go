package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// writeRigsJSON creates a mayor/rigs.json with a single rig entry.
func writeRigsJSON(t *testing.T, townRoot, rigName string) {
	t.Helper()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatalf("MkdirAll mayor: %v", err)
	}
	rigsConfig := config.RigsConfig{
		Version: 1,
		Rigs: map[string]config.RigEntry{
			rigName: {
				GitURL:  "https://github.com/test/" + rigName,
				AddedAt: time.Now(),
			},
		},
	}
	data, err := json.Marshal(rigsConfig)
	if err != nil {
		t.Fatalf("json.Marshal rigs: %v", err)
	}
	rigsPath := filepath.Join(mayorDir, "rigs.json")
	if err := os.WriteFile(rigsPath, data, 0644); err != nil {
		t.Fatalf("WriteFile rigs.json: %v", err)
	}
}

func TestNewPatrolHooksWiredCheck(t *testing.T) {
	t.Parallel()
	check := NewPatrolHooksWiredCheck()
	if check == nil {
		t.Fatal("NewPatrolHooksWiredCheck() returned nil")
	}
	if check.Name() != "patrol-hooks-wired" {
		t.Errorf("Name() = %q, want %q", check.Name(), "patrol-hooks-wired")
	}
	if !check.CanFix() {
		t.Error("CanFix() should return true")
	}
}

func TestPatrolHooksWiredCheck_NoDaemonConfig(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	mayorDir := filepath.Join(tmpDir, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}

	check := NewPatrolHooksWiredCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("Status = %v, want Warning", result.Status)
	}
	if result.FixHint == "" {
		t.Error("FixHint should not be empty")
	}
}

func TestPatrolHooksWiredCheck_ValidConfig(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	cfg := config.NewDaemonPatrolConfig()
	path := config.DaemonPatrolConfigPath(tmpDir)
	if err := config.SaveDaemonPatrolConfig(path, cfg); err != nil {
		t.Fatalf("SaveDaemonPatrolConfig: %v", err)
	}

	check := NewPatrolHooksWiredCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want OK", result.Status)
	}
}

func TestPatrolHooksWiredCheck_EmptyPatrols(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	cfg := &config.DaemonPatrolConfig{
		Type:    "daemon-patrol-config",
		Version: 1,
		Patrols: &config.PatrolsConfig{},
	}
	path := config.DaemonPatrolConfigPath(tmpDir)
	if err := config.SaveDaemonPatrolConfig(path, cfg); err != nil {
		t.Fatalf("SaveDaemonPatrolConfig: %v", err)
	}

	check := NewPatrolHooksWiredCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("Status = %v, want Warning (no patrols configured)", result.Status)
	}
}

func TestPatrolHooksWiredCheck_HeartbeatEnabled(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	cfg := &config.DaemonPatrolConfig{
		Type:    "daemon-patrol-config",
		Version: 1,
		Heartbeat: &config.PatrolConfig{
			Enabled:  true,
			Interval: "3m",
		},
		Patrols: &config.PatrolsConfig{},
	}
	path := config.DaemonPatrolConfigPath(tmpDir)
	if err := config.SaveDaemonPatrolConfig(path, cfg); err != nil {
		t.Fatalf("SaveDaemonPatrolConfig: %v", err)
	}

	check := NewPatrolHooksWiredCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want OK (heartbeat enabled triggers patrols)", result.Status)
	}
}

func TestPatrolHooksWiredCheck_Fix(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	mayorDir := filepath.Join(tmpDir, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}

	check := NewPatrolHooksWiredCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("Initial Status = %v, want Warning", result.Status)
	}

	err := check.Fix(ctx)
	if err != nil {
		t.Fatalf("Fix() error = %v", err)
	}

	path := config.DaemonPatrolConfigPath(tmpDir)
	loaded, err := config.LoadDaemonPatrolConfig(path)
	if err != nil {
		t.Fatalf("LoadDaemonPatrolConfig: %v", err)
	}
	if loaded.Type != "daemon-patrol-config" {
		t.Errorf("Type = %q, want 'daemon-patrol-config'", loaded.Type)
	}
	if loaded.Patrols.Count() != 1 || loaded.Patrols.PatrolScan == nil {
		t.Errorf("Patrols count = %d, want 1 (patrol_scan)", loaded.Patrols.Count())
	}

	result = check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("After Fix(), Status = %v, want OK", result.Status)
	}
}

func TestPatrolHooksWiredCheck_FixPreservesExisting(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	existing := &config.DaemonPatrolConfig{
		Type:    "daemon-patrol-config",
		Version: 1,
		Patrols: &config.PatrolsConfig{
			Handler: &config.PatrolConfig{Enabled: true, Agent: "custom-agent"},
		},
	}
	path := config.DaemonPatrolConfigPath(tmpDir)
	if err := config.SaveDaemonPatrolConfig(path, existing); err != nil {
		t.Fatalf("SaveDaemonPatrolConfig: %v", err)
	}

	check := NewPatrolHooksWiredCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("Status = %v, want OK (has patrols)", result.Status)
	}

	err := check.Fix(ctx)
	if err != nil {
		t.Fatalf("Fix() error = %v", err)
	}

	loaded, err := config.LoadDaemonPatrolConfig(path)
	if err != nil {
		t.Fatalf("LoadDaemonPatrolConfig: %v", err)
	}
	if loaded.Patrols.Count() != 1 {
		t.Errorf("Patrols count = %d, want 1 (should preserve existing)", loaded.Patrols.Count())
	}
	if loaded.Patrols.RolePatrol("handler") == nil {
		t.Error("existing custom patrol was overwritten")
	}
}

func TestCheckStuckWispsDolt_ErrorOnMissingBd(t *testing.T) {
	t.Parallel()
	// When bd sql fails, checkStuckWispsDolt returns the error: with
	// Dolt-only mode there is no JSONL fallback.
	bd := newFakeBD()
	bd.db("/nonexistent/rig/path").OnSQL(func(string) ([][]string, error) { return nil, errors.New("no beads database found") })
	check := NewPatrolNotStuckCheck()
	if _, err := check.checkStuckWispsDolt(bd.ctx(t.TempDir()), "/nonexistent/rig/path", "testrig"); err == nil {
		t.Error("expected error when bd sql fails")
	}
}

func TestPatrolNotStuckCheck_Run_DoltFailureReportsError(t *testing.T) {
	t.Parallel()
	// When Dolt fails for a rig, the check reports the error in details
	// rather than silently returning OK.
	tmpDir := t.TempDir()

	// Create rigs.json
	mayorDir := filepath.Join(tmpDir, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	rigsConfig := config.RigsConfig{
		Rigs: map[string]config.RigEntry{
			"testrig": {},
		},
	}
	rigsData, _ := json.Marshal(rigsConfig)
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), rigsData, 0644); err != nil {
		t.Fatalf("write rigs.json: %v", err)
	}
	rigDir := filepath.Join(tmpDir, "testrig")
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}

	bd := newFakeBD()
	bd.db(rigDir).OnSQL(func(string) ([][]string, error) { return nil, errors.New("no beads database found") })
	result := NewPatrolNotStuckCheck().Run(bd.ctx(tmpDir))
	if result.Status != StatusWarning || len(result.Details) != 1 ||
		!strings.Contains(result.Details[0], "testrig: Dolt query failed: no beads database found") {
		t.Errorf("result = %v %q %q, want a warning naming the failed rig", result.Status, result.Message, result.Details)
	}
}

// Suppress unused import warning for fmt (used in test output formatting).
var _ = fmt.Sprintf

// writePluginFixture creates a minimal plugin directory (dir/name/plugin.md)
// with the given content, so it is recognized as a plugin by hasPlugins/
// DetectDrift.
func writePluginFixture(t *testing.T, dir, name, content string) {
	t.Helper()
	pluginDir := filepath.Join(dir, name)
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", pluginDir, err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.md"), []byte(content), 0644); err != nil {
		t.Fatalf("write plugin.md: %v", err)
	}
}

func TestPatrolPluginDriftCheck_SourceNotFound_ReturnsWarningNotOK(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir() // No gastown checkout anywhere under here.

	check := NewPatrolPluginDriftCheck()
	ctx := &CheckContext{TownRoot: townRoot}
	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("Status = %v, want StatusWarning when source cannot be located (must never be OK/skip)", result.Status)
	}
	if result.Message != "cannot verify plugin drift" {
		t.Errorf("Message = %q, want %q", result.Message, "cannot verify plugin drift")
	}
}

func TestPatrolPluginDriftCheck_DriftedPlugin_WarningNamesFile(t *testing.T) {
	t.Chdir(t.TempDir())

	townRoot := t.TempDir()
	sourceDir := filepath.Join(townRoot, "gastown", "mayor", "rig", "plugins")
	targetDir := filepath.Join(townRoot, "plugins")

	writePluginFixture(t, sourceDir, "my-plugin", "+++\nname = \"my-plugin\"\n+++\nnew body")
	writePluginFixture(t, targetDir, "my-plugin", "+++\nname = \"my-plugin\"\n+++\nold stale body")

	check := NewPatrolPluginDriftCheck()
	ctx := &CheckContext{TownRoot: townRoot}
	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning for drifted plugin", result.Status)
	}
	found := false
	for _, d := range result.Details {
		if strings.Contains(d, "my-plugin") {
			found = true
		}
	}
	if !found {
		t.Errorf("Details = %v, want an entry naming the drifted plugin %q", result.Details, "my-plugin")
	}
}

func TestPatrolPluginDriftCheck_InSync_ReturnsOK(t *testing.T) {
	t.Chdir(t.TempDir())

	townRoot := t.TempDir()
	sourceDir := filepath.Join(townRoot, "gastown", "mayor", "rig", "plugins")
	targetDir := filepath.Join(townRoot, "plugins")

	content := "+++\nname = \"stable\"\n+++\nsame body"
	writePluginFixture(t, sourceDir, "stable", content)
	writePluginFixture(t, targetDir, "stable", content)

	check := NewPatrolPluginDriftCheck()
	ctx := &CheckContext{TownRoot: townRoot}
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want StatusOK when runtime matches source", result.Status)
	}
}
