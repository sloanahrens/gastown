package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/daemon"
)

// TestMetricsHourWeeklyRefusesAnUnreadablePatrolConfig: the weekly step reads
// story_points.auto_adopt from the patrol config before it runs. A config that does
// not parse fails the hour before any step, so the setting is never read as off or
// on and nothing is written.
func TestMetricsHourWeeklyRefusesAnUnreadablePatrolConfig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := daemon.PatrolConfigFile(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write patrol config: %v", err)
	}

	var out strings.Builder
	err := hostMetricsHourSteps().weekly(context.Background(), &out, town)
	if err == nil || !strings.Contains(err.Error(), "read story_points.auto_adopt") {
		t.Fatalf("weekly err = %v, want the unreadable patrol config named", err)
	}
	if out.Len() != 0 {
		t.Errorf("out = %q, want nothing written before the refusal", out.String())
	}
}
