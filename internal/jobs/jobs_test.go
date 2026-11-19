package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

var testNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// writeFile creates path and its parents under the test's own directory.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeMayorDaemon writes mayor/daemon.json with the given patrols JSON.
func writeMayorDaemon(t *testing.T, root, patrols string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "mayor", "daemon.json"),
		`{"type":"daemon-patrol-config","version":1,"patrols":`+patrols+`}`)
}

// writeLastRun writes the daemon's last-run record with one entry per name.
func writeLastRun(t *testing.T, root string, at map[string]time.Time) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"last_run": at})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "daemon", "patrol_last_run.json"), string(data))
}

func TestJobsState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		patrols  string     // mayor/daemon.json patrols object
		lastRun  *time.Time // nil: no last-run entry for the job
		runsFile string     // raw last-run file; overrides lastRun when set
		want     State
	}{
		{
			name:    "never run with a cadence",
			patrols: `{"job":{"interval":"1h"}}`,
			want:    StateNeverRun,
		},
		{
			name:    "overdue just past the bound",
			patrols: `{"job":{"interval":"1h"}}`,
			lastRun: ptrTime(testNow.Add(-(time.Hour + 15*time.Minute + time.Second))),
			want:    StateOverdue,
		},
		{
			name:    "ok just inside the bound",
			patrols: `{"job":{"interval":"1h"}}`,
			lastRun: ptrTime(testNow.Add(-(time.Hour + 15*time.Minute))),
			want:    StateOK,
		},
		{
			name:    "ok inside the ten-minute floor for a short cadence",
			patrols: `{"job":{"interval":"5m"}}`,
			lastRun: ptrTime(testNow.Add(-15 * time.Minute)),
			want:    StateOK,
		},
		{
			name:    "overdue just past the ten-minute floor for a short cadence",
			patrols: `{"job":{"interval":"5m"}}`,
			lastRun: ptrTime(testNow.Add(-(15*time.Minute + time.Second))),
			want:    StateOverdue,
		},
		{
			name:    "ok within the cadence",
			patrols: `{"job":{"interval":"1h"}}`,
			lastRun: ptrTime(testNow.Add(-10 * time.Minute)),
			want:    StateOK,
		},
		{
			name:    "no cadence is never ok, even with a run recorded",
			patrols: `{"job":{"enabled":true}}`,
			lastRun: ptrTime(testNow.Add(-time.Hour)),
			want:    StateNoCadence,
		},
		{
			name:     "unreadable last-run file is unknown",
			patrols:  `{"job":{"interval":"1h"}}`,
			runsFile: `{"last_run":`,
			want:     StateUnknown,
		},
		{
			name:    "interval that is not a Go duration is unknown",
			patrols: `{"job":{"interval":"daily"}}`,
			want:    StateUnknown,
		},
		{
			name:    "negative interval is unknown",
			patrols: `{"job":{"interval":"-1h"}}`,
			want:    StateUnknown,
		},
		{
			name:    "patrol entry that is not an object is unknown",
			patrols: `{"job":"1h"}`,
			want:    StateUnknown,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeMayorDaemon(t, root, c.patrols)
			switch {
			case c.runsFile != "":
				writeFile(t, filepath.Join(root, "daemon", "patrol_last_run.json"), c.runsFile)
			case c.lastRun != nil:
				writeLastRun(t, root, map[string]time.Time{"job": *c.lastRun})
			}

			jobs, err := list(root, []config.DaemonStep{{Name: "job", Kind: config.DaemonStepPatrol}}, testNow)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(jobs) != 1 {
				t.Fatalf("got %d rows, want 1", len(jobs))
			}
			if jobs[0].State != c.want {
				t.Errorf("State = %q, want %q", jobs[0].State, c.want)
			}
		})
	}
}

func TestJobsRowPerStep(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	jobs, err := List(root, testNow)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	steps := config.DaemonSteps()
	if len(jobs) != len(steps) {
		t.Fatalf("got %d rows, want %d (one per daemon step)", len(jobs), len(steps))
	}
	for i, s := range steps {
		if jobs[i].Name != s.Name || jobs[i].Kind != s.Kind {
			t.Errorf("row %d = %s (%s), want %s (%s)", i, jobs[i].Name, jobs[i].Kind, s.Name, s.Kind)
		}
	}
}

func TestJobsCadenceFromSectionOverFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// The daemon section of settings/config.json is the host the daemon reads,
	// so its 2h interval wins over the leftover mayor/daemon.json's 1h.
	writeMayorDaemon(t, root, `{"job":{"interval":"1h"}}`)
	writeFile(t, filepath.Join(root, "settings", "config.json"),
		`{"daemon":{"type":"daemon-patrol-config","version":1,"patrols":{"job":{"interval":"2h"}}}}`)
	writeLastRun(t, root, map[string]time.Time{"job": testNow.Add(-90 * time.Minute)})

	jobs, err := list(root, []config.DaemonStep{{Name: "job", Kind: config.DaemonStepPatrol}}, testNow)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if jobs[0].Cadence != 2*time.Hour {
		t.Errorf("Cadence = %v, want 2h from the section", jobs[0].Cadence)
	}
	if want := `settings/config.json (section "daemon")`; jobs[0].Source != want {
		t.Errorf("Source = %q, want %q", jobs[0].Source, want)
	}
	if jobs[0].State != StateOK {
		t.Errorf("State = %q, want ok under the 2h cadence", jobs[0].State)
	}
}

func TestJobsUnreadablePatrolConfigIsError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{name: "corrupt JSON", body: `{"patrols":`},
		{name: "patrols is not an object", body: `{"patrols":"1h"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "mayor", "daemon.json"), c.body)

			if _, err := List(root, testNow); err == nil {
				t.Fatal("List returned no error for an unreadable patrol config")
			}
		})
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// findJob returns the row named name.
func findJob(jobs []Job, name string) (Job, bool) {
	for _, j := range jobs {
		if j.Name == name {
			return j, true
		}
	}
	return Job{}, false
}

func TestJobsDoltGC(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		state         string // maintenance_state.json body; empty means no file
		wantRow       bool
		wantLastRun   *time.Time
		wantReclaimed int64
		wantOutcome   Outcome
		wantState     State
	}{
		{
			name: "newest gc is the last run and the reclaimed bytes are the sum",
			state: `{"last_gc":{"hq":"2026-11-19T10:00:00Z","gt":"2026-11-19T11:00:00Z"},` +
				`"reclaimed_bytes":{"hq":1024,"gt":2048}}`,
			wantRow:       true,
			wantLastRun:   ptrTime(time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC)),
			wantReclaimed: 3072,
			wantOutcome:   OutcomeOK,
			wantState:     StateOK,
		},
		{
			name:        "state file that does not parse is failed and unknown",
			state:       `{"last_gc":`,
			wantRow:     true,
			wantOutcome: OutcomeFailed,
			wantState:   StateUnknown,
		},
		{
			name: "no state file is no row",
		},
		{
			name:        "state file with no gc recorded has no last run",
			state:       `{"consecutive_deferred_windows":0}`,
			wantRow:     true,
			wantOutcome: "",
			wantState:   StateNeverRun,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			if c.state != "" {
				writeFile(t, filepath.Join(root, "daemon", "maintenance_state.json"), c.state)
			}

			jobs, err := List(root, testNow)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			row, ok := findJob(jobs, doltGCName)
			if ok != c.wantRow {
				t.Fatalf("dolt_gc row present = %v, want %v", ok, c.wantRow)
			}
			if !c.wantRow {
				return
			}
			switch {
			case c.wantLastRun == nil && row.LastRun != nil:
				t.Errorf("LastRun = %v, want none", *row.LastRun)
			case c.wantLastRun != nil && (row.LastRun == nil || !row.LastRun.Equal(*c.wantLastRun)):
				t.Errorf("LastRun = %v, want %v", row.LastRun, *c.wantLastRun)
			}
			if got := row.Stats["reclaimed_bytes"]; got != c.wantReclaimed {
				t.Errorf("Stats[reclaimed_bytes] = %d, want %d", got, c.wantReclaimed)
			}
			if row.Outcome != c.wantOutcome {
				t.Errorf("Outcome = %q, want %q", row.Outcome, c.wantOutcome)
			}
			if row.State != c.wantState {
				t.Errorf("State = %q, want %q", row.State, c.wantState)
			}
			if row.Cadence != daemon.MaintenanceGCCadence {
				t.Errorf("Cadence = %v, want the gc's weekly cadence %v", row.Cadence, daemon.MaintenanceGCCadence)
			}
		})
	}
}
