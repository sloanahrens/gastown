package land

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// fakePulls records the Forgejo calls the land PR makes and answers them from
// canned state, so no test needs a server.
type fakePulls struct {
	open        []forgejo.PullRequest
	statuses    []forgejo.StatusRequest
	created     []forgejo.CreatePullRequestOption
	merges      []forgejo.MergePullRequestOption
	deleted     []string
	mergePR     int64
	combined    *forgejo.CombinedStatus
	statusErr   error
	combinedErr error
	listErr     error
	createErr   error
	mergeErr    error
	deleteErr   error
	// onCall, when set, sees every call's name and context, so a test can pin
	// the deadline each call runs under.
	onCall func(name string, ctx context.Context)
	// mergeFn, when set, answers MergePull in place of mergeErr.
	mergeFn func(ctx context.Context, index int64, opt forgejo.MergePullRequestOption) error
}

func (f *fakePulls) called(name string, ctx context.Context) {
	if f.onCall != nil {
		f.onCall(name, ctx)
	}
}

func (f *fakePulls) PostStatus(ctx context.Context, _, _, _ string, req forgejo.StatusRequest) (*forgejo.CommitStatus, error) {
	f.called("PostStatus", ctx)
	f.statuses = append(f.statuses, req)
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return &forgejo.CommitStatus{Context: req.Context, Status: req.State}, nil
}

// CombinedStatus answers the creator check's read. The canned statuses stand
// for the commit's state after the verdict was posted.
func (f *fakePulls) CombinedStatus(ctx context.Context, _, _, _ string) (*forgejo.CombinedStatus, error) {
	f.called("CombinedStatus", ctx)
	if f.combinedErr != nil {
		return nil, f.combinedErr
	}
	if f.combined == nil {
		return &forgejo.CombinedStatus{}, nil
	}
	return f.combined, nil
}

func (f *fakePulls) OpenPulls(ctx context.Context, _, _ string) ([]forgejo.PullRequest, error) {
	f.called("OpenPulls", ctx)
	return f.open, f.listErr
}

func (f *fakePulls) CreatePull(ctx context.Context, _, _ string, opt forgejo.CreatePullRequestOption) (*forgejo.PullRequest, error) {
	f.called("CreatePull", ctx)
	f.created = append(f.created, opt)
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &forgejo.PullRequest{Number: 7, Head: forgejo.PRBranchInfo{Ref: opt.Head}, Base: forgejo.PRBranchInfo{Ref: opt.Base}}, nil
}

func (f *fakePulls) MergePull(ctx context.Context, _, _ string, index int64, opt forgejo.MergePullRequestOption) error {
	f.called("MergePull", ctx)
	f.merges = append(f.merges, opt)
	f.mergePR = index
	if f.mergeFn != nil {
		return f.mergeFn(ctx, index, opt)
	}
	return f.mergeErr
}

func (f *fakePulls) DeleteBranch(ctx context.Context, _, _, branch string) error {
	f.called("DeleteBranch", ctx)
	f.deleted = append(f.deleted, branch)
	return f.deleteErr
}

const (
	mergeHead = "0123456789abcdef0123456789abcdef01234567"
	// testBotLogin and testGateContext are the two facts the creator check
	// needs: the landing bot the rig configures and the context CI reports.
	testBotLogin    = "gt-landing"
	testGateContext = "ci / gate (push)"
)

func mergeWorkForTest() Work {
	return Work{BeadID: "gt-abc", Rig: "gastown", Branch: fixtureBranch, Head: mergeHead, Target: "main", Worker: "opal"}
}

func mergeRequestForTest(verdict Verdict) MergeRequest {
	return MergeRequest{Work: mergeWorkForTest(), Head: mergeHead, Verdict: verdict, GateContext: testGateContext}
}

// gateStatus is a status on the gate context: creator nil is how a workflow
// run's status arrives.
func gateStatus(creator *forgejo.User) forgejo.CommitStatus {
	return forgejo.CommitStatus{Context: testGateContext, Status: forgejo.StateSuccess, Creator: creator}
}

// reviewStatus is the om / review status the given account posted.
func reviewStatus(login string) forgejo.CommitStatus {
	st := forgejo.CommitStatus{Context: OMStatusContext, Status: forgejo.StateSuccess}
	if login != "" {
		st.Creator = &forgejo.User{Login: login}
	}
	return st
}

// cleanCandidate is the statuses a candidate that passes the creator check
// carries: the gate posted by CI, the review posted by the landing bot.
func cleanCandidate() *forgejo.CombinedStatus {
	return &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{gateStatus(nil), reviewStatus(testBotLogin)}}
}

// testMerger builds the production merger over a fake client carrying the
// statuses a clean candidate has, so a test about anything else does not have
// to restate them.
func testMerger(client *fakePulls) *ForgejoMerger {
	if client.combined == nil {
		client.combined = cleanCandidate()
	}
	return &ForgejoMerger{Client: client, Owner: "gastown", RepoName: "gastown", BotLogin: testBotLogin}
}

// TestForgejoMergerPostsTheVerdictAndFastForwards: the merge path posts om's
// verdict as the required om / review status, opens land/<bead> -> the target,
// and merges the PR pinned to the candidate commit.
func TestForgejoMergerPostsTheVerdictAndFastForwards(t *testing.T) {
	t.Parallel()
	client := &fakePulls{}
	err := testMerger(client).Merge(context.Background(), mergeRequestForTest(Verdict{Verdict: VerdictApprove, Score: 0.9}))
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(client.statuses) != 1 {
		t.Fatalf("posted %d statuses, want one", len(client.statuses))
	}
	got := client.statuses[0]
	if got.Context != OMStatusContext || got.State != forgejo.StateSuccess {
		t.Fatalf("status = %+v; want a successful %s", got, OMStatusContext)
	}
	if len(client.created) != 1 {
		t.Fatalf("opened %d pull requests, want one", len(client.created))
	}
	if c := client.created[0]; c.Head != "land/gt-abc" || c.Base != "main" {
		t.Fatalf("pull request = %+v; want land/gt-abc -> main", c)
	}
	if len(client.merges) != 1 {
		t.Fatalf("merged %d times, want once", len(client.merges))
	}
	if m := client.merges[0]; m.HeadCommitID != mergeHead {
		t.Fatalf("merge head_commit_id = %q, want the candidate %s", m.HeadCommitID, mergeHead)
	} else if m.Style != forgejo.MergeStyleFastForward {
		t.Fatalf("merge style = %q, want fast-forward-only so the target tip is the candidate", m.Style)
	}
	if client.mergePR != 7 {
		t.Fatalf("merged pull request #%d, want the created #7", client.mergePR)
	}
	// The API merge does not apply Forgejo's delete-branch-after-merge setting,
	// so the worker deletes the candidate itself; it is what the branch is
	// named after the merge.
	if len(client.deleted) != 1 || client.deleted[0] != "land/gt-abc" {
		t.Fatalf("deleted %v after the merge, want just land/gt-abc", client.deleted)
	}
}

// TestForgejoMergerDeleteFailureDoesNotFailTheMerge: the landing already
// succeeded when the candidate is deleted, so a refused delete is logged and
// swallowed — a later landing for the same bead force-updates the branch.
func TestForgejoMergerDeleteFailureDoesNotFailTheMerge(t *testing.T) {
	t.Parallel()
	client := &fakePulls{deleteErr: &forgejo.APIError{Method: "DELETE", Path: "/x", StatusCode: 403}}
	var out strings.Builder
	m := testMerger(client)
	m.Out = &out
	if err := m.Merge(context.Background(), mergeRequestForTest(Verdict{Verdict: VerdictApprove})); err != nil {
		t.Fatalf("Merge: %v; a failed candidate delete must not fail a landed merge", err)
	}
	if len(client.deleted) != 1 {
		t.Fatalf("deleted %v, want the attempt recorded", client.deleted)
	}
	if !strings.Contains(out.String(), "could not delete land/gt-abc") {
		t.Fatalf("log = %q; want the failed delete reported", out.String())
	}
}

// TestForgejoMergerLeavesTheCandidateWhenTheMergeDidNotHappen: a 409 rebuild, a
// 405 refusal and an infra error all leave land/<bead> in place, because the
// next attempt rebuilds and re-pushes it.
func TestForgejoMergerLeavesTheCandidateWhenTheMergeDidNotHappen(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		mergeEr error
	}{
		{"409 rebuild", &forgejo.APIError{Method: "POST", Path: "/x", StatusCode: 409}},
		{"405 refusal", &forgejo.APIError{Method: "POST", Path: "/x", StatusCode: 405}},
		{"infra error", &forgejo.APIError{Method: "POST", Path: "/x", StatusCode: 500}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &fakePulls{mergeErr: tc.mergeEr}
			if err := testMerger(client).Merge(context.Background(), mergeRequestForTest(Verdict{Verdict: VerdictApprove})); err == nil {
				t.Fatal("Merge succeeded; want the merge error to reach the caller")
			}
			if len(client.deleted) != 0 {
				t.Fatalf("deleted %v; a merge that did not happen must leave the branch for the retry", client.deleted)
			}
		})
	}
}

// TestForgejoMergerReusesAnOpenPullRequest: a rebuild after the outdated-branch
// guard finds the PR the first attempt opened and merges that, rather than
// failing to open a second one for the same branches.
func TestForgejoMergerReusesAnOpenPullRequest(t *testing.T) {
	t.Parallel()
	client := &fakePulls{open: []forgejo.PullRequest{
		{Number: 4, Head: forgejo.PRBranchInfo{Ref: "land/other"}},
		{Number: 3, Head: forgejo.PRBranchInfo{Ref: "land/gt-abc"}},
	}}
	err := testMerger(client).Merge(context.Background(), mergeRequestForTest(Verdict{Verdict: VerdictApprove}))
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(client.created) != 0 {
		t.Fatalf("opened %d pull requests; want the open one reused", len(client.created))
	}
	if client.mergePR != 3 {
		t.Fatalf("merged pull request #%d, want the existing #3", client.mergePR)
	}
}

// TestForgejoMergerConflictIsARebuild: the outdated-branch guard's 409 is the
// worker's cue to rebuild the candidate, not a rejection or a lost race.
func TestForgejoMergerConflictIsARebuild(t *testing.T) {
	t.Parallel()
	client := &fakePulls{mergeErr: &forgejo.APIError{Method: "POST", Path: "/x", StatusCode: 409}}
	err := testMerger(client).Merge(context.Background(), mergeRequestForTest(Verdict{Verdict: VerdictApprove}))
	var race *RaceError
	if !errors.As(err, &race) {
		t.Fatalf("Merge error = %T %v, want a *RaceError", err, err)
	}
	if !race.Rebuild || race.Target != "main" {
		t.Fatalf("race = %+v; want a rebuild on main", race)
	}
}

// TestForgejoMergerNotReadyToMergeIsRefused: a 405 with the base unmoved is a
// required status missing or red, not the race, so it is its own error for a
// human rather than a rebuild.
func TestForgejoMergerNotReadyToMergeIsRefused(t *testing.T) {
	t.Parallel()
	client := &fakePulls{mergeErr: &forgejo.APIError{Method: "POST", Path: "/x", StatusCode: 405, Body: `{"message":"Not ready to be merged"}`}}
	err := testMerger(client).Merge(context.Background(), mergeRequestForTest(Verdict{Verdict: VerdictApprove}))
	var refused *MergeRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Merge error = %T %v, want a *MergeRefusedError (a 405 is not the race)", err, err)
	}
	var race *RaceError
	if errors.As(err, &race) {
		t.Fatal("the 405 was misread as the outdated-branch race")
	}
}

// TestForgejoMergerStatusFailureIsInfrastructure: a review status that cannot
// be posted says nothing about the work, so the landing is retried, not
// rejected.
func TestForgejoMergerStatusFailureIsInfrastructure(t *testing.T) {
	t.Parallel()
	client := &fakePulls{statusErr: errors.New("connection refused")}
	err := testMerger(client).Merge(context.Background(), mergeRequestForTest(Verdict{Verdict: VerdictApprove}))
	var infra *InfraError
	if !errors.As(err, &infra) || infra.Stage != "post om review status" {
		t.Fatalf("Merge error = %T %v, want an *InfraError at the status post", err, err)
	}
	if len(client.created) != 0 {
		t.Fatal("a PR was opened after the review status failed to post")
	}
}

// TestOMVerdictStatus pins the mapping from a verdict to the required status:
// every verdict a merge carries is a success, so protection is satisfied, and
// the description keeps the audit trail.
func TestOMVerdictStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		verdict Verdict
		state   forgejo.CommitState
		desc    string
	}{
		{"approve", Verdict{Verdict: VerdictApprove, Score: 0.9}, forgejo.StateSuccess, "approved"},
		{"request changes", Verdict{Verdict: VerdictRequestChanges, Score: 0.2}, forgejo.StateFailure, "request_changes"},
		{"skipped", Verdict{Verdict: VerdictSkipped}, forgejo.StateSuccess, "disabled"},
		{"overseer waiver", Verdict{Verdict: VerdictOverseerPrefix + "abc1234"}, forgejo.StateSuccess, "overseer-reviewed"},
		{"om did not run", Verdict{Verdict: VerdictErrorPrefix + "exit 2"}, forgejo.StateSuccess, "did not run"},
	} {
		got := OMVerdictStatus(tc.verdict)
		if got.Context != OMStatusContext {
			t.Errorf("%s: context = %q, want %q", tc.name, got.Context, OMStatusContext)
		}
		if got.State != tc.state {
			t.Errorf("%s: state = %q, want %q", tc.name, got.State, tc.state)
		}
		if !strings.Contains(got.Description, tc.desc) {
			t.Errorf("%s: description %q does not mention %q", tc.name, got.Description, tc.desc)
		}
	}
}

// fakeMerger stands in for the Forgejo PR merge inside Land.
type fakeMerger struct {
	calls []MergeRequest
	fn    func(req MergeRequest) error
}

func (m *fakeMerger) Merge(_ context.Context, req MergeRequest) error {
	m.calls = append(m.calls, req)
	if m.fn != nil {
		return m.fn(req)
	}
	return nil
}

// TestLandMergesThroughTheForgejoPR: a cut-over rig lands through the pull
// request — om's status posted, the PR merged — instead of force-pushing the
// target.
func TestLandMergesThroughTheForgejoPR(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	cand := &fakeCandidate{res: CandidateResult{State: CandidatePassed, Context: "ci / gate (push)", Pushed: true}}
	l.Candidate = cand
	merger := &fakeMerger{fn: func(req MergeRequest) error {
		// Stand in for the server-side fast-forward: the target's tip becomes
		// the candidate the worker read back.
		f.git.SetRef(t, f.origin, "refs/heads/main", req.Head)
		return nil
	}}
	l.Merger = merger

	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if len(f.gate.dirs) != 0 {
		t.Fatalf("the local gate ran %d time(s) on a rig landing through CI", len(f.gate.dirs))
	}
	if len(merger.calls) != 1 {
		t.Fatalf("the merger ran %d time(s), want once", len(merger.calls))
	}
	call := merger.calls[0]
	if call.Head != res.LandedCommit {
		t.Fatalf("merge head = %s, want the landed %s", call.Head, res.LandedCommit)
	}
	if call.Verdict.Verdict != VerdictApprove {
		t.Fatalf("verdict handed to the merger = %q, want approve", call.Verdict.Verdict)
	}
	// The merger's creator check reads the status CI posted, so the context the
	// candidate gate polled has to reach it.
	if call.GateContext != "ci / gate (push)" {
		t.Fatalf("gate context handed to the merger = %q, want the one the candidate gate polled", call.GateContext)
	}
	if got := f.originMain(); got != res.LandedCommit {
		t.Fatalf("origin/main = %s, want the landed %s", got, res.LandedCommit)
	}
	if len(f.landingLines()) != 1 {
		t.Fatalf("landings file has %d records, want one", len(f.landingLines()))
	}
	if len(cand.discarded) != 0 {
		t.Fatalf("discarded %v; the merged candidate's branch is the merger's to delete", cand.discarded)
	}
}

// TestLandMergerRefusalIsAHumanRejection: a PR Forgejo refuses as not ready to
// be merged is written to the bead as a rejection no retry can lift.
func TestLandMergerRefusalIsAHumanRejection(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	l.Candidate = &fakeCandidate{res: CandidateResult{State: CandidatePassed, Context: "ci / gate (push)"}}
	l.Merger = &fakeMerger{fn: func(MergeRequest) error {
		return &MergeRefusedError{Err: errors.New("forgejo: POST /merge returned 405: Not ready to be merged")}
	}}

	_, err := l.Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectMergeRefused, LabelNeedsHuman)
	if !strings.Contains(rej.Reason, OMStatusContext) {
		t.Fatalf("reason %q; want it to name the required %s status", rej.Reason, OMStatusContext)
	}
}

// TestLandMergerOutdatedBranchIsARebuild: a stale candidate is not a rejection
// and not a lost race the author pays for; nothing is written and the landing
// is retried from the top — with the candidate branch intact, because the open
// land PR the retry reuses is that branch (gt-k796q).
func TestLandMergerOutdatedBranchIsARebuild(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	cand := &fakeCandidate{res: CandidateResult{State: CandidatePassed, Context: "ci / gate (push)", Pushed: true}}
	l.Candidate = cand
	l.Merger = &fakeMerger{fn: func(MergeRequest) error {
		return &RaceError{Target: "main", Rebuild: true}
	}}

	_, err := l.Land(context.Background(), f.work)
	var race *RaceError
	if !errors.As(err, &race) || !race.Rebuild {
		t.Fatalf("Land error = %T %v, want a rebuild *RaceError", err, err)
	}
	if len(cand.discarded) != 0 {
		t.Fatalf("discarded %v; the 409 rebuild path keeps the branch its open land PR is on", cand.discarded)
	}
	f.assertUntouched(t)
}

// TestForgejoMergerGivesEachCallItsOwnWindow: every Forgejo call's deadline is
// set when that call starts, so a slow call spends its own budget and not the
// next one's — under one shared deadline a slow list-pulls would consume the
// merge's (gt-mdet3). The merge makes six calls: post, combined status, list
// pulls, open the pull, merge, delete the candidate.
func TestForgejoMergerGivesEachCallItsOwnWindow(t *testing.T) {
	t.Parallel()
	client := &fakePulls{}
	deadlines := map[string]time.Time{}
	client.onCall = func(name string, ctx context.Context) {
		d, ok := ctx.Deadline()
		if !ok {
			t.Errorf("%s ran with no deadline", name)
			return
		}
		deadlines[name] = d
	}
	if err := testMerger(client).Merge(context.Background(), mergeRequestForTest(Verdict{Verdict: VerdictApprove})); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(deadlines) != 6 {
		t.Fatalf("%d calls ran under a deadline, want the 6 the merge makes", len(deadlines))
	}
	windows := map[time.Time]struct{}{}
	for _, d := range deadlines {
		windows[d] = struct{}{}
	}
	if len(windows) < 2 {
		t.Error("every call shared one deadline: one window, so the first slow call spends the budget of every call after it")
	}
}

// TestForgejoMergerVerifiesAMergeThatTimedOut: the MergePull response was lost,
// so whether the server merged is unknown. The target says: it carries the
// candidate, the merge took, and the landing continues rather than failing on a
// merge that happened.
func TestForgejoMergerVerifiesAMergeThatTimedOut(t *testing.T) {
	t.Parallel()
	client := &fakePulls{mergeErr: context.DeadlineExceeded}
	req := mergeRequestForTest(Verdict{Verdict: VerdictApprove})
	var asked []string
	req.TargetHasCommit = func(commit string) (bool, error) {
		asked = append(asked, commit)
		return true, nil
	}
	if err := testMerger(client).Merge(context.Background(), req); err != nil {
		t.Fatalf("Merge: %v; the target carries the candidate, so the merge took", err)
	}
	if len(asked) != 1 || asked[0] != mergeHead {
		t.Errorf("the target was asked about %v, want the candidate %s", asked, mergeHead)
	}
	if len(client.deleted) != 1 || client.deleted[0] != "land/gt-abc" {
		t.Errorf("deleted %v, want the candidate of a merge that took", client.deleted)
	}
}

// TestForgejoMergerReportsATimeoutNoEvidenceBacks: a lost response is the
// failure it is unless the target says otherwise — with the candidate left for
// the retry. The target row is the regression: it fails against a merge that
// reports every timeout without asking. The no-verifier row guards the other
// half, that an unknown outcome is never read as a merge that took.
func TestForgejoMergerReportsATimeoutNoEvidenceBacks(t *testing.T) {
	t.Parallel()
	t.Run("the target does not carry it", func(t *testing.T) {
		t.Parallel()
		client := &fakePulls{mergeErr: context.DeadlineExceeded}
		req := mergeRequestForTest(Verdict{Verdict: VerdictApprove})
		var asked []string
		req.TargetHasCommit = func(commit string) (bool, error) {
			asked = append(asked, commit)
			return false, nil
		}
		err := testMerger(client).Merge(context.Background(), req)
		var infra *InfraError
		if !errors.As(err, &infra) || infra.Stage != "merge pull request" {
			t.Fatalf("Merge = %T %v, want an InfraError at the merge", err, err)
		}
		if len(asked) != 1 || asked[0] != mergeHead {
			t.Errorf("the target was asked about %v, want the candidate %s", asked, mergeHead)
		}
		if len(client.deleted) != 0 {
			t.Errorf("deleted %v after a merge that did not happen", client.deleted)
		}
	})
	t.Run("there is no target check", func(t *testing.T) {
		t.Parallel()
		client := &fakePulls{mergeErr: context.DeadlineExceeded}
		err := testMerger(client).Merge(context.Background(), mergeRequestForTest(Verdict{Verdict: VerdictApprove}))
		var infra *InfraError
		if !errors.As(err, &infra) || infra.Stage != "merge pull request" {
			t.Fatalf("Merge = %T %v, want an InfraError at the merge", err, err)
		}
		if len(client.deleted) != 0 {
			t.Errorf("deleted %v on a merge nothing says took", client.deleted)
		}
	})
}

// TestForgejoMergerDoesNotReadTheCallersCancellationAsALostResponse: a caller
// that stopped is not a merge of unknown outcome, so the target is never asked
// and the cancellation reaches the caller.
func TestForgejoMergerDoesNotReadTheCallersCancellationAsALostResponse(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &fakePulls{mergeFn: func(callCtx context.Context, _ int64, _ forgejo.MergePullRequestOption) error {
		return callCtx.Err()
	}}
	req := mergeRequestForTest(Verdict{Verdict: VerdictApprove})
	req.TargetHasCommit = func(string) (bool, error) {
		t.Error("the target was asked about a merge the caller cancelled")
		return false, nil
	}
	if err := testMerger(client).Merge(ctx, req); err == nil {
		t.Fatal("Merge succeeded on a cancelled context")
	}
	if len(client.deleted) != 0 {
		t.Errorf("deleted %v on a cancelled merge", client.deleted)
	}
}

// TestForgejoMergerKeepsAnAPIErrorsClassificationWhenAVerifierIsSet: the
// target check answers a lost response, never a status the API reported, so a
// 409 is still the rebuild and the target is not asked.
func TestForgejoMergerKeepsAnAPIErrorsClassificationWhenAVerifierIsSet(t *testing.T) {
	t.Parallel()
	client := &fakePulls{mergeErr: &forgejo.APIError{Method: "POST", Path: "/x", StatusCode: 409}}
	req := mergeRequestForTest(Verdict{Verdict: VerdictApprove})
	req.TargetHasCommit = func(string) (bool, error) {
		t.Error("the target was asked about a merge the API answered")
		return false, nil
	}
	err := testMerger(client).Merge(context.Background(), req)
	var race *RaceError
	if !errors.As(err, &race) || !race.Rebuild {
		t.Fatalf("Merge = %T %v, want a rebuild *RaceError", err, err)
	}
}
