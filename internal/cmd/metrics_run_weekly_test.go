package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/storypoints"
)

// TestMetricsHourWeeklyRefusesAnUnreadablePatrolConfig: on a due week the weekly step reads
// story_points.auto_adopt from the patrol config before it retrains. A config that does not
// parse fails the step before the retrain, so the setting is never read as off or on and
// nothing is written.
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
	if err == nil || !strings.Contains(err.Error(), "reading story_points.auto_adopt") {
		t.Fatalf("weekly err = %v, want the unreadable patrol config named", err)
	}
	if out.Len() != 0 {
		t.Errorf("out = %q, want nothing written before the refusal", out.String())
	}
}

// TestWeeklyAutoAdoptClosureReadsTheConfigLazily: the weekly closure hands the metrics package a
// reader of story_points.auto_adopt instead of reading the patrol config itself, so a config that
// cannot be read leaves an hour whose week is not due green and unread (gt-xv5fsp).
func TestWeeklyAutoAdoptClosureReadsTheConfigLazily(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := daemon.PatrolConfigFile(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write patrol config: %v", err)
	}
	// The week's last run was an hour ago, so the retrain is not due.
	stamp := filepath.Join(filepath.Dir(storypoints.RuntimeWeightsPath(town)), "weekly.stamp")
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(stamp, []byte(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		t.Fatalf("write stamp: %v", err)
	}

	var out strings.Builder
	if err := hostMetricsHourSteps().weekly(context.Background(), &out, town); err != nil {
		t.Fatalf("weekly err = %v, want a green hour on a week that is not due", err)
	}
	if !strings.Contains(out.String(), "not due") {
		t.Errorf("out = %q, want the not-due receipt", out.String())
	}
}

// TestWeeklyAutoAdoptReaderAnswersTheConfigsKey: the reader the closure hands the metrics package
// answers on for a patrol config that sets story_points.auto_adopt, off for an absent config, an
// absent block and a key left out, and an error for a config that does not parse (gt-xv5fsp).
func TestWeeklyAutoAdoptReaderAnswersTheConfigsKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, config string
		want         bool
		wantErr      bool
	}{
		{name: "key on", config: `{"patrols":{"spec_dispatch":{"story_points":{"auto_adopt":true}}}}`, want: true},
		{name: "key off", config: `{"patrols":{"spec_dispatch":{"story_points":{"auto_adopt":false}}}}`},
		{name: "key left out", config: `{"patrols":{"spec_dispatch":{"story_points":{}}}}`},
		{name: "no spec_dispatch block", config: `{"patrols":{}}`},
		{name: "no config file"},
		{name: "unreadable", config: `{not json`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.config != "" {
				path := daemon.PatrolConfigFile(town)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(path, []byte(tc.config), 0o644); err != nil {
					t.Fatalf("write patrol config: %v", err)
				}
			}
			got, err := weeklyAutoAdoptReader(town)()
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("reader = %v, %v; want %v, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
