package daemon

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/tmuxsweep"
)

// newPollutionDaemon is a daemon whose sweep touches nothing on the machine:
// a scratch town root for the PID files, and both process-signalling parts
// replaced by the seams.
func newPollutionDaemon(t *testing.T, buf *bytes.Buffer) *Daemon {
	t.Helper()
	d := &Daemon{
		logger:       log.New(buf, "", 0),
		config:       &Config{TownRoot: t.TempDir()},
		patrolConfig: nil,
	}
	d.seams.imposterSweep = func(string) {}
	d.seams.orphanReap = func() {}
	d.seams.testSocketSweep = func() (tmuxsweep.Report, error) { return tmuxsweep.Report{}, nil }
	return d
}

func TestSweepTestTmuxServersLogsWhatItReaped(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	d := newPollutionDaemon(t, &buf)
	d.seams.testSocketSweep = func() (tmuxsweep.Report, error) {
		return tmuxsweep.Report{Leftovers: []string{"gt-test-91506"}, StaleFiles: []string{"gt-test-91507"}}, nil
	}

	d.sweepTestTmuxServers()

	if got := buf.String(); !strings.Contains(got, "reaped 1 abandoned test tmux server(s), removed 1 socket file(s)") {
		t.Errorf("log = %q, want the reap summary", got)
	}
}

func TestSweepTestTmuxServersReportsWhatItCouldNotProbe(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	d := newPollutionDaemon(t, &buf)
	d.seams.testSocketSweep = func() (tmuxsweep.Report, error) {
		return tmuxsweep.Report{Unprobed: []string{"gt-test-91506"}}, nil
	}

	d.sweepTestTmuxServers()

	if got := buf.String(); !strings.Contains(got, "1 tmux test socket(s) could not be probed") {
		t.Errorf("log = %q, want the unprobed count", got)
	}
}

func TestSweepTestTmuxServersReportsAFailedScan(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	d := newPollutionDaemon(t, &buf)
	d.seams.testSocketSweep = func() (tmuxsweep.Report, error) {
		return tmuxsweep.Report{}, errors.New("no socket directory")
	}

	d.sweepTestTmuxServers()

	if got := buf.String(); !strings.Contains(got, "scanning test tmux sockets: no socket directory") {
		t.Errorf("log = %q, want the scan failure", got)
	}
}

func TestSweepTestTmuxServersIsQuietWhenClean(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	d := newPollutionDaemon(t, &buf)

	d.sweepTestTmuxServers()

	if got := buf.String(); got != "" {
		t.Errorf("log = %q, want no line for a clean sweep", got)
	}
}

// TestRunTestPollutionSweepTouchesTheWholeJob pins the composition: every part
// runs, and the ones that signal a process go through their seams.
func TestRunTestPollutionSweepTouchesTheWholeJob(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	d := newPollutionDaemon(t, &buf)
	orphans, imposters, sockets := 0, 0, 0
	d.seams.orphanReap = func() { orphans++ }
	d.seams.imposterSweep = func(string) { imposters++ }
	d.seams.testSocketSweep = func() (tmuxsweep.Report, error) { sockets++; return tmuxsweep.Report{}, nil }

	d.runTestPollutionSweep()

	for _, tc := range []struct {
		name string
		got  int
	}{{"orphan reap", orphans}, {"imposter sweep", imposters}, {"test-socket sweep", sockets}} {
		if tc.got != 1 {
			t.Errorf("%s ran %d time(s), want 1", tc.name, tc.got)
		}
	}
}
