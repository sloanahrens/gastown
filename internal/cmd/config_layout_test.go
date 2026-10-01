package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

func fiveFileCmdTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	for rel, body := range map[string]string{
		"mayor/town.json":   `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z"}`,
		"mayor/rigs.json":   `{"version":1,"rigs":{"r":{"git_url":"x","added_at":"2026-01-01T00:00:00Z","beads":{"repo":"","prefix":"r"}}}}`,
		"mayor/daemon.json": `{"type":"daemon-patrol-config","version":1,"patrols":{}}`,
	} {
		path := filepath.Join(town, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return town
}

func TestConfigMigrateDryRunThenOnce(t *testing.T) {
	t.Parallel()
	town := fiveFileCmdTown(t)
	var out bytes.Buffer
	e := townConfigCmdEnv(town, &out)
	if err := configMigrate(e, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "move mayor/rigs.json into mayor/town.json") {
		t.Errorf("dry-run output:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(town, "mayor", "rigs.json")); err != nil {
		t.Fatalf("dry run removed rigs.json: %v", err)
	}
	if err := configMigrate(e, false); err != nil {
		t.Fatal(err)
	}
	if err := configMigrate(e, false); !errors.Is(err, config.ErrAlreadyMigrated) {
		t.Fatalf("second migrate = %v, want ErrAlreadyMigrated", err)
	}
	out.Reset()
	if err := configValidate(e); err != nil || !strings.Contains(out.String(), "two-file layout") {
		t.Fatalf("validate = %v:\n%s", err, out.String())
	}
}

func TestConfigValidateNamesTheBrokenFile(t *testing.T) {
	t.Parallel()
	town := fiveFileCmdTown(t)
	var out bytes.Buffer
	e := townConfigCmdEnv(town, &out)
	if err := configValidate(e); err != nil || !strings.Contains(out.String(), "five-file layout") {
		t.Fatalf("validate = %v:\n%s", err, out.String())
	}
	daemonPath := filepath.Join(town, "mayor", "daemon.json")
	if err := os.WriteFile(daemonPath, []byte(`{"type":"daemon-patrol-config","bogus":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := configValidate(e); err == nil || !strings.Contains(err.Error(), daemonPath) {
		t.Fatalf("validate over a broken daemon.json = %v", err)
	}
	if err := configMigrate(e, false); err == nil {
		t.Fatal("migrate over a broken daemon.json succeeded")
	}
}
