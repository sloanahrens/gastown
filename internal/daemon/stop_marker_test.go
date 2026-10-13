package daemon

import (
	"os"
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
