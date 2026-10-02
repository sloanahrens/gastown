package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

func TestConfigLayoutCheckWarnsUntilMigrated(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"town.json": `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z"}`,
		"rigs.json": `{"version":1,"rigs":{}}`,
	} {
		if err := os.WriteFile(filepath.Join(town, "mayor", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	check := NewConfigLayoutCheck()
	if r := check.Run(&CheckContext{TownRoot: town}); r.Status != StatusWarning || len(r.Details) != 1 {
		t.Fatalf("five-file town = %+v, want a warning listing rigs.json", r)
	}
	if _, err := config.MigrateLayout(town); err != nil {
		t.Fatal(err)
	}
	if r := check.Run(&CheckContext{TownRoot: town}); r.Status != StatusOK {
		t.Fatalf("two-file town = %+v, want OK", r)
	}
}

// TestRegistryChecksReadTheTwoFileLayout: the registry checks find the rig
// registry in mayor/town.json once the town is migrated, and never write a
// mayor/rigs.json back.
func TestRegistryChecksReadTheTwoFileLayout(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"town.json":   `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z"}`,
		"rigs.json":   `{"version":1,"rigs":{"gone":{"git_url":"x","added_at":"2026-01-01T00:00:00Z"}}}`,
		"daemon.json": `{"type":"daemon-patrol-config","version":1,"heartbeat":{"enabled":true}}`,
	} {
		if err := os.WriteFile(filepath.Join(town, "mayor", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := config.MigrateLayout(town); err != nil {
		t.Fatal(err)
	}
	ctx := &CheckContext{TownRoot: town}
	// The message must name the file the registry was read from
	// (mayor/town.json), not the retired mayor/rigs.json.
	if r := NewRigsRegistryExistsCheck().Run(ctx); r.Status != StatusOK || !strings.Contains(r.Message, "mayor/town.json") {
		t.Errorf("rigs-registry-exists = %+v, want OK naming mayor/town.json", r)
	}
	// The fixture's only rig carries no beads prefix, so the registry reads
	// from mayor/town.json but registers nothing: Warning, not Error.
	rj := NewRigsJSONCheck()
	if r := rj.Run(ctx); r.Status != StatusWarning || rj.CanFix() {
		t.Errorf("rigs-json = %+v, CanFix %v", r, rj.CanFix())
	}
	if r := NewPatrolHooksWiredCheck().Run(ctx); r.Status != StatusOK {
		t.Errorf("patrol-hooks-wired = %+v", r)
	}
	valid := NewRigsRegistryValidCheck()
	if r := valid.Run(ctx); r.Status != StatusWarning {
		t.Fatalf("rigs-registry-valid with a missing rig dir = %+v", r)
	}
	if err := valid.Fix(ctx); err != nil {
		t.Fatal(err)
	}
	if rc, err := config.LoadRigsConfig(filepath.Join(town, "mayor", "rigs.json")); err != nil || len(rc.Rigs) != 0 {
		t.Errorf("registry after fix = %+v, %v", rc, err)
	}
	if _, err := os.Stat(filepath.Join(town, "mayor", "rigs.json")); !os.IsNotExist(err) {
		t.Errorf("the fix wrote mayor/rigs.json (%v)", err)
	}
}
