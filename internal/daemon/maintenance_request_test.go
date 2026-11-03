package daemon

import (
	"bytes"
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
	d, f, _ := requestDaemonWithLogs(t, now)
	return d, f
}

// requestDaemonWithLogs is requestDaemon plus the daemon's log buffer.
func requestDaemonWithLogs(t *testing.T, now time.Time) (*Daemon, *gcFakes, *bytes.Buffer) {
	t.Helper()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {631 * mib, 500 * mib}}
	d.patrolConfig = maintenanceWindowAt("03:00")
	d.maint.now = func() time.Time { return now }
	recordCatchUpGC(t, d, "hq", now.Add(-2*time.Hour))
	return d, f, logs
}

// requestTickAndCycle runs one poll tick and then the gc cycle it dispatched,
// if any. Production dispatches a cycle off the heartbeat loop; holding it
// here and running it after the tick returns means a test's assertions never
// depend on how fast the dispatch seam runs it. A test asserting that no cycle
// was dispatched uses runScheduledMaintenance directly with the inline fake.
func requestTickAndCycle(t *testing.T, d *Daemon) {
	t.Helper()
	var cycle func()
	prev := d.maint.dispatch
	d.maint.dispatch = func(fn func()) { cycle = fn }
	defer func() { d.maint.dispatch = prev }()
	d.runScheduledMaintenance()
	if cycle != nil {
		cycle()
	}
}

// blockResultPath puts a directory where the result belongs, so the answer's
// write fails.
func blockResultPath(t *testing.T, d *Daemon) string {
	t.Helper()
	dir := update.Dir(d.config.TownRoot)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := resultPath(dir)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
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

		requestTickAndCycle(t, d)

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

		requestTickAndCycle(t, d)

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

	requestTickAndCycle(t, d)

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
	removeErr   error
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
	return f.removeErr
}

// requestBytes is the JSON of a request for runID.
func requestBytes(t *testing.T, runID string) []byte {
	t.Helper()
	data, err := json.Marshal(update.RequestFile{RunID: runID, At: outsideRequestNow})
	if err != nil {
		t.Fatal(err)
	}
	return data
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
			f := &recordingRequestFiles{read: requestBytes(t, "run-7")}
			if err := finishMaintenanceRequest(f, "/update", at, "run-7", tc.outcome, "the reason"); err != nil {
				t.Fatalf("finish: %v", err)
			}
			want := []string{"write " + update.ResultFileName + " 0600", "read " + update.RequestFileName, "remove " + update.RequestFileName}
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

	t.Run("a request already gone counts as answered", func(t *testing.T) {
		t.Parallel()
		f := &recordingRequestFiles{}
		if err := finishMaintenanceRequest(f, "/update", at, "run-7", gcOutcomeCompleted, ""); err != nil {
			t.Errorf("finish: %v, want nil when the request is already gone", err)
		}
		for _, e := range f.events {
			if strings.HasPrefix(e, "remove") {
				t.Errorf("removal was attempted for a request already gone: %v", f.events)
			}
		}
	})

	t.Run("a newer request for another run survives", func(t *testing.T) {
		t.Parallel()
		f := &recordingRequestFiles{read: requestBytes(t, "run-8")}
		if err := finishMaintenanceRequest(f, "/update", at, "run-7", gcOutcomeCompleted, ""); err != nil {
			t.Errorf("finish: %v, want nil — the answer to run-7 is still written", err)
		}
		for _, e := range f.events {
			if strings.HasPrefix(e, "remove") {
				t.Errorf("answering run-7 removed run-8's request: %v", f.events)
			}
		}
	})

	t.Run("a file that no longer parses is left for the tick", func(t *testing.T) {
		t.Parallel()
		f := &recordingRequestFiles{read: []byte("{not json")}
		if err := finishMaintenanceRequest(f, "/update", at, "run-7", gcOutcomeCompleted, ""); err != nil {
			t.Errorf("finish: %v, want nil — the answer to run-7 is still written", err)
		}
		for _, e := range f.events {
			if strings.HasPrefix(e, "remove") {
				t.Errorf("a malformed file was removed by the answer: %v", f.events)
			}
		}
	})

	t.Run("an unreadable request is left in place", func(t *testing.T) {
		t.Parallel()
		readErr := errors.New("permission denied")
		f := &recordingRequestFiles{readErr: readErr}
		err := finishMaintenanceRequest(f, "/update", at, "run-7", gcOutcomeCompleted, "")
		if !errors.Is(err, readErr) {
			t.Fatalf("error = %v, want the read error", err)
		}
		for _, e := range f.events {
			if strings.HasPrefix(e, "remove") {
				t.Errorf("an unreadable request was removed: %v", f.events)
			}
		}
	})

	t.Run("a remove error is returned after the result", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("read-only directory")
		f := &recordingRequestFiles{read: requestBytes(t, "run-7"), removeErr: boom}
		err := finishMaintenanceRequest(f, "/update", at, "run-7", gcOutcomeCompleted, "")
		if !errors.Is(err, boom) {
			t.Fatalf("error = %v, want the remove error", err)
		}
		want := []string{"write " + update.ResultFileName + " 0600", "read " + update.RequestFileName, "remove " + update.RequestFileName}
		if got := strings.Join(f.events, ","); got != strings.Join(want, ",") {
			t.Errorf("events = %v, want %v", f.events, want)
		}
	})

	t.Run("an unencodable time fails before writing", func(t *testing.T) {
		t.Parallel()
		f := &recordingRequestFiles{}
		// JSON cannot carry a year outside [0, 9999]; the job could not parse
		// such a result, so the write is skipped.
		far := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		if err := finishMaintenanceRequest(f, "/update", far, "run-7", gcOutcomeCompleted, ""); err == nil {
			t.Fatal("an unencodable result time returned no error")
		}
		if len(f.events) != 0 {
			t.Errorf("files were touched before the encode failed: %v", f.events)
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
		invalid   bool
		wantErr   bool
		wantRunID string
	}{
		{name: "no file", files: &recordingRequestFiles{}},
		{name: "valid", files: &recordingRequestFiles{read: valid}, ok: true, wantRunID: "run-1"},
		{name: "allowed punctuation", files: &recordingRequestFiles{read: []byte(`{"run_id":"run.1-a_b"}`)}, ok: true, wantRunID: "run.1-a_b"},
		{name: "64 characters", files: &recordingRequestFiles{read: []byte(`{"run_id":"` + strings.Repeat("a", 64) + `"}`)}, ok: true, wantRunID: strings.Repeat("a", 64)},
		{name: "unparseable", files: &recordingRequestFiles{read: []byte("{")}, malformed: true, wantErr: true},
		{name: "no run id", files: &recordingRequestFiles{read: []byte(`{"at":"2026-10-01T14:00:00Z"}`)}, malformed: true, wantErr: true},
		{name: "run id too long", files: &recordingRequestFiles{read: []byte(`{"run_id":"` + strings.Repeat("a", 65) + `"}`)}, invalid: true, wantErr: true},
		{name: "run id with a slash", files: &recordingRequestFiles{read: []byte(`{"run_id":"run/1"}`)}, invalid: true, wantErr: true},
		{name: "run id with a newline", files: &recordingRequestFiles{read: []byte(`{"run_id":"run\n1"}`)}, invalid: true, wantErr: true},
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
			if got := errors.Is(err, errInvalidRunID); got != tc.invalid {
				t.Errorf("invalid run id = %v, want %v (err=%v)", got, tc.invalid, err)
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

		requestTickAndCycle(t, d)

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

// A request with no configured window still runs its cycle; a deferral then
// records nothing against a window that does not exist.
func TestMaintenanceRequestWithNoWindowRuns(t *testing.T) {
	t.Parallel()

	t.Run("the requested cycle runs", func(t *testing.T) {
		t.Parallel()
		d, f := requestDaemon(t, outsideRequestNow)
		d.patrolConfig = maintenanceWindowAt("")
		path := writeRequestFile(t, d, "run-nowindow", outsideRequestNow)

		requestTickAndCycle(t, d)

		if got := strings.Join(f.gcCalls, ","); got != "hq" {
			t.Fatalf("gc calls = %q, want hq with no window configured", got)
		}
		res := readRequestResult(t, d)
		if res.RunID != "run-nowindow" || res.Outcome != "completed" {
			t.Errorf("result = %+v, want run-nowindow completed", res)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("request still there after a completed cycle: %v", err)
		}
		st, err := loadMaintenanceGCState(d.config.TownRoot)
		if err != nil {
			t.Fatal(err)
		}
		if st.PendingDeferral != nil || st.ConsecutiveDeferredWindows != 0 {
			t.Errorf("a no-window request recorded a deferral streak: %+v", st.PendingDeferral)
		}
	})

	t.Run("a deferral records nothing", func(t *testing.T) {
		t.Parallel()
		d, f := requestDaemon(t, outsideRequestNow)
		d.patrolConfig = maintenanceWindowAt("")
		f.quietUntil = 0
		path := writeRequestFile(t, d, "run-nowindow-busy", outsideRequestNow)

		requestTickAndCycle(t, d)

		if len(f.gcCalls) != 0 {
			t.Fatalf("gc ran while the town was busy: %v", f.gcCalls)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("request gone after a deferral: %v", err)
		}
		if _, err := os.Stat(resultPath(update.Dir(d.config.TownRoot))); !os.IsNotExist(err) {
			t.Errorf("a deferred cycle wrote a result")
		}
		st, err := loadMaintenanceGCState(d.config.TownRoot)
		if err != nil {
			t.Fatal(err)
		}
		if st.PendingDeferral != nil || st.ConsecutiveDeferredWindows != 0 || len(st.DeferralReasons) != 0 {
			t.Errorf("a deferral was recorded against a window that does not exist: %+v", st)
		}
	})
}

// A completed cycle whose result write fails is re-answered from the state on
// a later tick; the cycle itself is not run again.
func TestMaintenanceRequestResultWriteFailureDoesNotRerun(t *testing.T) {
	t.Parallel()
	d, f := requestDaemon(t, outsideRequestNow)
	dir := update.Dir(d.config.TownRoot)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory where the result belongs makes the first write fail.
	blocked := resultPath(dir)
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	path := writeRequestFile(t, d, "run-retry", outsideRequestNow)

	requestTickAndCycle(t, d)

	if got := strings.Join(f.gcCalls, ","); got != "hq" {
		t.Fatalf("first tick gc calls = %q, want hq", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("request removed although the result write failed: %v", err)
	}
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	if st.LastRequestAnswer == nil || st.LastRequestAnswer.RunID != "run-retry" || st.LastRequestAnswer.Outcome != "completed" {
		t.Fatalf("remembered answer = %+v, want run-retry completed", st.LastRequestAnswer)
	}

	// Clear the obstruction; the next tick must answer, not gc.
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	// Move the clock on, so a rerun would answer with this tick's time.
	later := outsideRequestNow.Add(30 * time.Hour)
	d.maint.now = func() time.Time { return later }
	d.runScheduledMaintenance()

	if len(f.gcCalls) != 1 {
		t.Fatalf("gc calls = %v after the re-answer, want only the first tick's cycle", f.gcCalls)
	}
	res := readRequestResult(t, d)
	if res.RunID != "run-retry" || res.Outcome != "completed" {
		t.Errorf("result = %+v, want run-retry completed", res)
	}
	// A rerun would answer with the second tick's clock; the remembered answer
	// carries the first cycle's.
	if !res.At.Equal(outsideRequestNow) {
		t.Errorf("result at = %v, want the remembered cycle's %v — the cycle reran", res.At, outsideRequestNow)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("request still there after the re-answer: %v", err)
	}
}

// A cycle the daemon cannot run at all is answered failed with the reason and
// removed, so the job never waits out its timeout for a request.
func TestMaintenanceRequestUnanswerablePathsWriteFailed(t *testing.T) {
	t.Parallel()

	t.Run("no databases", func(t *testing.T) {
		t.Parallel()
		d, f := requestDaemon(t, outsideRequestNow)
		f.sizes = map[string]gcMeasure{}
		path := writeRequestFile(t, d, "run-nodb", outsideRequestNow)

		d.runScheduledMaintenance()

		if len(f.gcCalls) != 0 {
			t.Errorf("gc calls = %v with no databases", f.gcCalls)
		}
		res := readRequestResult(t, d)
		if res.RunID != "run-nodb" || res.Outcome != "failed" || !strings.Contains(res.Detail, "no databases") {
			t.Errorf("result = %+v, want run-nodb failed naming the missing databases", res)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("request still there: %v", err)
		}
	})

	t.Run("no local server", func(t *testing.T) {
		t.Parallel()
		d, f := requestDaemon(t, outsideRequestNow)
		d.maint.gcExternal = func(*Daemon) (bool, string) { return true, "Dolt server is externally managed" }
		// A failed request is not the scheduled window: it must not consume the
		// gap a later cycle would have to wait out.
		lastRun := outsideRequestNow.Add(-48 * time.Hour)
		d.lastMaintenanceRun = lastRun
		path := writeRequestFile(t, d, "run-remote", outsideRequestNow)

		d.runScheduledMaintenance()

		if len(f.gcCalls) != 0 {
			t.Errorf("gc calls = %v against a remote server", f.gcCalls)
		}
		res := readRequestResult(t, d)
		if res.RunID != "run-remote" || res.Outcome != "failed" || !strings.Contains(res.Detail, "local server") {
			t.Errorf("result = %+v, want run-remote failed naming the local-server requirement", res)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("request still there: %v", err)
		}
		if !d.lastMaintenanceRun.Equal(lastRun) {
			t.Errorf("lastMaintenanceRun = %v, want %v — a failed request must not count as a cycle", d.lastMaintenanceRun, lastRun)
		}
	})
}

// The untrusted run id is never echoed: an id over the bound or outside
// [A-Za-z0-9._-] is answered with an id-less failed result and removed.
func TestMaintenanceRequestRunIDIsBounded(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, runID string }{
		{"too long", strings.Repeat("a", 65)},
		{"a slash", "run/1"},
		{"a newline", "run-1\nscheduled_maintenance: WARNING: forged"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, f, logs := requestDaemonWithLogs(t, outsideRequestNow)
			path := writeRequestFile(t, d, tc.runID, outsideRequestNow)

			d.runScheduledMaintenance()

			if len(f.gcCalls) != 0 {
				t.Errorf("gc calls = %v for an invalid run id", f.gcCalls)
			}
			res := readRequestResult(t, d)
			if res.Outcome != "failed" || !strings.Contains(res.Detail, "invalid run id") {
				t.Errorf("result = %+v, want a failed result naming the invalid run id", res)
			}
			if res.RunID != "" {
				t.Errorf("the untrusted run id was echoed into the result: %q", res.RunID)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("request still there: %v", err)
			}
			if strings.Contains(logs.String(), tc.runID) {
				t.Errorf("the untrusted run id reached the log:\n%s", logs.String())
			}
		})
	}
}

// A cycle answers the request it started with. A request written while it runs
// belongs to another job and must survive, then be answered by its own cycle.
func TestMaintenanceRequestAnswerKeepsANewerRequest(t *testing.T) {
	t.Parallel()
	d, f := requestDaemon(t, outsideRequestNow)
	var cycle func()
	d.maint.dispatch = func(fn func()) { cycle = fn }
	path := writeRequestFile(t, d, "run-a", outsideRequestNow)

	d.runScheduledMaintenance() // starts A's cycle; it has not run yet

	if cycle == nil {
		t.Fatal("the tick dispatched no cycle for the request")
	}
	// Request B replaces A's while A's cycle is still in flight.
	if err := os.WriteFile(path, requestBytes(t, "run-b"), 0o600); err != nil {
		t.Fatal(err)
	}
	cycle()

	if got := strings.Join(f.gcCalls, ","); got != "hq" {
		t.Fatalf("gc calls = %q, want hq", got)
	}
	if res := readRequestResult(t, d); res.RunID != "run-a" || res.Outcome != "completed" {
		t.Errorf("result = %+v, want run-a completed", res)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("answering run-a removed run-b's request: %v", err)
	}

	// B's own cycle runs on a later tick and answers it.
	d.maint.dispatch = func(fn func()) { fn() }
	later := outsideRequestNow.Add(7 * 24 * time.Hour)
	d.maint.now = func() time.Time { return later }
	requestTickAndCycle(t, d)

	if got := strings.Join(f.gcCalls, ","); got != "hq,hq" {
		t.Errorf("gc calls = %q, want a second cycle for run-b", got)
	}
	res := readRequestResult(t, d)
	if res.RunID != "run-b" || res.Outcome != "completed" || !res.At.Equal(later) {
		t.Errorf("result = %+v, want run-b completed by its own cycle at %v", res, later)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("run-b's request is still there after its cycle answered it: %v", err)
	}
}

// A request that arrives while a cycle is already running cannot always be
// served by this tick: the tick answers it rather than leaving it to the job's
// timeout.
func TestMaintenanceRequestBusyCycleAnswersDeferred(t *testing.T) {
	t.Parallel()

	t.Run("a request the running cycle is not serving is left alone", func(t *testing.T) {
		t.Parallel()
		d, f := requestDaemon(t, outsideRequestNow)
		d.maintenanceGCRunning.Store(true)
		d.maintenanceGCRequestRunID.Store("run-running")
		path := writeRequestFile(t, d, "run-waiting", outsideRequestNow)

		d.runScheduledMaintenance()

		if len(f.gcCalls) != 0 {
			t.Errorf("gc calls = %v while a cycle is already running", f.gcCalls)
		}
		if _, err := os.Stat(resultPath(update.Dir(d.config.TownRoot))); !os.IsNotExist(err) {
			t.Errorf("a result was written for a request the running cycle is not serving (the shared file may hold the running cycle's answer): %v", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the request the running cycle is not serving was removed: %v", err)
		}
	})

	t.Run("the running cycle's own request is answered and released", func(t *testing.T) {
		t.Parallel()
		d, f := requestDaemon(t, outsideRequestNow)
		d.maintenanceGCRunning.Store(true)
		d.maintenanceGCRequestRunID.Store("run-own")
		path := writeRequestFile(t, d, "run-own", outsideRequestNow)

		d.runScheduledMaintenance()

		if len(f.gcCalls) != 0 {
			t.Errorf("gc calls = %v while a cycle is already running", f.gcCalls)
		}
		res := readRequestResult(t, d)
		if res.RunID != "run-own" || res.Outcome != "deferred" {
			t.Errorf("result = %+v, want run-own deferred", res)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the request the running cycle is serving was kept: %v", err)
		}
	})

	t.Run("a scheduled cycle serves no request", func(t *testing.T) {
		t.Parallel()
		d, f := requestDaemon(t, outsideRequestNow)
		d.maintenanceGCRunning.Store(true) // no request run id recorded
		path := writeRequestFile(t, d, "run-scheduled", outsideRequestNow)

		d.runScheduledMaintenance()

		if len(f.gcCalls) != 0 {
			t.Errorf("gc calls = %v while a cycle is already running", f.gcCalls)
		}
		if _, err := os.Stat(resultPath(update.Dir(d.config.TownRoot))); !os.IsNotExist(err) {
			t.Errorf("a result was written for a request a scheduled cycle is not serving: %v", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the request a scheduled cycle is not serving was removed: %v", err)
		}
	})
}

// An unreadable maintenance state could hide an answer already given. The tick
// answers the request failed instead of running the cycle again.
func TestMaintenanceRequestUnreadableStateDefers(t *testing.T) {
	t.Parallel()
	// Inside the window with a week-old gc, a tick that went on as if it were
	// scheduled would find work and run it: the early return is what makes an
	// unreadable state stop the whole tick.
	d, f := requestDaemon(t, gcTestNow)
	recordCatchUpGC(t, d, "hq", gcTestNow.Add(-8*24*time.Hour))
	d.lastMaintenanceRun = gcTestNow.Add(-48 * time.Hour)
	if err := os.WriteFile(maintenanceGCStatePath(d.config.TownRoot), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeRequestFile(t, d, "run-state", gcTestNow)

	d.runScheduledMaintenance()

	if len(f.gcCalls) != 0 {
		t.Fatalf("gc calls = %v with an unreadable state: a rerun is the one thing it must not do", f.gcCalls)
	}
	res := readRequestResult(t, d)
	if res.RunID != "run-state" || res.Outcome != "failed" || !strings.Contains(res.Detail, "maintenance state") {
		t.Errorf("result = %+v, want run-state failed naming the unreadable state", res)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the request is still there after the tick answered it: %v", err)
	}
}

// Every way an answer can fail leaves an alarm in the log; a silent failure
// would read exactly like an answered request.
func TestMaintenanceRequestAnswerFailuresAreLogged(t *testing.T) {
	t.Parallel()

	t.Run("the cycle's result write", func(t *testing.T) {
		t.Parallel()
		d, _, logs := requestDaemonWithLogs(t, outsideRequestNow)
		blockResultPath(t, d)
		path := writeRequestFile(t, d, "run-unwritten", outsideRequestNow)

		requestTickAndCycle(t, d)

		if !strings.Contains(logs.String(), "cannot answer request for run run-unwritten") {
			t.Errorf("the failed result write was not logged:\n%s", logs.String())
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the request was removed although no result was written: %v", err)
		}
	})

	t.Run("the busy-cycle answer", func(t *testing.T) {
		t.Parallel()
		d, _, logs := requestDaemonWithLogs(t, outsideRequestNow)
		d.maintenanceGCRunning.Store(true)
		d.maintenanceGCRequestRunID.Store("run-busy")
		blockResultPath(t, d)
		path := writeRequestFile(t, d, "run-busy", outsideRequestNow)

		d.runScheduledMaintenance()

		if !strings.Contains(logs.String(), "cannot answer request for run run-busy") {
			t.Errorf("the failed busy-cycle answer was not logged:\n%s", logs.String())
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the request was removed although no result was written: %v", err)
		}
	})

	t.Run("the standby answer", func(t *testing.T) {
		t.Parallel()
		d, _, logs := requestDaemonWithLogs(t, outsideRequestNow)
		standbyConditions[1].set(t, d, outsideRequestNow) // town ESTOP
		blockResultPath(t, d)
		path := writeRequestFile(t, d, "run-estop", outsideRequestNow)

		d.runScheduledMaintenance()

		if !strings.Contains(logs.String(), "cannot answer request for run run-estop") {
			t.Errorf("the failed standby answer was not logged:\n%s", logs.String())
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the request was removed although no result was written: %v", err)
		}
	})

	t.Run("the remembered answer", func(t *testing.T) {
		t.Parallel()
		d, _, logs := requestDaemonWithLogs(t, outsideRequestNow)
		// The state is readable when the tick starts and unwritable once the
		// cycle runs: a directory takes the state file's place.
		statePath := maintenanceGCStatePath(d.config.TownRoot)
		d.maint.dispatch = func(fn func()) {
			if err := os.Remove(statePath); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(statePath, 0o700); err != nil {
				t.Fatal(err)
			}
			fn()
		}
		writeRequestFile(t, d, "run-remember", outsideRequestNow)

		d.runScheduledMaintenance()

		if !strings.Contains(logs.String(), "cannot remember the answer for run run-remember") {
			t.Errorf("the failed state write was not logged:\n%s", logs.String())
		}
	})

	t.Run("the invalid run id answer", func(t *testing.T) {
		t.Parallel()
		d, _, logs := requestDaemonWithLogs(t, outsideRequestNow)
		blockResultPath(t, d)
		path := writeRequestFile(t, d, "run/1", outsideRequestNow)

		d.runScheduledMaintenance()

		if !strings.Contains(logs.String(), "cannot answer the request") {
			t.Errorf("the failed invalid-id answer was not logged:\n%s", logs.String())
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the unanswerable request was removed: %v", err)
		}
	})
}

// The shared result file may already hold the running cycle's own answer for
// its job. A busy tick must not write a deferral over it, and a request the
// cycle serves whose answer is already there is left for the cycle's removal.
func TestMaintenanceRequestBusyTickKeepsTheCyclesCompletedResult(t *testing.T) {
	t.Parallel()
	d, f := requestDaemon(t, outsideRequestNow)
	d.maintenanceGCRunning.Store(true)
	d.maintenanceGCRequestRunID.Store("run-own")
	path := writeRequestFile(t, d, "run-own", outsideRequestNow)
	done := outsideRequestNow.Add(time.Minute)
	if err := writeMaintenanceResult(osRequestFiles{}, update.Dir(d.config.TownRoot), "run-own", done, "completed", "gc done"); err != nil {
		t.Fatal(err)
	}

	d.runScheduledMaintenance()

	if len(f.gcCalls) != 0 {
		t.Errorf("gc calls = %v while a cycle is already running", f.gcCalls)
	}
	res := readRequestResult(t, d)
	if res.RunID != "run-own" || res.Outcome != "completed" || !res.At.Equal(done) {
		t.Errorf("result = %+v, want the cycle's completed answer untouched", res)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the request was removed by the busy tick, not the cycle that answered it: %v", err)
	}
}

// replacingRequestFiles reads a malformed request the first time and a valid
// request from another job every time after: the file was replaced between
// the tick's read and its removal. It records what was removed.
type replacingRequestFiles struct {
	reads   int
	removed []string
}

func (f *replacingRequestFiles) ReadFile(path string) ([]byte, error) {
	f.reads++
	if f.reads == 1 {
		return []byte("{not json"), nil
	}
	return json.Marshal(update.RequestFile{RunID: "run-newer", At: outsideRequestNow})
}
func (f *replacingRequestFiles) WriteFile(string, []byte, os.FileMode) error { return nil }
func (f *replacingRequestFiles) Remove(path string) error {
	f.removed = append(f.removed, path)
	return nil
}

// Dropping a malformed or invalid-id request re-reads the file first: a valid
// request another job wrote meanwhile is that job's work and is kept.
func TestMaintenanceRequestDropKeepsAReplacementRequest(t *testing.T) {
	t.Parallel()
	t.Run("malformed", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		files := &replacingRequestFiles{reads: 1} // the tick already made its read
		d.dropMalformedRequest(files, t.TempDir(), errMalformedRequest)
		if len(files.removed) != 0 {
			t.Errorf("removed %v: a valid request had replaced the malformed one", files.removed)
		}
	})
	t.Run("invalid run id", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		files := &replacingRequestFiles{reads: 1} // the tick already made its read
		d.answerInvalidRequest(files, t.TempDir(), errInvalidRunID)
		if len(files.removed) != 0 {
			t.Errorf("removed %v: a valid request had replaced the invalid one", files.removed)
		}
	})
}
