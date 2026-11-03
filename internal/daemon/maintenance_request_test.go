package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/update"
)

// outsideRequestNow is mid-afternoon, hours after a 03:00 window closed.
var outsideRequestNow = time.Date(2026, 10, 1, 14, 0, 0, 0, time.UTC)

// maintenanceWindowAt is a patrol config whose maintenance window starts at
// window.
func maintenanceWindowAt(window string) *DaemonPatrolConfig {
	return &DaemonPatrolConfig{
		Type: "daemon-patrol-config", Version: 1,
		Patrols: &PatrolsConfig{
			ScheduledMaintenance: &ScheduledMaintenanceConfig{Enabled: true, Window: window},
		},
	}
}

// requestDaemon is a daemon on a 03:00 window with one database, gc'd two
// hours before now against an empty old-gen baseline, so a cycle that runs
// finds it due.
func requestDaemon(t *testing.T, now time.Time) (*Daemon, *gcFakes) {
	t.Helper()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {631 * mib, 500 * mib}}
	d.patrolConfig = maintenanceWindowAt("03:00")
	d.maint.now = func() time.Time { return now }
	recordCatchUpGC(t, d, "hq", now.Add(-2*time.Hour))
	return d, f
}

// writeRequestFile puts a request for runID in d's update directory.
func writeRequestFile(t *testing.T, d *Daemon, runID string, at time.Time) string {
	t.Helper()
	dir := update.Dir(d.config.TownRoot)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(update.RequestFile{RunID: runID, At: at})
	if err != nil {
		t.Fatal(err)
	}
	path := requestPath(dir)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// readRequestResult decodes the answer the daemon left in d's update directory.
func readRequestResult(t *testing.T, d *Daemon) update.MaintenanceResult {
	t.Helper()
	data, err := os.ReadFile(resultPath(update.Dir(d.config.TownRoot)))
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var res update.MaintenanceResult
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatalf("parse result: %v", err)
	}
	return res
}

// A request runs the cycle at any hour; the same tick without one leaves the
// day's scheduled work alone.
func TestMaintenanceRequestRunsOutsideTheWindow(t *testing.T) {
	t.Parallel()

	t.Run("a request starts the cycle", func(t *testing.T) {
		t.Parallel()
		d, f := requestDaemon(t, outsideRequestNow)
		path := writeRequestFile(t, d, "run-1", outsideRequestNow)

		d.runScheduledMaintenance()

		if got := strings.Join(f.gcCalls, ","); got != "hq" {
			t.Fatalf("gc calls = %q, want hq: a request must run the cycle outside the window", got)
		}
		res := readRequestResult(t, d)
		if res.RunID != "run-1" || res.Outcome != "completed" {
			t.Errorf("result = %+v, want run-1 completed", res)
		}
		if !res.At.Equal(outsideRequestNow) {
			t.Errorf("result at = %v, want the daemon's clock %v", res.At, outsideRequestNow)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("request file still there after a completed cycle: %v", err)
		}
	})

	t.Run("no request does nothing outside the window", func(t *testing.T) {
		t.Parallel()
		d, f := requestDaemon(t, outsideRequestNow)

		d.runScheduledMaintenance()

		if len(f.gcCalls) != 0 {
			t.Fatalf("gc calls = %v without a request outside the window", f.gcCalls)
		}
		if _, err := os.Stat(resultPath(update.Dir(d.config.TownRoot))); !os.IsNotExist(err) {
			t.Errorf("a result file was written without a request")
		}
	})

	// The gap since the last cycle is the other rule a request suspends.
	t.Run("a request runs though a cycle finished two hours ago", func(t *testing.T) {
		t.Parallel()
		d, f := requestDaemon(t, gcTestNow) // inside the 03:00 window
		d.lastMaintenanceRun = gcTestNow.Add(-2 * time.Hour)
		writeRequestFile(t, d, "run-2", gcTestNow)

		d.runScheduledMaintenance()

		if got := strings.Join(f.gcCalls, ","); got != "hq" {
			t.Fatalf("gc calls = %q, want hq — a request ignores the 20-hour gap", got)
		}
		if res := readRequestResult(t, d); res.RunID != "run-2" || res.Outcome != "completed" {
			t.Errorf("result = %+v, want run-2 completed", res)
		}
	})

	t.Run("no request respects the gap", func(t *testing.T) {
		t.Parallel()
		d, f := requestDaemon(t, gcTestNow)
		d.lastMaintenanceRun = gcTestNow.Add(-2 * time.Hour)

		d.runScheduledMaintenance()

		if len(f.gcCalls) != 0 {
			t.Fatalf("gc calls = %v although a cycle finished two hours ago", f.gcCalls)
		}
	})
}

// Under either standby the tick starts no cycle and answers the request
// deferred, naming the standby, instead of leaving the job to time out.
func TestMaintenanceRequestUnderStandbyAnswersDeferred(t *testing.T) {
	t.Parallel()
	for _, c := range standbyConditions {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d, f := requestDaemon(t, outsideRequestNow)
			c.set(t, d, outsideRequestNow)
			path := writeRequestFile(t, d, "run-standby", outsideRequestNow)

			d.runScheduledMaintenance()

			if len(f.gcCalls) != 0 {
				t.Errorf("gc calls = %v under %s, want none", f.gcCalls, c.name)
			}
			res := readRequestResult(t, d)
			if res.RunID != "run-standby" || res.Outcome != "deferred" {
				t.Errorf("result = %+v, want run-standby deferred", res)
			}
			wantDetail := "ESTOP"
			if c.name == "waned Ring" {
				wantDetail = "Ring waned"
			}
			if !strings.Contains(res.Detail, wantDetail) {
				t.Errorf("detail = %q, want it to name %q", res.Detail, wantDetail)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("request file still there under %s: %v", c.name, err)
			}
		})
	}
}

// A busy town defers the cycle; the request waits for the next tick, and no
// result says otherwise.
func TestMaintenanceRequestDeferredByTheQuietGuardKeepsTheFile(t *testing.T) {
	t.Parallel()
	d, f := requestDaemon(t, outsideRequestNow)
	f.quietUntil = 0 // the next quiet probe finds the town busy
	path := writeRequestFile(t, d, "run-busy", outsideRequestNow)

	d.runScheduledMaintenance()

	if len(f.gcCalls) != 0 {
		t.Fatalf("gc ran while the town was busy: %v", f.gcCalls)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("request file is gone after a deferral: %v", err)
	}
	if _, err := os.Stat(resultPath(update.Dir(d.config.TownRoot))); !os.IsNotExist(err) {
		t.Errorf("a deferred cycle wrote a result file")
	}
}

// recordingRequestFiles records every file operation in order so the
// result-before-remove order is visible, and fails the write on demand.
type recordingRequestFiles struct {
	events      []string
	read        []byte
	readErr     error
	writeErr    error
	written     []byte
	writtenPath string
}

func (f *recordingRequestFiles) ReadFile(path string) ([]byte, error) {
	f.events = append(f.events, "read "+filepath.Base(path))
	if f.readErr != nil {
		return nil, f.readErr
	}
	if f.read == nil {
		return nil, os.ErrNotExist
	}
	return f.read, nil
}

func (f *recordingRequestFiles) WriteFile(path string, data []byte, mode os.FileMode) error {
	f.events = append(f.events, fmt.Sprintf("write %s %04o", filepath.Base(path), mode))
	if f.writeErr != nil {
		return f.writeErr
	}
	f.written, f.writtenPath = data, path
	return nil
}

func (f *recordingRequestFiles) Remove(path string) error {
	f.events = append(f.events, "remove "+filepath.Base(path))
	return nil
}

// The result is on disk before the request goes: a job polling for the
// request's disappearance never finds it gone with no answer.
func TestFinishMaintenanceRequestWritesResultThenRemovesIt(t *testing.T) {
	t.Parallel()
	at := outsideRequestNow

	for _, tc := range []struct {
		name    string
		outcome maintenanceGCOutcome
		want    string
	}{
		{"completed", gcOutcomeCompleted, "completed"},
		{"failed", gcOutcomeFailed, "failed"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &recordingRequestFiles{}
			if err := finishMaintenanceRequest(f, "/update", at, "run-7", tc.outcome, "the reason"); err != nil {
				t.Fatalf("finish: %v", err)
			}
			want := []string{"write " + update.ResultFileName + " 0600", "remove " + update.RequestFileName}
			if got := strings.Join(f.events, ","); got != strings.Join(want, ",") {
				t.Errorf("events = %v, want %v — the result before the request", f.events, want)
			}
			var res update.MaintenanceResult
			if err := json.Unmarshal(f.written, &res); err != nil {
				t.Fatalf("decode result: %v", err)
			}
			if res.RunID != "run-7" || res.Outcome != tc.want || res.Detail != "the reason" || !res.At.Equal(at) {
				t.Errorf("result = %+v, want run-7 %s at %v with the detail", res, tc.want, at)
			}
			if wantPath := filepath.Join("/update", update.ResultFileName); f.writtenPath != wantPath {
				t.Errorf("wrote %s, want %s", f.writtenPath, wantPath)
			}
		})
	}

	for _, outcome := range []maintenanceGCOutcome{gcOutcomeDeferred, gcOutcomeInterrupted} {
		outcome := outcome
		t.Run(outcome.String()+" touches nothing", func(t *testing.T) {
			t.Parallel()
			f := &recordingRequestFiles{}
			if err := finishMaintenanceRequest(f, "/update", at, "run-7", outcome, "why"); err != nil {
				t.Errorf("finish: %v", err)
			}
			if len(f.events) != 0 {
				t.Errorf("%v touched the files: %v", outcome, f.events)
			}
		})
	}

	t.Run("a write error leaves the request", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("disk full")
		f := &recordingRequestFiles{writeErr: boom}
		err := finishMaintenanceRequest(f, "/update", at, "run-7", gcOutcomeCompleted, "")
		if !errors.Is(err, boom) {
			t.Fatalf("error = %v, want the write error", err)
		}
		for _, e := range f.events {
			if strings.HasPrefix(e, "remove") {
				t.Errorf("the request was removed after a failed result write: %v", f.events)
			}
		}
	})
}

// readMaintenanceRequest tells a missing request from one it cannot answer.
func TestReadMaintenanceRequest(t *testing.T) {
	t.Parallel()
	valid, err := json.Marshal(update.RequestFile{RunID: "run-1", At: outsideRequestNow})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		files     *recordingRequestFiles
		ok        bool
		malformed bool
		wantErr   bool
		wantRunID string
	}{
		{name: "no file", files: &recordingRequestFiles{}},
		{name: "valid", files: &recordingRequestFiles{read: valid}, ok: true, wantRunID: "run-1"},
		{name: "unparseable", files: &recordingRequestFiles{read: []byte("{")}, malformed: true, wantErr: true},
		{name: "no run id", files: &recordingRequestFiles{read: []byte(`{"at":"2026-10-01T14:00:00Z"}`)}, malformed: true, wantErr: true},
		{name: "unreadable", files: &recordingRequestFiles{readErr: errors.New("permission denied")}, wantErr: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, ok, err := readMaintenanceRequest(tc.files, "/update")
			if ok != tc.ok {
				t.Errorf("ok = %v, want %v", ok, tc.ok)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("error = %v, want error: %v", err, tc.wantErr)
			}
			if got := errors.Is(err, errMalformedRequest); got != tc.malformed {
				t.Errorf("malformed = %v, want %v (err=%v)", got, tc.malformed, err)
			}
			if tc.ok && req.RunID != tc.wantRunID {
				t.Errorf("run id = %q, want %q", req.RunID, tc.wantRunID)
			}
		})
	}
}

// A request the tick cannot answer is removed and the tick runs as scheduled;
// an inactive patrol answers it failed rather than leaving the job waiting.
func TestMaintenanceRequestMalformedOrPatrolInactive(t *testing.T) {
	t.Parallel()

	t.Run("malformed request is removed", func(t *testing.T) {
		t.Parallel()
		d, f := requestDaemon(t, gcTestNow) // inside the 03:00 window
		dir := update.Dir(d.config.TownRoot)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := requestPath(dir)
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}

		d.runScheduledMaintenance()

		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("malformed request still there: %v", err)
		}
		// As if there were none: the window's own cycle still runs.
		if got := strings.Join(f.gcCalls, ","); got != "hq" {
			t.Errorf("gc calls = %q, want hq — the tick must run as if there were no request", got)
		}
		if _, err := os.Stat(resultPath(dir)); !os.IsNotExist(err) {
			t.Errorf("a malformed request was answered with a result")
		}
	})

	t.Run("unreadable request is left for the next tick", func(t *testing.T) {
		t.Parallel()
		d, _ := requestDaemon(t, outsideRequestNow)
		dir := update.Dir(d.config.TownRoot)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := requestPath(dir)
		// A directory at the request path makes the read fail without the
		// file being malformed.
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}

		d.runScheduledMaintenance()

		if _, err := os.Stat(path); err != nil {
			t.Errorf("an unreadable request was removed: %v", err)
		}
		if _, err := os.Stat(resultPath(dir)); !os.IsNotExist(err) {
			t.Errorf("an unreadable request was answered with a result")
		}
	})

	t.Run("inactive patrol is answered failed and removed", func(t *testing.T) {
		t.Parallel()
		d, f := requestDaemon(t, outsideRequestNow)
		d.patrolConfig = maintenanceWindowAt("03:00")
		d.patrolConfig.Patrols.ScheduledMaintenance.Enabled = false
		path := writeRequestFile(t, d, "run-off", outsideRequestNow)

		d.runScheduledMaintenance()

		if len(f.gcCalls) != 0 {
			t.Errorf("gc calls = %v with the patrol inactive", f.gcCalls)
		}
		res := readRequestResult(t, d)
		if res.RunID != "run-off" || res.Outcome != "failed" || !strings.Contains(res.Detail, "not active") {
			t.Errorf("result = %+v, want run-off failed naming the inactive patrol", res)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("request still there after the patrol answered it: %v", err)
		}
	})
}
