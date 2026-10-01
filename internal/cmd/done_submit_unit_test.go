package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/supervisor"
)

// These are the unit tests of gt done's submit path (submitForLanding): its
// decisions against an in-memory repo, beadsfake and a recording gate. They
// start no process and read no package global or environment, so they run
// in parallel. The end-to-end runs against real git and a bd stub are
// TestIntegrationRunDone* in done_submit_integration_test.go.

// fakeDoneRepo is an in-memory worktree with one remote, origin. Commits are
// opaque names; the branch under test is doneTestBranch and the target is
// the base ref "origin/<target>". It models what the submit path relies on:
// ahead/behind counts, a rebase that replays the branch or conflicts, a
// lease-checked push, origin holding (or dropping) what was pushed, and the
// patch-ids the divergence check compares.
type fakeDoneRepo struct {
	mu sync.Mutex

	head string
	// ancestors are the commits reachable from head, head included.
	ancestors map[string]bool
	ahead     int // commits on head not on the target
	behind    int // commits on the target not on head
	conflicts []string
	rebasing  bool
	dirty     *git.UncommittedWorkStatus

	// origin maps a branch to its tip on origin.
	origin map[string]string
	// reachable maps a target branch to the commits origin has on it.
	reachable map[string]map[string]bool
	// pushErrs fail the push attempts in order; nil entries succeed.
	pushErrs []error
	// dropPushed makes origin delete a pushed branch after accepting it, as
	// a post-receive hook could.
	dropPushed bool
	// patchIDs are FirstParentPatchIDs answers keyed by the range's tip.
	patchIDs map[string][]string

	// verifyErr makes the worktree's own origin query fail to run.
	verifyErr error

	pushes   []string // "<branch> <sha> lease=<expected>"
	verified []string // targets VerifyPushedCommitReachableFromPushTarget ran against
}

func newFakeDoneRepo() *fakeDoneRepo {
	return &fakeDoneRepo{
		head:      "feature1",
		ancestors: map[string]bool{"main1": true, "feature1": true},
		ahead:     1,
		origin:    map[string]string{"main": "main1"},
		reachable: map[string]map[string]bool{},
		patchIDs:  map[string][]string{},
	}
}

func (f *fakeDoneRepo) CheckUncommittedWork() (*git.UncommittedWorkStatus, error) {
	if f.dirty != nil {
		return f.dirty, nil
	}
	return &git.UncommittedWorkStatus{}, nil
}

func (f *fakeDoneRepo) CleanBaseRef(remote, defaultBranch, target string) string {
	return remote + "/" + target
}

func (f *fakeDoneRepo) Fetch(string) error { return nil }

func (f *fakeDoneRepo) Rev(ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ref == "HEAD" {
		return f.head, nil
	}
	if tip, ok := f.origin[strings.TrimPrefix(ref, "origin/")]; ok && tip != "" {
		return tip, nil
	}
	return "", fmt.Errorf("unknown revision %s", ref)
}

func (f *fakeDoneRepo) CommitsAhead(base, branch string) (int, error) {
	if base == "HEAD" {
		return f.behind, nil
	}
	return f.ahead, nil
}

func (f *fakeDoneRepo) Rebase(onto string) error {
	if len(f.conflicts) > 0 {
		f.rebasing = true
		return fmt.Errorf("CONFLICT (content): merge conflict in %s", f.conflicts[0])
	}
	f.head += "-rebased"
	f.ancestors = map[string]bool{f.head: true, f.origin["main"]: true}
	f.behind = 0
	return nil
}

func (f *fakeDoneRepo) GetConflictingFiles() ([]string, error) { return f.conflicts, nil }

func (f *fakeDoneRepo) AbortRebase() error {
	f.rebasing = false
	return nil
}

func (f *fakeDoneRepo) BranchPushedToRemote(localBranch, remote string) (bool, int, error) {
	tip := f.origin[localBranch]
	return tip != "" && f.ancestors[tip], 0, nil
}

func (f *fakeDoneRepo) ForkBackedRemote(string) bool { return false }

func (f *fakeDoneRepo) VerifyPushedCommitReachableFromPushTarget(remote, branch, commit string) error {
	f.verified = append(f.verified, branch)
	if f.reachable[branch][commit] {
		return nil
	}
	return fmt.Errorf("commit %s is not reachable from %s/%s", commit, remote, branch)
}

func (f *fakeDoneRepo) VerifyPushedCommit(remote, branch, commit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.verifyErr != nil {
		return f.verifyErr
	}
	if got := f.origin[branch]; got != commit {
		return fmt.Errorf("%s/%s is %q, not %s", remote, branch, got, commit)
	}
	return nil
}

func (f *fakeDoneRepo) PushRemoteBranchTip(remote, branch string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.origin[branch], nil
}

func (f *fakeDoneRepo) IsAncestor(ancestor, descendant string) (bool, error) {
	return f.ancestors[ancestor], nil
}

func (f *fakeDoneRepo) MergeBase(a, b string) (string, error) { return "main1", nil }

func (f *fakeDoneRepo) PatchID(base, head string) (string, error) {
	return strings.Join(f.patchIDs[head], "+"), nil
}

func (f *fakeDoneRepo) FirstParentPatchIDs(base, head string) ([]git.PatchIDCommit, error) {
	var out []git.PatchIDCommit
	for _, id := range f.patchIDs[head] {
		out = append(out, git.PatchIDCommit{PatchID: id, Commit: head})
	}
	return out, nil
}

// PushForceWithLease pushes HEAD to the branch named by refspec's
// destination, refusing when origin's tip is not the lease.
func (f *fakeDoneRepo) PushForceWithLease(remote, refspec, branchRef, expectedSHA string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, dst, _ := strings.Cut(refspec, ":")
	branch := strings.TrimPrefix(dst, "refs/heads/")
	f.pushes = append(f.pushes, fmt.Sprintf("%s %s lease=%s", branch, f.head, expectedSHA))
	if f.origin[branch] != expectedSHA {
		return fmt.Errorf("! [rejected] %s (stale info)", branch)
	}
	if len(f.pushErrs) > 0 {
		err := f.pushErrs[0]
		f.pushErrs = f.pushErrs[1:]
		if err != nil {
			return err
		}
	}
	if !f.dropPushed {
		f.origin[branch] = f.head
	}
	return nil
}

var _ doneRepo = (*fakeDoneRepo)(nil)

// doneTestBranch is the polecat branch the submit tests push.
const doneTestBranch = "feature/routed-submit"

// fakeGate records the heads it gated and answers with result.
type fakeGate struct {
	mu     sync.Mutex
	repo   *fakeDoneRepo
	result land.GateResult
	heads  []string
}

func (g *fakeGate) Run(_ context.Context, _ string) land.GateResult {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.heads = append(g.heads, g.repo.head)
	return g.result
}

// failingBD fails the writes its fields name and passes the rest to the
// wrapped fake.
type failingBD struct {
	beads.Client
	updateErr error
	closeErr  error
	closes    int
}

func (b *failingBD) Update(id string, opts beads.UpdateOptions) error {
	if b.updateErr != nil {
		return b.updateErr
	}
	return b.Client.Update(id, opts)
}

func (b *failingBD) ForceCloseWithReason(reason string, ids ...string) error {
	b.closes++
	if b.closeErr != nil {
		return b.closeErr
	}
	return b.Client.ForceCloseWithReason(reason, ids...)
}

// submitHarness is one polecat's gt done submit in a temp town: the repo,
// the source bead's database, the gate and what the branch stages saw.
type submitHarness struct {
	r        *doneRun
	repo     *fakeDoneRepo
	bd       *beadsfake.Fake
	client   beads.Client
	gate     *fakeGate
	sleeps   []time.Duration
	stages   []string
	townRoot string
}

func newSubmitHarness(t *testing.T) *submitHarness {
	t.Helper()
	h := &submitHarness{
		repo:     newFakeDoneRepo(),
		bd:       beadsfake.New(beadsfake.WithPrefix("bd")),
		townRoot: t.TempDir(),
	}
	h.bd.Seed(beads.Issue{ID: "bd-source", Title: "the work", Type: "task", Status: string(beads.StatusHooked)})
	h.client = h.bd
	h.gate = &fakeGate{repo: h.repo, result: land.GateResult{Passed: true, Steps: []land.StepResult{{Name: "test"}}}}
	h.r = &doneRun{
		cwd:           t.TempDir(),
		townRoot:      h.townRoot,
		rigName:       "gastown",
		polecatName:   "refuge",
		sender:        "gastown/polecats/refuge",
		branch:        doneTestBranch,
		issueID:       "bd-source",
		defaultBranch: "main",
		cleanupStatus: "unpushed",
		opts:          doneOptions{polecatEnv: true},
	}
	h.r.deps = doneSubmitDeps{
		repo: h.repo,
		source: func(issueID string) (*beads.Issue, beads.Client, error) {
			issue, err := h.client.Show(issueID)
			return issue, h.client, err
		},
		localGate:      func(string, string, string) (land.Gate, error) { return h.gate, nil },
		checkBranch:    func(string, doneSubmission) error { h.stages = append(h.stages, "check@"+h.repo.head); return nil },
		rewriteBranch:  func(string, doneSubmission) error { h.stages = append(h.stages, "rewrite@"+h.repo.head); return nil },
		pushSubmodules: func(string) { h.stages = append(h.stages, "submodules") },
		sleep:          func(d time.Duration) { h.sleeps = append(h.sleeps, d) },
	}
	return h
}

func (h *submitHarness) submit() error { return submitForLanding(h.r) }

func (h *submitHarness) source(t *testing.T) *beads.Issue {
	t.Helper()
	issue, err := h.bd.Show("bd-source")
	if err != nil {
		t.Fatalf("show bd-source: %v", err)
	}
	return issue
}

func (h *submitHarness) intent(t *testing.T) intent.Record {
	t.Helper()
	rec, err := intent.Read(h.townRoot, intent.Seat{Rig: "gastown", Role: "polecat", Name: "refuge"})
	if err != nil {
		t.Fatalf("reading the intent record: %v", err)
	}
	return rec
}

func isDoneExit(err error, code int) bool {
	var coded *ExitCodeError
	return errors.As(err, &coded) && coded.Code == code
}

func wantDoneExit(t *testing.T, err error, code int, text string) {
	t.Helper()
	if !isDoneExit(err, code) {
		t.Fatalf("submit error = %T %v, want exit %d", err, err, code)
	}
	if !strings.Contains(err.Error(), text) {
		t.Errorf("error %q lacks %q", err, text)
	}
}

// TestSubmitRebasesGatesPushesAndMarksReady is the whole author side: the
// branch behind its target is rebased, checked and rewritten, the rebased
// head gated, exactly that head pushed, and the work bead marked ready to
// land with the intent record saying submitted. Nothing is closed.
func TestSubmitRebasesGatesPushesAndMarksReady(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.repo.behind = 1
	if err := h.submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	head := "feature1-rebased"
	if want := []string{"check@" + head, "rewrite@" + head, "submodules"}; strings.Join(h.stages, ",") != strings.Join(want, ",") {
		t.Errorf("stages = %v, want %v", h.stages, want)
	}
	if len(h.gate.heads) != 1 || h.gate.heads[0] != head {
		t.Errorf("gate ran on %v, want exactly %s", h.gate.heads, head)
	}
	if got := h.repo.origin[doneTestBranch]; got != head {
		t.Errorf("origin/%s = %q, want the gated head %s", doneTestBranch, got, head)
	}
	if h.repo.origin["main"] != "main1" {
		t.Errorf("origin/main moved to %s", h.repo.origin["main"])
	}
	issue := h.source(t)
	if !beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Errorf("labels %v lack %s", issue.Labels, land.LabelReadyToLand)
	}
	w, ok := land.ParseReadyNote(issue.Notes)
	if !ok || w.Head != head || w.Branch != doneTestBranch || w.Target != "main" || w.Worker != "refuge" {
		t.Errorf("READY TO LAND note = %+v (parsed %v) from %q", w, ok, issue.Notes)
	}
	if issue.Status == string(beads.StatusClosed) {
		t.Error("gt done closed the source bead; only the landing worker may")
	}
	if rec := h.intent(t); !rec.Submitted() || rec.WorkBead != "bd-source" {
		t.Errorf("intent record = %+v, want submitted for bd-source", rec)
	}
	if h.r.cleanupStatus != cleanupStatusAfterSuccessfulPush("unpushed") {
		t.Errorf("cleanup status = %q after a verified push", h.r.cleanupStatus)
	}
}

// TestSubmitLeavesWorkItsConsumersReadAsSubmitted feeds what a submit wrote
// to the readers that decide whether a sessionless polecat with hooked work
// is dead: the polecat states, the inventory and the supervisor's restart.
func TestSubmitLeavesWorkItsConsumersReadAsSubmitted(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	if err := h.submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	issue := h.source(t)
	if !polecat.IsSubmittedWork(issue) {
		t.Errorf("polecat.IsSubmittedWork(%+v) = false; the polecat would read as stalled", issue)
	}
	if ev := assessPolecatAssignedIssueWork(issue); !ev.BlocksCleanup || !ev.Submitted {
		t.Errorf("inventory does not hold the seat for submitted work: %+v", ev)
	}
	restarted := false
	s := supervisor.New(supervisor.Options{
		TownRoot: h.townRoot,
		Restart:  func(supervisor.Seat) error { restarted = true; return nil },
		Logf:     t.Logf,
	})
	err := s.Restart(supervisor.SeatFor("gastown", "polecat", "refuge"), "dead agent", "witness")
	if !errors.Is(err, supervisor.ErrSubmitted) || !strings.Contains(err.Error(), "bd-source") {
		t.Errorf("Restart after submit = %v, want ErrSubmitted naming bd-source", err)
	}
	if restarted {
		t.Error("a session was raised on submitted work")
	}
}

// TestSubmitRecordsTheActualWorkerNotTheBranchName: a rework reuses another
// polecat's branch; the READY TO LAND note names whoever submits (gt-fl0n).
func TestSubmitRecordsTheActualWorkerNotTheBranchName(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.r.branch = "polecat/malachite/bd-source+mudreworkab"
	if err := h.submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	w, _ := land.ParseReadyNote(h.source(t).Notes)
	if w.Worker != "refuge" {
		t.Errorf("Worker = %q, want the submitter refuge", w.Worker)
	}
}

// TestSubmitReplacesAnOlderBranchTipUnderLease: an earlier attempt pushed
// the branch, which was rebased since; the new tip replaces it under a lease
// on the tip origin had.
func TestSubmitReplacesAnOlderBranchTipUnderLease(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.repo.origin[doneTestBranch] = "feature1"
	h.repo.behind = 1
	// The rebase rewrote feature1; both ranges carry the same change.
	h.repo.patchIDs["feature1"] = []string{"p1"}
	h.repo.patchIDs["feature1-rebased"] = []string{"p1"}
	if err := h.submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := h.repo.origin[doneTestBranch]; got != "feature1-rebased" {
		t.Errorf("origin branch = %q, want the rebased tip", got)
	}
	if len(h.repo.pushes) != 1 || !strings.HasSuffix(h.repo.pushes[0], "lease=feature1") {
		t.Errorf("pushes = %v, want one push leased on feature1", h.repo.pushes)
	}
}

// TestSubmitRefusesToPushOverSomeoneElsesWork: origin's branch holds a
// commit this worktree lacks. The lease would allow replacing it, so the
// change-sets are compared and real divergence refused: exit 10, origin
// untouched (gt-bf5x, gt-i0z3).
func TestSubmitRefusesToPushOverSomeoneElsesWork(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.repo.origin[doneTestBranch] = "theirs"
	h.repo.patchIDs["theirs"] = []string{"p1", "their-rework"}
	h.repo.patchIDs["feature1"] = []string{"p1", "mine"}
	err := h.submit()
	wantDoneExit(t, err, doneExitPushFailed, "real divergence")
	if got := h.repo.origin[doneTestBranch]; got != "theirs" {
		t.Errorf("origin branch = %q, want their commit kept", got)
	}
	if beads.HasLabel(h.source(t), land.LabelReadyToLand) {
		t.Error("an unpushed branch was marked ready to land")
	}
}

// TestSubmitRedGateExits15: a red local gate stops before the push: exit
// 15 with the failure tail, nothing on origin, no ready mark, no intent.
func TestSubmitRedGateExits15(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.gate.result = land.GateResult{Steps: []land.StepResult{{Name: "test", ExitCode: 2, Tail: "FAIL\tpkg/x\n"}}}
	err := h.submit()
	wantDoneExit(t, err, doneExitGateFailed, "FAIL\tpkg/x")
	if len(h.repo.pushes) != 0 {
		t.Errorf("a red gate pushed: %v", h.repo.pushes)
	}
	if beads.HasLabel(h.source(t), land.LabelReadyToLand) {
		t.Error("a red gate marked the bead ready to land")
	}
}

// TestSubmitGateThatCouldNotRunExits16: a gate that could not run says
// nothing about the code, so it has its own exit and pushes nothing.
func TestSubmitGateThatCouldNotRunExits16(t *testing.T) {
	t.Parallel()
	for name, broken := range map[string]func(h *submitHarness){
		"gate errored": func(h *submitHarness) { h.gate.result = land.GateResult{Err: errors.New("sh: not found")} },
		"no gate": func(h *submitHarness) {
			h.r.deps.localGate = func(string, string, string) (land.Gate, error) { return nil, errors.New("no gate config") }
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newSubmitHarness(t)
			broken(h)
			err := h.submit()
			wantDoneExit(t, err, doneExitGateUnavailable, "not a verdict on your change")
			if len(h.repo.pushes) != 0 {
				t.Errorf("pushed without a gate verdict: %v", h.repo.pushes)
			}
		})
	}
}

// TestSubmitRebaseConflictExits14: the target changed the same lines: exit
// 14 naming the file, and the rebase is aborted, not left half done.
func TestSubmitRebaseConflictExits14(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.repo.behind = 1
	h.repo.conflicts = []string{"file.txt"}
	err := h.submit()
	wantDoneExit(t, err, doneExitRebaseConflict, "file.txt")
	if h.repo.rebasing {
		t.Error("worktree left mid-rebase")
	}
	if len(h.stages) != 0 || len(h.gate.heads) != 0 {
		t.Errorf("a conflicted rebase went on to %v / gate %v", h.stages, h.gate.heads)
	}
}

// TestSubmitBranchCheckRefusalStopsBeforeTheGate: a refused branch (revert
// of merged work, throwaway file, unchanged rework) neither rewrites nor
// gates nor pushes.
func TestSubmitBranchCheckRefusalStopsBeforeTheGate(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	refusal := errors.New("branch reverts merged work")
	h.r.deps.checkBranch = func(string, doneSubmission) error { return refusal }
	if err := h.submit(); !errors.Is(err, refusal) {
		t.Fatalf("submit = %v, want the check's refusal", err)
	}
	if len(h.stages) != 0 || len(h.gate.heads) != 0 || len(h.repo.pushes) != 0 {
		t.Errorf("after a refusal: stages %v, gate %v, pushes %v", h.stages, h.gate.heads, h.repo.pushes)
	}
}

// TestSubmitReadyRecordFailedExits12: the branch is on origin but bd could
// not mark the work bead: exit 12, and the intent record is not submitted.
func TestSubmitReadyRecordFailedExits12(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.client = &failingBD{Client: h.bd, updateErr: errors.New("database not found: gastown")}
	err := h.submit()
	wantDoneExit(t, err, doneExitReadyFailed, "ready to land")
	if h.repo.origin[doneTestBranch] != "feature1" {
		t.Errorf("origin branch = %q, want the pushed head", h.repo.origin[doneTestBranch])
	}
	if rec := h.intent(t); rec.Submitted() {
		t.Errorf("an unmarked bead was recorded as submitted: %+v", rec)
	}
}

// TestSubmitPushFailedWhenOriginRejects: origin refuses every attempt, so
// the work is only local: exit 10 after one retry, and the bead is noted.
func TestSubmitPushFailedWhenOriginRejects(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.repo.pushErrs = []error{errors.New("rejected by hook"), errors.New("rejected by hook")}
	err := h.submit()
	wantDoneExit(t, err, doneExitPushFailed, doneTestBranch)
	if len(h.repo.pushes) != 2 || len(h.sleeps) != 1 {
		t.Errorf("pushes %v sleeps %v, want one push and one retry", h.repo.pushes, h.sleeps)
	}
	if issue := h.source(t); issue.Status != "in_progress" || beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Errorf("bead after an unlanded push: status %s labels %v", issue.Status, issue.Labels)
	}
}

// TestSubmitPushUnverifiedWhenOriginDropsTheBranch: every push command
// succeeds but origin never holds the commit afterwards: exit 11.
func TestSubmitPushUnverifiedWhenOriginDropsTheBranch(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.repo.dropPushed = true
	err := h.submit()
	wantDoneExit(t, err, doneExitPushUnverified, doneTestBranch)
}

// TestSubmitClassifiesOnTheLastPushAttempt: the first push fails and the
// retry succeeds, yet origin does not hold the commit. The push did not
// fail; origin is unverified: exit 11, not 10.
func TestSubmitClassifiesOnTheLastPushAttempt(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.repo.pushErrs = []error{errors.New("transient rejection")}
	h.repo.dropPushed = true
	err := h.submit()
	wantDoneExit(t, err, doneExitPushUnverified, doneTestBranch)
}

// TestSubmitRecoversAPushThatErroredButLanded: the first push reports an
// error while origin took the commit; the retry proves it without failing.
func TestSubmitRecoversAPushThatErroredButLanded(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.repo.pushErrs = []error{nil}
	first := true
	inner := h.repo
	h.r.deps.repo = &erroringFirstPush{fakeDoneRepo: inner, first: &first}
	if err := h.submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !beads.HasLabel(h.source(t), land.LabelReadyToLand) {
		t.Error("a recovered push was not marked ready")
	}
}

// erroringFirstPush lands the first push on origin but reports it failed.
type erroringFirstPush struct {
	*fakeDoneRepo
	first *bool
}

func (e *erroringFirstPush) PushForceWithLease(remote, refspec, branchRef, expected string) error {
	err := e.fakeDoneRepo.PushForceWithLease(remote, refspec, branchRef, expected)
	if *e.first && err == nil {
		*e.first = false
		return errors.New("timed out after the remote took the objects")
	}
	return err
}

// TestSubmitNoCodeChecksTheResolvedTarget: a branch with nothing ahead of a
// non-default target is verified on THAT target and closed with it
// recorded; checking the rig default would refuse it or record the wrong
// landing branch.
func TestSubmitNoCodeChecksTheResolvedTarget(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.r.opts.target = "release"
	h.repo.ahead = 0
	h.repo.origin[doneTestBranch] = "feature1"
	h.repo.origin["release"] = "feature1"
	h.repo.reachable["release"] = map[string]bool{"feature1": true}
	if err := h.submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if strings.Join(h.repo.verified, ",") != "release" {
		t.Errorf("verified against %v, want release", h.repo.verified)
	}
	issue := h.source(t)
	for _, want := range []string{"target_branch: release", "commit_sha: feature1"} {
		if !strings.Contains(issue.CloseReason, want) {
			t.Errorf("close reason %q lacks %q", issue.CloseReason, want)
		}
	}
	if len(h.repo.pushes) != 0 || len(h.gate.heads) != 0 {
		t.Errorf("no-code work pushed %v or gated %v", h.repo.pushes, h.gate.heads)
	}
}

// TestSubmitNoCodeCloseFailedExits13: bd cannot close the no-code bead:
// three attempts with backoff, then exit 13.
func TestSubmitNoCodeCloseFailedExits13(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.repo.ahead = 0
	h.repo.origin[doneTestBranch] = "feature1"
	h.repo.reachable["main"] = map[string]bool{"feature1": true}
	failing := &failingBD{Client: h.bd, closeErr: errors.New("database not found: gastown")}
	h.client = failing
	err := h.submit()
	wantDoneExit(t, err, doneExitCloseFailed, "could not close issue bd-source")
	if failing.closes != 3 || len(h.sleeps) != 2 {
		t.Errorf("closes %d sleeps %v, want 3 attempts with 2 waits", failing.closes, h.sleeps)
	}
}

// TestSubmitNoCommitsRefusesAnUnpushedPolecatBranch: a polecat with nothing
// ahead of the target and nothing pushed brought no work; it is refused
// without a hint that would let it bypass the check (gastown#1484).
func TestSubmitNoCommitsRefusesAnUnpushedPolecatBranch(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.repo.ahead = 0
	err := h.submit()
	if err == nil || !strings.Contains(err.Error(), "no commits on branch ahead of origin/main") {
		t.Fatalf("submit = %v, want the no-commits refusal", err)
	}
	if strings.Contains(err.Error(), "cleanup-status") {
		t.Errorf("refusal %q names the bypass", err)
	}
}

// TestSubmitRefusesUncommittedWork: changes outside the runtime dirs would
// be lost by a submit, so it refuses before touching origin.
func TestSubmitRefusesUncommittedWork(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.repo.dirty = &git.UncommittedWorkStatus{HasUncommittedChanges: true, ModifiedFiles: []string{"main.go"}}
	err := h.submit()
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes would be lost") {
		t.Fatalf("submit = %v, want the uncommitted-work refusal", err)
	}
	if len(h.repo.pushes) != 0 {
		t.Errorf("pushed with uncommitted work: %v", h.repo.pushes)
	}
}

// TestDoneLandingFlagsAreGone: gt done has no landing modes and no polecat
// gate bypass (ADR 0004). --pre-verified exists for crew only and the
// polecat path refuses it (runDone).
func TestDoneLandingFlagsAreGone(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"skip-tests", "skip-verify", "merge", "resume", "priority"} {
		if doneCmd.Flags().Lookup(name) != nil {
			t.Errorf("gt done still has --%s", name)
		}
	}
}

// TestSubmitDropsAnOverseerReview: an overseer review covers one head, and a
// submission brings a new one, so gt done removes the label (gt-g8t3m).
func TestSubmitDropsAnOverseerReview(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.bd.Seed(beads.Issue{ID: "bd-source", Title: "the work", Type: "task", Status: string(beads.StatusHooked),
		Labels: []string{land.LabelOverseerReviewed}, Notes: land.OverseerReviewedMarker + " oldhead"})
	if err := h.submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if issue := h.source(t); beads.HasLabel(issue, land.LabelOverseerReviewed) {
		t.Errorf("labels %v still carry %s after a new submission", issue.Labels, land.LabelOverseerReviewed)
	}
}
