package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// pushTick runs one guard tick a minute after the last, without the seat list
// (a stopped seat is no longer listed; the pending stop resumes from its state).
func pushTick(d *Daemon, f *overrunFake) {
	f.seats, f.calls = nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
}

// Failures short of the cap that end in a good push hand the bead back with no
// note and release the seat as before (gt-bcgyj).
func TestStop_PushThatRecoversBeforeTheCapDoesNotGiveUp(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.failOn = map[string]error{"push": errors.New("remote unreachable")}
	d.runOverrunGuard(f.seams())
	for i := 2; i < overrunPushMaxAttempts; i++ {
		pushTick(d, f)
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); st.PushFailures != overrunPushMaxAttempts-1 || st.PushGaveUp {
		t.Fatalf("state %+v, want %d failures and no give-up", st, overrunPushMaxAttempts-1)
	}

	f.failOn = nil
	pushTick(d, f)

	if len(f.handBackNotes) != 1 || f.handBackNotes[0] != "" {
		t.Errorf("notes = %q, want one empty note", f.handBackNotes)
	}
	want := []string{"recheck gastown/capable", "push gastown/capable", "hand-back gastown/capable", "recheck gastown/capable", "reset-agent-bead gastown/capable", "record-cleanup gastown/capable", "retire gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls = %q, want %q (seat released)", f.calls, want)
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); st.PushedAt.IsZero() || st.PushGaveUp || st.StoppedAt.IsZero() {
		t.Errorf("state %+v, want pushed, not gave up, stopped", st)
	}
}

// A push refused on every tick is bounded: after overrunPushMaxAttempts the
// bead is handed back with a note naming the worktree and the cause, the pause
// stays down and the agent bead is left alone, and the stop does not run again.
func TestStop_PersistentlyFailedPushIsGivenUpOnAndHandedBack(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.failOn = map[string]error{"push": errors.New("remote: permission\ndenied")}

	d.runOverrunGuard(f.seams())
	for i := 2; i < overrunPushMaxAttempts; i++ {
		pushTick(d, f)
		if len(f.handedBack) != 0 {
			t.Fatalf("tick %d: handed back before the cap: %q", i, f.handedBack)
		}
	}
	pushTick(d, f)

	want := []string{"recheck gastown/capable", "push gastown/capable", "hand-back gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls at the cap = %q, want %q (no reset, no retire: the seat stays parked)", f.calls, want)
	}
	if len(f.handedBack) != 1 || len(f.handBackNotes) != 1 {
		t.Fatalf("hand-backs = %q notes = %q, want one each", f.handedBack, f.handBackNotes)
	}
	worktree := filepath.Join(d.config.TownRoot, "gastown", "polecats", "capable")
	for _, w := range []string{
		"exists only in the seat's worktree (" + worktree + ")",
		"could not be pushed to origin (remote: permission denied)",
		"gastown/capable stays paused", "gt agent resume",
	} {
		if !strings.Contains(f.handBackNotes[0], w) {
			t.Errorf("hand-back note %q lacks %q", f.handBackNotes[0], w)
		}
	}
	if !strings.Contains(buf.String(), "giving up on the push of branch polecat/capable/gt-aaa11+x1 after 5 attempt(s)") {
		t.Errorf("log lacks the give-up line:\n%s", buf.String())
	}
	st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11")
	if st.StoppedAt.IsZero() || !st.PushedAt.IsZero() || !st.PushGaveUp || st.PushFailures != overrunPushMaxAttempts {
		t.Errorf("state %+v, want stopped, not pushed, gave up after %d failures", st, overrunPushMaxAttempts)
	}

	pushTick(d, f)
	if len(f.calls) != 0 || len(f.handedBack) != 1 {
		t.Errorf("a finished stop ran again: calls %q hand-backs %q", f.calls, f.handedBack)
	}
}

// A rejection no retry can change, an origin that holds other history for the
// branch, gives up on the first tick and reports the origin sha.
func TestStop_NonFastForwardPushGivesUpAtOnceWithTheOriginSha(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.failOn = map[string]error{"push": &overrunPushDivergedError{
		Branch: stopSeat.Branch, OriginSHA: "9f8e7d6", Err: errors.New("rejected (non-fast-forward)"),
	}}

	d.runOverrunGuard(f.seams())

	want := []string{"pause gastown/capable", "stop gastown/capable", "checkpoint gastown/capable", "push gastown/capable", "hand-back gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls = %q, want %q (one push, then the hand-back, seat left paused)", f.calls, want)
	}
	if len(f.handBackNotes) != 1 || !strings.Contains(f.handBackNotes[0], "origin holds a different history for this branch (origin sha 9f8e7d6)") {
		t.Errorf("notes = %q, want the different-history note with the origin sha", f.handBackNotes)
	}
	st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11")
	if st.StoppedAt.IsZero() || !st.PushGaveUp || st.PushFailures != 1 {
		t.Errorf("state %+v, want stopped and gave up after one failure", st)
	}
}

// Both caps can trip in one stop: the comment carries both notes.
func TestStop_GivenUpCheckpointAndPushBothAreNoted(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(22*time.Minute, stopSeat)
	f.failOn = map[string]error{"checkpoint": errors.New("refused"), "push": errors.New("no route")}
	d.runOverrunGuard(f.seams())
	for i := 1; i < overrunCheckpointMaxAttempts+overrunPushMaxAttempts; i++ {
		pushTick(d, f)
	}

	if len(f.handBackNotes) != 1 {
		t.Fatalf("notes = %q, want one hand-back", f.handBackNotes)
	}
	for _, w := range []string{"could not be checkpointed", "exists only in the seat's worktree"} {
		if !strings.Contains(f.handBackNotes[0], w) {
			t.Errorf("note %q lacks %q", f.handBackNotes[0], w)
		}
	}
	if len(f.agentBeadsReset) != 0 {
		t.Errorf("agent bead reset %q although the seat stays parked", f.agentBeadsReset)
	}
}

// The push comes after the checkpoint and before the hand-back, and a push that
// fails stops the stop there: the bead is not handed back (its comment would
// name a branch that exists only in the worktree), and every later tick retries
// the push alone.
func TestStop_FailedPushHoldsTheHandBackAndIsRetried(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.failOn = map[string]error{"push": errors.New("remote unreachable")}

	d.runOverrunGuard(f.seams())

	want := []string{"pause gastown/capable", "stop gastown/capable", "checkpoint gastown/capable", "push gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls = %q, want %q (nothing after the failed push)", f.calls, want)
	}
	if len(f.handedBack) != 0 || len(f.agentBeadsReset) != 0 {
		t.Errorf("hand-back %q / agent bead reset %q happened past a failed push", f.handedBack, f.agentBeadsReset)
	}
	if !strings.Contains(buf.String(), "push of branch polecat/capable/gt-aaa11+x1 failed, retrying next tick: remote unreachable") {
		t.Errorf("log lacks the push failure:\n%s", buf.String())
	}
	st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11")
	if !st.StoppedAt.IsZero() || !st.PushedAt.IsZero() || st.CheckpointedAt.IsZero() {
		t.Errorf("state %+v, want checkpointed, not pushed, not stopped", st)
	}

	// Still failing on the next tick: the checkpoint is not repeated.
	f.seats, f.calls = nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	want = []string{"recheck gastown/capable", "push gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("retry calls = %q, want %q", f.calls, want)
	}

	// The push goes through: the hand-back follows it, the seat is released,
	// and the pushed branch is the one the comment names.
	f.calls, f.failOn = nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	want = []string{"recheck gastown/capable", "push gastown/capable", "hand-back gastown/capable", "recheck gastown/capable", "reset-agent-bead gastown/capable", "record-cleanup gastown/capable", "retire gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls after the push recovered = %q, want %q", f.calls, want)
	}
	if len(f.pushed) != 1 || f.pushed[0] != "gt-aaa11 polecat/capable/gt-aaa11+x1" {
		t.Errorf("pushed %q, want the kept branch", f.pushed)
	}
	if len(f.handedBack) != 1 || !strings.HasPrefix(f.handedBack[0], "gt-aaa11 polecat/capable/gt-aaa11+x1 ") {
		t.Errorf("hand-back %q, want one naming the pushed branch", f.handedBack)
	}
}

// A stop that gave up on the checkpoint still pushes what the branch holds
// before its hand-back, and stays paused with the agent bead untouched.
func TestStop_GivenUpCheckpointStillPushesAndLeavesTheAgentBead(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(22*time.Minute, stopSeat)
	f.failOn = map[string]error{"checkpoint": errors.New("refused")}
	for i := 0; i < overrunCheckpointMaxAttempts; i++ {
		d.runOverrunGuard(f.seams())
		f.seats = nil
		f.now = f.now.Add(time.Minute)
	}

	if len(f.pushed) != 1 || len(f.handedBack) != 1 {
		t.Fatalf("pushed %q, handed back %q, want one each", f.pushed, f.handedBack)
	}
	if len(f.agentBeadsReset) != 0 {
		t.Errorf("agent bead reset %q although the seat stays parked", f.agentBeadsReset)
	}
}

// The agent-bead reset is part of the release: a reset that fails leaves the
// pause down and the stop pending, and the next tick finishes both.
func TestStop_FailedAgentBeadResetKeepsTheSeatParkedAndIsRetried(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, stopSeat)
	f.failOn = map[string]error{"reset-agent-bead": errors.New("bd down")}

	d.runOverrunGuard(f.seams())

	if got := f.calls[len(f.calls)-1]; got != "reset-agent-bead gastown/capable" {
		t.Errorf("last call = %q, want the failed reset (pause still down)", got)
	}
	if !strings.Contains(buf.String(), "agent bead not reset after the hand-back of gt-aaa11: bd down") {
		t.Errorf("log lacks the failed reset:\n%s", buf.String())
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); !st.StoppedAt.IsZero() {
		t.Error("StoppedAt written although the release did not finish")
	}

	f.seats, f.calls, f.failOn = nil, nil, nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	want := []string{"recheck gastown/capable", "hand-back gastown/capable", "recheck gastown/capable", "reset-agent-bead gastown/capable", "record-cleanup gastown/capable", "retire gastown/capable"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("retry calls = %q, want %q (the push is not repeated)", f.calls, want)
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); st.StoppedAt.IsZero() {
		t.Error("StoppedAt not written once the seat was released")
	}
}

const nuxAgentBead = "gt-gastown-polecat-nux"

// agentBeadFake is a store holding nux's agent bead, hooked to hook.
func agentBeadFake(t *testing.T, hook string) *beadsfake.Fake {
	t.Helper()
	c := beadsfake.New()
	if _, err := c.Create(beads.CreateOptions{
		ID: nuxAgentBead, Title: nuxAgentBead, Labels: []string{"gt:agent"}, Priority: -1,
		Description: beads.FormatAgentDescription(nuxAgentBead, &beads.AgentFields{
			RoleType: "polecat", Rig: "gastown", AgentState: "working", HookBead: hook,
			Branch: "polecat/nux/gt-0pt32+x1", CleanupStatus: "has_unpushed",
		}),
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

func nuxAgentFields(t *testing.T, c beads.Client) *beads.AgentFields {
	t.Helper()
	issue, err := c.Show(nuxAgentBead)
	if err != nil {
		t.Fatal(err)
	}
	return beads.ParseAgentFields(issue.Description)
}

// The release step resets the stopped seat's agent bead like a nuke: the hook
// is cleared and the state reads nuked, so the roster stops counting the seat.
func TestResetOverrunAgentBead_ClearsTheHandedBackHook(t *testing.T) {
	t.Parallel()
	c := agentBeadFake(t, "gt-0pt32")

	if err := resetOverrunAgentBead(c, nuxAgentBead, "gt-0pt32"); err != nil {
		t.Fatal(err)
	}

	got := nuxAgentFields(t, c)
	if got.HookBead != "" || got.Branch != "" || got.CleanupStatus != "" || got.AgentState != string(beads.AgentStateNuked) {
		t.Errorf("agent bead after the reset = %+v, want hook, branch and cleanup cleared and state nuked", got)
	}
	if got.RoleType != "polecat" || got.Rig != "gastown" {
		t.Errorf("reset lost the identity fields: %+v", got)
	}
}

// A re-sling to the same polecat name that has already hooked another bead is
// not clobbered, and a missing agent bead or one already reset is not an error.
func TestResetOverrunAgentBead_LeavesAReSlungSeatAlone(t *testing.T) {
	t.Parallel()

	c := agentBeadFake(t, "gt-newer")
	if err := resetOverrunAgentBead(c, nuxAgentBead, "gt-0pt32"); err != nil {
		t.Fatal(err)
	}
	if got := nuxAgentFields(t, c); got.HookBead != "gt-newer" || got.AgentState != "working" {
		t.Errorf("re-slung agent bead = %+v, want hook gt-newer and state working untouched", got)
	}

	// Already reset by an earlier tick: nothing left to clear, nothing rewritten.
	unhooked := agentBeadFake(t, "")
	if err := resetOverrunAgentBead(unhooked, nuxAgentBead, "gt-0pt32"); err != nil {
		t.Fatal(err)
	}
	if got := nuxAgentFields(t, unhooked); got.AgentState != "working" {
		t.Errorf("agent bead with no hook was rewritten: %+v", got)
	}

	if err := resetOverrunAgentBead(beadsfake.New(), nuxAgentBead, "gt-0pt32"); err != nil {
		t.Errorf("missing agent bead: %v, want nil", err)
	}
}

// A read that fails is an error, not a seat to leave alone: the reset reports
// it and the hook stays for the next tick to retry.
func TestResetOverrunAgentBead_ReturnsAFailedRead(t *testing.T) {
	t.Parallel()
	c := agentBeadFake(t, "gt-0pt32")
	boom := errors.New("show timed out")
	c.FailWith("show", boom)

	if err := resetOverrunAgentBead(c, nuxAgentBead, "gt-0pt32"); !errors.Is(err, boom) {
		t.Fatalf("reset over a failed read = %v, want the read error", err)
	}

	c.FailWith("show", nil)
	if got := nuxAgentFields(t, c); got.HookBead != "gt-0pt32" {
		t.Errorf("agent bead after a failed read = %+v, want hook gt-0pt32 kept", got)
	}
}

// A seat with no bead names no hook to match, so the reset never clears one.
func TestResetOverrunAgentBead_LeavesASeatWithNoBeadAlone(t *testing.T) {
	t.Parallel()
	c := agentBeadFake(t, "")

	if err := resetOverrunAgentBead(c, nuxAgentBead, ""); err != nil {
		t.Fatal(err)
	}
	if got := nuxAgentFields(t, c); got.AgentState != "working" {
		t.Errorf("agent bead with an empty bead was rewritten: %+v", got)
	}
}

// The cleanup status the reset cleared is recorded from the worktree the
// release keeps: clean once the checkpoint committed and the push put the
// branch on origin, and the worktree's own state when it holds more.
func TestOverrunCleanupStatus_ReadsTheWorktree(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	workDir, f := checkpointSeat(t, d, map[string]string{"a.go": "package a\n"})
	g := d.gitAt(workDir)

	if got := overrunCleanupStatus(g); got != string(polecat.CleanupClean) {
		t.Errorf("clean worktree = %q, want clean", got)
	}
	writeWorkFiles(t, workDir, map[string]string{"a.go": "package a\n// work\n"})
	if got := overrunCleanupStatus(g); got != string(polecat.CleanupUncommitted) {
		t.Errorf("worktree with uncommitted work = %q, want has_uncommitted", got)
	}
	f.Stash(t, workDir, "wip")
	if got := overrunCleanupStatus(g); got != string(polecat.CleanupStash) {
		t.Errorf("worktree with a stash = %q, want has_stash", got)
	}
	// A worktree the probe cannot read is unknown, never clean: the nuke
	// preflight reads unknown and a missing status the same way.
	if got := overrunCleanupStatus(d.gitAt(t.TempDir())); got != string(polecat.CleanupUnknown) {
		t.Errorf("unreadable worktree = %q, want unknown", got)
	}
}

// recordOverrunCleanupStatus writes the probed status onto the seat's agent
// bead, which the release's reset left missing.
func TestRecordOverrunCleanupStatus_WritesTheAgentBead(t *testing.T) {
	t.Parallel()
	c := agentBeadFake(t, "gt-0pt32")

	if err := recordOverrunCleanupStatus(c, nuxAgentBead, string(polecat.CleanupClean)); err != nil {
		t.Fatal(err)
	}

	got := nuxAgentFields(t, c)
	if got.CleanupStatus != string(polecat.CleanupClean) || got.HookBead != "gt-0pt32" {
		t.Errorf("agent bead after recording = %+v, want cleanup clean with the rest untouched", got)
	}
	if err := recordOverrunCleanupStatus(c, "", string(polecat.CleanupClean)); err == nil {
		t.Error("recording on no bead id succeeded, want an error")
	}
}

// A seat the guard stopped and released is not reported dead: the release
// retires its record to stop in the write that lifts the pause, so a seat with
// no session is never left reading as a running seat whose session died
// (gt-6ixtp).
func TestStop_ReleasedSeatReadsIdleNotDead(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 1, 0, 0, 0, time.UTC)
	d, _ := newOverrunDaemon(t, false)
	town := d.config.TownRoot
	// The fixture is a stopped, released seat: the polecat directory the kept
	// branch lives in is still there, the session is gone, and the last
	// liveness sample found the seat dead.
	if err := os.MkdirAll(filepath.Join(town, stopSeat.Rig, "polecats", stopSeat.Polecat), 0o755); err != nil {
		t.Fatal(err)
	}
	seat := intent.Seat{Rig: stopSeat.Rig, Role: constants.RolePolecat, Name: stopSeat.Polecat}
	park := func() {
		t.Helper()
		if err := agentpause.Pause(town, stopSeat.Rig, constants.RolePolecat, stopSeat.Polecat, overrunPauseReason, overrunActor, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := intent.Update(town, seat, func(r *intent.Record) error {
			r.Progress = &intent.Progress{SampledAt: now, DeadSamples: 2}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	health := func() []townhealth.Seat {
		t.Helper()
		got, err := (&healthSources{d: d, evidence: time.Hour, now: now}).Seats()
		if err != nil {
			t.Fatalf("Seats: %v", err)
		}
		return got
	}
	park()

	// Lifting the pause alone — what the release used to do — is the bug: the
	// record stays at run with no session, so the field judges the seat dead.
	if err := agentpause.Resume(town, stopSeat.Rig, constants.RolePolecat, stopSeat.Polecat); err != nil {
		t.Fatal(err)
	}
	if got := health(); len(got) != 1 || got[0].DeadSamples != 2 {
		t.Fatalf("after the pause alone: health = %+v, want the sessionless seat judged dead", got)
	}

	// The release retires it: nothing runs the seat, so the field does not
	// judge it at all, with no operator action.
	park()
	if err := agentpause.Retire(town, stopSeat.Rig, constants.RolePolecat, stopSeat.Polecat, overrunActor); err != nil {
		t.Fatal(err)
	}
	if got := health(); len(got) != 0 {
		t.Errorf("after the release: health = %+v, want the released seat left out of the seat field", got)
	}
}

// pushStubGit stands in for the seat's worktree git: it knows one local branch
// and records the pushes.
type pushStubGit struct {
	daemonGit
	branch, sha string
	pushErr     error
	pushes      []string
	// originRefs is what ls-remote answers; originErr fails it.
	originRefs []git.RemoteRef
	originErr  error
	// commitTime is what CommitTime answers for the branch head; the zero time
	// means the commit time cannot be read.
	commitTime time.Time
}

func (g *pushStubGit) ListRemoteRefsWithHashes(remote, prefix string) ([]git.RemoteRef, error) {
	return g.originRefs, g.originErr
}

func (g *pushStubGit) CurrentBranch() (string, error) { return g.branch, nil }

func (g *pushStubGit) CommitTime(rev string) (time.Time, error) {
	if g.commitTime.IsZero() {
		return time.Time{}, errors.New("no commit time for " + rev)
	}
	return g.commitTime, nil
}

func (g *pushStubGit) Rev(ref string) (string, error) {
	if ref != "refs/heads/"+g.branch {
		return "", errors.New("unknown revision " + ref)
	}
	return g.sha, nil
}

func (g *pushStubGit) Push(remote, refspec string, force bool) error {
	g.pushes = append(g.pushes, remote+" "+refspec)
	if force {
		g.pushes = append(g.pushes, "FORCED")
	}
	return g.pushErr
}

// The production push sends the local branch's commit to the same branch name
// on origin, without force: git push origin <sha>:refs/heads/<branch>.
func TestOverrunPushBranch_PushesTheBranchTipToOrigin(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	worktree := filepath.Join(d.config.TownRoot, "gastown", "polecats", "capable", "gastown")
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	g := &pushStubGit{branch: stopSeat.Branch, sha: "abc123"}
	d.openGitFn = func(dir string) daemonGit {
		if dir != worktree {
			t.Errorf("git opened in %s, want the seat's worktree %s", dir, worktree)
		}
		return g
	}

	if err := d.overrunPushBranch(stopSeat); err != nil {
		t.Fatal(err)
	}
	if want := []string{"origin abc123:refs/heads/" + stopSeat.Branch}; !reflect.DeepEqual(g.pushes, want) {
		t.Errorf("pushes = %q, want %q", g.pushes, want)
	}

	// A refused push is the stop's error, so it retries.
	g.pushErr = errors.New("rejected")
	if err := d.overrunPushBranch(stopSeat); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("push error = %v, want the rejection", err)
	}

	// A seat with no worktree has nothing to push.
	other := stopSeat
	other.Polecat = "gone"
	if err := d.overrunPushBranch(other); err != nil {
		t.Errorf("seat with no worktree: %v, want nil", err)
	}
}

// An unforced push origin rejects as non-fast-forward comes back as the typed
// divergence error carrying origin's sha for the branch (read exactly, not by
// prefix), so the stop gives up on it; any other refusal stays a plain error.
func TestOverrunPushBranch_NonFastForwardReportsTheOriginSha(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	worktree := filepath.Join(d.config.TownRoot, "gastown", "polecats", "capable", "gastown")
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	g := &pushStubGit{branch: stopSeat.Branch, sha: "abc123", originRefs: []git.RemoteRef{
		{Hash: "1111111", Name: "refs/heads/" + stopSeat.Branch + "-other"},
		{Hash: "9f8e7d6", Name: "refs/heads/" + stopSeat.Branch},
	}}
	d.openGitFn = func(string) daemonGit { return g }

	for _, stderr := range []string{
		" ! [rejected]  abc123 -> branch (non-fast-forward)",
		" ! [rejected]  abc123 -> branch (fetch first)",
	} {
		g.pushErr = &git.GitError{Command: "push", Stderr: stderr, Err: errors.New("exit status 1")}
		var diverged *overrunPushDivergedError
		if err := d.overrunPushBranch(stopSeat); !errors.As(err, &diverged) || diverged.OriginSHA != "9f8e7d6" {
			t.Errorf("stderr %q: error = %v, want the divergence error at origin sha 9f8e7d6", stderr, err)
		}
	}
	if slices.Contains(g.pushes, "FORCED") {
		t.Errorf("pushes %q include a force push", g.pushes)
	}

	// Origin cannot be asked: still a divergence, sha unknown.
	g.originErr = errors.New("unreachable")
	var diverged *overrunPushDivergedError
	if err := d.overrunPushBranch(stopSeat); !errors.As(err, &diverged) || diverged.OriginSHA != "" ||
		!strings.Contains(pushFailureReason(err), "origin sha unknown") {
		t.Errorf("error = %v, want a divergence error with no sha", err)
	}

	// Any other refusal is retried like before.
	g.pushErr = &git.GitError{Command: "push", Stderr: "remote: permission denied", Err: errors.New("exit status 1")}
	if err := d.overrunPushBranch(stopSeat); err == nil || errors.As(err, &diverged) {
		t.Errorf("error = %v, want a plain push error", err)
	}
}
