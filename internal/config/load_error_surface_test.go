package config

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// TestLoadOperationalConfigCheckedReportsParseError: a settings file that does
// not parse must reach a caller that can act on it, not revert every
// operational threshold — slot caps, Dolt thresholds, the respawn cap, nudge
// settings — to its compiled-in default in silence (gt-ptysu).
func TestLoadOperationalConfigCheckedReportsParseError(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeFile(t, TownSettingsPath(townRoot), `{"type":"town-settings","version":1,"zz_unknown":1}`)

	cfg, err := LoadOperationalConfigChecked(townRoot)
	if !errors.Is(err, ErrUnparseable) {
		t.Fatalf("LoadOperationalConfigChecked = %v, want the parse error", err)
	}
	if cfg == nil {
		t.Fatal("LoadOperationalConfigChecked returned nil")
	}
	if got := cfg.GetNudgeConfig().MaxQueueDepthV(); got != DefaultNudgeMaxQueueDepth {
		t.Errorf("MaxQueueDepthV = %d, want the compiled-in default %d", got, DefaultNudgeMaxQueueDepth)
	}
}

// TestLoadOperationalConfigCheckedDefaultsWhenAbsent: an absent settings file
// is the documented fallback, not an error.
func TestLoadOperationalConfigCheckedDefaultsWhenAbsent(t *testing.T) {
	t.Parallel()
	cfg, err := LoadOperationalConfigChecked(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOperationalConfigChecked(absent) = %v, want nil", err)
	}
	if cfg == nil {
		t.Fatal("LoadOperationalConfigChecked returned nil")
	}
}

// TestOverseerDetectionKeepsAnUnparseableFile: gt status and gt install both
// reach detection, so a hand-edited overseer.json must survive them rather
// than be replaced by whatever git or gh reports (gt-ptysu).
func TestOverseerDetectionKeepsAnUnparseableFile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		call func(townRoot string) error
	}{
		{"LoadOrDetectOverseer", func(r string) error { _, err := LoadOrDetectOverseer(r); return err }},
		{"DetectOverseer", func(r string) error { _, err := DetectOverseer(r); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			townRoot := t.TempDir()
			path := OverseerConfigPath(townRoot)
			handEdited := `{"type":"overseer","version":1,"name":"Operator","note":"hand-edited"}`
			writeFile(t, path, handEdited)

			if err := tc.call(townRoot); err == nil {
				t.Fatal("= nil error, want the parse error")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != handEdited {
				t.Errorf("file = %q, want it left untouched", after)
			}
		})
	}
}

// TestOverseerDetectionCommandsAreTimeBounded: detection commands run inside
// read-only paths, so a wedged one must not outlive its deadline (gt-ptysu).
// The 100ms deadline stands in for overseerDetectTimeout, which
// runOverseerDetection installs, so the proof costs 100ms rather than 5s.
func TestOverseerDetectionCommandsAreTimeBounded(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := runDetectionCommand(ctx, "", "/bin/sleep", "3"); err == nil {
		t.Fatal("runDetectionCommand(ctx, sleep 3) = nil error, want the deadline to cut it")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v, want it cut off at the 100ms deadline", elapsed)
	}
}
