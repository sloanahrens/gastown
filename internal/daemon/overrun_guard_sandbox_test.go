package daemon

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
//
// The clone's head is not in the table: the script models it, so a rev-parse
// answers what the clone held when the save made that call. The commit call
// moves the head, which is what tells a save that resolved the branch after
// committing from one that resolved it before and would push the stale tip.
type overrunSandboxSaveScript struct {
	clone overrunSandboxSaveClone
	// commitKey is the WIP commit call; serving it moves the clone's head.
	commitKey string
	answers   map[string]overrunSandboxSaveAnswer
	calls     []overrunSandboxGitCall
}

// preCommitSHA is the clone's head before the save's WIP commit; the head moves
// to wipSHA when the script serves that commit.
const (
	preCommitSHA = "b7e2d4c6a8f01b3d5e7f9a2c4b6d8e0f1a3c5e7b"
	wipSHA       = "5f3a1c2b9e8d7f6a5b4c3d2e1f0a9b8c7d6e5f4a"
)

// run is the runner handed to the daemon as its in-sandbox git runner.
func (s *overrunSandboxSaveScript) run(t *testing.T, dir string, args ...string) ([]byte, error) {
	t.Helper()
	s.calls = append(s.calls, overrunSandboxGitCall{dir: dir, args: append([]string(nil), args...)})
	key := strings.Join(args, " ")
	if sha, ok := s.clone.headFor(args); ok {
		return []byte(sha + "\n"), nil
	}
	answer, ok := s.answers[key]
	if !ok {
		t.Errorf("the save ran an unscripted in-sandbox git call %q in %q", key, dir)
		return nil, errors.New("unscripted in-sandbox git call: " + key)
	}
	// The WIP commit moves the clone's head. A refused commit does not, and the
	// save returns on that refusal before it reads the head again.
	if key == s.commitKey {
		s.clone.head = s.clone.wipCommit
	}
	return []byte(answer.out), answer.err
}

// overrunSandboxSaveClone is the clone the script answers as git would: one
// branch at one head. The head starts at the pre-commit SHA and the commit call
// moves it, so the calls before the commit read one SHA and the calls after it
// read the WIP SHA.
type overrunSandboxSaveClone struct {
	branch    string
	head      string
	wipCommit string
}

// headFor returns the head a rev-parse of HEAD or the clone's branch is
// answered with; ok is false for any other call, which comes from the table.
func (c overrunSandboxSaveClone) headFor(args []string) (string, bool) {
	if len(args) != 2 || args[0] != "rev-parse" {
		return "", false
	}
	if args[1] != "HEAD" && args[1] != "refs/heads/"+c.branch {
		return "", false
	}
	return c.head, true
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
	)
	script := &overrunSandboxSaveScript{
		// The clone is on the seat's branch at the pre-commit SHA; the commit
		// call moves it to wipSHA, so the rev-parse calls after the commit answer
		// the WIP commit and the push's refspec shows it reaching the branch.
		clone:     overrunSandboxSaveClone{branch: stopSeat.Branch, head: preCommitSHA, wipCommit: wipSHA},
		commitKey: "commit -m " + overrunSaveMessage(stopSeat.Bead),
		answers: map[string]overrunSandboxSaveAnswer{
			// The clone holds one staged modification: the seat ran git add and was
			// stopped before it committed.
			"status --porcelain -uall":     {out: "M  " + stagedFile + "\n"},
			"ls-files -v -z":               {out: ""},
			"rev-parse --absolute-git-dir": {out: clone + "/.git\n"},
			// The clone is in no merge and no rebase, so the checkpoint dog's
			// marker probes all print nothing.
			"rev-parse --revs-only MERGE_HEAD":            {out: ""},
			"rev-parse --revs-only rebase-merge/onto":     {out: ""},
			"rev-parse --revs-only rebase-apply/onto":     {out: ""},
			"diff --cached --name-status --no-renames -z": {out: "M\x00" + stagedFile + "\x00"},
			"add -A": {},
			// These are the revert guard's reads. The clone never fetched origin's
			// main, so the guard has no merged baseline to compare against and
			// passes, the behavior checkpointRevertGuard documents.
			"remote get-url origin":                                    {out: "git@github.com:steveyegge/gastown.git\n"},
			"remote get-url upstream":                                  {err: errors.New("fatal: No such remote 'upstream'")},
			"rev-parse origin/main":                                    {err: errors.New("fatal: ambiguous argument 'origin/main': unknown revision")},
			"commit -m " + overrunSaveMessage(stopSeat.Bead):           {},
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

// TestOverrunSandboxCloneDirtyCheckReadsTheCloneNotTheCleanStub proves the
// late dirty check, the read the stop makes before the sandbox is removed,
// examines the sandbox seat's clone and not its host stub. The clone holds an
// edit the seat staged and never committed; the stub is a clean checkout of
// origin's main. A check that read the stub would answer clean and let the
// sandbox, and the only copy of the edit, be deleted.
func TestOverrunSandboxCloneDirtyCheckReadsTheCloneNotTheCleanStub(t *testing.T) {
	t.Parallel()
	const (
		clone      = "/sandbox/gt/gastown/polecats/capable/gastown"
		stagedFile = "internal/foo.go"
	)
	script := &overrunSandboxSaveScript{
		clone: overrunSandboxSaveClone{branch: stopSeat.Branch, head: preCommitSHA, wipCommit: wipSHA},
		answers: map[string]overrunSandboxSaveAnswer{
			// The clone holds one staged modification: the seat ran git add and
			// was stopped before it committed.
			"status --porcelain -uall": {out: "M  " + stagedFile + "\n"},
			// The status read's skip-worktree lookup (sparse checkout), which finds
			// none here.
			"ls-files -v -z": {out: ""},
		},
	}

	d, _ := newOverrunDaemon(t, false)
	reg := session.NewPrefixRegistry()
	reg.Register(overrunStopPrefix, stopSeat.Rig)
	d.prefixRegistryFn = func() *session.PrefixRegistry { return reg }
	d.readSandboxRecord = func(townRoot, name string) (session.SandboxRecord, bool, error) {
		return *overrunStopSandboxRecord(clone), true, nil
	}
	d.sandboxGitRunnerFn = func(townRoot string, rec session.SandboxRecord) (func(dir string, args ...string) ([]byte, error), error) {
		if rec.WorkDir != clone {
			t.Errorf("the in-sandbox runner was built for %q, want the record's clone %q", rec.WorkDir, clone)
		}
		return func(dir string, args ...string) ([]byte, error) { return script.run(t, dir, args...) }, nil
	}

	// The host stub: a clean checkout of origin's main at the seat's host
	// worktree, so a dirty check that read it would answer clean. The opener
	// records every call, and a sandbox seat must never make one.
	stub := gitfake.New()
	origin := filepath.Join(t.TempDir(), "origin.git")
	stub.InitBare(t, origin)
	stub.Commit(t, origin, "main", "initial", map[string]string{"a.txt": "a\n"})
	hostWork := filepath.Join(d.config.TownRoot, stopSeat.Rig, "polecats", stopSeat.Polecat, stopSeat.Rig)
	stub.Clone(t, origin, hostWork)
	hostGit, ok := stub.Open(hostWork).(daemonGit)
	if !ok {
		t.Fatalf("the gitfake stub does not implement daemonGit for %s", hostWork)
	}
	if status, err := hostGit.Status(); err != nil || !status.Clean {
		t.Fatalf("the host stub reads %+v, %v; want a clean checkout, or the test proves nothing", status, err)
	}
	var hostOpens []string
	d.openGitFn = func(dir string) daemonGit {
		hostOpens = append(hostOpens, dir)
		g, ok := stub.Open(dir).(daemonGit)
		if !ok {
			t.Fatalf("the gitfake stub does not implement daemonGit for %s", dir)
		}
		return g
	}

	dirty, err := d.overrunWorktreeDirty(stopSeat)

	if err != nil {
		t.Fatalf("dirty check = %v, want the clone's staged edit read as dirty", err)
	}
	if !dirty {
		t.Fatal("the sandbox clone's uncommitted edit was read as clean: the check read the host stub instead")
	}
	// The check's one status read is two calls: git status, and the
	// skip-worktree lookup its parser makes after it. Nothing else runs.
	want := []overrunSandboxGitCall{
		{dir: clone, args: []string{"status", "--porcelain", "-uall"}},
		{dir: clone, args: []string{"ls-files", "-v", "-z"}},
	}
	if !slices.EqualFunc(script.calls, want, func(a, b overrunSandboxGitCall) bool {
		return a.dir == b.dir && slices.Equal(a.args, b.args)
	}) {
		t.Errorf("in-sandbox calls = %v, want one status read in the clone %q: %v", script.calls, clone, want)
	}
	if len(hostOpens) != 0 {
		t.Errorf("the dirty check opened the host worktree for a sandbox seat: %v", hostOpens)
	}
}

// The clone the mid-rebase fixtures live in, and the edit it holds staged: the
// seat ran `git add` for it and was stopped before it committed.
const (
	overrunMidRebaseClone      = "/sandbox/gt/gastown/polecats/capable/gastown"
	overrunMidRebaseStagedFile = "internal/foo.go"
)

// overrunMidRebaseScript is the in-sandbox runner of a clone the seat left
// mid-rebase: it holds one staged edit, and rebase-merge/onto holds the commit
// the rebase replays onto, the file git writes when a merge-backed rebase
// starts. The rest of what it answers is what the save does to that clone when
// no guard stops it, so a run that reaches add, commit or push is a guard that
// let a mid-rebase clone through.
func overrunMidRebaseScript() *overrunSandboxSaveScript {
	commitKey := "commit -m " + overrunSaveMessage(stopSeat.Bead)
	return &overrunSandboxSaveScript{
		// The clone is on the seat's branch at the pre-commit SHA; the commit
		// call moves it to wipSHA.
		clone:     overrunSandboxSaveClone{branch: stopSeat.Branch, head: preCommitSHA, wipCommit: wipSHA},
		commitKey: commitKey,
		answers: map[string]overrunSandboxSaveAnswer{
			"status --porcelain -uall":                    {out: "M  " + overrunMidRebaseStagedFile + "\n"},
			"ls-files -v -z":                              {out: ""},
			"rev-parse --absolute-git-dir":                {out: overrunMidRebaseClone + "/.git\n"},
			"rev-parse --revs-only MERGE_HEAD":            {out: ""},
			"rev-parse --revs-only rebase-merge/onto":     {out: preCommitSHA + "\n"},
			"rev-parse --revs-only rebase-apply/onto":     {out: ""},
			"diff --cached --name-status --no-renames -z": {out: "M\x00" + overrunMidRebaseStagedFile + "\x00"},
			"add -A": {},
			// The revert guard's reads. The clone never fetched origin's main, so
			// the guard has no merged baseline to compare against and passes, the
			// behavior checkpointRevertGuard documents.
			"remote get-url origin":   {out: "git@github.com:steveyegge/gastown.git\n"},
			"remote get-url upstream": {err: errors.New("fatal: No such remote 'upstream'")},
			"rev-parse origin/main":   {err: errors.New("fatal: ambiguous argument 'origin/main': unknown revision")},
			// The WIP commit and its push, which only a run past the guard makes.
			commitKey:                               {},
			"symbolic-ref refs/remotes/origin/HEAD": {out: "refs/remotes/origin/main\n"},
			"push origin " + wipSHA + ":refs/heads/" + stopSeat.Branch: {},
		},
	}
}

// overrunSandboxSeat makes d route the seat's git calls into script: the seat's
// sandbox record, written to the town root, names the clone directory, and the
// script is the in-sandbox runner.
func overrunSandboxSeat(t *testing.T, d *Daemon, script *overrunSandboxSaveScript) {
	t.Helper()
	reg := session.NewPrefixRegistry()
	reg.Register(overrunStopPrefix, stopSeat.Rig)
	d.prefixRegistryFn = func() *session.PrefixRegistry { return reg }
	if err := session.WriteSandboxRecord(d.config.TownRoot, *overrunStopSandboxRecord(overrunMidRebaseClone)); err != nil {
		t.Fatalf("write the sandbox record: %v", err)
	}
	d.sandboxGitRunnerFn = func(townRoot string, rec session.SandboxRecord) (func(dir string, args ...string) ([]byte, error), error) {
		if rec.WorkDir != overrunMidRebaseClone {
			t.Errorf("the in-sandbox runner was built for %q, want the record's clone %q", rec.WorkDir, overrunMidRebaseClone)
		}
		return func(dir string, args ...string) ([]byte, error) { return script.run(t, dir, args...) }, nil
	}
}

// overrunSandboxHostStub is the host stub the mid-rebase tests give d: a clean
// checkout of origin's main at the seat's host worktree, so a read that reached
// this host would find a clean worktree and nothing to save. It records every
// handle opened here, which a sandbox seat's steps must never open.
type overrunSandboxHostStub struct{ opens []string }

func overrunSandboxHostStubFor(t *testing.T, d *Daemon) *overrunSandboxHostStub {
	t.Helper()
	s := &overrunSandboxHostStub{}
	stub := gitfake.New()
	origin := filepath.Join(t.TempDir(), "origin.git")
	stub.InitBare(t, origin)
	stub.Commit(t, origin, "main", "initial", map[string]string{"a.txt": "a\n"})
	hostWork := filepath.Join(d.config.TownRoot, stopSeat.Rig, "polecats", stopSeat.Polecat, stopSeat.Rig)
	stub.Clone(t, origin, hostWork)
	d.openGitFn = func(dir string) daemonGit {
		s.opens = append(s.opens, dir)
		g, ok := stub.Open(dir).(daemonGit)
		if !ok {
			t.Fatalf("the host stub does not implement daemonGit for %s", dir)
		}
		return g
	}
	return s
}

// overrunSandboxCallAt is the index of the first recorded in-sandbox call whose
// arguments are args, or -1.
func overrunSandboxCallAt(calls []overrunSandboxGitCall, args ...string) int {
	return slices.IndexFunc(calls, func(c overrunSandboxGitCall) bool { return slices.Equal(c.args, args) })
}

// TestOverrunSandboxCloneMidRebaseIsRefusedByTheCheckpointGuard is this bead's
// repro: the seat's clone stopped mid-rebase holds a half-replayed commit and
// one staged edit, and the host stub is clean at main. The save's checkpoint
// must read the rebase markers through the in-sandbox runner, in the clone, and
// refuse the work the host path refuses, before any add, commit or push. A
// guard that read this host finds no marker, commits the half-replayed tree and
// pushes it to the seat's branch.
func TestOverrunSandboxCloneMidRebaseIsRefusedByTheCheckpointGuard(t *testing.T) {
	t.Parallel()
	script := overrunMidRebaseScript()
	d, buf := newOverrunDaemon(t, false)
	overrunSandboxSeat(t, d, script)
	stub := overrunSandboxHostStubFor(t, d)

	saved, err := d.overrunSaveWork(stopSeat)

	if err == nil {
		t.Fatalf("save = %+v, nil; want a refusal: the clone is mid-rebase", saved)
	}
	if !strings.Contains(buf.String(), "checkpoint_dog: skipping "+stopSeat.Rig+"/"+stopSeat.Polecat+": rebase in progress") {
		t.Errorf("log lacks the checkpoint guard's rebase skip for the clone:\n%s", buf.String())
	}
	if overrunSandboxCallAt(script.calls, "rev-parse", "--revs-only", "rebase-merge/onto") < 0 {
		t.Errorf("the guard never asked the clone for its rebase marker: %v", script.calls)
	}
	for _, forbidden := range [][]string{
		{"add", "-A"},
		{"commit", "-m", overrunSaveMessage(stopSeat.Bead)},
		{"push", "origin", wipSHA + ":refs/heads/" + stopSeat.Branch},
	} {
		if at := overrunSandboxCallAt(script.calls, forbidden...); at >= 0 {
			t.Errorf("a clone mid-rebase ran %v (call %d): the guard let it through", forbidden, at)
		}
	}
	for _, c := range script.calls {
		if c.dir != overrunMidRebaseClone {
			t.Errorf("in-sandbox call %v ran in %q, want the record's clone %q", c.args, c.dir, overrunMidRebaseClone)
		}
	}
	if len(stub.opens) != 0 {
		t.Errorf("the save opened the host worktree for a sandbox seat: %v", stub.opens)
	}
}

// A marker probe that fails is an error the checkpoint refuses on, never an
// absent marker: a runner failure must not checkpoint a clone that may be
// mid-rebase.
func TestOverrunSandboxCloneMarkerProbeFailureRefusesTheSave(t *testing.T) {
	t.Parallel()
	script := overrunMidRebaseScript()
	script.answers["rev-parse --revs-only MERGE_HEAD"] = overrunSandboxSaveAnswer{err: errors.New("sandbox gt-capable: the clone is unreachable")}
	d, buf := newOverrunDaemon(t, false)
	overrunSandboxSeat(t, d, script)
	stub := overrunSandboxHostStubFor(t, d)

	saved, err := d.overrunSaveWork(stopSeat)

	if err == nil {
		t.Fatalf("save = %+v, nil; want a refusal: the marker probe failed", saved)
	}
	if !strings.Contains(buf.String(), "cannot read the worktree's operation markers") {
		t.Errorf("log lacks the refused marker probe:\n%s", buf.String())
	}
	if at := overrunSandboxCallAt(script.calls, "add", "-A"); at >= 0 {
		t.Errorf("a clone whose markers could not be read was staged (call %d)", at)
	}
	if len(stub.opens) != 0 {
		t.Errorf("the save opened the host worktree for a sandbox seat: %v", stub.opens)
	}
}

// A stop that cannot save a clone mid-rebase holds at the retry before any
// sandbox step: the sandbox and its record are kept, and the bead is not handed
// back over the work the stop refused to save.
func TestOverrunSandboxCloneMidRebaseKeepsTheSandbox(t *testing.T) {
	t.Parallel()
	script := overrunMidRebaseScript()
	d, buf := newOverrunDaemon(t, false)
	overrunSandboxSeat(t, d, script)
	overrunSandboxHostStubFor(t, d)

	var deleted []string
	f := newOverrunFake(22*time.Minute, stopSeat)
	f.removeSandbox = func(seat overrunSeat) error {
		return d.overrunRemoveSandbox(seat,
			func(string, session.SandboxRecord) error { return nil },
			func(_ string, rec session.SandboxRecord) error {
				deleted = append(deleted, rec.Session)
				return nil
			})
	}
	seams := f.seams()
	seams.saveWork = d.overrunSaveWork

	d.runOverrunGuard(seams)

	if len(deleted) != 0 {
		t.Errorf("the sandbox holding a clone mid-rebase was deleted: %q", deleted)
	}
	if slices.Contains(f.calls, "sandbox "+stopSeat.Rig+"/"+stopSeat.Polecat) {
		t.Errorf("the stop asked to remove the sandbox of a clone mid-rebase: %q", f.calls)
	}
	if _, ok, err := session.ReadSandboxRecord(d.config.TownRoot, overrunStopSessionName); !ok || err != nil {
		t.Errorf("the sandbox record of the kept sandbox is gone (ok %v, err %v)", ok, err)
	}
	if len(f.handedBack) != 0 {
		t.Errorf("the bead was handed back over work the stop refused to save: %q", f.handedBack)
	}
	if !strings.Contains(buf.String(), "work not saved, retrying next tick (1 of") {
		t.Errorf("log lacks the held save:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "rebase in progress") {
		t.Errorf("log lacks the checkpoint guard's rebase skip for the clone:\n%s", buf.String())
	}
}
