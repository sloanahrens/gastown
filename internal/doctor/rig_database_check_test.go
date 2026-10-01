package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

func TestRigDatabaseCheckWarnsFixesThenFlagsDrift(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	for rel, body := range map[string]string{
		"mayor/town.json":                        `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z"}`,
		"mayor/rigs.json":                        `{"version":1,"rigs":{"gastown":{"git_url":"x","added_at":"2026-01-01T00:00:00Z"},"bare":{"git_url":"y","added_at":"2026-01-01T00:00:00Z"}}}`,
		"gastown/mayor/rig/.beads/metadata.json": `{"dolt_mode":"server","dolt_database":"gt"}`,
	} {
		path := filepath.Join(town, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	check := NewRigDatabaseCheck()
	ctx := &CheckContext{TownRoot: town}
	if r := check.Run(ctx); r.Status != StatusWarning || len(r.Details) != 1 {
		t.Fatalf("unrecorded town = %+v, want a warning for gastown", r)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatal(err)
	}
	rc, err := config.LoadRigsConfig(filepath.Join(town, "mayor", "rigs.json"))
	if err != nil || rc.Rigs["gastown"].DoltDatabase != "gt" || rc.Rigs["bare"].DoltDatabase != "" {
		t.Fatalf("registry after fix = %+v, %v", rc, err)
	}
	if r := check.Run(ctx); r.Status != StatusOK {
		t.Fatalf("after fix = %+v, want OK", r)
	}
	meta := filepath.Join(town, "gastown", "mayor", "rig", ".beads", "metadata.json")
	if err := os.WriteFile(meta, []byte(`{"dolt_mode":"server","dolt_database":"gastown"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := check.Run(ctx); r.Status != StatusError || len(r.Details) != 1 {
		t.Fatalf("drifted = %+v, want an error", r)
	}
}
