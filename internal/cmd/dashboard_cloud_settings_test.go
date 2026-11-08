package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The hub reads the cloud step's source from the town settings, so a settings
// file that does not parse stops it before any pane is wired.
func TestNewDashboardHubRefusesUnreadableSettings(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "settings", "config.json"), []byte(`{"cloud":`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := newDashboardHub(townRoot, time.Now(), time.UTC, "", nil, 0, 1, "", io.Discard)
	if err == nil {
		t.Fatal("a settings file that does not parse built the hub")
	}
	if !strings.Contains(err.Error(), "town settings") {
		t.Errorf("err = %v, want the settings named", err)
	}
}
