package daemon

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/reaper"
)

func TestWispAlertBaselineRoundTrip(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := WispAlertBaselinePath(townRoot)

	previous, err := LoadWispAlertState(path)
	if err != nil {
		t.Fatalf("load with no baseline recorded: %v", err)
	}
	if previous != nil {
		t.Fatalf("a town that has never run the reaper has no baseline, got %+v", previous)
	}

	want := reaper.OpenWispAlertState{
		Baseline: reaper.OpenWispSample{OpenWisps: 138, Databases: 2, DryRun: true},
		Held:     3,
	}
	if err := SaveWispAlertState(path, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadWispAlertState(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got == nil || *got != want {
		t.Fatalf("round trip returned %+v, want %+v", got, want)
	}
}

// A corrupt baseline is replaced by the cycle that finds it, not compared
// against: an unreadable reading is the thing the alert would otherwise judge
// the town by (gt-11kyy).
func TestWispAlertBaselineCorruptFileIsAnErrorAndIsReplaced(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := WispAlertBaselinePath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0644); err != nil {
		t.Fatalf("seed corrupt baseline: %v", err)
	}

	if _, err := LoadWispAlertState(path); err == nil {
		t.Fatal("a corrupt baseline must be reported, not read as an absent one")
	}

	state := reaper.OpenWispAlertState{Baseline: reaper.OpenWispSample{OpenWisps: 42, Databases: 1}}
	if err := SaveWispAlertState(path, state); err != nil {
		t.Fatalf("save over a corrupt baseline: %v", err)
	}
	got, err := LoadWispAlertState(path)
	if err != nil {
		t.Fatalf("load after repair: %v", err)
	}
	if got == nil || *got != state {
		t.Fatalf("after repair the baseline is %+v, want %+v", got, state)
	}
}

// The patrol and a hand-run `gt reaper run` sample on schedules that have
// nothing to do with each other, so one series would read the other's idle gap
// as growth (gt-11kyy).
func TestWispAlertSeriesAreSeparatePerProducer(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	daemonPath := WispAlertBaselinePath(townRoot)
	cliPath := WispAlertCLIBaselinePath(townRoot)
	if daemonPath == cliPath {
		t.Fatalf("both producers write %s: one would overwrite the other", daemonPath)
	}

	daemonState := reaper.OpenWispAlertState{Baseline: reaper.OpenWispSample{OpenWisps: 1966, Databases: 6}}
	cliState := reaper.OpenWispAlertState{Baseline: reaper.OpenWispSample{OpenWisps: 300, Databases: 1}}
	if err := SaveWispAlertState(daemonPath, daemonState); err != nil {
		t.Fatalf("save patrol baseline: %v", err)
	}
	if err := SaveWispAlertState(cliPath, cliState); err != nil {
		t.Fatalf("save CLI baseline: %v", err)
	}

	gotDaemon, err := LoadWispAlertState(daemonPath)
	if err != nil {
		t.Fatalf("load patrol baseline: %v", err)
	}
	if gotDaemon == nil || *gotDaemon != daemonState {
		t.Fatalf("patrol baseline is %+v, want %+v", gotDaemon, daemonState)
	}
	gotCLI, err := LoadWispAlertState(cliPath)
	if err != nil {
		t.Fatalf("load CLI baseline: %v", err)
	}
	if gotCLI == nil || *gotCLI != cliState {
		t.Fatalf("CLI baseline is %+v, want %+v", gotCLI, cliState)
	}
}

func TestReportOpenWispAlert(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		previous   *reaper.OpenWispSample
		current    reaper.OpenWispSample
		wantWarned bool
	}{
		{
			name:       "first cycle only records",
			current:    reaper.OpenWispSample{OpenWisps: 900, Databases: 2},
			wantWarned: false,
		},
		{
			name:       "steady state is silent",
			previous:   &reaper.OpenWispSample{OpenWisps: 138, Databases: 2},
			current:    reaper.OpenWispSample{OpenWisps: 138, Databases: 2},
			wantWarned: false,
		},
		{
			name:       "runaway accumulation warns",
			previous:   &reaper.OpenWispSample{OpenWisps: 138, Databases: 2},
			current:    reaper.OpenWispSample{OpenWisps: 900, Databases: 2},
			wantWarned: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			path := WispAlertBaselinePath(townRoot)
			if tt.previous != nil {
				if err := SaveWispAlertState(path, reaper.OpenWispAlertState{Baseline: *tt.previous}); err != nil {
					t.Fatalf("seed baseline: %v", err)
				}
			}

			d, logs := newTestDaemon(townRoot)
			d.reportOpenWispAlert(tt.current)

			if warned := strings.Contains(logs.String(), "investigate wisp lifecycle"); warned != tt.wantWarned {
				t.Fatalf("warned = %v, want %v; log: %q", warned, tt.wantWarned, logs.String())
			}

			// The cycle is the next one's baseline whether or not it warned.
			// A reaper that only records on an alert has no baseline on the
			// steady cycles that matter.
			recorded, err := LoadWispAlertState(path)
			if err != nil {
				t.Fatalf("load recorded baseline: %v", err)
			}
			if recorded == nil || recorded.Baseline != tt.current {
				t.Fatalf("recorded baseline is %+v, want %+v", recorded, tt.current)
			}
		})
	}
}

// A cycle that lost databases to reap errors counts fewer wisps than the town
// has, so it is not a reading of the town (gt-11kyy). Recording it would anchor
// the series on the undercount, and the growth it hid would never be reported.
func TestRecordCycleOpenWispsSkipsACycleThatLostDatabases(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := WispAlertBaselinePath(townRoot)
	baseline := reaper.OpenWispAlertState{Baseline: reaper.OpenWispSample{OpenWisps: 138, Databases: 2}, Held: 5}
	if err := SaveWispAlertState(path, baseline); err != nil {
		t.Fatalf("seed baseline: %v", err)
	}

	d, logs := newTestDaemon(townRoot)
	d.recordCycleOpenWisps(1, reaper.OpenWispSample{OpenWisps: 900, Databases: 1})

	if !strings.Contains(logs.String(), "open-wisp alert skipped") {
		t.Errorf("a cycle missing a database was not reported as skipped: %q", logs.String())
	}
	if strings.Contains(logs.String(), "investigate wisp lifecycle") {
		t.Errorf("a cycle missing a database alerted on its undercount: %q", logs.String())
	}
	recorded, err := LoadWispAlertState(path)
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	if recorded == nil || *recorded != baseline {
		t.Fatalf("the incomplete cycle moved the baseline to %+v, want %+v", recorded, baseline)
	}

	// The next complete cycle is judged against the baseline the failure left
	// in place, and reports the growth the failed cycle hid.
	d.recordCycleOpenWisps(0, reaper.OpenWispSample{OpenWisps: 900, Databases: 2})
	if !strings.Contains(logs.String(), "investigate wisp lifecycle") {
		t.Errorf("the first complete cycle after a failure did not alert: %q", logs.String())
	}
}

func TestReportOpenWispAlertWarnsWhenTheBaselineCannotBeRead(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := WispAlertBaselinePath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0644); err != nil {
		t.Fatalf("seed corrupt baseline: %v", err)
	}

	d, logs := newTestDaemon(townRoot)
	sample := reaper.OpenWispSample{OpenWisps: 900, Databases: 2}
	d.reportOpenWispAlert(sample)

	if !strings.Contains(logs.String(), "cannot read the open-wisp baseline") {
		t.Fatalf("an unreadable baseline went unreported: %q", logs.String())
	}
	// The cycle replaces what it could not read, so the next one has a series.
	recorded, err := LoadWispAlertState(path)
	if err != nil {
		t.Fatalf("the cycle did not repair the baseline it could not read: %v", err)
	}
	if recorded == nil || recorded.Baseline != sample {
		t.Fatalf("recorded baseline is %+v, want %+v", recorded, sample)
	}
}

// A series that cannot be written leaves the next cycle with nothing to
// compare against, so the cycle reports it rather than passing over it
// (gt-11kyy). The seeded file blocks the read as well, but the record warning
// is only reachable through a write that failed.
func TestReportOpenWispAlertWarnsWhenTheBaselineCannotBeRecorded(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, "daemon"), []byte("not a directory"), 0644); err != nil {
		t.Fatalf("seed a file where the series directory belongs: %v", err)
	}

	d, logs := newTestDaemon(townRoot)
	d.reportOpenWispAlert(reaper.OpenWispSample{OpenWisps: 900, Databases: 2})

	if !strings.Contains(logs.String(), "cannot record the open-wisp baseline") {
		t.Fatalf("an unwritable baseline went unreported: %q", logs.String())
	}
}

// newTestDaemon returns a Daemon whose reaper log the test can read back.
func newTestDaemon(townRoot string) (*Daemon, *strings.Builder) {
	logs := &strings.Builder{}
	return &Daemon{
		logger: log.New(logs, "", 0),
		config: &Config{TownRoot: townRoot},
	}, logs
}
