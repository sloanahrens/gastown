//go:build integration

package shelltier

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIntegrationFailedScriptLogsCarriesTheScriptsVerdict runs a failing script
// through the real runner and requires the failure text to hold the script's
// own output, so a red in the sweep names the check that failed.
func TestIntegrationFailedScriptLogsCarriesTheScriptsVerdict(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fail_test.sh"),
		[]byte("echo 'boom: a check failed'\nexit 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	logDir := t.TempDir()
	result := Run(t.Context(), Config{
		Entries:     entriesAt("fail_test.sh"),
		Root:        root,
		LogDir:      logDir,
		Budget:      time.Minute,
		Exec:        OSExec,
		Now:         time.Now,
		WithTimeout: context.WithTimeout,
	})
	if result.Failed != 1 {
		t.Fatalf("failed = %d, want 1", result.Failed)
	}
	got := failedScriptLogs(t, logDir, result.FailedNames)
	if !strings.Contains(got, "boom: a check failed") {
		t.Fatalf("failed script logs = %q, want the script's own output", got)
	}
}
