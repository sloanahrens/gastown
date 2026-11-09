package townstate

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// cutoverTime is the cut-over the fixture towns record. Every log line and
// landing is placed relative to it.
var cutoverTime = time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local)

const defaultRigsJSON = `{"version":1,"rigs":{"gastown":{"git_url":"https://example.invalid/gastown.git"}}}`

// fakeAnswer is what the fake host answers one command with: how long it took,
// its output, its exit status, and an error when it could not start.
type fakeAnswer struct {
	took time.Duration
	out  string
	exit int
	err  error
}

// fakeHost is the clock and runner the checks run through. The clock advances
// by each answer's duration, so a check times what the fake reports and nothing
// else.
type fakeHost struct {
	now    time.Time
	answer func(dir, name string, args []string) fakeAnswer
}

func (h *fakeHost) env(town string) Env {
	return Env{
		Town: town,
		GT:   "gt",
		Now:  func() time.Time { return h.now },
		Run: func(_ context.Context, dir, name string, args ...string) (string, int, error) {
			a := h.answer(dir, name, args)
			h.now = h.now.Add(a.took)
			return a.out, a.exit, a.err
		},
	}
}

func newFakeHost(answer func(dir, name string, args []string) fakeAnswer) *fakeHost {
	return &fakeHost{now: cutoverTime.Add(time.Hour), answer: answer}
}

// gitAnswer answers each git run with the next duration in runs for its
// command, in order. A run past the list takes 100ms.
func gitAnswer(runs map[string][]time.Duration) func(dir, name string, args []string) fakeAnswer {
	seen := map[string]int{}
	return func(_, _ string, args []string) fakeAnswer {
		key := strings.Join(args, " ")
		i := seen[key]
		seen[key]++
		if i < len(runs[key]) {
			return fakeAnswer{took: runs[key][i]}
		}
		return fakeAnswer{took: 100 * time.Millisecond}
	}
}

// townFiles is what a fixture town holds beyond the default one rig. An empty
// field leaves that file absent.
type townFiles struct {
	rigsJSON   string
	daemonJSON string
	cutover    string
	log        string
	baseline   *GitBaseline
	landings   map[string][]land.LandingRecord
	// files holds raw content for paths under the town root, written after the
	// fields above so it overrides them. dirs are paths made as directories.
	files map[string]string
	dirs  []string
}

// writeTown lays the files out under a fresh town root and returns the root.
func writeTown(t *testing.T, tf townFiles) string {
	t.Helper()
	town := t.TempDir()
	put := func(rel, body string) {
		t.Helper()
		path := filepath.Join(town, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rigs := tf.rigsJSON
	if rigs == "" {
		rigs = defaultRigsJSON
	}
	put("mayor/rigs.json", rigs)
	if tf.daemonJSON != "" {
		put("mayor/daemon.json", tf.daemonJSON)
	}
	if tf.cutover != "" {
		put(CutoverRecordRel, tf.cutover)
	}
	if tf.log != "" {
		put(daemonLogRel, tf.log)
	}
	if tf.baseline != nil {
		data, err := json.Marshal(tf.baseline)
		if err != nil {
			t.Fatal(err)
		}
		put(gitBaselineRel, string(data))
	}
	for rig, recs := range tf.landings {
		f, err := land.RigLandingsFile(town, rig)
		if err != nil {
			t.Fatal(err)
		}
		var body strings.Builder
		for _, rec := range recs {
			line, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			body.Write(append(line, '\n'))
		}
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.Path, []byte(body.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for rel, body := range tf.files {
		put(rel, body)
	}
	for _, rel := range tf.dirs {
		if err := os.MkdirAll(filepath.Join(town, filepath.FromSlash(rel)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return town
}

// cutoverJSON is the cut-over record `migrate copy --cutover` writes.
func cutoverJSON(at time.Time) string {
	return `{"cutover_at":"` + at.Format(time.RFC3339) + `","volume":"/volume","copy_seconds":1.5}`
}

// logAt is one daemon log line stamped the way the daemon's logger stamps it.
func logAt(at time.Time, text string) string {
	return at.Format(daemonLogLayout) + " " + text + "\n"
}

// findRow returns the row whose check starts with prefix, failing the test when
// there is none.
func findRow(t *testing.T, rows []Row, prefix string) Row {
	t.Helper()
	for _, r := range rows {
		if strings.HasPrefix(r.Check, prefix) {
			return r
		}
	}
	t.Fatalf("no row starts with %q in %+v", prefix, rows)
	return Row{}
}

func TestCheckGitSpeedAgainstTheBaseline(t *testing.T) {
	t.Parallel()
	baseline := func(status, fetch float64) *GitBaseline {
		return &GitBaseline{RecordedAt: cutoverTime.Add(-time.Hour), Rigs: map[string]map[string]float64{
			"gastown": {"status --short": status, "fetch --dry-run": fetch},
		}}
	}
	tests := []struct {
		name     string
		baseline *GitBaseline
		runs     map[string][]time.Duration
		failRow  string // the row that must fail; empty when every row must pass
		wantText string // a substring of that row's result
	}{
		{
			name:     "faster than the baseline passes",
			baseline: baseline(0.10, 2.0),
			runs: map[string][]time.Duration{
				"status --short":  {50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond},
				"fetch --dry-run": {time.Second, time.Second, time.Second},
			},
		},
		{
			name:     "a median over twice the baseline fails, naming the rig",
			baseline: baseline(0.10, 2.0),
			runs: map[string][]time.Duration{
				"status --short": {250 * time.Millisecond, 250 * time.Millisecond, 250 * time.Millisecond},
			},
			failRow:  "git speed gastown: status --short",
			wantText: "rig gastown takes 0.25s, over 2.0x",
		},
		{
			name:     "one slow run does not fail a median of three",
			baseline: baseline(0.10, 2.0),
			runs: map[string][]time.Duration{
				"status --short": {100 * time.Millisecond, 100 * time.Millisecond, 5 * time.Second},
			},
		},
		{
			name:     "a median over three seconds fails whatever the baseline",
			baseline: baseline(0.10, 2.0),
			runs: map[string][]time.Duration{
				"fetch --dry-run": {3500 * time.Millisecond, 3500 * time.Millisecond, 3500 * time.Millisecond},
			},
			failRow:  "git speed gastown: fetch --dry-run",
			wantText: "rig gastown takes 3.50s, over 3.0s",
		},
		{
			name: "a command the baseline does not name fails",
			baseline: &GitBaseline{RecordedAt: cutoverTime.Add(-time.Hour), Rigs: map[string]map[string]float64{
				"gastown": {"status --short": 0.10},
			}},
			failRow:  "git speed gastown: fetch --dry-run",
			wantText: "record it with gt town state check --record-baseline",
		},
		{
			name:     "a missing baseline fails with the instruction to record it",
			baseline: nil,
			failRow:  "git speed baseline",
			wantText: "record it with gt town state check --record-baseline",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			town := writeTown(t, townFiles{baseline: tt.baseline})
			host := newFakeHost(gitAnswer(tt.runs))
			rows := CheckGitSpeed(context.Background(), host.env(town))
			if tt.failRow == "" {
				if len(rows) != 2 {
					t.Fatalf("got %d rows, want the status and fetch rows: %+v", len(rows), rows)
				}
				for _, r := range rows {
					if !r.OK {
						t.Errorf("row %q did not pass: %s", r.Check, r.Result)
					}
				}
				return
			}
			r := findRow(t, rows, tt.failRow)
			if r.OK {
				t.Fatalf("row %q passed, want a failure: %+v", r.Check, r)
			}
			if !strings.Contains(r.Result, tt.wantText) {
				t.Errorf("row %q result = %q, want it to contain %q", r.Check, r.Result, tt.wantText)
			}
		})
	}
}

func TestCheckDaemonStartAndStatus(t *testing.T) {
	t.Parallel()
	start := cutoverTime.Add(30 * time.Second)
	healthy := logAt(start, "Daemon starting (PID 4)") + logAt(start.Add(time.Second), "Daemon running")
	tests := []struct {
		name         string
		out          string
		exit         int
		took         time.Duration
		log          string
		cutover      string
		wantFail     string // the row prefix that must fail; empty when every row must pass
		wantText     string
		wantMeasured string // a substring of the failing row's measured column
	}{
		{name: "a green daemon and a fast status line pass", out: "GREEN tick 2m ago\n", took: 300 * time.Millisecond, log: healthy, cutover: cutoverJSON(cutoverTime)},
		{name: "an unhealthy probe fails with the town line", out: "RED daemon down\n", exit: 2, took: 300 * time.Millisecond, log: healthy, cutover: cutoverJSON(cutoverTime), wantFail: "daemon health probe", wantText: `not GREEN: "RED daemon down"`},
		{name: "a slow gt status --line fails with the measurement", out: "GREEN tick 2m ago\n", took: 2500 * time.Millisecond, log: healthy, cutover: cutoverJSON(cutoverTime), wantFail: "gt status --line", wantText: "over 2s", wantMeasured: "2.50s"},
		{name: "a probe that does not answer within the bound fails", out: "GREEN\n", took: 6 * time.Second, log: healthy, cutover: cutoverJSON(cutoverTime), wantFail: "daemon health probe", wantText: "no answer within 5s"},
		{name: "a restart loop in the window fails with the start count", out: "GREEN\n", took: 300 * time.Millisecond, cutover: cutoverJSON(cutoverTime), log: healthy + logAt(start.Add(time.Minute), "Daemon starting (PID 5)") + logAt(start.Add(2*time.Minute), "Daemon starting (PID 6)"), wantFail: "daemon log: starts", wantText: "a restart loop", wantMeasured: "3 start(s)"},
		{name: "a panic in the window fails", out: "GREEN\n", took: 300 * time.Millisecond, cutover: cutoverJSON(cutoverTime), log: healthy + logAt(start.Add(time.Minute), "panic: runtime error: index out of range"), wantFail: "daemon log: panics", wantText: "1 panic line(s)"},
		{name: "no daemon start since the cut-over fails", out: "GREEN\n", took: 300 * time.Millisecond, cutover: cutoverJSON(cutoverTime), log: logAt(cutoverTime.Add(-time.Hour), "Daemon starting (PID 3)"), wantFail: "daemon log: starts", wantText: "no daemon start since the cut-over"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			town := writeTown(t, townFiles{cutover: tt.cutover, log: tt.log})
			host := newFakeHost(func(_, _ string, _ []string) fakeAnswer {
				return fakeAnswer{out: tt.out, exit: tt.exit, took: tt.took}
			})
			rows := CheckDaemonStart(context.Background(), host.env(town))
			if tt.wantFail == "" {
				for _, r := range rows {
					if !r.OK {
						t.Errorf("row %q did not pass: %s", r.Check, r.Result)
					}
				}
				return
			}
			r := findRow(t, rows, tt.wantFail)
			if r.OK {
				t.Fatalf("row %q passed, want a failure: %+v", r.Check, r)
			}
			if !strings.Contains(r.Result, tt.wantText) {
				t.Errorf("row %q result = %q, want it to contain %q", r.Check, r.Result, tt.wantText)
			}
			if tt.wantMeasured != "" && !strings.Contains(r.Measured, tt.wantMeasured) {
				t.Errorf("row %q measured = %q, want it to contain %q", r.Check, r.Measured, tt.wantMeasured)
			}
		})
	}
}

func TestCheckLandingSinceCutover(t *testing.T) {
	t.Parallel()
	landing := func(bead string, at time.Time, gate, merged string) land.LandingRecord {
		return land.LandingRecord{BeadID: bead, Rig: "gastown", LandedAt: at, GateResult: gate, LandedCommit: merged}
	}
	tests := []struct {
		name     string
		cutover  string
		rigsJSON string
		landings map[string][]land.LandingRecord
		wantOK   bool
		wantText string
	}{
		{
			name:     "no landing newer than the cut-over fails saying so",
			cutover:  cutoverJSON(cutoverTime),
			landings: map[string][]land.LandingRecord{"gastown": {landing("gt-a", cutoverTime.Add(-time.Hour), "gate ok", "abc123")}},
			wantText: "no landing since the cut-over",
		},
		{
			name:     "a landing exactly at the cut-over is not newer",
			cutover:  cutoverJSON(cutoverTime),
			landings: map[string][]land.LandingRecord{"gastown": {landing("gt-a", cutoverTime, "gate ok", "abc123")}},
			wantText: "no landing since the cut-over",
		},
		{
			name:     "a landing after the cut-over with its gate and merge passes",
			cutover:  cutoverJSON(cutoverTime),
			landings: map[string][]land.LandingRecord{"gastown": {landing("gt-b", cutoverTime.Add(10*time.Minute), "gate ok", "abc123")}},
			wantOK:   true,
		},
		{
			name:     "the newest landing across rigs decides, and without its merge it fails",
			cutover:  cutoverJSON(cutoverTime),
			rigsJSON: `{"version":1,"rigs":{"gastown":{"git_url":"https://example.invalid/g.git"},"other":{"git_url":"https://example.invalid/o.git"}}}`,
			landings: map[string][]land.LandingRecord{
				"gastown": {landing("gt-c", cutoverTime.Add(5*time.Minute), "gate ok", "abc123")},
				"other":   {landing("gt-d", cutoverTime.Add(10*time.Minute), "gate ok", "")},
			},
			wantText: "no merged commit",
		},
		{
			name:     "the newest landing without a gate result fails",
			cutover:  cutoverJSON(cutoverTime),
			landings: map[string][]land.LandingRecord{"gastown": {landing("gt-e", cutoverTime.Add(time.Minute), "", "abc123")}},
			wantText: "no gate result",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			town := writeTown(t, townFiles{rigsJSON: tt.rigsJSON, cutover: tt.cutover, landings: tt.landings})
			r := CheckLandingSinceCutover(Env{Town: town})
			if tt.wantOK {
				if !r.OK {
					t.Fatalf("landing check failed: %s", r.Result)
				}
				if !strings.Contains(r.Measured, "gt-b") {
					t.Errorf("measured = %q, want the newest landing named", r.Measured)
				}
				return
			}
			if r.OK {
				t.Fatalf("landing check passed, want a failure: %+v", r)
			}
			if !strings.Contains(r.Result, tt.wantText) {
				t.Errorf("result = %q, want it to contain %q", r.Result, tt.wantText)
			}
		})
	}
}

func TestCheckNotRunIsAFailure(t *testing.T) {
	t.Parallel()
	notFound := errors.New("executable file not found in the search path")
	t.Run("a probe that cannot start is not run, and counts as a failure", func(t *testing.T) {
		town := writeTown(t, townFiles{cutover: cutoverJSON(cutoverTime), log: logAt(cutoverTime.Add(time.Minute), "Daemon starting (PID 4)")})
		host := newFakeHost(func(_, _ string, _ []string) fakeAnswer {
			return fakeAnswer{took: time.Millisecond, err: notFound}
		})
		rows := CheckDaemonStart(context.Background(), host.env(town))
		for _, prefix := range []string{"daemon health probe", "gt status --line"} {
			r := findRow(t, rows, prefix)
			if r.OK || !strings.HasPrefix(r.Result, "not run: ") || !strings.Contains(r.Result, notFound.Error()) {
				t.Errorf("row %q = %+v, want not run with the reason", prefix, r)
			}
		}
		if DayOneFailures(rows) < 2 {
			t.Errorf("DayOneFailures = %d, want the two probe rows counted", DayOneFailures(rows))
		}
		data, err := json.Marshal(rows)
		if err != nil {
			t.Fatal(err)
		}
		var got []Row
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, rows) {
			t.Errorf("JSON rows = %+v, want the same rows as the table %+v", got, rows)
		}
	})
	t.Run("a git command that cannot start is not run for its rig", func(t *testing.T) {
		town := writeTown(t, townFiles{baseline: &GitBaseline{Rigs: map[string]map[string]float64{"gastown": {"status --short": 0.1, "fetch --dry-run": 0.1}}}})
		host := newFakeHost(func(_, _ string, _ []string) fakeAnswer {
			return fakeAnswer{took: time.Millisecond, err: notFound}
		})
		rows := CheckGitSpeed(context.Background(), host.env(town))
		if len(rows) != 2 {
			t.Fatalf("got %d rows, want one per git command: %+v", len(rows), rows)
		}
		for _, r := range rows {
			if r.OK || !strings.HasPrefix(r.Result, "not run: ") {
				t.Errorf("row %q = %+v, want not run", r.Check, r)
			}
		}
	})
	t.Run("a missing cut-over record is not run for the log and the landing", func(t *testing.T) {
		town := writeTown(t, townFiles{log: logAt(cutoverTime, "Daemon starting (PID 4)")})
		host := newFakeHost(func(_, _ string, _ []string) fakeAnswer {
			return fakeAnswer{out: "GREEN\n", took: time.Millisecond}
		})
		rows := CheckDaemonStart(context.Background(), host.env(town))
		landing := CheckLandingSinceCutover(host.env(town))
		for _, r := range append([]Row{landing}, findRow(t, rows, "daemon log: starts")) {
			if r.OK || !strings.Contains(r.Result, "no cut-over record") {
				t.Errorf("row %q = %+v, want not run for the missing cut-over record", r.Check, r)
			}
		}
	})
}

func TestCheckRecordBaseline(t *testing.T) {
	t.Parallel()
	recordAt := func(t *testing.T, town string, force bool, runs map[string][]time.Duration) (string, error) {
		t.Helper()
		host := newFakeHost(gitAnswer(runs))
		return RecordGitBaseline(context.Background(), host.env(town), force)
	}
	readBaseline := func(t *testing.T, town string) GitBaseline {
		t.Helper()
		gb, err := loadGitBaseline(town)
		if err != nil {
			t.Fatal(err)
		}
		return gb
	}

	t.Run("the baseline is written once, and a second record without force is refused", func(t *testing.T) {
		town := writeTown(t, townFiles{})
		if _, err := recordAt(t, town, false, nil); err != nil {
			t.Fatalf("first record: %v", err)
		}
		got := readBaseline(t, town).Rigs["gastown"]["status --short"]
		if math.Abs(got-0.1) > 1e-9 {
			t.Fatalf("recorded status median = %v, want 0.1", got)
		}
		path := filepath.Join(town, filepath.FromSlash(gitBaselineRel))
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = recordAt(t, town, false, nil)
		if err == nil || !strings.Contains(err.Error(), "already recorded") {
			t.Fatalf("second record err = %v, want the already-recorded refusal", err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Error("the refused record changed the baseline file")
		}
	})

	t.Run("force replaces the baseline", func(t *testing.T) {
		town := writeTown(t, townFiles{})
		if _, err := recordAt(t, town, false, nil); err != nil {
			t.Fatalf("first record: %v", err)
		}
		slow := map[string][]time.Duration{"status --short": {200 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond}}
		if _, err := recordAt(t, town, true, slow); err != nil {
			t.Fatalf("forced record: %v", err)
		}
		got := readBaseline(t, town).Rigs["gastown"]["status --short"]
		if math.Abs(got-0.2) > 1e-9 {
			t.Fatalf("forced status median = %v, want 0.2", got)
		}
	})

	t.Run("an unreadable rig list fails naming the settings key and writes nothing", func(t *testing.T) {
		town := writeTown(t, townFiles{daemonJSON: "{not json"})
		_, err := recordAt(t, town, false, nil)
		if err == nil || !strings.Contains(err.Error(), "patrols.landing_worker.rigs") {
			t.Fatalf("err = %v, want the settings key named", err)
		}
		if _, statErr := os.Stat(filepath.Join(town, filepath.FromSlash(gitBaselineRel))); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("a baseline was written after the refusal: %v", statErr)
		}
	})

	t.Run("an unreadable rig repository fails naming the rig and writes nothing", func(t *testing.T) {
		town := writeTown(t, townFiles{})
		host := newFakeHost(func(_, _ string, _ []string) fakeAnswer {
			return fakeAnswer{took: time.Millisecond, exit: 128}
		})
		_, err := RecordGitBaseline(context.Background(), host.env(town), false)
		if err == nil || !strings.Contains(err.Error(), "rig gastown") {
			t.Fatalf("err = %v, want the rig named", err)
		}
		if _, statErr := os.Stat(filepath.Join(town, filepath.FromSlash(gitBaselineRel))); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("a baseline was written after the refusal: %v", statErr)
		}
	})

	t.Run("a registry name that is not a rig directory is refused before any path is built", func(t *testing.T) {
		town := writeTown(t, townFiles{rigsJSON: `{"version":1,"rigs":{"../escape":{"git_url":"https://example.invalid/e.git"}}}`})
		_, err := recordAt(t, town, false, nil)
		if err == nil || !strings.Contains(err.Error(), `"../escape"`) {
			t.Fatalf("err = %v, want the bad rig name refused", err)
		}
		if _, statErr := os.Stat(filepath.Join(town, filepath.FromSlash(gitBaselineRel))); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("a baseline was written after the refusal: %v", statErr)
		}
	})

	t.Run("the landing worker's rig list narrows the baseline", func(t *testing.T) {
		town := writeTown(t, townFiles{
			rigsJSON:   `{"version":1,"rigs":{"gastown":{"git_url":"https://example.invalid/g.git"},"other":{"git_url":"https://example.invalid/o.git"}}}`,
			daemonJSON: `{"patrols":{"landing_worker":{"enabled":true,"rigs":["gastown"]}}}`,
		})
		if _, err := recordAt(t, town, false, nil); err != nil {
			t.Fatalf("record: %v", err)
		}
		if rigs := readBaseline(t, town).Rigs; len(rigs) != 1 || rigs["gastown"] == nil {
			t.Fatalf("baseline rigs = %v, want only gastown", rigs)
		}
	})
}

// TestCheckGitSpeedRefusesNoRigToTime drives the two ways the rig list can name
// nothing to time: an empty registry, and a landing worker list that names no
// registered rig. Both leave git unmeasured, so the check must fail, not return
// no rows, and must run no git command at all.
func TestCheckGitSpeedRefusesNoRigToTime(t *testing.T) {
	t.Parallel()
	baseline := &GitBaseline{Rigs: map[string]map[string]float64{
		"gastown": {"status --short": 0.1, "fetch --dry-run": 0.1},
	}}
	tests := []struct {
		name       string
		rigsJSON   string
		daemonJSON string
	}{
		{name: "an empty rig registry", rigsJSON: `{"version":1,"rigs":{}}`},
		{name: "a landing worker list that names no registered rig", daemonJSON: `{"patrols":{"landing_worker":{"enabled":true,"rigs":["nowhere"]}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := writeTown(t, townFiles{rigsJSON: tt.rigsJSON, daemonJSON: tt.daemonJSON, baseline: baseline})
			gitRuns := 0
			host := newFakeHost(func(_, _ string, _ []string) fakeAnswer {
				gitRuns++
				return fakeAnswer{took: 100 * time.Millisecond}
			})
			rows := CheckGitSpeed(context.Background(), host.env(town))
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want the one unmet row: %+v", len(rows), rows)
			}
			r := rows[0]
			if r.OK || !strings.Contains(r.Result, "git speed not measured: no rig to time") {
				t.Errorf("row = %+v, want an unmet row saying git speed was not measured", r)
			}
			if DayOneFailures(rows) != 1 {
				t.Errorf("DayOneFailures = %d, want the unmet row counted so the command exits non-zero", DayOneFailures(rows))
			}
			if gitRuns != 0 {
				t.Errorf("%d git runs, want none when there is no rig to time", gitRuns)
			}
		})
	}
}

// TestGitSpeedRunsInEachRigsMayorClone asserts the directory every git command
// runs in: each rig's mayor/rig clone, which is the work tree git status needs.
// Both the check and the baseline record run through the same rig list.
func TestGitSpeedRunsInEachRigsMayorClone(t *testing.T) {
	t.Parallel()
	rigsJSON := `{"version":1,"rigs":{"gastown":{"git_url":"https://example.invalid/g.git"},"other":{"git_url":"https://example.invalid/o.git"}}}`
	baseline := &GitBaseline{Rigs: map[string]map[string]float64{
		"gastown": {"status --short": 0.1, "fetch --dry-run": 0.1},
		"other":   {"status --short": 0.1, "fetch --dry-run": 0.1},
	}}
	// Each rig gets GitSpeedRuns runs of each of the two git commands.
	wantRuns := func(town string) map[string]int {
		return map[string]int{
			filepath.Join(town, "gastown", "mayor", "rig"): 2 * GitSpeedRuns,
			filepath.Join(town, "other", "mayor", "rig"):   2 * GitSpeedRuns,
		}
	}
	record := func(t *testing.T, dirs map[string]int) func(dir, name string, args []string) fakeAnswer {
		return func(dir, name string, _ []string) fakeAnswer {
			if name != "git" {
				t.Errorf("ran %q, want git", name)
			}
			dirs[dir]++
			return fakeAnswer{took: 100 * time.Millisecond}
		}
	}

	t.Run("the check runs each git command in each rig's mayor/rig clone", func(t *testing.T) {
		t.Parallel()
		town := writeTown(t, townFiles{rigsJSON: rigsJSON, baseline: baseline})
		dirs := map[string]int{}
		host := newFakeHost(record(t, dirs))
		CheckGitSpeed(context.Background(), host.env(town))
		if want := wantRuns(town); !reflect.DeepEqual(dirs, want) {
			t.Errorf("git ran in %v, want %v", dirs, want)
		}
	})

	t.Run("the baseline record runs each git command in each rig's mayor/rig clone", func(t *testing.T) {
		t.Parallel()
		town := writeTown(t, townFiles{rigsJSON: rigsJSON})
		dirs := map[string]int{}
		host := newFakeHost(record(t, dirs))
		if _, err := RecordGitBaseline(context.Background(), host.env(town), false); err != nil {
			t.Fatalf("record: %v", err)
		}
		if want := wantRuns(town); !reflect.DeepEqual(dirs, want) {
			t.Errorf("git ran in %v, want %v", dirs, want)
		}
	})
}

// TestCheckDaemonProbeQuotesTheLine drives a passing probe whose first line
// carries a control byte and whose later lines carry text. The table must show
// the first line quoted, so a raw byte or a later line never reaches the output.
func TestCheckDaemonProbeQuotesTheLine(t *testing.T) {
	t.Parallel()
	town := writeTown(t, townFiles{cutover: cutoverJSON(cutoverTime), log: logAt(cutoverTime.Add(time.Minute), "Daemon starting (PID 4)")})
	host := newFakeHost(func(_, _ string, _ []string) fakeAnswer {
		return fakeAnswer{out: "GREEN \x1b[2Jcleared\nsecond line with token=abc\n", took: 300 * time.Millisecond}
	})
	row := findRow(t, CheckDaemonStart(context.Background(), host.env(town)), "daemon health probe")
	if !row.OK {
		t.Fatalf("row = %+v, want the green probe to pass", row)
	}
	if want := strconv.Quote("GREEN \x1b[2Jcleared"); !strings.Contains(row.Measured, want) {
		t.Errorf("measured = %q, want the first line quoted as %s", row.Measured, want)
	}
	if strings.ContainsRune(row.Measured, '\x1b') || strings.Contains(row.Measured, "token=abc") {
		t.Errorf("measured = %q, want no raw control byte and no later line", row.Measured)
	}
}
