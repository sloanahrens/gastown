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
			name:    "the daily word on a patrol other than disk_maintenance is unknown",
			patrols: `{"job":{"interval":"daily"}}`,
			want:    StateUnknown,
		},
		{
			name:    "interval that is not a Go duration is unknown",
			patrols: `{"job":{"interval":"banana"}}`,
			want:    StateUnknown,
		},
		{
			name:    "negative interval is unknown",
			patrols: `{"job":{"interval":"-1h"}}`,
			want:    StateUnknown,
		},
		{
			name:    "negative interval under an hour is unknown",
			patrols: `{"job":{"interval":"-5m"}}`,
			want:    StateUnknown,
		},
		{
			name:    "patrol entry that is not an object is unknown",
			patrols: `{"job":"1h"}`,
			want:    StateUnknown,
		},
		{
			name:    "enabled null is unknown, never ok",
			patrols: `{"job":{"interval":"1h","enabled":null}}`,
			lastRun: ptrTime(testNow.Add(-time.Minute)),
			want:    StateUnknown,
		},
		{
			name:    "enabled that is a string is unknown",
			patrols: `{"job":{"interval":"1h","enabled":"false"}}`,
			lastRun: ptrTime(testNow.Add(-time.Minute)),
			want:    StateUnknown,
		},
		{
			name:    "enabled that is a number is unknown",
			patrols: `{"job":{"interval":"1h","enabled":1}}`,
			lastRun: ptrTime(testNow.Add(-time.Minute)),
			want:    StateUnknown,
		},
		{
			name:    "enabled that is an object is unknown",
			patrols: `{"job":{"interval":"1h","enabled":{}}}`,
			lastRun: ptrTime(testNow.Add(-time.Minute)),
			want:    StateUnknown,
		},
		{
			name:    "enabled that is an array is unknown",
			patrols: `{"job":{"interval":"1h","enabled":[]}}`,
			lastRun: ptrTime(testNow.Add(-time.Minute)),
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

// TestJobsDisabledPatrolIsNotOverdue is bead gt-378vji: a patrol whose config
// entry says enabled false has nothing due, however old its last run, and keeps
// that run on the row; the same entry with enabled true or with no enabled key
// is judged by its cadence as before.
func TestJobsDisabledPatrolIsNotOverdue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		patrols string
		want    State
	}{
		{
			name:    "enabled false with a 19-day-old run is no_cadence",
			patrols: `{"job":{"interval":"1h","enabled":false}}`,
			want:    StateNoCadence,
		},
		{
			name:    "enabled true with a 19-day-old run is overdue",
			patrols: `{"job":{"interval":"1h","enabled":true}}`,
			want:    StateOverdue,
		},
		{
			name:    "no enabled key with a 19-day-old run is overdue",
			patrols: `{"job":{"interval":"1h"}}`,
			want:    StateOverdue,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeMayorDaemon(t, root, c.patrols)
			lastRun := testNow.Add(-19 * 24 * time.Hour)
			writeLastRun(t, root, map[string]time.Time{"job": lastRun})

			job := jobsRow(t, root, Options{}, []config.DaemonStep{{Name: "job", Kind: config.DaemonStepPatrol}}, "job")
			if job.State != c.want {
				t.Errorf("State = %q, want %q", job.State, c.want)
			}
			if job.LastRun == nil || !job.LastRun.Equal(lastRun) {
				t.Errorf("LastRun = %v, want the recorded run %v kept on the row", job.LastRun, lastRun)
			}
			if c.want == StateNoCadence && job.Due != nil {
				t.Errorf("Due = %v, want none: nothing is due for a patrol turned off", *job.Due)
			}
		})
	}
}

// TestJobsDisabledPatrolIgnoresRunDue is bead gt-378vji: a run the ledger holds
// for a patrol turned off supplies the row's last run, but its own next-due time
// does not judge the row, which stays no_cadence with nothing due.
func TestJobsDisabledPatrolIgnoresRunDue(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeMayorDaemon(t, root, `{"job":{"interval":"1h","enabled":false}}`)
	due := testNow.Add(-5 * time.Minute)
	appendLedgerRuns(t, root, jobledger.Run{
		Job:       "job",
		StartedAt: testNow.Add(-11 * time.Minute),
		EndedAt:   testNow.Add(-10 * time.Minute),
		Outcome:   "ok",
		NextDue:   &due,
	})

	job := jobsRow(t, root, Options{}, []config.DaemonStep{{Name: "job", Kind: config.DaemonStepPatrol}}, "job")
	if job.State != StateNoCadence {
		t.Errorf("State = %q, want no_cadence: the patrol is off, so the run's next-due time does not judge it", job.State)
	}
	if job.Due != nil {
		t.Errorf("Due = %v, want none for a patrol turned off", *job.Due)
	}
	if job.LastRun == nil || !job.LastRun.Equal(testNow.Add(-10*time.Minute)) {
		t.Errorf("LastRun = %v, want the ledger run's end kept on the row", job.LastRun)
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

func TestJobsCadenceDailyWord(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		lastRun *time.Time
		want    State
	}{
		{name: "no last run is never_run", want: StateNeverRun},
		{name: "last run 2h ago is ok", lastRun: ptrTime(testNow.Add(-2 * time.Hour)), want: StateOK},
		// A day's grace is a quarter of it, 6h, so the bound is 30h; the
		// overdue row sits a minute past it.
		{name: "last run past the day and its grace is overdue", lastRun: ptrTime(testNow.Add(-(30*time.Hour + time.Minute))), want: StateOverdue},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeMayorDaemon(t, root, `{"disk_maintenance":{"interval":"daily"}}`)
			if c.lastRun != nil {
				writeLastRun(t, root, map[string]time.Time{diskMaintenanceName: *c.lastRun})
			}
			step := []config.DaemonStep{{Name: diskMaintenanceName, Kind: config.DaemonStepPatrol}}
			job := jobsRow(t, root, Options{}, step, diskMaintenanceName)
			if job.Cadence != 24*time.Hour {
				t.Errorf("Cadence = %v, want 24h for the daily word", job.Cadence)
			}
			if job.State != c.want {
				t.Errorf("State = %q, want %q", job.State, c.want)
			}
		})
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

// TestJobsLedgerSourceKeepsUnknown is bead gt-pqgbd7 rework: a run that records
// its own next-due time does not judge a row its own reader could not read. The
// Dolt gc's state file does not parse, so the row is unknown; the ledger's clean
// run with a future due time must leave it unknown and failed.
func TestJobsLedgerSourceKeepsUnknown(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "daemon", "maintenance_state.json"), `{"last_gc":`)
	due := testNow.Add(time.Hour)
	appendLedgerRuns(t, root, jobledger.Run{
		Job:       doltGCName,
		StartedAt: testNow.Add(-2 * time.Minute),
		EndedAt:   testNow.Add(-time.Minute),
		Outcome:   "ok",
		NextDue:   &due,
	})

	jobs, err := List(root, Options{}, testNow)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	row, ok := findJob(jobs, doltGCName)
	if !ok {
		t.Fatalf("no %q row in %d rows", doltGCName, len(jobs))
	}
	if row.State != StateUnknown {
		t.Errorf("State = %q, want unknown: the state file does not parse, so a clean run cannot judge the row", row.State)
	}
	if row.Outcome != OutcomeFailed {
		t.Errorf("Outcome = %q, want failed: the clean run does not repair the unreadable state file", row.Outcome)
	}
	if row.Due == nil || !row.Due.Equal(due) {
		t.Errorf("Due = %v, want the run's recorded due time %v: a fact even though the row stays unknown", row.Due, due)
	}
	if len(row.Runs) != 1 {
		t.Errorf("got %d runs, want the run the ledger holds", len(row.Runs))
	}
}

// TestJobsLedgerSourceKeepsFailedOutcome is bead gt-pqgbd7 rework: a node whose
// probe failed keeps its failed outcome, its figures and its own state when the
// ledger holds a clean run under the row's name.
func TestJobsLedgerSourceKeepsFailedOutcome(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	readAt := testNow.Add(-5 * time.Minute)
	writeNodeDiskRecord(t, root, `{"nodes":{"colima-gt-control":`+
		`{"used_percent":91,"read_at":"`+readAt.Format(time.RFC3339)+`","error":"df: no such file or directory"}}}`)
	name := "node_disk:colima-gt-control"
	appendLedgerRuns(t, root, jobledger.Run{
		Job:       name,
		StartedAt: testNow.Add(-2 * time.Minute),
		EndedAt:   testNow.Add(-time.Minute),
		Outcome:   "ok",
	})

	jobs, err := List(root, Options{}, testNow)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	rows := nodeDiskRowsNamed(jobs, name)
	if len(rows) != 1 {
		t.Fatalf("got %d rows named %q, want one: %+v", len(rows), name, jobs)
	}
	row := rows[0]
	if row.Outcome != OutcomeFailed {
		t.Errorf("Outcome = %q, want failed: the row's own reader reported the failed probe", row.Outcome)
	}
	if got := row.Stats["used_percent"]; got != 91 {
		t.Errorf("Stats[used_percent] = %d, want the reader's 91 kept beside the run's figures", got)
	}
	if row.State != StateOK {
		t.Errorf("State = %q, want ok: the reading is fresh, so the row's own state stands", row.State)
	}
}

// TestJobsLedgerSourceMergesStats is bead gt-pqgbd7 rework: a run's stats join
// the row's, so a figure only the reader recorded survives and a key both record
// takes the run's value.
func TestJobsLedgerSourceMergesStats(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	readAt := testNow.Add(-5 * time.Minute)
	writeNodeDiskRecord(t, root, `{"nodes":{"colima-gt-control":`+
		`{"used_percent":91,"used_bytes":1000,"read_at":"`+readAt.Format(time.RFC3339)+`"}}}`)
	name := "node_disk:colima-gt-control"
	appendLedgerRuns(t, root, jobledger.Run{
		Job:       name,
		StartedAt: testNow.Add(-2 * time.Minute),
		EndedAt:   testNow.Add(-time.Minute),
		Outcome:   "ok",
		Stats:     map[string]int64{"used_percent": 80, "databases": 3},
	})

	jobs, err := List(root, Options{}, testNow)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	rows := nodeDiskRowsNamed(jobs, name)
	if len(rows) != 1 {
		t.Fatalf("got %d rows named %q, want one: %+v", len(rows), name, jobs)
	}
	row := rows[0]
	if got := row.Stats["used_percent"]; got != 80 {
		t.Errorf("Stats[used_percent] = %d, want the run's 80 on the shared key", got)
	}
	if got := row.Stats["used_bytes"]; got != 1000 {
		t.Errorf("Stats[used_bytes] = %d, want the reader's 1000, which the run does not carry", got)
	}
	if got := row.Stats["databases"]; got != 3 {
		t.Errorf("Stats[databases] = %d, want the run's 3", got)
	}
}

// TestJobsEmptyTownRoot is bead gt-pqgbd7 rework: the one town root the ledger
// refuses is the empty one, which no row can be read from, so the list is an
// error rather than rows no reader can vouch for.
func TestJobsEmptyTownRoot(t *testing.T) {
	t.Parallel()
	if _, err := List("", Options{}, testNow); err == nil {
		t.Fatal("List returned no error for an empty town root")
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
			name:        "a state file the daemon never writes (no last_gc) is failed and unknown, not never-run",
			state:       `{"consecutive_deferred_windows":0}`,
			wantRow:     true,
			wantOutcome: OutcomeFailed,
			wantState:   StateUnknown,
		},
		{
			name:        "state file with no gc recorded has no last run",
			state:       `{"consecutive_deferred_windows":0,"last_gc":{}}`,
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

// TestJobDueAllowsTheRunDuration is bead gt-sn4eru: a job is due only after its
// cadence, its newest run's own duration and the grace, so a 30-minute job
// whose run takes 19 minutes is not overdue while that run's next cycle is
// still running. A row that records no duration keeps the cadence-plus-grace
// allowance it had before.
func TestJobDueAllowsTheRunDuration(t *testing.T) {
	t.Parallel()
	const (
		cadence = 30 * time.Minute
		run     = 19 * time.Minute
		grace   = 10 * time.Minute // max(cadence/4, 10m)
	)
	end := testNow.Add(-time.Hour)
	cases := []struct {
		name    string
		stats   map[string]int64 // the newest run's stats
		after   time.Duration    // how long after the run's end the row is judged
		wantDue time.Duration    // allowance past the run's end
		want    State
	}{
		{
			name:    "a 19-minute run is ok at the cadence plus the run duration",
			stats:   map[string]int64{"duration_ms": run.Milliseconds()},
			after:   cadence + run,
			wantDue: cadence + run + grace,
			want:    StateOK,
		},
		{
			name:    "a 19-minute run is ok just before the allowance ends",
			stats:   map[string]int64{"duration_ms": run.Milliseconds()},
			after:   cadence + run + grace - time.Minute,
			wantDue: cadence + run + grace,
			want:    StateOK,
		},
		{
			name:    "a 19-minute run is overdue past the allowance",
			stats:   map[string]int64{"duration_ms": run.Milliseconds()},
			after:   cadence + run + grace + time.Minute,
			wantDue: cadence + run + grace,
			want:    StateOverdue,
		},
		{
			name:    "no duration stat is ok within the cadence plus grace",
			after:   cadence + grace,
			wantDue: cadence + grace,
			want:    StateOK,
		},
		{
			name:    "no duration stat is overdue past the cadence plus grace",
			after:   cadence + grace + time.Minute,
			wantDue: cadence + grace,
			want:    StateOverdue,
		},
		{
			name:    "a negative duration stat is no allowance, never a shorter one",
			stats:   map[string]int64{"duration_ms": -run.Milliseconds()},
			after:   cadence + grace + time.Minute,
			wantDue: cadence + grace,
			want:    StateOverdue,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeMayorDaemon(t, root, `{"job":{"interval":"30m"}}`)
			appendLedgerRuns(t, root, jobledger.Run{
				Job:       "job",
				StartedAt: end.Add(-run),
				EndedAt:   end,
				Outcome:   "ok",
				Stats:     c.stats,
			})

			jobs, err := list(root, Options{}, []config.DaemonStep{
				{Name: "job", Kind: config.DaemonStepPatrol},
			}, end.Add(c.after))
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			row := jobs[0]
			if want := end.Add(c.wantDue); row.Due == nil || !row.Due.Equal(want) {
				t.Errorf("Due = %v, want %v", row.Due, want)
			}
			if row.State != c.want {
				t.Errorf("State = %q, want %q", row.State, c.want)
			}
		})
	}
}

// TestJobDueCapsTheRunDurationAtTheCadence is bead gt-utcevt: the extra time a
// row earns for its newest run is that run's duration, capped at one cadence.
// A run reporting ten cadences earns one cadence, so an absurd duration cannot
// hold the job out of overdue for ever; a run shorter than the cadence still
// earns exactly its duration.
func TestJobDueCapsTheRunDurationAtTheCadence(t *testing.T) {
	t.Parallel()
	const (
		cadence = 30 * time.Minute
		grace   = 10 * time.Minute // max(cadence/4, 10m)
	)
	end := testNow.Add(-time.Hour)
	cases := []struct {
		name    string
		run     time.Duration // the newest run's reported duration
		after   time.Duration // how long after the run's end the row is judged
		wantDue time.Duration // allowance past the run's end
		want    State
	}{
		{
			name:    "a run of ten cadences earns one cadence",
			run:     10 * cadence,
			after:   cadence + cadence + grace,
			wantDue: cadence + cadence + grace,
			want:    StateOK,
		},
		{
			name:    "a run of ten cadences is overdue one cadence past the cap",
			run:     10 * cadence,
			after:   cadence + cadence + grace + time.Minute,
			wantDue: cadence + cadence + grace,
			want:    StateOverdue,
		},
		{
			name:    "a run just over the cadence earns one cadence",
			run:     cadence + time.Minute,
			after:   cadence + cadence + grace,
			wantDue: cadence + cadence + grace,
			want:    StateOK,
		},
		{
			name:    "a run shorter than the cadence earns exactly its duration",
			run:     19 * time.Minute,
			after:   cadence + 19*time.Minute + grace,
			wantDue: cadence + 19*time.Minute + grace,
			want:    StateOK,
		},
		{
			name:    "a run shorter than the cadence is overdue past its duration",
			run:     19 * time.Minute,
			after:   cadence + 19*time.Minute + grace + time.Minute,
			wantDue: cadence + 19*time.Minute + grace,
			want:    StateOverdue,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeMayorDaemon(t, root, `{"job":{"interval":"30m"}}`)
			appendLedgerRuns(t, root, jobledger.Run{
				Job:       "job",
				StartedAt: end.Add(-c.run),
				EndedAt:   end,
				Outcome:   "ok",
				Stats:     map[string]int64{"duration_ms": c.run.Milliseconds()},
			})

			jobs, err := list(root, Options{}, []config.DaemonStep{
				{Name: "job", Kind: config.DaemonStepPatrol},
			}, end.Add(c.after))
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			row := jobs[0]
			if want := end.Add(c.wantDue); row.Due == nil || !row.Due.Equal(want) {
				t.Errorf("Due = %v, want %v", row.Due, want)
			}
			if row.State != c.want {
				t.Errorf("State = %q, want %q", row.State, c.want)
			}
		})
	}
}

// TestJobDueKeepsALedgerNextDue is bead gt-4i2sdl: a run that records its own
// next-due time is judged on that time, exactly, so the run-duration allowance
// never moves it. The run below takes 19 minutes and states next due five
// minutes after it ends, while its 30-minute cadence would put the row due 54
// minutes later and the allowance on top of the recorded time 19 minutes later.
// The row is due at the recorded time and nowhere else.
func TestJobDueKeepsALedgerNextDue(t *testing.T) {
	t.Parallel()
	const run = 19 * time.Minute
	end := testNow.Add(-time.Hour)
	nextDue := end.Add(5 * time.Minute)
	cases := []struct {
		name string
		// after is how long past the recorded next-due time the row is judged:
		// negative before it, zero at it, positive after it.
		after time.Duration
		want  State
	}{
		{name: "ok before the run's own next due time", after: -time.Minute, want: StateOK},
		{name: "ok at the run's own next due time", after: 0, want: StateOK},
		{name: "overdue a minute after the run's own next due time", after: time.Minute, want: StateOverdue},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeMayorDaemon(t, root, `{"job":{"interval":"30m"}}`)
			due := nextDue
			appendLedgerRuns(t, root, jobledger.Run{
				Job:       "job",
				StartedAt: end.Add(-run),
				EndedAt:   end,
				Outcome:   "ok",
				Stats:     map[string]int64{"duration_ms": run.Milliseconds()},
				NextDue:   &due,
			})

			jobs, err := list(root, Options{}, []config.DaemonStep{
				{Name: "job", Kind: config.DaemonStepPatrol},
			}, nextDue.Add(c.after))
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			row := jobs[0]
			if row.Due == nil || !row.Due.Equal(nextDue) {
				t.Errorf("Due = %v, want the run's recorded next due %v: the allowance does not move a recorded time", row.Due, nextDue)
			}
			if row.State != c.want {
				t.Errorf("State = %q, want %q", row.State, c.want)
			}
		})
	}
}
