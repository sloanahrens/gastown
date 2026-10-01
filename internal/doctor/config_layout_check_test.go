package doctor

import (
	"os"
	"path/filepath"
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
