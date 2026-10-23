package daemon

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStopMarkerRoundTripIsSingleUse(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Now()
	if consumeStopRequested(town, 42, now) {
		t.Fatal("no marker written, but a stop was reported requested")
	}
	if err := writeStopRequested(town, 42, now); err != nil {
		t.Fatal(err)
	}
	if !consumeStopRequested(town, 42, now.Add(5*time.Second)) {
		t.Fatal("a fresh marker for this pid was not honoured")
	}
	if consumeStopRequested(town, 42, now) {
		t.Fatal("the marker was honoured twice")
	}
}

// A marker that does not belong to this stop must not excuse it (gt-swsqm):
// an unexplained SIGTERM after a failed gt stop has to exit 75.
func TestStopMarkerIgnoredWhenForeignOrExpired(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cases := []struct {
		name  string
		pid   int
		wrote time.Time
		read  time.Time
	}{
		{"other pid", 7, now, now},
		{"expired", 42, now.Add(-2 * time.Minute), now},
		{"written in the far future", 42, now.Add(2 * time.Minute), now},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if err := writeStopRequested(town, c.pid, c.wrote); err != nil {
				t.Fatal(err)
			}
			if consumeStopRequested(town, 42, c.read) {
				t.Fatal("a marker that is not for this stop was honoured")
			}
			if _, err := os.Stat(stopMarkerPath(town)); !os.IsNotExist(err) {
				t.Errorf("a rejected marker was left behind: %v", err)
			}
		})
	}
}

func TestStopMarkerGarbageIsNotARequest(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(town+"/daemon", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stopMarkerPath(town), []byte("not a marker"), 0o644); err != nil {
		t.Fatal(err)
	}
	if consumeStopRequested(town, 42, time.Now()) {
		t.Fatal("garbage was read as a stop request")
	}
}

// TestUnrequestedStopLogLineDoesNotPromiseARelaunch is the gt-2gctr
// regression: the daemon logs this line after a signal gt did not send, and a
// launchctl bootout — one of the ways that happens — unloads the job, so no
// launchd relaunch follows it.
func TestUnrequestedStopLogLineDoesNotPromiseARelaunch(t *testing.T) {
	t.Parallel()
	if strings.Contains(unrequestedStopLogLine, "so launchd relaunches") {
		t.Errorf("the unrequested-stop line still promises a relaunch:\n%s", unrequestedStopLogLine)
	}
	for _, want := range []string{"bootout", "KeepAlive"} {
		if !strings.Contains(unrequestedStopLogLine, want) {
			t.Errorf("the unrequested-stop line lacks %q:\n%s", want, unrequestedStopLogLine)
		}
	}
}

// TestRecordStopForHeal covers the write Run makes on its signal branch: the
// upgrade heal helper reads this record to tell a bootout from a respawn that
// failed (gt-r8ux8).
func TestRecordStopForHeal(t *testing.T) {
	t.Parallel()
	t.Run("a signal stop leaves its time for the helper", func(t *testing.T) {
		t.Parallel()
		d := upgradeTestDaemon(t)
		now := time.Now()
		d.recordStopForHeal(now)
		data, err := os.ReadFile(signalStopPath(d.config.TownRoot))
		if err != nil {
			t.Fatalf("a signal stop left no record for the heal helper: %v", err)
		}
		if got, want := string(data), strconv.FormatInt(now.Unix(), 10)+"\nunrequested\n"; got != want {
			t.Errorf("record = %q, want the stop time and an unrequested stop %q", got, want)
		}
	})
	t.Run("a stop gt requested is recorded as requested, and its marker is left for the exit", func(t *testing.T) {
		t.Parallel()
		d := upgradeTestDaemon(t)
		now := time.Now()
		if err := writeStopRequested(d.config.TownRoot, os.Getpid(), now); err != nil {
			t.Fatal(err)
		}
		d.recordStopForHeal(now)
		data, err := os.ReadFile(signalStopPath(d.config.TownRoot))
		if err != nil {
			t.Fatalf("a signal stop left no record for the heal helper: %v", err)
		}
		if got, want := string(data), strconv.FormatInt(now.Unix(), 10)+"\nrequested\n"; got != want {
			t.Errorf("record = %q, want the stop time and a requested stop %q", got, want)
		}
		if !stopRequested(d.config.TownRoot, os.Getpid(), now) {
			t.Errorf("recording the stop consumed the marker; exitAfterSignal must still find it")
		}
	})
	t.Run("a record that cannot be written is logged, and the stop stands", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		// A file where the daemon directory belongs fails the record write.
		if err := os.WriteFile(filepath.Join(town, "daemon"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		var logs bytes.Buffer
		d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(&logs, "", 0)}
		d.recordStopForHeal(time.Now())
		if !strings.Contains(logs.String(), "could not record the signal stop") {
			t.Errorf("the failed write was not reported:\n%s", logs.String())
		}
	})
}

// TestClearSpentSignalStop covers the clear Run makes at startup: a record of
// the daemon before this one must not outlive the town coming back up
// (gt-r8ux8).
func TestClearSpentSignalStop(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	d.recordStopForHeal(time.Now())
	d.clearSpentSignalStop()
	if _, err := os.Stat(signalStopPath(d.config.TownRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a spent signal-stop record outlived the daemon start (stat err %v)", err)
	}
}

func TestExitAfterSignal(t *testing.T) {
	t.Parallel()
	now := time.Now()
	t.Run("requested stop exits cleanly", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		if err := writeStopRequested(town, 42, now); err != nil {
			t.Fatal(err)
		}
		if err := exitAfterSignal(town, 42, now, nil); err != nil {
			t.Fatalf("exitAfterSignal = %v, want nil", err)
		}
	})
	t.Run("unrequested stop asks to be relaunched", func(t *testing.T) {
		t.Parallel()
		if err := exitAfterSignal(t.TempDir(), 42, now, nil); err != ErrUnrequestedStop {
			t.Fatalf("exitAfterSignal = %v, want ErrUnrequestedStop", err)
		}
	})
	t.Run("a shutdown error is kept", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		if err := writeStopRequested(town, 42, now); err != nil {
			t.Fatal(err)
		}
		boom := os.ErrDeadlineExceeded
		if err := exitAfterSignal(town, 42, now, boom); err != boom {
			t.Fatalf("exitAfterSignal = %v, want the shutdown error", err)
		}
	})
}
