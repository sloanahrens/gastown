package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/jobledger"
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

			job := jobsRow(t, root, Options{}, []config.DaemonStep{{Name: "job", Kind: config.DaemonStepPatrol}}, "job")
			if job.State != c.want {
				t.Errorf("State = %q, want %q", job.State, c.want)
			}
		})
	}
}

func TestJobsRowPerStep(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	jobs, err := List(root, Options{}, testNow)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	steps := config.DaemonSteps()
	// A root with no maintenance state has no Dolt gc row, and its missing node
	// disk record is one unknown row, so the steps, the nightly backup and that
	// row make the whole list.
	if len(jobs) != len(steps)+2 {
		t.Fatalf("got %d rows, want %d daemon step rows plus the dolt_backup and node_disk rows", len(jobs), len(steps)+2)
	}
	for i, s := range steps {
		if jobs[i].Name != s.Name || jobs[i].Kind != s.Kind {
			t.Errorf("row %d = %s (%s), want %s (%s)", i, jobs[i].Name, jobs[i].Kind, s.Name, s.Kind)
		}
	}
	if _, ok := findJob(jobs, doltBackupJobName); !ok {
		t.Errorf("no %q row in %d rows", doltBackupJobName, len(jobs))
	}
	if _, ok := findJob(jobs, nodeDiskJobName); !ok {
		t.Errorf("no %q row in %d rows", nodeDiskJobName, len(jobs))
	}
	if _, ok := findJob(jobs, doltGCName); ok {
		t.Errorf("dolt_gc row present, want none without maintenance state")
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

	jobs, err := list(root, Options{}, []config.DaemonStep{{Name: "job", Kind: config.DaemonStepPatrol}}, testNow)
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

			if _, err := List(root, Options{}, testNow); err == nil {
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

// appendLedgerRuns writes runs to the town's run ledger in the order given.
func appendLedgerRuns(t *testing.T, root string, runs ...jobledger.Run) {
	t.Helper()
	ledger, err := jobledger.NewLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if err := ledger.Append(r); err != nil {
			t.Fatal(err)
		}
	}
}

// TestJobsLedgerSource is bead gt-pqgbd7: a job named in the run ledger takes
// its newest run's outcome, stats and time, the run's own next-due time (in
// place of the cadence's) and its newest five runs, and the array carries them.
func TestJobsLedgerSource(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeMayorDaemon(t, root, `{"job":{"interval":"1h"}}`)

	// Five older runs, then the newest. The newest is due five minutes ago,
	// while the cadence alone would put it due in 1h05m: only the run's own
	// next-due time makes the row overdue.
	older := make([]jobledger.Run, 0, 5)
	for i := range 5 {
		started := testNow.Add(-6*time.Hour + time.Duration(i)*30*time.Minute)
		older = append(older, jobledger.Run{
			Job:       "job",
			StartedAt: started,
			EndedAt:   started.Add(time.Minute),
			Outcome:   "ok",
			Stats:     map[string]int64{"bytes_freed": int64(i)},
		})
	}
	due := testNow.Add(-5 * time.Minute)
	newest := jobledger.Run{
		Job:       "job",
		StartedAt: testNow.Add(-11 * time.Minute),
		EndedAt:   testNow.Add(-10 * time.Minute),
		Outcome:   "failed",
		Stats:     map[string]int64{"bytes_freed": 4242, "trimmed_files": 7},
		NextDue:   &due,
	}
	appendLedgerRuns(t, root, append(older, newest)...)

	jobs, err := list(root, Options{}, []config.DaemonStep{{Name: "job", Kind: config.DaemonStepPatrol}}, testNow)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	row := jobs[0]

	if row.LastRun == nil || !row.LastRun.Equal(newest.EndedAt) {
		t.Errorf("LastRun = %v, want the newest run's end %v", row.LastRun, newest.EndedAt)
	}
	if row.Outcome != OutcomeFailed {
		t.Errorf("Outcome = %q, want failed from the newest run", row.Outcome)
	}
	if got := row.Stats["bytes_freed"]; got != 4242 {
		t.Errorf("Stats[bytes_freed] = %d, want 4242", got)
	}
	if got := row.Stats["trimmed_files"]; got != 7 {
		t.Errorf("Stats[trimmed_files] = %d, want 7", got)
	}
	if row.Due == nil || !row.Due.Equal(due) {
		t.Errorf("Due = %v, want the run's next due %v", row.Due, due)
	}
	if row.State != StateOverdue {
		t.Errorf("State = %q, want overdue by the run's own due time", row.State)
	}
	if len(row.Runs) != ledgerRunCap {
		t.Fatalf("got %d runs, want the newest %d", len(row.Runs), ledgerRunCap)
	}
	if got := row.Runs[0].Stats["bytes_freed"]; got != 1 {
		t.Errorf("Runs[0].bytes_freed = %d, want 1: the oldest of the newest five", got)
	}
	if got := row.Runs[len(row.Runs)-1]; got.Outcome != "failed" || got.Stats["bytes_freed"] != 4242 {
		t.Errorf("newest carried run = %+v, want the failed 4242 run last", got)
	}

	// The array gt jobs --json prints carries the same five runs and the due
	// time under the keys the dashboard reads.
	data, err := json.Marshal(jobs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var report []struct {
		Name    string           `json:"name"`
		State   string           `json:"state"`
		Outcome string           `json:"outcome"`
		LastRun *time.Time       `json:"last_run"`
		Due     *time.Time       `json:"due"`
		Stats   map[string]int64 `json:"stats"`
		Runs    []jobledger.Run  `json:"runs"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(report) != len(jobs) {
		t.Fatalf("JSON holds %d rows, want %d", len(report), len(jobs))
	}
	got := report[0]
	if got.Name != "job" || got.State != "overdue" || got.Outcome != "failed" {
		t.Errorf("JSON row = %+v, want job/overdue/failed", got)
	}
	if len(got.Runs) != ledgerRunCap {
		t.Errorf("JSON runs = %d, want %d", len(got.Runs), ledgerRunCap)
	}
	if got.Due == nil || !got.Due.Equal(due) {
		t.Errorf("JSON due = %v, want %v", got.Due, due)
	}
}

// TestJobsLedgerSourceUnreadable is bead gt-pqgbd7: a ledger that exists but
// cannot be read gives the job State unknown, never ok.
func TestJobsLedgerSourceUnreadable(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeMayorDaemon(t, root, `{"job":{"interval":"1h"}}`)
	// A directory where the live ledger file belongs: the ledger is there, but
	// every read of it fails.
	if err := os.MkdirAll(filepath.Join(root, ".runtime", "jobs", "runs.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}

	jobs, err := list(root, Options{}, []config.DaemonStep{{Name: "job", Kind: config.DaemonStepPatrol}}, testNow)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if jobs[0].State != StateUnknown {
		t.Errorf("State = %q, want unknown for an unreadable ledger", jobs[0].State)
	}
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

			jobs, err := List(root, Options{}, testNow)
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
