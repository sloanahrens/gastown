package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

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
	want = []string{"recheck gastown/capable", "push gastown/capable", "hand-back gastown/capable", "reset-agent-bead gastown/capable", "clear-pause gastown/capable"}
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
	want := []string{"recheck gastown/capable", "hand-back gastown/capable", "reset-agent-bead gastown/capable", "clear-pause gastown/capable"}
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

// pushStubGit stands in for the seat's worktree git: it knows one local branch
// and records the pushes.
type pushStubGit struct {
	daemonGit
	branch, sha string
	pushErr     error
	pushes      []string
}

func (g *pushStubGit) CurrentBranch() (string, error) { return g.branch, nil }

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
