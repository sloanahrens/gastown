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
// read-only paths, so a wedged one must not outlive its context (gt-ptysu). No
// deadline is timed here: the wedged child records its pid and stops itself,
// the test cancels the context only once that pid is on disk, and the proof is
// that the call returned the context's error and the child is gone, however
// slow the host is to start it (gt-ddp7c).
func TestOverseerDetectionCommandsAreTimeBounded(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		//testpolicy:allow no-skip — /bin/sh is the wedged child; Windows has none
		t.Skip("the wedged child is a /bin/sh process")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		// The pid file is the script's first argument, so no path is ever
		// spliced into the shell text.
		_, err := runDetectionCommand(ctx, "", "/bin/sh", "-c", `echo $$ > "$1"; kill -STOP $$`, "sh", pidFile)
		done <- err
	}()

	pid := waitForPid(t, pidFile, done)
	cancel()

	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second): //testpolicy:allow no-sleep — a hang guard only: it bounds a helper that never returns, so a broken kill fails in seconds, and it is not the behaviour under test
		t.Fatal("runDetectionCommand never returned: the wedged command was not killed when its context ended")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runDetectionCommand = %v, want the context's error", err)
	}
	// A stopped process still answers signal 0; only a kill makes it answer
	// ESRCH, so this is the cancellation having killed the child rather than
	// the call returning beside it.
	if sigErr := syscall.Kill(pid, 0); !errors.Is(sigErr, syscall.ESRCH) {
		t.Fatalf("the wedged child %d was not killed when its context ended (signal 0 = %v)", pid, sigErr)
	}
}

// waitForPid returns the pid the wedged child wrote once it has written a whole
// number, and fails the test if the call returned first or the child never
// recorded one. The wait is a condition, not a duration: it ends as soon as the
// child has run its first line, and the bound only stops a child that never
// starts.
func waitForPid(t *testing.T, pidFile string, done <-chan error) int {
	t.Helper()
	deadline := time.After(30 * time.Second) //testpolicy:allow no-sleep — a bound on a child that never starts; the wait itself ends on the pid file, not on this
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if text, err := os.ReadFile(pidFile); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(text))); convErr == nil && pid > 0 {
				return pid
			}
		}
		select {
		case err := <-done:
			t.Fatalf("runDetectionCommand returned (%v) before the wedged child recorded its pid", err)
		case <-deadline:
			t.Fatal("the wedged child never recorded its pid")
		case <-tick.C:
		}
	}
}

// TestOverseerDetectionReportsAnEndedDeadlineAsTheDeadline: a call whose
// context deadline has passed reports context.DeadlineExceeded, not the kill
// signal exec returns for the child, so a read-only path can tell a deadline
// from a failing command. The deadline is already in the past, so nothing is
// timed (gt-ddp7c).
func TestOverseerDetectionReportsAnEndedDeadlineAsTheDeadline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := runDetectionCommand(ctx, "", "git", "--version")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runDetectionCommand with an ended deadline = %v, want context.DeadlineExceeded", err)
	}
}
