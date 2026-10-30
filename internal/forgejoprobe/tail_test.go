package forgejoprobe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejo"
)

const testJobName = "gate"

// logCapture records every line PrintTail logs, formatted as the probe prints it.
type logCapture struct {
	lines []string
}

func (l *logCapture) logf(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// hasLine reports whether one logged line contains sub.
func (l *logCapture) hasLine(sub string) bool {
	for _, line := range l.lines {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}

// pushRun is the run the probe should find for testSHA.
func pushRun(id int64) *forgejo.RunList {
	return &forgejo.RunList{Runs: []forgejo.ActionRun{{ID: id, CommitSHA: testSHA}}}
}

func TestPrintTailRedAndRenamedJob(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		jobs     []forgejo.ActionRunJob
		wantJob  int64
		wantLogs string
	}{
		{
			name:     "the job with the probe's name",
			jobs:     []forgejo.ActionRunJob{{ID: 70, Name: testJobName, Status: "failure"}},
			wantJob:  70,
			wantLogs: "log tail of the log of run 7's job 'gate':",
		},
		{
			name: "a renamed job falls back to the failed job",
			jobs: []forgejo.ActionRunJob{
				{ID: 71, Name: "build", Status: "success"},
				{ID: 72, Name: "lint", Status: "failure"},
			},
			wantJob:  72,
			wantLogs: "log tail of the log of run 7's job 'lint':",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := &fakeStatuses{runs: pushRun(7), jobs: tc.jobs, log: "vet failed\n"}
			var out bytes.Buffer
			var logs logCapture

			PrintTail(context.Background(), st, &out, logs.logf, "acme", "rig", testSHA, testJobName)

			if st.loggedJob != tc.wantJob {
				t.Errorf("log read for job %d, want %d", st.loggedJob, tc.wantJob)
			}
			if got, want := out.String(), "vet failed\n\n"; got != want {
				t.Errorf("out = %q, want %q", got, want)
			}
			if !logs.hasLine(tc.wantLogs) {
				t.Errorf("logs = %q, want a line %q", logs.lines, tc.wantLogs)
			}
		})
	}
}

// TestPrintTailUsesNewestRun covers a re-probe of an unchanged main: the push
// run from the first probe stays listed, and the failure is on the newer run.
func TestPrintTailUsesNewestRun(t *testing.T) {
	t.Parallel()
	runs := &forgejo.RunList{Runs: []forgejo.ActionRun{
		{ID: 6, CommitSHA: testSHA},
		{ID: 9, CommitSHA: testSHA},
	}}
	st := &fakeStatuses{
		runs: runs,
		jobs: []forgejo.ActionRunJob{{ID: 90, Name: testJobName, Status: "failure"}},
		log:  "vet failed\n",
	}
	var out bytes.Buffer
	var logs logCapture

	PrintTail(context.Background(), st, &out, logs.logf, "acme", "rig", testSHA, testJobName)

	if st.jobsRun != 9 {
		t.Errorf("jobs read for run %d, want the newest run 9", st.jobsRun)
	}
}

func TestPrintTailKeepsTheTail(t *testing.T) {
	t.Parallel()
	const head = "HEAD-MARKER\n"
	const tail = "\nTAIL-MARKER"
	// The server may ignore the range and send the whole log; the probe must
	// still print only the end of it.
	whole := head + strings.Repeat("x", 20000) + tail
	st := &fakeStatuses{
		runs: pushRun(7),
		jobs: []forgejo.ActionRunJob{{ID: 70, Name: testJobName, Status: "failure"}},
		log:  whole,
	}
	var out bytes.Buffer
	var logs logCapture

	PrintTail(context.Background(), st, &out, logs.logf, "acme", "rig", testSHA, testJobName)

	got := out.String()
	if !strings.Contains(got, "TAIL-MARKER") {
		t.Errorf("out does not carry the end of the log")
	}
	if strings.Contains(got, "HEAD-MARKER") {
		t.Errorf("out carries the start of the log")
	}
	if want := tailBytes + 1; len(got) != want {
		t.Errorf("len(out) = %d, want %d (tail plus newline)", len(got), want)
	}
}

func TestPrintTailReadFailuresLogAndStop(t *testing.T) {
	t.Parallel()
	// wantJob is the job whose log the probe reads: 0 when none, since a silent
	// gate with no run or no job reads no log.
	cases := []struct {
		name    string
		st      *fakeStatuses
		wantJob int64
		wantLog string
	}{
		{
			name:    "no push run on the sha",
			st:      &fakeStatuses{runs: &forgejo.RunList{Runs: []forgejo.ActionRun{{ID: 7, CommitSHA: "other"}}}},
			wantLog: "no workflow run tested " + testSHA,
		},
		{
			name:    "the run list fails",
			st:      &fakeStatuses{runsErr: errors.New("boom")},
			wantLog: "no run to read for " + testSHA + " (boom)",
		},
		{
			name:    "the jobs fail",
			st:      &fakeStatuses{runs: pushRun(7), jobsErr: errors.New("boom")},
			wantLog: "run 7: jobs could not be read (boom)",
		},
		{
			name: "no job has the name and none failed",
			st: &fakeStatuses{
				runs: pushRun(7),
				jobs: []forgejo.ActionRunJob{{ID: 71, Name: "build", Status: "success"}},
			},
			wantLog: "run 7 has no job 'gate' and no failed job",
		},
		{
			name: "the log fails",
			st: &fakeStatuses{
				runs:   pushRun(7),
				jobs:   []forgejo.ActionRunJob{{ID: 70, Name: testJobName, Status: "failure"}},
				logErr: errors.New("boom"),
			},
			wantJob: 70,
			wantLog: "the job log could not be read (boom)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			var logs logCapture

			PrintTail(context.Background(), tc.st, &out, logs.logf, "acme", "rig", testSHA, testJobName)

			if out.Len() != 0 {
				t.Errorf("out = %q, want nothing written", out.String())
			}
			if !logs.hasLine(tc.wantLog) {
				t.Errorf("logs = %q, want a line %q", logs.lines, tc.wantLog)
			}
			if tc.st.loggedJob != tc.wantJob {
				t.Errorf("log read for job %d, want %d", tc.st.loggedJob, tc.wantJob)
			}
		})
	}
}
