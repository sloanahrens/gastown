package land

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// agedEntry is a kept log a fake directory lists beside its age: old enough for
// the prune to want it gone unless the test says otherwise.
type agedEntry struct {
	name string
	at   time.Time
}

func (e agedEntry) Name() string               { return e.name }
func (e agedEntry) IsDir() bool                { return false }
func (e agedEntry) Type() os.FileMode          { return 0 }
func (e agedEntry) Info() (os.FileInfo, error) { return agedInfo{e: e}, nil }

type agedInfo struct{ e agedEntry }

func (i agedInfo) Name() string       { return i.e.name }
func (i agedInfo) Size() int64        { return 0 }
func (i agedInfo) Mode() os.FileMode  { return 0o600 }
func (i agedInfo) ModTime() time.Time { return i.e.at }
func (i agedInfo) IsDir() bool        { return false }
func (i agedInfo) Sys() any           { return nil }

// TestKeepGateLogReportsItsFailures: a log that cannot be written costs the
// log and not the rejection, while a log that cannot be pruned stays kept and
// named (gt-wwqx8). The filesystem is a fake because a directory that refuses
// a write cannot be built on every host: root ignores the permission bits.
func TestKeepGateLogReportsItsFailures(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC)
	old := now.Add(-GateLogRetention - time.Hour)
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
					return []os.DirEntry{agedEntry{name: "deadbeef-1.log", at: old}}, nil
				},
				remove: func(string) error { return errors.New("stuck") },
			},
			wantPath: true,
		},
		{
			name: "a kept log cannot be stat'ed",
			fs: gateLogFS{
				mkdirAll:  okMkdir,
				writeFile: okWrite,
				readDir:   func(string) ([]os.DirEntry, error) { return []os.DirEntry{brokenEntry{name: "deadbeef-1.log"}}, nil },
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
// gate job log under its log directory, named <short-sha>-<unix-time>.log, so
// the note's excerpt can be checked against everything the job printed after
// the run record is gone (gt-wwqx8).
func TestCandidateGateKeepsTheRejectedJobLog(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	dir := writeGateWorkflow(t, gateWorkflowYAML)
	tests := []struct {
		name  string
		tail  string
		whole string
	}{
		{
			// The tail alone satisfies the note; the kept file still has to be
			// the whole log the gate can fetch.
			name:  "the tail names the failure",
			tail:  gateLogPastTheTailCap(gateJobLog("--- FAIL: TestThing (0.01s)", "FAIL\tgithub.com/x/a\t0.2s")),
			whole: gateJobLog("⭐ Run Main actions/checkout@v4", "--- FAIL: TestThing (0.01s)", "FAIL\tgithub.com/x/a\t0.2s"),
		},
		{
			// Neither fetch names the failure, and the wider one is still the
			// log worth keeping.
			name:  "the failure sits past the full-log cap",
			tail:  gateLogPastTheTailCap(""),
			whole: gateLogPastTheTailCap(gateJobLog("⭐ Run Main actions/checkout@v4")),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, sha := redCandidate(t, f, tt.tail)
			client.fullLog = tt.whole
			logDir := filepath.Join(t.TempDir(), "gate-logs", "gastown")
			gate := fastGate(client)
			gate.GateLogDir = logDir

			res := gate.Run(context.Background(), f.git.Open(f.repo), dir, f.work, sha)
			if res.State != CandidateFailed {
				t.Fatalf("state = %v, want failed (err %v)", res.State, res.Err)
			}
			prefix := filepath.Join(logDir, shortSHA(sha)+"-")
			if !strings.HasPrefix(res.GateLog, prefix) || !strings.HasSuffix(res.GateLog, ".log") {
				t.Fatalf("GateLog = %q; want %s<unix-time>.log", res.GateLog, prefix)
			}
			kept, err := os.ReadFile(res.GateLog)
			if err != nil {
				t.Fatalf("reading the kept log: %v", err)
			}
			if string(kept) != tt.whole {
				t.Fatalf("kept %d bytes; want the whole %d-byte log, not the capped tail", len(kept), len(tt.whole))
			}
			// CI output can carry what a failing step printed, secrets included.
			info, err := os.Stat(res.GateLog)
			if err != nil {
				t.Fatalf("stat the kept log: %v", err)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Fatalf("kept log mode = %o; want 0600", perm)
			}
			if len(client.limits) != 2 || client.limits[0] != candidateTailBytes || client.limits[1] != candidateFullLogBytes {
				t.Fatalf("log fetches %v; want the tail, then the whole log", client.limits)
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
// logs older than 14 days and leaves the newer files, so the directory stays
// bounded without a second task (gt-wwqx8).
func TestCandidateGatePrunesGateLogsPastRetention(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	client, sha := redCandidate(t, f, redGateLog())
	dir := filepath.Join(t.TempDir(), "gate-logs", "gastown")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	expired := filepath.Join(dir, "deadbeef-1.log")
	recent := filepath.Join(dir, "cafebabe-2.log")
	// A file the gate did not write is not the prune's to remove.
	foreign := filepath.Join(dir, "notes.txt")
	for path, age := range map[string]time.Duration{
		expired: GateLogRetention + time.Hour,
		recent:  GateLogRetention - time.Hour,
		foreign: GateLogRetention + time.Hour,
	} {
		if err := os.WriteFile(path, []byte("kept\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := now.Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	gate := fastGate(client)
	gate.GateLogDir = dir

	res := gate.Run(context.Background(), f.git.Open(f.repo), writeGateWorkflow(t, gateWorkflowYAML), f.work, sha)
	if res.State != CandidateFailed {
		t.Fatalf("state = %v, want failed (err %v)", res.State, res.Err)
	}
	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Errorf("the log past retention was not removed (stat: %v)", err)
	}
	for _, path := range []string{recent, foreign, res.GateLog} {
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
