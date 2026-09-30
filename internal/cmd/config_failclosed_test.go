package cmd

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// TestDaemonJSONSettersRefuseAnUnparseableFile: `gt config set` on a
// lifecycle, maintenance or dolt key used to start from defaults when
// daemon.json did not parse and save them over the operator's file (G3-03).
func TestDaemonJSONSettersRefuseAnUnparseableFile(t *testing.T) {
	t.Parallel()
	const broken = "{\"patrols\": {\"witness\": {\"enabled\": false},}}"
	for name, set := range map[string]func(town string) error{
		"lifecycle":   func(town string) error { return setLifecycleConfig(town, "lifecycle.reaper.enabled", "true") },
		"maintenance": func(town string) error { return setMaintenanceConfig(town, "maintenance.window", "03:00") },
		"get lifecycle": func(town string) error {
			return getLifecycleConfig(town, "lifecycle.reaper.enabled")
		},
		"get maintenance": func(town string) error {
			return getMaintenanceConfig(io.Discard, town, "maintenance.window")
		},
	} {
		town := t.TempDir()
		path := filepath.Join(town, "mayor", "daemon.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := set(town); !errors.Is(err, config.ErrUnparseable) {
			t.Errorf("%s on a broken daemon.json = %v, want ErrUnparseable", name, err)
		}
		if got, _ := os.ReadFile(path); string(got) != broken {
			t.Errorf("%s rewrote the broken daemon.json: %q", name, got)
		}
	}
}
