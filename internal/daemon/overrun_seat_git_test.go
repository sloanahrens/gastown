package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/session"
)

// overrunSeatGitFixture is a daemon whose seat runs in a sandbox: its record
// names the clone directory, the in-sandbox runner is scripted and records
// every call, and the host handle fails the test if the stop opens it.
type overrunSeatGitFixture struct {
	d       *Daemon
	calls   *[]overrunSandboxGitCall
	hostDir *[]string
}

// newOverrunSeatGitFixture seeds stopSeat's sandbox. record is the record the
// stop reads; a nil runner leaves the runner unlinked; out is what every
// in-sandbox git call prints.
func newOverrunSeatGitFixture(t *testing.T, record *session.SandboxRecord, runnerErr error, out string) overrunSeatGitFixture {
	t.Helper()
	d, _ := newOverrunDaemon(t, false)
	reg := session.NewPrefixRegistry()
	reg.Register(overrunStopPrefix, stopSeat.Rig)
	d.prefixRegistryFn = func() *session.PrefixRegistry { return reg }
	d.readSandboxRecord = func(townRoot, name string) (session.SandboxRecord, bool, error) {
		if record == nil {
			return session.SandboxRecord{}, false, nil
		}
		return *record, true, nil
	}
	var calls []overrunSandboxGitCall
	d.sandboxGitRunnerFn = func(townRoot string, rec session.SandboxRecord) (func(dir string, args ...string) ([]byte, error), error) {
		if runnerErr != nil {
			return nil, runnerErr
		}
		return overrunSandboxGitRecorder(&calls, []byte(out), nil), nil
	}
	var hostDirs []string
	d.openGitFn = func(dir string) daemonGit {
		hostDirs = append(hostDirs, dir)
		t.Errorf("the stop opened the host worktree %q for a sandbox seat", dir)
		return nil
	}
	return overrunSeatGitFixture{d: d, calls: &calls, hostDir: &hostDirs}
}

func overrunStopSandboxRecord(workDir string) *session.SandboxRecord {
	return &session.SandboxRecord{
		Session: overrunStopSessionName,
		Rig:     stopSeat.Rig,
		Polecat: stopSeat.Polecat,
		Branch:  stopSeat.Branch,
		WorkDir: workDir,
	}
}

// TestOverrunSeatGitReadsTheSandboxCloneForASandboxSeat is the repro of the
// 2026-11-14 losses: the sandbox clone holds uncommitted work and the host
// stub is clean. The dirty check must see the clone's work through the
// in-sandbox runner, in the clone directory the record names, and never open
// the host worktree.
func TestOverrunSeatGitReadsTheSandboxCloneForASandboxSeat(t *testing.T) {
	t.Parallel()
	const clone = "/sandbox/gt/gastown/polecats/capable/gastown"
	f := newOverrunSeatGitFixture(t, overrunStopSandboxRecord(clone), nil, " M internal/foo.go\n")

	dirty, err := f.d.overrunWorktreeDirty(stopSeat)
	if err != nil {
		t.Fatalf("overrunWorktreeDirty: %v", err)
	}
	if !dirty {
		t.Fatal("a sandbox clone with uncommitted work was read as clean")
	}
	if len(*f.calls) == 0 {
		t.Fatal("the dirty check made no in-sandbox git call")
	}
	for _, c := range *f.calls {
		if c.dir != clone {
			t.Errorf("in-sandbox call %v ran in %q, want the record's clone %q", c.args, c.dir, clone)
		}
	}
	if first := (*f.calls)[0].args; len(first) == 0 || first[0] != "status" {
		t.Errorf("first in-sandbox call = %v, want git status", first)
	}
	if len(*f.hostDir) != 0 {
		t.Errorf("the host worktree was opened for a sandbox seat: %v", *f.hostDir)
	}
}

// TestOverrunSeatGitKeepsTheHostWorktreeForASeatWithNoSandbox: a seat with no
// sandbox record is a host seat and keeps its host handle, the very one the
// daemon's opener returns; no in-sandbox runner is built for it.
func TestOverrunSeatGitKeepsTheHostWorktreeForASeatWithNoSandbox(t *testing.T) {
	t.Parallel()
	f := newOverrunSeatGitFixture(t, nil, nil, "")
	built := false
	f.d.sandboxGitRunnerFn = func(string, session.SandboxRecord) (func(dir string, args ...string) ([]byte, error), error) {
		built = true
		return nil, errors.New("no runner expected")
	}
	host := git.NewGit(t.TempDir())
	var opened []string
	f.d.openGitFn = func(dir string) daemonGit {
		opened = append(opened, dir)
		return host
	}
	// The host worktree must resolve for the seat: the polecat directory holds one.
	worktree := filepath.Join(f.d.config.TownRoot, stopSeat.Rig, "polecats", stopSeat.Polecat, stopSeat.Rig)
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := f.d.overrunSeatGit(stopSeat)

	if err != nil {
		t.Fatalf("overrunSeatGit: %v", err)
	}
	if got != daemonGit(host) {
		t.Errorf("overrunSeatGit = %v, want the host handle the opener returned", got)
	}
	if len(opened) != 1 || opened[0] != worktree {
		t.Errorf("host handle opened for %v, want exactly the seat's worktree %q", opened, worktree)
	}
	if built {
		t.Error("an in-sandbox runner was built for a seat with no sandbox record")
	}
}

// TestOverrunSeatGitRefusesAnUnreadableSandboxRecord: a record that cannot be
// read is an error, never the host stub.
func TestOverrunSeatGitRefusesAnUnreadableSandboxRecord(t *testing.T) {
	t.Parallel()
	f := newOverrunSeatGitFixture(t, overrunStopSandboxRecord("/sandbox/x"), nil, "")
	f.d.readSandboxRecord = func(string, string) (session.SandboxRecord, bool, error) {
		return session.SandboxRecord{}, false, errors.New("registry unreadable")
	}

	if g, err := f.d.overrunSeatGit(stopSeat); err == nil {
		t.Fatalf("overrunSeatGit = %v, nil error; want a refusal", g)
	}
	if len(*f.hostDir) != 0 {
		t.Errorf("the host worktree was opened: %v", *f.hostDir)
	}
}

// TestOverrunSeatGitRefusesWhatCannotSeeASandboxSeatsWork: a sandbox seat whose
// runner is not linked, or whose record names no clone directory, is an error
// and never the host stub, which would read a clean tree and call the work
// saved.
func TestOverrunSeatGitRefusesWhatCannotSeeASandboxSeatsWork(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		record    *session.SandboxRecord
		runnerErr error
		want      string
	}{
		{"runner not linked", overrunStopSandboxRecord("/sandbox/gt/gastown/polecats/capable/gastown"), session.ErrNoSandboxGitExec, "no in-sandbox git runner"},
		{"record names no clone directory", overrunStopSandboxRecord(""), nil, "names no clone directory"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newOverrunSeatGitFixture(t, tc.record, tc.runnerErr, "")

			g, err := f.d.overrunSeatGit(stopSeat)

			if err == nil {
				t.Fatalf("overrunSeatGit = %v, nil error; want a refusal", g)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
			if len(*f.hostDir) != 0 {
				t.Errorf("the host worktree was opened: %v", *f.hostDir)
			}
		})
	}
}
