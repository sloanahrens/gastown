package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/jobs"
)

// jobsTestNow is the clock the fixture's ages are judged against.
var jobsTestNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// jobsTestJob is the patrol the fixture gives a cadence and a last run.
const jobsTestJob = "scheduled_maintenance"

// jobsWriteFile creates path and its parents under the test's own directory.
func jobsWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// jobsTestTown is a town root with one cadenced patrol that last ran three
// hours before jobsTestNow, and the backup root the daemon writes a night into.
func jobsTestTown(t *testing.T) (townRoot, backupRoot string) {
	t.Helper()
	townRoot = t.TempDir()
	backupRoot = t.TempDir()
	jobsWriteFile(t, filepath.Join(townRoot, "mayor", "daemon.json"),
		`{"type":"daemon-patrol-config","version":1,"patrols":{"`+jobsTestJob+`":{"interval":"24h"}}}`)
	jobsWriteFile(t, filepath.Join(townRoot, "daemon", "patrol_last_run.json"),
		`{"last_run":{"`+jobsTestJob+`":"2026-11-19T09:00:00Z"}}`)
	// A reading inside nodeDiskStaleAfter, so the node disk row is ok rather
	// than the unknown row a missing record makes it.
	jobsWriteFile(t, filepath.Join(townRoot, ".runtime", "jobs", "node_disk.json"),
		`{"nodes":{"control":{"used_percent":42,"used_bytes":1024,"read_at":"2026-11-19T11:55:00Z"}}}`)
	return townRoot, backupRoot
}

// jobsTestDeps is jobsDeps over the fixture, with no host read.
func jobsTestDeps(townRoot, backupRoot string) jobsDeps {
	return jobsDeps{
		findTownRoot: func() (string, error) { return townRoot, nil },
		backupRoot:   func() (string, error) { return backupRoot, nil },
		now:          func() time.Time { return jobsTestNow },
	}
}

// runJobs executes a fresh command, so the flag state is the run's own.
func runJobs(t *testing.T, d jobsDeps, out io.Writer, args ...string) error {
	t.Helper()
	c := newJobsCmd(d)
	c.SetArgs(args)
	c.SetOut(out)
	c.SetErr(io.Discard)
	return c.Execute()
}

// jobsRow returns the printed line whose first field is name.
func jobsRow(t *testing.T, out, name string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == name {
			return line
		}
	}
	t.Fatalf("no row for %s in:\n%s", name, out)
	return ""
}

// jobsWantExit requires a run to have failed with the given exit status. A nil
// error is a failure here: exitCodeForError reports 1 for it, so a test that
// only reads the status would pass a command that never failed.
func jobsWantExit(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want exit %d", want)
	}
	if code := exitCodeForError(err); code != want {
		t.Fatalf("exit %d (%v), want %d", code, err, want)
	}
}

// jobsReportRow returns the JSON array's row for name.
func jobsReportRow(t *testing.T, list []jobs.Job, name string) jobs.Job {
	t.Helper()
	for _, j := range list {
		if j.Name == name {
			return j
		}
	}
	t.Fatalf("no JSON row for %s", name)
	return jobs.Job{}
}

func TestJobsCommand(t *testing.T) {
	t.Parallel()

	t.Run("text prints each job's last run, cadence and state", func(t *testing.T) {
		t.Parallel()
		townRoot, backupRoot := jobsTestTown(t)
		var out bytes.Buffer
		if err := runJobs(t, jobsTestDeps(townRoot, backupRoot), &out); err != nil {
			t.Fatalf("exit %d, want 0: %v", exitCodeForError(err), err)
		}
		for _, header := range []string{"NAME", "LAST RUN", "CADENCE", "STATE", "OUTCOME"} {
			if !strings.Contains(out.String(), header) {
				t.Errorf("output has no %q column:\n%s", header, out.String())
			}
		}
		got := strings.Join(strings.Fields(jobsRow(t, out.String(), jobsTestJob)), " ")
		if want := jobsTestJob + " 3h ago 24h ok -"; got != want {
			t.Errorf("row = %q, want %q", got, want)
		}
		if !strings.Contains(out.String(), "dolt_backup") {
			t.Errorf("output has no dolt_backup row:\n%s", out.String())
		}
	})

	t.Run("--json prints the array the dashboard parses", func(t *testing.T) {
		t.Parallel()
		townRoot, backupRoot := jobsTestTown(t)
		var out bytes.Buffer
		if err := runJobs(t, jobsTestDeps(townRoot, backupRoot), &out, "--json"); err != nil {
			t.Fatalf("exit %d, want 0: %v", exitCodeForError(err), err)
		}
		var list []jobs.Job
		if err := json.Unmarshal(out.Bytes(), &list); err != nil {
			t.Fatalf("--json output does not parse as an array: %v\n%s", err, out.String())
		}
		row := jobsReportRow(t, list, jobsTestJob)
		if row.LastRun == nil || !row.LastRun.Equal(time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)) {
			t.Errorf("last_run = %v, want 2026-11-19T09:00:00Z", row.LastRun)
		}
		if row.State != jobs.StateOK {
			t.Errorf("state = %q, want %q", row.State, jobs.StateOK)
		}

		section, err := dashboard.ParseJobs(out.Bytes())
		if err != nil {
			t.Fatalf("the dashboard cannot parse the array: %v", err)
		}
		found := false
		for _, r := range section.Rows {
			if r.Name == jobsTestJob {
				found = true
				if r.State != string(jobs.StateOK) {
					t.Errorf("dashboard row state = %q, want %q", r.State, jobs.StateOK)
				}
			}
		}
		if !found {
			t.Errorf("the dashboard section has no %s row", jobsTestJob)
		}
	})
}

func TestJobsCommandUnreadable(t *testing.T) {
	t.Parallel()
	townRoot, backupRoot := jobsTestTown(t)
	jobsWriteFile(t, filepath.Join(townRoot, "daemon", "patrol_last_run.json"), `{"last_run":`)
	d := jobsTestDeps(townRoot, backupRoot)

	t.Run("text prints the row unknown", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		err := runJobs(t, d, &out)
		jobsWantExit(t, err, 1)
		row := jobsRow(t, out.String(), jobsTestJob)
		// The STATE column sits before OUTCOME, which is this row's "-".
		if fields := strings.Fields(row); fields[len(fields)-2] != string(jobs.StateUnknown) {
			t.Errorf("row = %q, want state %q", row, jobs.StateUnknown)
		}
	})

	t.Run("--json prints the row unknown", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		err := runJobs(t, d, &out, "--json")
		jobsWantExit(t, err, 1)
		var list []jobs.Job
		if err := json.Unmarshal(out.Bytes(), &list); err != nil {
			t.Fatalf("--json output does not parse as an array: %v", err)
		}
		if row := jobsReportRow(t, list, jobsTestJob); row.State != jobs.StateUnknown {
			t.Errorf("state = %q, want %q", row.State, jobs.StateUnknown)
		}
	})
}

func TestJobsCommandBadUsage(t *testing.T) {
	t.Parallel()
	townRoot, backupRoot := jobsTestTown(t)
	d := jobsTestDeps(townRoot, backupRoot)

	t.Run("an extra argument exits 2", func(t *testing.T) {
		t.Parallel()
		err := runJobs(t, d, io.Discard, "extra")
		jobsWantExit(t, err, 2)
	})

	t.Run("an unknown flag exits 2", func(t *testing.T) {
		t.Parallel()
		err := runJobs(t, d, io.Discard, "--nope")
		jobsWantExit(t, err, 2)
	})
}

func TestJobsCommandFailures(t *testing.T) {
	t.Parallel()

	t.Run("a town root that cannot be found is an error", func(t *testing.T) {
		t.Parallel()
		d := jobsDeps{
			findTownRoot: func() (string, error) { return "", errors.New("not in a town") },
			backupRoot:   func() (string, error) { return t.TempDir(), nil },
			now:          func() time.Time { return jobsTestNow },
		}
		err := runJobs(t, d, io.Discard)
		jobsWantExit(t, err, 1)
		if !strings.Contains(err.Error(), "finding the town root") {
			t.Fatalf("error = %v, want it to name the town root", err)
		}
	})

	t.Run("an unreadable patrol config is an error", func(t *testing.T) {
		t.Parallel()
		townRoot, backupRoot := jobsTestTown(t)
		jobsWriteFile(t, filepath.Join(townRoot, "mayor", "daemon.json"), `{"type":`)
		var out bytes.Buffer
		err := runJobs(t, jobsTestDeps(townRoot, backupRoot), &out)
		jobsWantExit(t, err, 1)
		if !strings.Contains(err.Error(), "listing the maintenance jobs") {
			t.Fatalf("error = %v, want it to name the list", err)
		}
		if out.Len() != 0 {
			t.Errorf("a failed list printed %q, want nothing", out.String())
		}
	})

	t.Run("an unresolved backup root leaves the row unknown and exits 1", func(t *testing.T) {
		t.Parallel()
		townRoot, backupRoot := jobsTestTown(t)
		d := jobsTestDeps(townRoot, backupRoot)
		d.backupRoot = func() (string, error) { return "", errors.New("no home directory") }
		var out bytes.Buffer
		err := runJobs(t, d, &out)
		jobsWantExit(t, err, 1)
		if !strings.Contains(err.Error(), "backup root") {
			t.Fatalf("error = %v, want it to name the backup root", err)
		}
		if row := jobsRow(t, out.String(), "dolt_backup"); !strings.Contains(row, "unknown") {
			t.Errorf("dolt_backup row = %q, want unknown", row)
		}
	})

	t.Run("a failed last run exits 1", func(t *testing.T) {
		t.Parallel()
		townRoot, backupRoot := jobsTestTown(t)
		jobsWriteFile(t, filepath.Join(townRoot, ".runtime", "jobs", "runs.jsonl"),
			`{"job":"`+jobsTestJob+`","started_at":"2026-11-19T09:00:00Z","ended_at":"2026-11-19T09:05:00Z","outcome":"failed"}`+"\n")
		var out bytes.Buffer
		err := runJobs(t, jobsTestDeps(townRoot, backupRoot), &out)
		jobsWantExit(t, err, 1)
		if row := jobsRow(t, out.String(), jobsTestJob); !strings.Contains(row, "failed") {
			t.Errorf("row = %q, want outcome failed", row)
		}
	})

	t.Run("a writer that refuses is an error", func(t *testing.T) {
		t.Parallel()
		townRoot, backupRoot := jobsTestTown(t)
		writeErr := errors.New("broken pipe")
		for _, args := range [][]string{nil, {"--json"}} {
			err := runJobs(t, jobsTestDeps(townRoot, backupRoot), refusingWriter{err: writeErr}, args...)
			if !errors.Is(err, writeErr) {
				t.Errorf("args %q: err = %v, want the writer's error", args, err)
			}
		}
	})
}
