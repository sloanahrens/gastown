package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// TestOverseerDetectionKeepsAFailedCommandsError: a detection command that
// fails on its own, with no deadline involved, is reported as the failure it
// was — the deadline mapping only applies to a call the deadline ended
// (gt-ddp7c).
func TestOverseerDetectionKeepsAFailedCommandsError(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		//testpolicy:allow no-skip — /bin/sh is the failing child; Windows has none
		t.Skip("the failing child is a /bin/sh process")
	}
	_, err := runDetectionCommand(context.Background(), "", "/bin/sh", "-c", "exit 3")
	if err == nil {
		t.Fatal("runDetectionCommand(exit 3) = nil error, want the child's failure")
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err = %v, want the child's own exit status", err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the child's own failure, not a context error", err)
	}
}

// TestOverseerDetectionCommandsAreTimeBounded: detection commands run inside
// read-only paths, so a wedged one must not outlive its deadline (gt-ptysu).
// The child records its pid and stops itself before the deadline, so the
// proof is that the call returned the deadline error and the pid is gone —
// not that the call was quick on a quiet host (gt-ddp7c).
func TestOverseerDetectionCommandsAreTimeBounded(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		//testpolicy:allow no-skip — /bin/sh is the wedged child; Windows has none
		t.Skip("the wedged child is a /bin/sh process")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	// The deadline only has to outlast the child's own start, so it is a
	// second, not the detection timeout runOverseerDetection installs.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := runDetectionCommand(ctx, "", "/bin/sh", "-c", "echo $$ > "+pidFile+"; kill -STOP $$")
		done <- err
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(time.Minute): //testpolicy:allow no-sleep — a hang guard only: the deadline the call must enforce is this test's subject, so this fails a helper that never returns at all
		t.Fatal("runDetectionCommand never returned: the wedged command was not killed at the deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runDetectionCommand = %v, want the deadline error", err)
	}
	pidText, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("the wedged child never recorded its pid: %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(pidText)))
	if convErr != nil {
		t.Fatalf("the wedged child wrote pid %q: %v", pidText, convErr)
	}
	// A stopped process still answers signal 0; only a kill makes it answer
	// ESRCH, so this is the deadline having killed the child rather than the
	// call returning beside it.
	if sigErr := syscall.Kill(pid, 0); !errors.Is(sigErr, syscall.ESRCH) {
		t.Fatalf("the wedged child %d was not killed at the deadline (signal 0 = %v)", pid, sigErr)
	}
}
