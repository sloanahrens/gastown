package land

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// agedEntry is a file a fake directory lists. Its name is its whole age: the
// prune reads the unix time the name carries, and a stat it did not need is
// reported as a failure of the row that asked for it.
type agedEntry struct{ name string }

func (e agedEntry) Name() string      { return e.name }
func (e agedEntry) IsDir() bool       { return false }
func (e agedEntry) Type() os.FileMode { return 0 }
func (e agedEntry) Info() (os.FileInfo, error) {
	return nil, errors.New("retention stat'ed a kept log instead of reading its name")
}

// TestKeepGateLogReportsItsFailures: a log that cannot be written costs the
// log and not the rejection, while a log that cannot be pruned stays kept and
// named (gt-wwqx8). The filesystem is a fake because a directory that refuses
// a write cannot be built on every host: root ignores the permission bits.
func TestKeepGateLogReportsItsFailures(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC)
	okMkdir := func(string, os.FileMode) error { return nil }
	okWrite := func(string, []byte, os.FileMode) error { return nil }
	tests := []struct {
		name     string
		fs       gateLogFS
		wantPath bool
	}{
		{
			name: "the directory cannot be created",
			fs:   gateLogFS{mkdirAll: func(string, os.FileMode) error { return errors.New("no room") }},
		},
		{
			name: "the log cannot be written",
			fs:   gateLogFS{mkdirAll: okMkdir, writeFile: func(string, []byte, os.FileMode) error { return errors.New("no room") }},
		},
		{
			name:     "the directory cannot be listed for the prune",
			fs:       gateLogFS{mkdirAll: okMkdir, writeFile: okWrite, readDir: func(string) ([]os.DirEntry, error) { return nil, errors.New("unreadable") }},
			wantPath: true,
		},
		{
			name: "a kept log cannot be removed",
			fs: gateLogFS{
				mkdirAll:  okMkdir,
				writeFile: okWrite,
				readDir: func(string) ([]os.DirEntry, error) {
					return []os.DirEntry{agedEntry{name: "deadbeef-1.log"}}, nil
				},
				remove: func(string) error { return errors.New("stuck") },
			},
			wantPath: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, err := keepGateLog(tt.fs, "/kept/gastown", "8cc7b0ab", "log\n", now)
			if err == nil {
				t.Fatalf("keepGateLog = %q, nil; want the failure reported", path)
			}
			if tt.wantPath && path == "" {
				t.Fatalf("path = %q; the log was written, so it is still named", path)
			}
			if !tt.wantPath && path != "" {
				t.Fatalf("path = %q; a write that failed names no log", path)
			}
		})
	}
}

// TestPruneGateLogsReadsTheNamesNotTheStat: retention is decided by the time a
// log's name carries, so a kept log is aged by the clock that wrote it and not
// by a filesystem mtime from another one (gt-zua7o). The entries here cannot be
// stat'ed at all: a prune that asked would fail. A name written before
// gt-zua7o carries the same time in nanoseconds and ages the same way.
func TestPruneGateLogsReadsTheNamesNotTheStat(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC)
	expired := fmt.Sprintf("deadbeef-%d.log", now.Add(-GateLogRetention-time.Hour).Unix())
	recent := fmt.Sprintf("cafebabe-%d.log", now.Add(-GateLogRetention+time.Hour).Unix())
	legacyExpired := fmt.Sprintf("deadbeef-%d.log", now.Add(-GateLogRetention-time.Hour).UnixNano())
	legacyRecent := fmt.Sprintf("cafebabe-%d.log", now.Add(-GateLogRetention+time.Hour).UnixNano())
	var removed []string
	fs := gateLogFS{
		readDir: func(string) ([]os.DirEntry, error) {
			return []os.DirEntry{
				agedEntry{name: expired}, agedEntry{name: recent},
				agedEntry{name: legacyExpired}, agedEntry{name: legacyRecent},
			}, nil
		},
		remove: func(path string) error { removed = append(removed, filepath.Base(path)); return nil },
	}
	if err := pruneGateLogs(fs, "/kept/gastown", now); err != nil {
		t.Fatalf("pruneGateLogs: %v; the prune needs no stat", err)
	}
	if !slices.Equal(removed, []string{expired, legacyExpired}) {
		t.Fatalf("removed %v; want only the logs whose name is past retention, seconds or nanoseconds", removed)
	}
}

// redCandidate is the fake Forgejo a rejected gate reads, beside the commit it
// tested: a red required context, the run that posted it, and its gate job
// printing log.
func redCandidate(t *testing.T, f *landFixture, log string) (*fakeForgejo, string) {
	t.Helper()
	sha := f.git.Commit(t, f.repo, "main", "candidate", map[string]string{"c.txt": "x\n"})
	client := &fakeForgejo{
		statuses: []forgejo.CommitStatus{status(forgejo.StateFailure, "ci / gate (push)")},
		runs:     []forgejo.ActionRun{{ID: 7, CommitSHA: sha, Status: "failure"}},
		jobs:     []forgejo.ActionRunJob{{ID: 9, RunID: 7, Name: "gate", Status: "failure"}},
		log:      log,
	}
	return client, sha
}

// redGateLog is a whole gate job log that names the work's own failure: it
// carries the runner's step marker, so the infrastructure signatures leave the
// red as a verdict on the work.
func redGateLog() string {
	return gateJobLog("⭐ Run Main actions/checkout@v4", "--- FAIL: TestThing (0.01s)", "FAIL\tgithub.com/x/a\t0.2s")
}

// gateLogPastTheTailCap pads log beyond the byte cap, so the gate's first
// fetch comes back a truncated tail, as it does for a real long job.
func gateLogPastTheTailCap(log string) string {
	for len(log) < candidateTailBytes {
		log += gateJobLog("ok  \tgithub.com/x/filler\t0.1s")
	}
	return log
}

// TestCandidateGateKeepsTheRejectedJobLog: a red candidate gate keeps the whole
// job log it read for its verdict under its log directory, named
// <short-sha>-<unix-time>.log, so the note's excerpt can be checked against
// everything that log holds after the run record is gone (gt-wwqx8, gt-zua7o).
func TestCandidateGateKeepsTheRejectedJobLog(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	dir := writeGateWorkflow(t, gateWorkflowYAML)
	now := time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC)
	tailCapLog := gateLogPastTheTailCap(redGateLog())
	// A log longer than the read bound: the gate keeps its last
	// candidateFullLogBytes bytes, the widest read the bead allows.
	fullCapLog := strings.Repeat(gateJobLog("ok  \tgithub.com/x/filler\t0.1s"), candidateFullLogBytes/40+1) + redGateLog()
	tests := []struct {
		name string
		log  string
		// want is the part of the log the gate keeps: all of it, or the last
		// candidateFullLogBytes bytes of a log past the bound.
		want string
	}{
		{name: "a short log", log: redGateLog(), want: redGateLog()},
		{
			// The failure sits above more than the excerpt's tail cap: the tail
			// alone would name nothing, and the whole log is what is kept.
			name: "a log past the tail cap",
			log:  tailCapLog,
			want: tailCapLog,
		},
		{
			name: "a log past the full cap",
			log:  fullCapLog,
			want: fullCapLog[len(fullCapLog)-candidateFullLogBytes:],
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, sha := redCandidate(t, f, tt.log)
			logDir := filepath.Join(t.TempDir(), "gate-logs", "gastown")
			gate := fastGate(client)
			gate.GateLogDir = logDir
			gate.Now = func() time.Time { return now }

			res := gate.Run(context.Background(), f.git.Open(f.repo), dir, f.work, sha)
			if res.State != CandidateFailed {
				t.Fatalf("state = %v, want failed (err %v)", res.State, res.Err)
			}
			want := filepath.Join(logDir, fmt.Sprintf("%s-%d.log", shortSHA(sha), now.Unix()))
			if res.GateLog != want {
				t.Fatalf("GateLog = %q; want %s", res.GateLog, want)
			}
			kept, err := os.ReadFile(res.GateLog)
			if err != nil {
				t.Fatalf("reading the kept log: %v", err)
			}
			if string(kept) != tt.want {
				t.Fatalf("kept %d bytes; want the %d bytes of the log the gate keeps", len(kept), len(tt.want))
			}
			// CI output can carry what a failing step printed, secrets included.
			info, err := os.Stat(res.GateLog)
			if err != nil {
				t.Fatalf("stat the kept log: %v", err)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Fatalf("kept log mode = %o; want 0600", perm)
			}
			if !slices.Equal(client.limits, []int64{candidateFullLogBytes}) {
				t.Fatalf("log fetches %v; want the one read the kept log came from", client.limits)
			}
		})
	}
}

// TestGateLogDirKeepsLogsUnderTheTownRuntime: a rig name resolves to a
// directory under the town's runtime, and a name that could leave it -- empty,
// relative, or carrying a separator -- resolves to no directory at all, so no
// log is kept (gt-wwqx8).
func TestGateLogDirKeepsLogsUnderTheTownRuntime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		townRoot string
		rig      string
		want     string
	}{
		{name: "a plain rig", townRoot: "/home/x/gt", rig: "gastown", want: "/home/x/gt/.runtime/gate-logs/gastown"},
		{name: "no town root", townRoot: "", rig: "gastown"},
		{name: "no rig", townRoot: "/home/x/gt", rig: ""},
		{name: "a parent", townRoot: "/home/x/gt", rig: ".."},
		{name: "this directory", townRoot: "/home/x/gt", rig: "."},
		{name: "a nested name", townRoot: "/home/x/gt", rig: "gastown/polecats"},
		{name: "an absolute name", townRoot: "/home/x/gt", rig: "/etc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GateLogDir(tt.townRoot, tt.rig); got != tt.want {
				t.Fatalf("GateLogDir(%q, %q) = %q; want %q", tt.townRoot, tt.rig, got, tt.want)
			}
		})
	}
}

// TestCandidateGateKeepsNoLogWhenTheWriteFails: the red verdict is the
// rejection, and a log the gate could not keep changes neither it nor the
// excerpt the note carries (gt-wwqx8).
func TestCandidateGateKeepsNoLogWhenTheWriteFails(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	client, sha := redCandidate(t, f, redGateLog())
	// A regular file where the log directory belongs makes the write fail.
	blocker := filepath.Join(t.TempDir(), "gate-logs")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	gate := fastGate(client)
	gate.GateLogDir = filepath.Join(blocker, "gastown")
	var out strings.Builder
	gate.Out = &out

	res := gate.Run(context.Background(), f.git.Open(f.repo), writeGateWorkflow(t, gateWorkflowYAML), f.work, sha)
	if res.State != CandidateFailed || res.Err != nil {
		t.Fatalf("state = %v err = %v; want the red verdict unchanged", res.State, res.Err)
	}
	if res.GateLog != "" {
		t.Fatalf("GateLog = %q; a write that failed names no log", res.GateLog)
	}
	if !strings.Contains(res.Tail, "TestThing") {
		t.Fatalf("tail %q; the excerpt has to stand without a kept log", res.Tail)
	}
	if !strings.Contains(out.String(), "keeping the gate job log") {
		t.Fatalf("output %q; want the failed keep logged", out.String())
	}
}

// TestCandidateGateWithNoLogDirectoryKeepsNoLog: a gate built with no log
// directory still rejects exactly as before, and names no log (gt-wwqx8).
func TestCandidateGateWithNoLogDirectoryKeepsNoLog(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	client, sha := redCandidate(t, f, redGateLog())
	gate := fastGate(client)
	var out strings.Builder
	gate.Out = &out

	res := gate.Run(context.Background(), f.git.Open(f.repo), writeGateWorkflow(t, gateWorkflowYAML), f.work, sha)
	if res.State != CandidateFailed || res.GateLog != "" {
		t.Fatalf("state = %v GateLog = %q; want the red verdict with no kept log", res.State, res.GateLog)
	}
	if !strings.Contains(res.Tail, "TestThing") {
		t.Fatalf("tail %q; the excerpt stands without a log directory", res.Tail)
	}
	if strings.Contains(out.String(), "gate job log") {
		t.Fatalf("output %q; a gate that keeps no log reports no failure to keep one", out.String())
	}
}

// TestCandidateGatePrunesGateLogsPastRetention: writing a log deletes the kept
// logs whose name dates them more than 14 days before the new one's clock and
// leaves the newer files, so the directory stays bounded without a second task.
// The age is the name's, so no test stages a filesystem mtime (gt-wwqx8,
// gt-zua7o).
func TestCandidateGatePrunesGateLogsPastRetention(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	client, sha := redCandidate(t, f, redGateLog())
	dir := filepath.Join(t.TempDir(), "gate-logs", "gastown")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC)
	keptAt := func(at time.Time) string {
		return filepath.Join(dir, fmt.Sprintf("deadbeef-%d.log", at.Unix()))
	}
	expired := keptAt(now.Add(-GateLogRetention - time.Hour))
	recent := keptAt(now.Add(-GateLogRetention + time.Hour))
	// Logs kept before gt-zua7o carry the same time in nanoseconds. They are
	// dated by it, so the expired one goes and the recent one stays.
	keptAtNano := func(at time.Time) string {
		return filepath.Join(dir, fmt.Sprintf("cafebabe-%d.log", at.UnixNano()))
	}
	legacyExpired := keptAtNano(now.Add(-GateLogRetention - time.Hour))
	legacyRecent := keptAtNano(now.Add(-GateLogRetention + time.Hour))
	// Files the gate did not write are not the prune's to remove: another
	// suffix, and log names that carry no time.
	foreign := filepath.Join(dir, "notes.txt")
	undated := filepath.Join(dir, "cafebabe.log")
	unstamped := filepath.Join(dir, "deadbeef-x.log")
	for _, path := range []string{expired, recent, legacyExpired, legacyRecent, foreign, undated, unstamped} {
		if err := os.WriteFile(path, []byte("kept\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gate := fastGate(client)
	gate.GateLogDir = dir
	gate.Now = func() time.Time { return now }

	res := gate.Run(context.Background(), f.git.Open(f.repo), writeGateWorkflow(t, gateWorkflowYAML), f.work, sha)
	if res.State != CandidateFailed {
		t.Fatalf("state = %v, want failed (err %v)", res.State, res.Err)
	}
	for _, path := range []string{expired, legacyExpired} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the log past retention was not removed (stat: %v)", err)
		}
	}
	for _, path := range []string{recent, legacyRecent, foreign, undated, unstamped, res.GateLog} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed or never written: %v", path, err)
		}
	}
}

// TestRejectGateNoteNamesTheKeptGateLog: a gate rejection's note names the kept
// log on one line, so a polecat can read past the excerpt's cut (gt-wwqx8).
func TestRejectGateNoteNamesTheKeptGateLog(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	logPath := filepath.Join(t.TempDir(), "gate-logs", "gastown", "8cc7b0ab-1793123456.log")
	cand := &fakeCandidate{res: CandidateResult{
		State: CandidateFailed, Context: "ci / gate (push)", Tail: "--- FAIL: TestThing\n", GateLog: logPath, Pushed: true}}
	l.Candidate = cand

	_, err := l.Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectGate, LabelRework)
	if rej.GateLog != logPath {
		t.Errorf("rejection GateLog = %q, want %q", rej.GateLog, logPath)
	}
	notes := f.bead().Notes
	if !strings.Contains(notes, "\nGate log: "+logPath) {
		t.Fatalf("the rejection note does not name the kept log %q:\n%s", logPath, notes)
	}
	got, ok := ParseRejectionNote(notes)
	if !ok || got.GateLog != logPath {
		t.Fatalf("ParseRejectionNote GateLog = %q ok = %v; want the named path", got.GateLog, ok)
	}
}
