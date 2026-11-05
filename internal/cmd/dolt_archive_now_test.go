package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/update"
)

// archiveNowFakeStat reports path as present when it is in files and absent
// otherwise, so a test scripts exactly which hold and request files exist.
func archiveNowFakeStat(files ...string) func(string) error {
	present := make(map[string]struct{}, len(files))
	for _, path := range files {
		present[path] = struct{}{}
	}
	return func(path string) error {
		if _, ok := present[path]; ok {
			return nil
		}
		return fs.ErrNotExist
	}
}

// archiveNowFakeRequest scripts the daemon request protocol and records the run
// ids the command asked about.
type archiveNowFakeRequest struct {
	requestErr error
	result     update.MaintenanceResult
	waitErr    error

	requested []string
	waited    []string
}

func (r *archiveNowFakeRequest) Request(runID string) error {
	r.requested = append(r.requested, runID)
	return r.requestErr
}

func (r *archiveNowFakeRequest) Wait(_ context.Context, _ func(context.Context, time.Duration) error, runID string, _ time.Duration, _ time.Duration) (update.MaintenanceResult, error) {
	r.waited = append(r.waited, runID)
	return r.result, r.waitErr
}

// newArchiveNowTestReads is a t.TempDir town's reads with every seam failing the
// test until the test scripts it: a command that reaches an unscripted seam is
// the failure the test wants to see.
func newArchiveNowTestReads(t *testing.T, townRoot string) doltArchiveNowReads {
	t.Helper()
	return doltArchiveNowReads{
		townRoot: func() (string, error) { return townRoot, nil },
		stat: func(string) error {
			t.Error("the command read a file path without a script")
			return fs.ErrNotExist
		},
		databases: func(string) ([]string, error) { return []string{"gt", "hq"}, nil },
		behind: func(context.Context, string, []string) (map[string]archiveNowReading, error) {
			t.Error("the command read the archive lag without a script")
			return nil, errors.New("the archive lag is not scripted")
		},
		request: func(string) archiveNowRequester {
			t.Error("the command used the request protocol without a script")
			return &archiveNowFakeRequest{}
		},
		requestPath: func(root string) string {
			return filepath.Join(update.Dir(root), update.ArchiveRequestFileName)
		},
		runID:   func() string { return "run-1" },
		sleep:   func(context.Context, time.Duration) error { return nil },
		timeout: time.Minute,
	}
}

// archiveNowBehindScripted returns a behind seam answering the two readings in
// order and counting the calls.
func archiveNowBehindScripted(t *testing.T, readings ...map[string]archiveNowReading) (func(context.Context, string, []string) (map[string]archiveNowReading, error), *int) {
	t.Helper()
	calls := 0
	snapshots := append([]map[string]archiveNowReading(nil), readings...)
	return func(context.Context, string, []string) (map[string]archiveNowReading, error) {
		if calls >= len(snapshots) {
			t.Fatalf("the archive lag was read %d times, want %d", calls+1, len(snapshots))
		}
		counts := snapshots[calls]
		calls++
		return counts, nil
	}, &calls
}

// A town without the overseer's dispatch hold is refused before the command
// reads a database or writes a request: the pass is the step that makes a
// flatten safe, and only the overseer authorizes it (gt-xr2byt).
func TestDoltArchiveNowRefusesWithoutTheHold(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	holdPath := dispatch.HoldFilePath(townRoot)
	fake := &archiveNowFakeRequest{}
	reads := newArchiveNowTestReads(t, townRoot)
	reads.stat = archiveNowFakeStat()
	reads.request = func(string) archiveNowRequester { return fake }

	var out bytes.Buffer
	err := runDoltArchiveNowWith(context.Background(), &out, reads)

	if code, ok := IsSilentExit(err); !ok || code != 3 {
		t.Fatalf("exit = (%v, %v), want silent exit 3", code, ok)
	}
	if len(fake.requested) != 0 {
		t.Errorf("the request protocol was used without the hold: %v", fake.requested)
	}
	requestPath := filepath.Join(update.Dir(townRoot), update.ArchiveRequestFileName)
	if _, statErr := os.Stat(requestPath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("a request file was written without the hold: stat %s = %v", requestPath, statErr)
	}
	message := out.String()
	if !strings.Contains(message, holdPath) {
		t.Errorf("refusal = %q, want it to name the hold path %s", message, holdPath)
	}
	if !strings.Contains(message, "overseer creates") {
		t.Errorf("refusal = %q, want it to say the overseer creates the hold", message)
	}
}

// The pass's report is one before/after line per database from the two
// readings, and the command exits 0 only when the daemon answered completed
// and every database reads 0 behind afterwards (gt-xr2byt).
func TestDoltArchiveNowPrintsBeforeAndAfterPerDatabase(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	run := func(t *testing.T, before, after map[string]archiveNowReading) (string, error) {
		t.Helper()
		reads := newArchiveNowTestReads(t, townRoot)
		reads.stat = archiveNowFakeStat(dispatch.HoldFilePath(townRoot))
		behind, calls := archiveNowBehindScripted(t, before, after)
		reads.behind = behind
		reads.request = func(string) archiveNowRequester {
			return &archiveNowFakeRequest{result: update.MaintenanceResult{Outcome: archiveOutcomeCompleted}}
		}

		var out bytes.Buffer
		err := runDoltArchiveNowWith(context.Background(), &out, reads)
		if *calls != 2 {
			t.Fatalf("the archive lag was read %d times, want a reading before and after the pass", *calls)
		}
		return out.String(), err
	}

	t.Run("every database caught up exits 0", func(t *testing.T) {
		t.Parallel()
		out, err := run(t,
			map[string]archiveNowReading{"gt": {commits: 3, events: 12}, "hq": {commits: 0, events: 0}},
			map[string]archiveNowReading{"gt": {commits: 0, events: 0}, "hq": {commits: 0, events: 0}},
		)
		if err != nil {
			t.Fatalf("completed and caught up: %v", err)
		}
		want := "gt: before 3 commits behind, 12 events behind; after 0 commits behind, 0 events behind\n" +
			"hq: before 0 commits behind, 0 events behind; after 0 commits behind, 0 events behind\n"
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to hold %q", out, want)
		}
	})

	t.Run("a database still behind after a completed answer exits 2", func(t *testing.T) {
		t.Parallel()
		out, err := run(t,
			map[string]archiveNowReading{"gt": {commits: 3, events: 12}, "hq": {commits: 1, events: 0}},
			map[string]archiveNowReading{"gt": {commits: 0, events: 0}, "hq": {commits: 0, events: 5}},
		)
		if code, ok := IsSilentExit(err); !ok || code != 2 {
			t.Fatalf("exit = (%v, %v), want silent exit 2 for a database still behind", code, ok)
		}
		for _, want := range []string{
			"hq: before 1 commit behind, 0 events behind; after 0 commits behind, 5 events behind",
			"the daemon answered completed, but hq still reads history behind the archive",
			"Run archive-now again",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output = %q, want it to hold %q", out, want)
			}
		}
	})

	t.Run("an events existence check is not caught up", func(t *testing.T) {
		t.Parallel()
		out, err := run(t,
			map[string]archiveNowReading{"gt": {commits: 0, events: eventsBehindUncounted}, "hq": {commits: 0, events: 0}},
			map[string]archiveNowReading{"gt": {commits: 0, events: eventsBehindUncounted}, "hq": {commits: 0, events: 0}},
		)
		if code, ok := IsSilentExit(err); !ok || code != 2 {
			t.Fatalf("exit = (%v, %v), want silent exit 2 for an unarchived events table", code, ok)
		}
		for _, want := range []string{
			"gt: before 0 commits behind, at least one event behind; after 0 commits behind, at least one event behind",
			"but gt still reads history behind the archive",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output = %q, want it to hold %q", out, want)
			}
		}
	})
}

// Each daemon answer maps to the exit code the runbook branches on: failed is
// 1 with its detail, a stop still behind or a deferred pass is 2 saying to run
// it again, and a request another job still waits for is refused at 3
// (gt-xr2byt).
func TestDoltArchiveNowExitCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		databases   []string
		before      map[string]archiveNowReading
		after       map[string]archiveNowReading
		requestErr  error
		requestOpen bool // the request file exists when Request fails
		result      update.MaintenanceResult
		waitErr     error
		wantCode    int
		wantOut     []string
		wantErr     string
	}{
		{
			name:     "failed answer prints its detail",
			result:   update.MaintenanceResult{Outcome: archiveOutcomeFailed, Detail: "the history archive is off: maintenance.archive_dir is unset in the daemon config"},
			wantCode: 1,
			wantOut:  []string{"the daemon answered failed", "maintenance.archive_dir is unset in the daemon config"},
		},
		{
			name:     "a deferred pass says to run it again",
			result:   update.MaintenanceResult{Outcome: archiveOutcomeDeferred, Detail: "the daemon is in standby"},
			wantCode: 2,
			wantOut:  []string{"still pending: the daemon is in standby", "Run archive-now again"},
		},
		{
			name:     "a timeout is still pending",
			result:   update.MaintenanceResult{Outcome: archiveOutcomeDeferred, Detail: "the daemon did not finish in time"},
			wantCode: 2,
			wantOut:  []string{"the daemon did not finish in time", "Run archive-now again"},
		},
		{
			name:     "a partial pass prints its per-database stop lines",
			result:   update.MaintenanceResult{Outcome: archiveOutcomePartial, Detail: "gt: commits archived 2 (stopped at the archive_budget, still behind); events archived 2 (caught up)"},
			wantCode: 2,
			wantOut:  []string{"gt: commits archived 2 (stopped at the archive_budget, still behind)", "Run archive-now again"},
		},
		{
			name:        "a pending request is refused",
			requestErr:  errors.New(`archive request /update/archive-request is for run "run-other": another update job is waiting for it`),
			requestOpen: true,
			wantCode:    3,
			wantOut:     []string{"an archive request is already pending", update.ArchiveRequestFileName},
		},
		{
			name:     "an unknown outcome is not a completed pass",
			result:   update.MaintenanceResult{Outcome: "odd"},
			wantCode: 1,
			wantOut:  []string{`the daemon answered "odd"`, "Run archive-now again"},
		},
		{
			name:      "no databases is refused before any request",
			databases: []string{},
			wantCode:  1,
			wantErr:   "no databases to archive",
		},
		{
			name:     "a reading that omits a database is refused, never counted as zero",
			before:   map[string]archiveNowReading{"gt": {commits: 4, events: 2}},
			wantCode: 1,
			wantErr:  "no reading for database hq",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			reads := newArchiveNowTestReads(t, townRoot)
			holdPath := dispatch.HoldFilePath(townRoot)
			requestPath := filepath.Join(update.Dir(townRoot), update.ArchiveRequestFileName)
			if tc.requestOpen {
				reads.stat = archiveNowFakeStat(holdPath, requestPath)
			} else {
				reads.stat = archiveNowFakeStat(holdPath)
			}
			if tc.databases != nil {
				reads.databases = func(string) ([]string, error) { return tc.databases, nil }
			}
			before := tc.before
			if before == nil {
				before = map[string]archiveNowReading{"gt": {commits: 4, events: 2}, "hq": {commits: 0, events: 0}}
			}
			after := tc.after
			if after == nil {
				after = map[string]archiveNowReading{"gt": {commits: 2, events: 0}, "hq": {commits: 0, events: 0}}
			}
			behind, _ := archiveNowBehindScripted(t, before, after)
			reads.behind = behind
			fake := &archiveNowFakeRequest{requestErr: tc.requestErr, result: tc.result, waitErr: tc.waitErr}
			reads.request = func(string) archiveNowRequester { return fake }

			var out bytes.Buffer
			err := runDoltArchiveNowWith(context.Background(), &out, reads)

			if code := archiveNowExitCode(err); code != tc.wantCode {
				t.Fatalf("exit = %d (%v), want %d", code, err, tc.wantCode)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output = %q, want it to hold %q", out.String(), want)
				}
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %v, want it to hold %q", err, tc.wantErr)
				}
			}
			if tc.wantErr != "" && len(fake.requested) != 0 {
				t.Errorf("the request protocol was used before the command could read what it needs: %v", fake.requested)
			}
		})
	}
}

// archiveNowExitCode is the process status the command's error maps to.
func archiveNowExitCode(err error) int {
	if err == nil {
		return 0
	}
	return exitCodeForError(err)
}

// archiveNowStubReader scripts each archive count's result and failure
// separately, so a commits failure and an events failure are reachable without
// a server.
type archiveNowStubReader struct {
	commits   map[string]int64
	events    map[string]int64
	commitErr map[string]error
	eventErr  map[string]error
}

func (r archiveNowStubReader) commitsBehind(_ context.Context, db string, _ daemon.ArchiveWatermark) (int64, error) {
	return r.commits[db], r.commitErr[db]
}

func (r archiveNowStubReader) eventsBehind(_ context.Context, db string, _ daemon.EventsArchiveWatermark) (int64, error) {
	return r.events[db], r.eventErr[db]
}

// The production reading refuses an unreadable watermark file and a count that
// fails, rather than handing back a partial map the command could print and
// check as zeros (gt-xr2byt).
func TestDoltArchiveNowBehindRefusesAnUnreadableReading(t *testing.T) {
	t.Parallel()
	databases := []string{"gt", "hq"}

	t.Run("unreadable watermarks", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("maintenance_state.json: unexpected end of JSON input")
		readings, err := archiveNowBehind(context.Background(), databases,
			func() (daemon.ArchiveWatermarks, error) { return daemon.ArchiveWatermarks{}, boom },
			func([]string) archiveBehindReader {
				t.Error("an archive reader was built with no watermarks to read")
				return nil
			})
		if !errors.Is(err, boom) {
			t.Errorf("error = %v, want it to wrap %v", err, boom)
		}
		if readings != nil {
			t.Errorf("a failed reading returned %v, want no partial map", readings)
		}
	})

	t.Run("a commit count that fails", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("connection refused")
		readings, err := archiveNowBehind(context.Background(), databases,
			func() (daemon.ArchiveWatermarks, error) { return daemon.ArchiveWatermarks{}, nil },
			func([]string) archiveBehindReader {
				return archiveNowStubReader{commitErr: map[string]error{"gt": boom}}
			})
		if !errors.Is(err, boom) {
			t.Errorf("error = %v, want it to wrap %v", err, boom)
		}
		if readings != nil {
			t.Errorf("a failed reading returned %v, want no partial map", readings)
		}
	})

	t.Run("an events count that fails", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("events read failed")
		readings, err := archiveNowBehind(context.Background(), databases,
			func() (daemon.ArchiveWatermarks, error) { return daemon.ArchiveWatermarks{}, nil },
			func([]string) archiveBehindReader {
				return archiveNowStubReader{eventErr: map[string]error{"gt": boom}}
			})
		if !errors.Is(err, boom) {
			t.Errorf("error = %v, want it to wrap %v", err, boom)
		}
		if readings != nil {
			t.Errorf("a failed reading returned %v, want no partial map", readings)
		}
	})
}

// Every read the command needs, failing, is an error (exit 1), never a pass
// with nothing checked: the town root, the hold, the database list, the
// before and after readings, the request write and the wait (gt-xr2byt).
func TestDoltArchiveNowReadsRefuseFailures(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	zeros := map[string]archiveNowReading{"gt": {}, "hq": {}}

	cases := []struct {
		name string
		set  func(t *testing.T, reads *doltArchiveNowReads)
		want string
	}{
		{
			name: "not in a workspace",
			set: func(_ *testing.T, reads *doltArchiveNowReads) {
				reads.townRoot = func() (string, error) { return "", boom }
			},
			want: "not in a Gas Town workspace",
		},
		{
			name: "an unreadable hold",
			set: func(_ *testing.T, reads *doltArchiveNowReads) {
				reads.stat = func(string) error { return boom }
			},
			want: "checking the dispatch hold",
		},
		{
			name: "a database listing that fails",
			set: func(_ *testing.T, reads *doltArchiveNowReads) {
				reads.databases = func(string) ([]string, error) { return nil, boom }
			},
			want: "listing the databases to archive",
		},
		{
			name: "a before reading that fails",
			set: func(_ *testing.T, reads *doltArchiveNowReads) {
				reads.behind = func(context.Context, string, []string) (map[string]archiveNowReading, error) {
					return nil, boom
				}
			},
			want: "reading the archive lag before the pass",
		},
		{
			name: "an after reading that fails",
			set: func(t *testing.T, reads *doltArchiveNowReads) {
				calls := 0
				reads.behind = func(context.Context, string, []string) (map[string]archiveNowReading, error) {
					calls++
					if calls == 1 {
						return zeros, nil
					}
					return nil, boom
				}
			},
			want: "reading the archive lag after the pass",
		},
		{
			name: "a request write that fails",
			set: func(_ *testing.T, reads *doltArchiveNowReads) {
				reads.request = func(string) archiveNowRequester {
					return &archiveNowFakeRequest{requestErr: boom}
				}
			},
			want: "writing the archive request",
		},
		{
			name: "a wait that fails",
			set: func(_ *testing.T, reads *doltArchiveNowReads) {
				reads.request = func(string) archiveNowRequester {
					return &archiveNowFakeRequest{waitErr: boom}
				}
			},
			want: "waiting for the archive request",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			reads := newArchiveNowTestReads(t, townRoot)
			reads.stat = archiveNowFakeStat(dispatch.HoldFilePath(townRoot))
			reads.behind = func(context.Context, string, []string) (map[string]archiveNowReading, error) {
				return zeros, nil
			}
			reads.request = func(string) archiveNowRequester {
				return &archiveNowFakeRequest{result: update.MaintenanceResult{Outcome: archiveOutcomeCompleted}}
			}
			tc.set(t, &reads)

			var out bytes.Buffer
			err := runDoltArchiveNowWith(context.Background(), &out, reads)

			if code := archiveNowExitCode(err); code != 1 {
				t.Fatalf("exit = %d (%v), want 1", code, err)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to hold %q", err, tc.want)
			}
			if !errors.Is(err, boom) {
				t.Errorf("error = %v, want it to wrap the failure", err)
			}
		})
	}
}

// The cobra wrapper silences its own usage print when the exit code is the
// answer, and leaves a plain failure for cobra to report.
func TestDoltArchiveNowCmdSilencesCodedExits(t *testing.T) {
	t.Parallel()

	t.Run("a coded exit is the answer", func(t *testing.T) {
		t.Parallel()
		cmd := &cobra.Command{}
		cmd.SetOut(&bytes.Buffer{})
		reads := newArchiveNowTestReads(t, t.TempDir())
		reads.stat = archiveNowFakeStat() // the hold is absent: a refusal

		err := runDoltArchiveNowCmd(cmd, context.Background(), reads)

		if code, ok := IsSilentExit(err); !ok || code != 3 {
			t.Fatalf("exit = (%v, %v), want silent exit 3", code, ok)
		}
		if !cmd.SilenceErrors || !cmd.SilenceUsage {
			t.Errorf("usage printing = (SilenceErrors %v, SilenceUsage %v), want both silenced", cmd.SilenceErrors, cmd.SilenceUsage)
		}
	})

	t.Run("a plain failure is left to cobra", func(t *testing.T) {
		t.Parallel()
		cmd := &cobra.Command{}
		cmd.SetOut(&bytes.Buffer{})
		reads := newArchiveNowTestReads(t, t.TempDir())
		reads.townRoot = func() (string, error) { return "", errors.New("outside a town") }

		if err := runDoltArchiveNowCmd(cmd, context.Background(), reads); err == nil {
			t.Fatal("a workspace failure returned no error")
		}
		if cmd.SilenceErrors || cmd.SilenceUsage {
			t.Errorf("usage printing = (SilenceErrors %v, SilenceUsage %v), want a plain failure left to cobra", cmd.SilenceErrors, cmd.SilenceUsage)
		}
	})
}

// archiveNowRecordingFiles is update.MaintFiles over an in-memory map,
// recording every exclusive create so a test sees exactly which files the
// command wrote.
type archiveNowRecordingFiles struct {
	entries map[string][]byte
	created []string
}

func newArchiveNowRecordingFiles() *archiveNowRecordingFiles {
	return &archiveNowRecordingFiles{entries: make(map[string][]byte)}
}

func (f *archiveNowRecordingFiles) ReadFile(path string) ([]byte, error) {
	data, ok := f.entries[path]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	return data, nil
}

func (f *archiveNowRecordingFiles) CreateExclusive(path string, data []byte, _ os.FileMode) error {
	if _, ok := f.entries[path]; ok {
		return &fs.PathError{Op: "open", Path: path, Err: fs.ErrExist}
	}
	f.entries[path] = data
	f.created = append(f.created, path)
	return nil
}

func (f *archiveNowRecordingFiles) Remove(path string) error {
	if _, ok := f.entries[path]; !ok {
		return &fs.PathError{Op: "remove", Path: path, Err: fs.ErrNotExist}
	}
	delete(f.entries, path)
	return nil
}

// archiveNowFakeClock stamps the request; the test's sleep plays the daemon,
// so the wait never acts on a real clock.
type archiveNowFakeClock struct{ now time.Time }

func (c archiveNowFakeClock) Now() time.Time { return c.now }

// The command drives the real archive request protocol and writes nothing but
// the archive request: no maintenance request, no gc or backup file
// (gt-xr2byt).
func TestDoltArchiveNowWritesOnlyTheArchiveRequest(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	requestPath := filepath.Join(update.Dir(townRoot), update.ArchiveRequestFileName)
	resultPath := filepath.Join(update.Dir(townRoot), update.ArchiveResultFileName)

	files := newArchiveNowRecordingFiles()
	clock := archiveNowFakeClock{now: time.Date(2026, 11, 5, 21, 0, 0, 0, time.UTC)}
	reads := newArchiveNowTestReads(t, townRoot)
	reads.stat = archiveNowFakeStat(dispatch.HoldFilePath(townRoot))
	behind, _ := archiveNowBehindScripted(t,
		map[string]archiveNowReading{"gt": {commits: 2, events: 1}, "hq": {commits: 0, events: 0}},
		map[string]archiveNowReading{"gt": {commits: 0, events: 0}, "hq": {commits: 0, events: 0}},
	)
	reads.behind = behind
	reads.request = func(string) archiveNowRequester {
		return &update.ArchiveRequest{Dir: update.Dir(townRoot), Files: files, Clock: clock}
	}
	// The sleep is the daemon's tick: it answers the request it read and drops
	// it, exactly as scheduled_maintenance does.
	reads.sleep = func(context.Context, time.Duration) error {
		data, err := files.ReadFile(requestPath)
		if err != nil {
			return err
		}
		var req update.RequestFile
		if err := json.Unmarshal(data, &req); err != nil {
			return err
		}
		answer, err := json.Marshal(update.MaintenanceResult{RunID: req.RunID, Outcome: archiveOutcomeCompleted, At: clock.Now()})
		if err != nil {
			return err
		}
		files.entries[resultPath] = answer
		return files.Remove(requestPath)
	}

	var out bytes.Buffer
	if err := runDoltArchiveNowWith(context.Background(), &out, reads); err != nil {
		t.Fatalf("archive-now: %v\n%s", err, out.String())
	}

	if len(files.created) != 1 || files.created[0] != requestPath {
		t.Errorf("created %v, want only the archive request %s", files.created, requestPath)
	}
	for _, path := range files.created {
		for _, wrong := range []string{update.RequestFileName, update.ResultFileName, "gc", "backup"} {
			if strings.Contains(filepath.Base(path), wrong) {
				t.Errorf("the command wrote %s, which is not the archive request", path)
			}
		}
	}
	if _, ok := files.entries[filepath.Join(update.Dir(townRoot), update.RequestFileName)]; ok {
		t.Errorf("a maintenance request was written")
	}
	if !strings.Contains(string(files.entries[resultPath]), `"run_id":"run-1"`) {
		t.Errorf("the daemon's answer was not for this run: %s", files.entries[resultPath])
	}
}
