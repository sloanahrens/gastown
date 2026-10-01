package cmd

// Shared file and config helpers for scheduler tests.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/scheduler/capacity"
)

// --- File helpers ---

// writeJSONFile marshals v as indented JSON and writes it to path,
// creating parent directories as needed.
func writeJSONFile(t *testing.T, path string, v interface{}) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal JSON for %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// --- Scheduler config helpers ---

// configureScheduler writes a TownSettings file with the given scheduler configuration.
// maxPolecats > 0 enables deferred dispatch; -1 means direct dispatch.
func configureScheduler(t *testing.T, hqPath string, maxPolecats, batchSize int) {
	t.Helper()
	settings := config.NewTownSettings()
	settings.Scheduler = &capacity.SchedulerConfig{
		MaxPolecats: &maxPolecats,
		BatchSize:   &batchSize,
	}
	writeJSONFile(t, config.TownSettingsPath(hqPath), settings)
}

// --- gt command helpers ---

// gtTestCmdTimeout bounds one gt subprocess. A gt that never exited used to
// hold the package's sequential test, and every parallel test parked behind
// it, until the go test timeout (gt-6ox58.1).
const gtTestCmdTimeout = 2 * time.Minute
