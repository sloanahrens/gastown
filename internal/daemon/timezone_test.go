package daemon

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// zoneStart is one parked daemon start a zone test reads the start-up log
// from: the log written so far and every zone name the loader was asked for.
type zoneStart struct {
	logs   string
	loaded []string
	finish func() error
}

// startParkedZoneStart runs a start on an empty temp town and parks it at the
// legacy-socket step, the first point after the start-up lines, so the test
// reads the start-up log while the daemon is still up. load answers TZ.
func startParkedZoneStart(t *testing.T, env map[string]string, clock clockwork.Clock, load func(string) (*time.Location, error)) zoneStart {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	logs := &strings.Builder{}
	var (
		mu     sync.Mutex
		loaded []string
	)
	d := &Daemon{
		config:     DefaultConfig(town),
		logger:     log.New(logs, "", 0),
		getenv:     ownerEnv(env),
		hostname:   fixtureHostname("fixture-host"),
		clock:      clock,
		ownerProbe: func(Owner) (bool, bool) { return false, true },
	}
	// The rest of the start does real host work: the heartbeat dispatches
	// and restarts sessions, the sweep touches the tmux socket directory, and
	// the probes reach the network.
	d.disabledPatrols = map[string]bool{
		"landing_infra":    true,
		"resource_sampler": true,
		"spec_dispatch":    true,
	}
	d.seams.heartbeatWork = func(*Daemon, *State, bool) {}
	d.seams.loadZone = func(name string) (*time.Location, error) {
		mu.Lock()
		loaded = append(loaded, name)
		mu.Unlock()
		return load(name)
	}

	parked := make(chan struct{})
	release := make(chan struct{})
	d.seams.cleanupLegacySockets = func(string) (int, int) {
		close(parked)
		<-release
		return 0, 0
	}

	d.ctx, d.cancel = context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run() }()
	waitForSignal(t, parked, "Run never reached the end of its start-time writes")

	var (
		once      sync.Once
		runResult error
	)
	finish := func() error {
		once.Do(func() {
			d.cancel()
			close(release)
			bounded, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			select {
			case runResult = <-done:
			case <-bounded.Done():
				t.Fatal("Run did not return after the daemon context ended")
			}
		})
		return runResult
	}
	t.Cleanup(func() { _ = finish() })
	return zoneStart{logs: logs.String(), loaded: loaded, finish: finish}
}

// TestZoneLoaderSeamDefaultsToTheTimePackage: with no loader set, the start-up
// line resolves TZ through time.LoadLocation. UTC needs no zone database, so
// the guard is hermetic.
func TestZoneLoaderSeamDefaultsToTheTimePackage(t *testing.T) {
	t.Parallel()
	loc, err := (&Daemon{}).seams.zoneLoader()("UTC")
	if err != nil {
		t.Fatalf("the default zone loader failed for UTC: %v", err)
	}
	if loc.String() != "UTC" {
		t.Fatalf("the default zone loader returned %q, want UTC", loc.String())
	}
}

// TestDaemonLogsItsZoneAtStart: the start-up log names the zone TZ loaded and
// its offset at that moment, so a reader of the log can tell which zone its
// stamps are in (gt-gg9hv.34). With TZ unset the daemon reads the host's
// zone and says so without a CRITICAL line.
func TestDaemonLogsItsZoneAtStart(t *testing.T) {
	t.Parallel()

	t.Run("winter", func(t *testing.T) {
		t.Parallel()
		start := startParkedZoneStart(t,
			map[string]string{"TZ": "America/Chicago"},
			clockwork.NewFakeClockAt(time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC)),
			func(string) (*time.Location, error) { return time.FixedZone("America/Chicago", -6*60*60), nil })

		if want := "log timezone: America/Chicago (UTC-06:00)"; !strings.Contains(start.logs, want) {
			t.Fatalf("start-up log = %q, want %q", start.logs, want)
		}
		if len(start.loaded) != 1 || start.loaded[0] != "America/Chicago" {
			t.Errorf("the loader was asked for %q, want exactly America/Chicago", start.loaded)
		}
		if strings.Contains(start.logs, "CRITICAL") {
			t.Errorf("a zone that loaded logged a CRITICAL line: %q", start.logs)
		}
	})

	t.Run("summer", func(t *testing.T) {
		t.Parallel()
		start := startParkedZoneStart(t,
			map[string]string{"TZ": "America/Chicago"},
			clockwork.NewFakeClockAt(time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)),
			func(string) (*time.Location, error) { return time.FixedZone("America/Chicago", -5*60*60), nil })

		if want := "log timezone: America/Chicago (UTC-05:00)"; !strings.Contains(start.logs, want) {
			t.Fatalf("start-up log = %q, want %q", start.logs, want)
		}
	})

	t.Run("no TZ reads the host zone", func(t *testing.T) {
		t.Parallel()
		start := startParkedZoneStart(t,
			nil,
			clockwork.NewFakeClockAt(time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC)),
			func(name string) (*time.Location, error) {
				return nil, errors.New("the loader was asked for " + name + " with TZ unset")
			})

		if want := "log timezone: " + time.Local.String() + " (UTC"; !strings.Contains(start.logs, want) {
			t.Fatalf("start-up log = %q, want a line naming the host zone %q", start.logs, want)
		}
		if len(start.loaded) != 0 {
			t.Errorf("TZ was unset and the loader was still asked for %q", start.loaded)
		}
		if strings.Contains(start.logs, "CRITICAL") {
			t.Errorf("TZ was unset and the start logged a CRITICAL line: %q", start.logs)
		}
	})
}

// TestDaemonWarnsWhenItsZoneCannotBeLoaded: a zone name the image cannot load
// is a CRITICAL line naming it, not a silent fall back to UTC, and the start
// goes on (gt-gg9hv.34).
func TestDaemonWarnsWhenItsZoneCannotBeLoaded(t *testing.T) {
	t.Parallel()
	start := startParkedZoneStart(t,
		map[string]string{"TZ": "America/Chicago"},
		clockwork.NewFakeClockAt(time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC)),
		func(string) (*time.Location, error) {
			return nil, errors.New("unknown time zone America/Chicago")
		})

	want := "CRITICAL: TZ=America/Chicago cannot be loaded (no zoneinfo in this image): log stamps and maintenance windows are UTC"
	if !strings.Contains(start.logs, want) {
		t.Fatalf("start-up log = %q, want the line %q", start.logs, want)
	}
	if strings.Contains(start.logs, "log timezone:") {
		t.Errorf("a zone that did not load still logged a zone line: %q", start.logs)
	}
	// The start reached the end of its start-time writes (the park point the
	// helper waits for) and shuts down cleanly.
	if err := start.finish(); err != nil {
		t.Fatalf("the start failed after the unloadable zone, want it to keep running: %v", err)
	}
}
