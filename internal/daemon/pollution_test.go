package daemon

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
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

// newImposterDaemon is a daemon whose sweep reads a scratch town's Dolt
// config and takes its port probe and kill through the seams, so nothing on
// the machine is looked at or signalled. The scratch town has no town.json;
// writeTownJSON adds one.
func newImposterDaemon(t *testing.T, buf *bytes.Buffer) *Daemon {
	t.Helper()
	d := newPollutionDaemon(t, buf)
	d.seams.imposterSweep = nil
	if err := os.MkdirAll(filepath.Join(d.config.TownRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

// writeTownJSON writes a valid town.json whose "dolt" section is doltJSON.
func writeTownJSON(t *testing.T, d *Daemon, doltJSON string) {
	t.Helper()
	writeRawTownJSON(t, d, `{"type":"town","version":2,"name":"t","dolt":`+doltJSON+`}`)
}

// writeRawTownJSON writes body verbatim as the town's mayor/town.json.
func writeRawTownJSON(t *testing.T, d *Daemon, body string) {
	t.Helper()
	path := filepath.Join(d.config.TownRoot, "mayor", "town.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// probeAndKillCounts wraps the two process seams so a test can assert that a
// path that skips the sweep's work calls neither.
type probeAndKillCounts struct {
	probes, kills int
}

func (c *probeAndKillCounts) install(d *Daemon) {
	d.seams.doltPortConflict = func(string) (int, string) {
		c.probes++
		return 4242, "/foreign/data"
	}
	d.seams.killImposters = func(string) error {
		c.kills++
		return nil
	}
}

func TestSweepImposterDoltSkipsContainerMode(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	d := newImposterDaemon(t, &buf)
	writeTownJSON(t, d, `{"port":3307,"external_container":"gt-dolt"}`)
	var calls probeAndKillCounts
	calls.install(d)

	d.sweepImposterDolt()

	if calls.probes != 0 || calls.kills != 0 {
		t.Errorf("container town probed %d time(s) and killed %d time(s), want neither", calls.probes, calls.kills)
	}
	if got := buf.String(); got != "" {
		t.Errorf("log = %q, want no line for a container town", got)
	}
}

func TestSweepImposterDoltUnparsableSettingSkips(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	d := newImposterDaemon(t, &buf)
	writeTownJSON(t, d, `{"port":3307,"external_container":"-v"}`)
	var calls probeAndKillCounts
	calls.install(d)

	d.sweepImposterDolt()

	if calls.probes != 0 || calls.kills != 0 {
		t.Errorf("unreadable setting probed %d time(s) and killed %d time(s), want neither", calls.probes, calls.kills)
	}
	got := buf.String()
	if !strings.Contains(got, "dolt.external_container") {
		t.Errorf("log = %q, want the setting named", got)
	}
	if lines := strings.Count(got, "\n"); lines != 1 {
		t.Errorf("logged %d line(s), want 1: %q", lines, got)
	}
}

func TestSweepImposterDoltMalformedTownJSONSkips(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	d := newImposterDaemon(t, &buf)
	writeRawTownJSON(t, d, `{"type":"town","version":2,"name":"t","dolt":`)
	var calls probeAndKillCounts
	calls.install(d)

	d.sweepImposterDolt()

	if calls.probes != 0 || calls.kills != 0 {
		t.Errorf("malformed town.json probed %d time(s) and killed %d time(s), want neither", calls.probes, calls.kills)
	}
	got := buf.String()
	if !strings.Contains(got, filepath.Join(d.config.TownRoot, "mayor", "town.json")) {
		t.Errorf("log = %q, want the town.json path named", got)
	}
	if lines := strings.Count(got, "\n"); lines != 1 {
		t.Errorf("logged %d line(s), want 1: %q", lines, got)
	}
}

func TestSweepImposterDoltHostModeUnchanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		townJSON string // the whole file body; "" leaves the town without one
		killErr  error
		wantLog  string
	}{
		{
			name:     "conflict logged and killed",
			townJSON: `{"type":"town","version":2,"name":"t","dolt":{"port":3307}}`,
			wantLog:  `test_pollution: Dolt port conflict: PID 4242 holds the port serving "/foreign/data"`,
		},
		{
			name:     "kill refused",
			townJSON: `{"type":"town","version":2,"name":"t","dolt":{"port":3307}}`,
			killErr:  errors.New("holder is not a dolt sql-server"),
			wantLog:  "test_pollution: killing imposter Dolt server (PID 4242): holder is not a dolt sql-server",
		},
		{
			name:    "no town.json at all",
			wantLog: `test_pollution: Dolt port conflict: PID 4242 holds the port serving "/foreign/data"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			d := newImposterDaemon(t, &buf)
			if tt.townJSON != "" {
				writeRawTownJSON(t, d, tt.townJSON)
			}
			var calls probeAndKillCounts
			calls.install(d)
			d.seams.killImposters = func(string) error {
				calls.kills++
				return tt.killErr
			}

			d.sweepImposterDolt()

			if calls.probes != 1 || calls.kills != 1 {
				t.Errorf("host town probed %d time(s) and killed %d time(s), want one each", calls.probes, calls.kills)
			}
			if got := buf.String(); !strings.Contains(got, tt.wantLog) {
				t.Errorf("log = %q, want %q", got, tt.wantLog)
			}
		})
	}
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
