package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/update"
)

// updateStep is the daemon step the update row comes from.
var updateStep = []config.DaemonStep{{Name: updateJobName, Kind: config.DaemonStepPatrol}}

// writeUpdateHistory writes raw history.jsonl lines under the town's update
// state directory.
func writeUpdateHistory(t *testing.T, root, lines string) {
	t.Helper()
	writeFile(t, filepath.Join(update.Dir(root), "history.jsonl"), lines)
}

// jobsRow lists the jobs over steps and returns the one named name.
func jobsRow(t *testing.T, townRoot string, opts Options, steps []config.DaemonStep, name string) Job {
	t.Helper()
	rows, err := list(townRoot, opts, steps, testNow)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, j := range rows {
		if j.Name == name {
			return j
		}
	}
	t.Fatalf("no %q row in %d rows", name, len(rows))
	return Job{}
}

// writeBackupManifest writes one night's backup.json under root.
func writeBackupManifest(t *testing.T, root, night, body string) {
	t.Helper()
	writeFile(t, filepath.Join(root, night, "backup.json"), body)
}

func TestJobsUpdateRow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		history      string
		wantLastRun  time.Time
		wantOutcome  Outcome
		wantDuration int64
	}{
		{
			name: "finished run",
			history: `{"v":1,"run_id":"r1","outcome":"ok","ended_at":"2026-11-19T11:00:00Z",` +
				`"durations_ms":{"plan":100,"install":250}}` + "\n",
			wantLastRun:  time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC),
			wantOutcome:  OutcomeOK,
			wantDuration: 350,
		},
		{
			name: "failed newest run after an older ok run",
			history: `{"v":1,"run_id":"r1","outcome":"ok","ended_at":"2026-11-18T11:00:00Z",` +
				`"durations_ms":{"plan":100}}` + "\n" +
				`{"v":1,"run_id":"r2","outcome":"failed","ended_at":"2026-11-19T11:30:00Z",` +
				`"durations_ms":{"verify":42,"install":8}}` + "\n",
			wantLastRun:  time.Date(2026, 11, 19, 11, 30, 0, 0, time.UTC),
			wantOutcome:  OutcomeFailed,
			wantDuration: 50,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeUpdateHistory(t, root, c.history)

			j := jobsRow(t, root, Options{}, updateStep, updateJobName)
			if j.LastRun == nil || !j.LastRun.Equal(c.wantLastRun) {
				t.Errorf("LastRun = %v, want %v", j.LastRun, c.wantLastRun)
			}
			if j.Outcome != c.wantOutcome {
				t.Errorf("Outcome = %q, want %q", j.Outcome, c.wantOutcome)
			}
			if got := j.Stats["duration_ms"]; got != c.wantDuration {
				t.Errorf("Stats[duration_ms] = %d, want %d", got, c.wantDuration)
			}
			if j.State != StateOK {
				t.Errorf("State = %q, want ok for a run inside the nightly cadence", j.State)
			}
			if j.Cadence != nightlyCadence || j.Source != "" {
				t.Errorf("Cadence = %v, Source = %q, want %v from the nightly rule and no config source", j.Cadence, j.Source, nightlyCadence)
			}
		})
	}

	t.Run("empty history", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		j := jobsRow(t, root, Options{}, updateStep, updateJobName)
		if j.LastRun != nil {
			t.Errorf("LastRun = %v, want none for an empty history", j.LastRun)
		}
		if j.Outcome != "" {
			t.Errorf("Outcome = %q, want empty", j.Outcome)
		}
		if j.Stats != nil {
			t.Errorf("Stats = %v, want nil", j.Stats)
		}
		if j.State != StateNeverRun {
			t.Errorf("State = %q, want never_run", j.State)
		}
	})
}

func TestJobsUpdateRowOutcomeMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		outcome update.Outcome
		want    Outcome
	}{
		{update.OutcomeOK, OutcomeOK},
		{update.OutcomeRunning, OutcomeFailed},
		{update.OutcomePartial, OutcomeFailed},
		{update.OutcomeAborted, OutcomeFailed},
		{update.OutcomeFailed, OutcomeFailed},
		{update.OutcomeInterrupted, OutcomeFailed},
		{update.Outcome("not-an-outcome"), OutcomeFailed},
	}
	for _, c := range cases {
		t.Run(string(c.outcome), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeUpdateHistory(t, root, fmt.Sprintf(
				`{"v":1,"run_id":"r1","outcome":%q,"ended_at":"2026-11-19T11:00:00Z","durations_ms":{}}`+"\n",
				c.outcome))

			j := jobsRow(t, root, Options{}, updateStep, updateJobName)
			if j.Outcome != c.want {
				t.Errorf("Outcome = %q, want %q for update outcome %q", j.Outcome, c.want, c.outcome)
			}
		})
	}
}

func TestJobsUpdateRowUnreadableHistory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// A directory where the history file belongs opens but cannot be read.
	if err := os.MkdirAll(filepath.Join(update.Dir(root), "history.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}

	j := jobsRow(t, root, Options{}, updateStep, updateJobName)
	if j.State != StateUnknown {
		t.Errorf("State = %q, want unknown for an unreadable history", j.State)
	}
	if j.LastRun != nil || j.Outcome != "" {
		t.Errorf("LastRun = %v, Outcome = %q, want neither for an unreadable history", j.LastRun, j.Outcome)
	}
}

func TestJobsUpdateRowCorruptHistory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Every complete line is unreadable: nothing says whether the file is a
	// history of failed runs or junk, so the row is unknown, never never_run.
	writeUpdateHistory(t, root, "not json at all\n"+`{"v":2,"run_id":"r1"}`+"\n")

	j := jobsRow(t, root, Options{}, updateStep, updateJobName)
	if j.State != StateUnknown {
		t.Errorf("State = %q, want unknown for a history no line of which parses", j.State)
	}
	if j.LastRun != nil || j.Outcome != "" {
		t.Errorf("LastRun = %v, Outcome = %q, want neither for a corrupt history", j.LastRun, j.Outcome)
	}
}

func TestJobsUpdateRowWithoutEndTime(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeUpdateHistory(t, root, `{"v":1,"run_id":"r1","outcome":"ok","durations_ms":{"plan":100}}`+"\n")

	j := jobsRow(t, root, Options{}, updateStep, updateJobName)
	if j.State != StateUnknown {
		t.Errorf("State = %q, want unknown for a run with no end time", j.State)
	}
	if j.LastRun != nil {
		t.Errorf("LastRun = %v, want none for a run with no end time", j.LastRun)
	}
}

func TestJobsDoltBackupRow(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	root := filepath.Join(t.TempDir(), "dolt")
	writeBackupManifest(t, root, "2026-11-18",
		`{"started":"2026-11-18T03:00:00Z","finished":"2026-11-18T03:05:00Z","databases":["hq","gt"],"method":"test"}`)
	writeBackupManifest(t, root, "2026-11-19",
		`{"started":"2026-11-19T03:00:00Z","finished":"2026-11-19T03:07:00Z","databases":["hq","gt","other"],"method":"test"}`)

	j := jobsRow(t, townRoot, Options{BackupRoot: root}, nil, doltBackupJobName)
	wantLastRun := time.Date(2026, 11, 19, 3, 7, 0, 0, time.UTC)
	if j.LastRun == nil || !j.LastRun.Equal(wantLastRun) {
		t.Errorf("LastRun = %v, want the newest night's finish %v", j.LastRun, wantLastRun)
	}
	if j.Outcome != OutcomeOK {
		t.Errorf("Outcome = %q, want ok", j.Outcome)
	}
	if got := j.Stats["databases"]; got != 3 {
		t.Errorf("Stats[databases] = %d, want the newest night's 3", got)
	}
	if j.State != StateOK {
		t.Errorf("State = %q, want ok for a night nine hours old", j.State)
	}
	if j.Kind != config.DaemonStepPatrol || j.Cadence != nightlyCadence {
		t.Errorf("Kind = %q, Cadence = %v, want %q and %v", j.Kind, j.Cadence, config.DaemonStepPatrol, nightlyCadence)
	}
}

func TestJobsDoltBackupRowNewestByName(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "dolt")
	// The older night's manifest finished later than the newer one's, as a
	// clock that ran ahead leaves it, so only the date name can say which night
	// is the newest: the daemon writes one directory per date.
	writeBackupManifest(t, root, "2026-11-18",
		`{"started":"2026-11-18T23:00:00Z","finished":"2026-11-19T04:00:00Z","databases":["hq"],"method":"test"}`)
	writeBackupManifest(t, root, "2026-11-19",
		`{"started":"2026-11-19T03:00:00Z","finished":"2026-11-19T03:05:00Z","databases":["hq","gt"],"method":"test"}`)
	// Neither a night still being written nor a directory that is not a date is
	// a night, however new its manifest looks.
	writeBackupManifest(t, root, "2026-11-20.partial",
		`{"started":"2026-11-20T03:00:00Z","finished":"2026-11-20T03:00:00Z","databases":["hq"],"method":"test"}`)
	writeBackupManifest(t, root, "nightly.tmp",
		`{"started":"2026-11-21T03:00:00Z","finished":"2026-11-21T03:00:00Z","databases":["hq"],"method":"test"}`)

	j := jobsRow(t, t.TempDir(), Options{BackupRoot: root}, nil, doltBackupJobName)
	wantLastRun := time.Date(2026, 11, 19, 3, 5, 0, 0, time.UTC)
	if j.LastRun == nil || !j.LastRun.Equal(wantLastRun) {
		t.Errorf("LastRun = %v, want the newest night by name %v", j.LastRun, wantLastRun)
	}
	if got := j.Stats["databases"]; got != 2 {
		t.Errorf("Stats[databases] = %d, want the newest night's 2", got)
	}
}

func TestJobsDoltBackupRowCorruptNewestNight(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "dolt")
	// The older night is healthy, but the newest backup is the corrupt one:
	// doltbackup.List would drop it and report the older night as the last run.
	writeBackupManifest(t, root, "2026-11-18",
		`{"started":"2026-11-18T03:00:00Z","finished":"2026-11-18T03:05:00Z","databases":["hq"],"method":"test"}`)
	writeBackupManifest(t, root, "2026-11-19", `{"started":"2026-11-19T03:00:00Z",`)

	j := jobsRow(t, t.TempDir(), Options{BackupRoot: root}, nil, doltBackupJobName)
	if j.State != StateUnknown {
		t.Errorf("State = %q, want unknown for a corrupt newest night beside a healthy older one", j.State)
	}
	if j.Outcome != OutcomeFailed {
		t.Errorf("Outcome = %q, want failed for a corrupt newest night", j.Outcome)
	}
	if j.LastRun != nil || j.Stats != nil {
		t.Errorf("LastRun = %v, Stats = %v, want neither for a corrupt newest night", j.LastRun, j.Stats)
	}
}

func TestJobsDoltBackupRowUnreadableRoot(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "dolt")
	if err := os.WriteFile(root, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	j := jobsRow(t, t.TempDir(), Options{BackupRoot: root}, nil, doltBackupJobName)
	if j.State != StateUnknown {
		t.Errorf("State = %q, want unknown for an unreadable backup root", j.State)
	}
	if j.LastRun != nil || j.Outcome != "" {
		t.Errorf("LastRun = %v, Outcome = %q, want neither for an unreadable backup root", j.LastRun, j.Outcome)
	}
}

func TestJobsDoltBackupRowEmptyRoot(t *testing.T) {
	t.Parallel()
	j := jobsRow(t, t.TempDir(), Options{}, nil, doltBackupJobName)
	if j.State != StateUnknown {
		t.Errorf("State = %q, want unknown when the caller resolved no backup root", j.State)
	}
}

func TestJobsDoltBackupRowNoNights(t *testing.T) {
	t.Parallel()
	j := jobsRow(t, t.TempDir(), Options{BackupRoot: t.TempDir()}, nil, doltBackupJobName)
	if j.State != StateNeverRun {
		t.Errorf("State = %q, want never_run for a root with no nights", j.State)
	}
	if j.LastRun != nil || j.Outcome != "" || j.Stats != nil {
		t.Errorf("LastRun = %v, Outcome = %q, Stats = %v, want none for a root with no nights", j.LastRun, j.Outcome, j.Stats)
	}
}

func TestJobsDoltBackupRowWithoutFinish(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "dolt")
	writeBackupManifest(t, root, "2026-11-19", `{"started":"2026-11-19T03:00:00Z","databases":["hq"],"method":"test"}`)

	j := jobsRow(t, t.TempDir(), Options{BackupRoot: root}, nil, doltBackupJobName)
	if j.State != StateUnknown {
		t.Errorf("State = %q, want unknown for a night with no finish time", j.State)
	}
	if j.LastRun != nil {
		t.Errorf("LastRun = %v, want none for a night with no finish time", j.LastRun)
	}
}
