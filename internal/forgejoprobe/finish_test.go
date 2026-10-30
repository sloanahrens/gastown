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
	finishWork   = "tmp/forgejo-probe.424242"
)

// staleRecord is what an earlier run left under finishRecord: a green record of
// another commit, which Finish must not let the cutover read after this run.
const staleRecord = "rig=gastown\nrepo=acme/gastown\ncommit=stale-sha\ncontext=forgejo/gastown\nverdict=green\ntime=2026-10-29T01:00:00Z\nepoch=1793235600\n"

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
	removeErr  error
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
func (f *finisher) RecordPath() string  { return finishRecord }
func (f *finisher) WorkDir() string     { return finishWork }

func (f *finisher) DeleteBranch() error {
	f.calls = append(f.calls, "delete")
	return f.deleteErr
}

func (f *finisher) Record() (string, error) {
	f.calls = append(f.calls, "record")
	return finishRecord, f.recordErr
}

func (f *finisher) RemoveRecord() error {
	f.calls = append(f.calls, "remove")
	return f.removeErr
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

// storeFinisher is a finisher whose record lives in an in-memory FS, so a row can
// start with a record an earlier run left and see whether Finish removed it.
type storeFinisher struct {
	finisher
	fsys *recordFS
}

func (s *storeFinisher) Record() (string, error) {
	s.calls = append(s.calls, "record")
	return writeTestRecord(s.fsys)
}

func (s *storeFinisher) RemoveRecord() error {
	s.calls = append(s.calls, "remove")
	return RemoveRecord(s.fsys, "cfg", "gastown")
}

// allCalls is the full sequence of a run that deletes the branch, records and
// removes its work directory.
var allCalls = []string{"cleanup", "delete", "record", "workdir"}

// notRecordedCalls is the sequence of a run that deletes the branch and writes
// no record, so it removes the record an earlier run left.
var notRecordedCalls = []string{"cleanup", "delete", "remove", "workdir"}

func TestFinishOrderAndExitCodes(t *testing.T) {
	t.Parallel()
	gone := &forgejo.APIError{Method: http.MethodDelete, Path: "/branches/" + testBranch, StatusCode: http.StatusNotFound}
	for _, tc := range []struct {
		name  string
		f     finisher
		green bool
		mode  PushMode
		rc    int
		want  int
		calls []string
	}{
		{
			name:  "green run with the hooks cleans up, deletes, records, then removes the work directory",
			f:     finisher{created: true},
			green: true,
			mode:  PushedFromWorktree,
			want:  ExitOK,
			calls: allCalls,
		},
		{
			name:  "a green run whose push rode the clone exits 3 and writes no record",
			f:     finisher{created: true},
			green: true,
			mode:  PushedFromClone,
			want:  ExitHooksNotExercised,
			calls: notRecordedCalls,
		},
		{
			name:  "a green run with the zero push mode exits 3 and writes no record",
			f:     finisher{created: true},
			green: true,
			mode:  PushMode(0),
			want:  ExitHooksNotExercised,
			calls: notRecordedCalls,
		},
		{
			name:  "a red run that exited 0 exits 1, not 0",
			f:     finisher{created: true},
			green: false,
			mode:  PushedFromWorktree,
			rc:    ExitOK,
			want:  ExitFailure,
			calls: notRecordedCalls,
		},
		{
			name:  "a red run that already failed keeps its exit 1",
			f:     finisher{created: true},
			green: false,
			mode:  PushedFromWorktree,
			rc:    ExitFailure,
			want:  ExitFailure,
			calls: notRecordedCalls,
		},
		{
			name:  "a refused run keeps its exit 2",
			f:     finisher{created: true},
			green: false,
			mode:  PushedFromWorktree,
			rc:    ExitUsage,
			want:  ExitUsage,
			calls: notRecordedCalls,
		},
		{
			name:  "a delete that fails makes a green run exit 1 and writes no record",
			f:     finisher{created: true, deleteErr: errors.New("boom")},
			green: true,
			mode:  PushedFromWorktree,
			want:  ExitFailure,
			calls: notRecordedCalls,
		},
		{
			name:  "a delete that finds the branch gone counts as success",
			f:     finisher{created: true, deleteErr: gone},
			green: true,
			mode:  PushedFromWorktree,
			want:  ExitOK,
			calls: allCalls,
		},
		{
			name:  "a worktree that will not be removed sets exit 1 and writes no record",
			f:     finisher{created: true, cleanupErr: errors.New("boom")},
			green: true,
			mode:  PushedFromWorktree,
			want:  ExitFailure,
			calls: notRecordedCalls,
		},
		{
			name:  "a record that will not be written sets exit 1",
			f:     finisher{created: true, recordErr: errors.New("boom")},
			green: true,
			mode:  PushedFromWorktree,
			want:  ExitFailure,
			calls: []string{"cleanup", "delete", "record", "remove", "workdir"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.f
			got := Finish(&f, tc.green, tc.mode, tc.rc)
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
	t.Parallel()
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
	t.Parallel()
	f := &finisher{created: false}
	got := Finish(f, false, PushedFromWorktree, ExitUsage)
	if got != ExitUsage {
		t.Errorf("exit = %d, want %d", got, ExitUsage)
	}
	if want := []string{"cleanup", "remove", "workdir"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v: a refusal before the push must send no delete and write no record", f.calls, want)
	}
}

// TestFinishStaleRecordIsRemoved starts each row with a record an earlier run
// left. Every outcome that did not write its own record must leave none behind.
func TestFinishStaleRecordIsRemoved(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		f     finisher
		green bool
		mode  PushMode
		rc    int
		want  int
		fail  string
	}{
		{
			name: "a red run",
			f:    finisher{created: true},
			mode: PushedFromWorktree,
			want: ExitFailure,
		},
		{
			name: "a refused run",
			f:    finisher{created: false},
			rc:   ExitUsage,
			want: ExitUsage,
		},
		{
			name:  "a green run whose push rode the clone",
			f:     finisher{created: true},
			green: true,
			mode:  PushedFromClone,
			want:  ExitHooksNotExercised,
		},
		{
			name:  "a green run whose branch will not be deleted",
			f:     finisher{created: true, deleteErr: errors.New("boom")},
			green: true,
			mode:  PushedFromWorktree,
			want:  ExitFailure,
		},
		{
			name:  "a green run whose record will not be written",
			f:     finisher{created: true},
			green: true,
			mode:  PushedFromWorktree,
			want:  ExitFailure,
			fail:  "rename",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := newRecordFS()
			fsys.files[finishRecord] = &recordEntry{data: staleRecord, mode: 0o600}
			if tc.fail != "" {
				fsys.fail[tc.fail] = errors.New("boom")
			}
			f := &storeFinisher{finisher: tc.f, fsys: fsys}
			got := Finish(f, tc.green, tc.mode, tc.rc)
			if got != tc.want {
				t.Errorf("exit = %d, want %d (logs %q)", got, tc.want, f.logs)
			}
			if entry, ok := fsys.files[finishRecord]; ok {
				t.Errorf("the stale record survived the run: %q", entry.data)
			}
		})
	}
}

// TestFinishGreenReplacesAStaleRecord holds the one outcome that keeps a record:
// a proven green run replaces the record an earlier run left.
func TestFinishGreenReplacesAStaleRecord(t *testing.T) {
	t.Parallel()
	fsys := newRecordFS()
	fsys.files[finishRecord] = &recordEntry{data: staleRecord, mode: 0o600}
	f := &storeFinisher{finisher: finisher{created: true}, fsys: fsys}
	if got := Finish(f, true, PushedFromWorktree, ExitOK); got != ExitOK {
		t.Fatalf("exit = %d, want %d (logs %q)", got, ExitOK, f.logs)
	}
	entry, ok := fsys.files[finishRecord]
	if !ok {
		t.Fatalf("no record at %s after a proven green run", finishRecord)
	}
	if strings.Contains(entry.data, "stale-sha") {
		t.Errorf("record still holds the stale commit:\n%s", entry.data)
	}
	if !strings.Contains(entry.data, "commit="+testSHA+"\n") {
		t.Errorf("record does not name this run's commit %s:\n%s", testSHA, entry.data)
	}
}

// TestFinishNamesAFailedRecordRemoval makes the removal fail on each outcome that
// must remove the record: the failure is logged with the path, and the exit is
// not a success.
func TestFinishNamesAFailedRecordRemoval(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		f     finisher
		green bool
		mode  PushMode
		rc    int
		want  int
	}{
		{
			name: "a red run exits 1",
			f:    finisher{created: true},
			mode: PushedFromWorktree,
			want: ExitFailure,
		},
		{
			name: "a refused run's stale record that will not go becomes exit 1, not 2",
			f:    finisher{created: false},
			rc:   ExitUsage,
			want: ExitFailure,
		},
		{
			name:  "a clone-only green run becomes exit 1, not 3",
			f:     finisher{created: true},
			green: true,
			mode:  PushedFromClone,
			want:  ExitFailure,
		},
		{
			name:  "a branch that will not be deleted keeps its exit 1",
			f:     finisher{created: true, deleteErr: errors.New("boom")},
			green: true,
			mode:  PushedFromWorktree,
			want:  ExitFailure,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := newRecordFS()
			fsys.files[finishRecord] = &recordEntry{data: staleRecord, mode: 0o600}
			fsys.fail["remove"] = errors.New("boom")
			f := &storeFinisher{finisher: tc.f, fsys: fsys}
			got := Finish(f, tc.green, tc.mode, tc.rc)
			if got != tc.want {
				t.Errorf("exit = %d, want %d (logs %q)", got, tc.want, f.logs)
			}
			if !f.hasLog("could not remove the stale probe record " + finishRecord) {
				t.Errorf("no log line naming %s in %q", finishRecord, f.logs)
			}
		})
	}
}

// TestFinishWorkDirWarningNamesThePath pins the work-directory warning: a
// directory that will not come off is logged with its path, and the exit does
// not change.
func TestFinishWorkDirWarningNamesThePath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		green bool
		want  int
	}{
		{name: "a green run keeps exit 0", green: true, want: ExitOK},
		{name: "a red run keeps exit 1", green: false, want: ExitFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &finisher{created: true, workErr: errors.New("boom")}
			if got := Finish(f, tc.green, PushedFromWorktree, ExitOK); got != tc.want {
				t.Errorf("exit = %d, want %d (logs %q)", got, tc.want, f.logs)
			}
			if !f.hasLog("could not remove the probe's work directory " + finishWork + " (boom); remove it by hand") {
				t.Errorf("no warning naming %s in %q", finishWork, f.logs)
			}
		})
	}
}

func TestDryRunLines(t *testing.T) {
	t.Parallel()
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
