package forgejoprobe

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

const (
	finishRepo   = "acme/gastown"
	finishRecord = "cfg/probe-gastown.record"
)

// finisher is a Finisher that records the calls Finish makes, in order, and logs
// what it is told. Each step's error is a field: a step that is not expected is
// still recorded, so a test sees every call.
type finisher struct {
	calls      []string
	logs       []string
	created    bool
	cleanupErr error
	deleteErr  error
	recordErr  error
	workErr    error
}

func (f *finisher) Cleanup() error {
	f.calls = append(f.calls, "cleanup")
	return f.cleanupErr
}

func (f *finisher) BranchCreated() bool { return f.created }
func (f *finisher) Branch() string      { return testBranch }
func (f *finisher) Repo() string        { return finishRepo }
func (f *finisher) Bare() string        { return testBare }

func (f *finisher) DeleteBranch() error {
	f.calls = append(f.calls, "delete")
	return f.deleteErr
}

func (f *finisher) Record() (string, error) {
	f.calls = append(f.calls, "record")
	return finishRecord, f.recordErr
}

func (f *finisher) RemoveWorkDir() error {
	f.calls = append(f.calls, "workdir")
	return f.workErr
}

func (f *finisher) Logf(format string, args ...any) {
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
}

// hasLog reports whether one logged line contains sub.
func (f *finisher) hasLog(sub string) bool {
	for _, line := range f.logs {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}

// allCalls is the full sequence of a run that deletes the branch, records and
// removes its work directory.
var allCalls = []string{"cleanup", "delete", "record", "workdir"}

func TestFinishOrderAndExitCodes(t *testing.T) {
	gone := &forgejo.APIError{Method: http.MethodDelete, Path: "/branches/" + testBranch, StatusCode: http.StatusNotFound}
	for _, tc := range []struct {
		name  string
		f     finisher
		green bool
		rc    int
		want  int
		calls []string
	}{
		{
			name:  "green run cleans up, deletes, records, then removes the work directory",
			f:     finisher{created: true},
			green: true,
			want:  ExitOK,
			calls: allCalls,
		},
		{
			name:  "a delete that fails makes a green run exit 1 and writes no record",
			f:     finisher{created: true, deleteErr: errors.New("boom")},
			green: true,
			want:  ExitFailure,
			calls: []string{"cleanup", "delete", "workdir"},
		},
		{
			name:  "a delete that finds the branch gone counts as success",
			f:     finisher{created: true, deleteErr: gone},
			green: true,
			want:  ExitOK,
			calls: allCalls,
		},
		{
			name:  "a red run writes no record",
			f:     finisher{created: true},
			green: false,
			want:  ExitOK,
			calls: []string{"cleanup", "delete", "workdir"},
		},
		{
			name:  "a worktree that will not be removed sets exit 1 and writes no record",
			f:     finisher{created: true, cleanupErr: errors.New("boom")},
			green: true,
			want:  ExitFailure,
			calls: []string{"cleanup", "delete", "workdir"},
		},
		{
			name:  "a record that will not be written sets exit 1",
			f:     finisher{created: true, recordErr: errors.New("boom")},
			green: true,
			want:  ExitFailure,
			calls: allCalls,
		},
		{
			name:  "a code the run already has is kept when nothing fails",
			f:     finisher{created: true},
			green: false,
			rc:    ExitUsage,
			want:  ExitUsage,
			calls: []string{"cleanup", "delete", "workdir"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.f
			got := Finish(&f, tc.green, PushedFromWorktree, tc.rc)
			if got != tc.want {
				t.Errorf("exit = %d, want %d (logs %q)", got, tc.want, f.logs)
			}
			if !slices.Equal(f.calls, tc.calls) {
				t.Errorf("calls = %v, want %v", f.calls, tc.calls)
			}
		})
	}
}

func TestFinishWritesNoRecordWhenHooksNotExercised(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    PushMode
		want    int
		record  bool
		logLine string
	}{
		{
			name:    "a push from the worktree writes the record",
			mode:    PushedFromWorktree,
			want:    ExitOK,
			record:  true,
			logLine: "recorded the green probe in " + finishRecord,
		},
		{
			name:    "a push from the clone writes no record and exits 3",
			mode:    PushedFromClone,
			want:    ExitHooksNotExercised,
			logLine: "the rig's hooks were NOT exercised: no rig repository at " + testBare,
		},
		{
			name:    "the zero push mode is not exercised either",
			mode:    PushMode(0),
			want:    ExitHooksNotExercised,
			logLine: "the rig's hooks were NOT exercised: no rig repository at " + testBare,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &finisher{created: true}
			got := Finish(f, true, tc.mode, ExitOK)
			if got != tc.want {
				t.Errorf("exit = %d, want %d", got, tc.want)
			}
			if wroteRecord := slices.Contains(f.calls, "record"); wroteRecord != tc.record {
				t.Errorf("record written = %v, want %v; calls %v", wroteRecord, tc.record, f.calls)
			}
			if !f.hasLog(tc.logLine) {
				t.Errorf("no log line containing %q in %q", tc.logLine, f.logs)
			}
		})
	}
}

func TestFinishWithoutABranchDeletesNothing(t *testing.T) {
	f := &finisher{created: false}
	got := Finish(f, false, PushedFromWorktree, ExitFailure)
	if got != ExitFailure {
		t.Errorf("exit = %d, want %d", got, ExitFailure)
	}
	if want := []string{"cleanup", "workdir"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v: a refusal before the push must send no delete and write no record", f.calls, want)
	}
}

func TestDryRunLines(t *testing.T) {
	plan := DryRunPlan{
		Main:    testMain,
		SHA:     testSHA,
		GitURL:  testPushURL,
		Branch:  testBranch,
		Bare:    testBare,
		Repo:    finishRepo,
		Context: "forgejo/gastown",
		Timeout: 1200 * time.Second,
	}
	for _, tc := range []struct {
		name    string
		rigRepo bool
		second  string
	}{
		{
			name:    "a rig repository is present: the push rides a worktree of it",
			rigRepo: true,
			second:  "would push from a throwaway worktree of " + testBare + ", so the rig's hooks run, and remove the worktree afterwards",
		},
		{
			name:    "no rig repository: the warning says the hooks will not run",
			rigRepo: false,
			second:  "warning: no rig repository at " + testBare + ", so the probe would push from the clone and the rig's hooks would not be exercised",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := plan
			p.RigRepo = tc.rigRepo
			want := []string{
				"would push " + testMain + " (" + testSHA + ") to " + testPushURL + " as " + testBranch,
				tc.second,
				"would wait up to 1200s for forgejo/gastown on " + testSHA,
				"would delete branch " + testBranch + " on " + finishRepo,
				"dry run: no push and no deletion was sent",
			}
			if got := DryRunLines(p); !slices.Equal(got, want) {
				t.Errorf("DryRunLines =\n%q\nwant\n%q", got, want)
			}
		})
	}
}
