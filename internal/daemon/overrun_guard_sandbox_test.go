package daemon

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/session"
)

// overrunSandboxSaveAnswer is what the scripted clone prints for one git call,
// or the error it answers with.
type overrunSandboxSaveAnswer struct {
	out string
	err error
}

// overrunSandboxSaveScript is the in-sandbox runner of the repro: it answers
// each git call from a table keyed by the call's arguments and records every
// call it saw, so the test reads the calls the save really made. A call the
// table does not hold is named as a failure, never answered with an empty
// success the save could read as done.
type overrunSandboxSaveScript struct {
	answers map[string]overrunSandboxSaveAnswer
	calls   []overrunSandboxGitCall
}

// run is the runner handed to the daemon as its in-sandbox git runner.
func (s *overrunSandboxSaveScript) run(t *testing.T, dir string, args ...string) ([]byte, error) {
	t.Helper()
	s.calls = append(s.calls, overrunSandboxGitCall{dir: dir, args: append([]string(nil), args...)})
	key := strings.Join(args, " ")
	answer, ok := s.answers[key]
	if !ok {
		t.Errorf("the save ran an unscripted in-sandbox git call %q in %q", key, dir)
		return nil, errors.New("unscripted in-sandbox git call: " + key)
	}
	return []byte(answer.out), answer.err
}

// TestOverrunSandboxGitSaveCommitsAndPushesTheDirtyClone is the repro of the
// 2026-11-14 losses the stop exists to prevent: the seat's sandbox clone holds
// an edit the seat staged and never committed, and the host stub is clean at
// main. The save step must make the WIP commit and push it through the
// in-sandbox runner, in the record's clone directory, and never read the host
// stub, which is clean and so would save nothing.
func TestOverrunSandboxGitSaveCommitsAndPushesTheDirtyClone(t *testing.T) {
	t.Parallel()
	const (
		clone      = "/sandbox/gt/gastown/polecats/capable/gastown"
		stagedFile = "internal/foo.go"
		// The commit the WIP commit leaves HEAD on. The script answers the
		// rev-parse calls made after the commit with it, so the push's refspec
		// shows the WIP commit reaching the branch.
		wipSHA = "5f3a1c2b9e8d7f6a5b4c3d2e1f0a9b8c7d6e5f4a"
	)
	script := &overrunSandboxSaveScript{answers: map[string]overrunSandboxSaveAnswer{
		// The clone holds one staged modification: the seat ran git add and was
		// stopped before it committed.
		"status --porcelain -uall":                    {out: "M  " + stagedFile + "\n"},
		"ls-files -v -z":                              {out: ""},
		"rev-parse --absolute-git-dir":                {out: clone + "/.git\n"},
		"diff --cached --name-status --no-renames -z": {out: "M\x00" + stagedFile + "\x00"},
		"add -A": {},
		// These are the revert guard's reads. The clone never fetched origin's
		// main, so the guard has no merged baseline to compare against and
		// passes, the behavior checkpointRevertGuard documents.
		"remote get-url origin":                                    {out: "git@github.com:steveyegge/gastown.git\n"},
		"remote get-url upstream":                                  {err: errors.New("fatal: No such remote 'upstream'")},
		"rev-parse origin/main":                                    {err: errors.New("fatal: ambiguous argument 'origin/main': unknown revision")},
		"commit -m " + overrunSaveMessage(stopSeat.Bead):           {},
		"rev-parse refs/heads/" + stopSeat.Branch:                  {out: wipSHA + "\n"},
		"rev-parse HEAD":                                           {out: wipSHA + "\n"},
		"symbolic-ref refs/remotes/origin/HEAD":                    {out: "refs/remotes/origin/main\n"},
		"push origin " + wipSHA + ":refs/heads/" + stopSeat.Branch: {},
	}}

	d, _ := newOverrunDaemon(t, false)
	reg := session.NewPrefixRegistry()
	reg.Register(overrunStopPrefix, stopSeat.Rig)
	d.prefixRegistryFn = func() *session.PrefixRegistry { return reg }
	record := overrunStopSandboxRecord(clone)
	d.readSandboxRecord = func(townRoot, name string) (session.SandboxRecord, bool, error) {
		return *record, true, nil
	}
	d.sandboxGitRunnerFn = func(townRoot string, rec session.SandboxRecord) (func(dir string, args ...string) ([]byte, error), error) {
		if rec.WorkDir != clone {
			t.Errorf("the in-sandbox runner was built for %q, want the record's clone %q", rec.WorkDir, clone)
		}
		return func(dir string, args ...string) ([]byte, error) { return script.run(t, dir, args...) }, nil
	}

	// The host stub: a clean checkout of origin's main at the seat's host
	// worktree, so a save that read it would find nothing to commit. The opener
	// records every call, and a sandbox seat must never make one.
	stub := gitfake.New()
	origin := filepath.Join(t.TempDir(), "origin.git")
	stub.InitBare(t, origin)
	stubBase := stub.Commit(t, origin, "main", "initial", map[string]string{"a.txt": "a\n"})
	hostWork := filepath.Join(d.config.TownRoot, stopSeat.Rig, "polecats", stopSeat.Polecat, stopSeat.Rig)
	stub.Clone(t, origin, hostWork)
	var hostOpens []string
	d.openGitFn = func(dir string) daemonGit {
		hostOpens = append(hostOpens, dir)
		g, ok := stub.Open(dir).(daemonGit)
		if !ok {
			t.Fatalf("the gitfake stub does not implement daemonGit for %s", dir)
		}
		return g
	}

	got, err := d.overrunSaveWork(stopSeat)

	if err != nil {
		t.Fatalf("save = %v, want the WIP commit and its push", err)
	}
	if !got.Committed || got.Head != wipSHA {
		t.Errorf("save = %+v, want the WIP commit %s made and pushed", got, wipSHA)
	}
	commitAt := slices.IndexFunc(script.calls, func(c overrunSandboxGitCall) bool {
		return slices.Equal(c.args, []string{"commit", "-m", overrunSaveMessage(stopSeat.Bead)})
	})
	pushAt := slices.IndexFunc(script.calls, func(c overrunSandboxGitCall) bool {
		return slices.Equal(c.args, []string{"push", "origin", wipSHA + ":refs/heads/" + stopSeat.Branch})
	})
	if commitAt < 0 {
		t.Errorf("recorded in-sandbox calls hold no WIP commit named for the bead: %v", script.calls)
	}
	if pushAt < 0 {
		t.Errorf("recorded in-sandbox calls hold no push of the WIP commit %s to %s: %v", wipSHA, stopSeat.Branch, script.calls)
	}
	if commitAt >= 0 && pushAt >= 0 && pushAt < commitAt {
		t.Errorf("the push (call %d) came before the commit (call %d); the WIP commit must exist first", pushAt, commitAt)
	}
	for _, c := range script.calls {
		if c.dir != clone {
			t.Errorf("in-sandbox call %v ran in %q, want the record's clone %q", c.args, c.dir, clone)
		}
	}
	if len(hostOpens) != 0 {
		t.Errorf("the stop opened the host worktree for a sandbox seat: %v", hostOpens)
	}
	if tip := stub.Ref(hostWork, "refs/heads/main"); tip != stubBase {
		t.Errorf("the host stub moved to %q, want it left at %q: the stub's worktree is not the seat's work", tip, stubBase)
	}
}
