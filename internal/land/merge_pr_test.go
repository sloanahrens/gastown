package land

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// fakePulls records the Forgejo calls the land PR makes and answers them from
// canned state, so no test needs a server.
type fakePulls struct {
	open      []forgejo.PullRequest
	statuses  []forgejo.StatusRequest
	created   []forgejo.CreatePullRequestOption
	merges    []forgejo.MergePullRequestOption
	mergePR   int64
	statusErr error
	listErr   error
	createErr error
	mergeErr  error
}

func (f *fakePulls) PostStatus(_ context.Context, _, _, _ string, req forgejo.StatusRequest) (*forgejo.CommitStatus, error) {
	f.statuses = append(f.statuses, req)
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return &forgejo.CommitStatus{Context: req.Context, Status: req.State}, nil
}

func (f *fakePulls) OpenPulls(context.Context, string, string) ([]forgejo.PullRequest, error) {
	return f.open, f.listErr
}

func (f *fakePulls) CreatePull(_ context.Context, _, _ string, opt forgejo.CreatePullRequestOption) (*forgejo.PullRequest, error) {
	f.created = append(f.created, opt)
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &forgejo.PullRequest{Number: 7, Head: forgejo.PRBranchInfo{Ref: opt.Head}, Base: forgejo.PRBranchInfo{Ref: opt.Base}}, nil
}

func (f *fakePulls) MergePull(_ context.Context, _, _ string, index int64, opt forgejo.MergePullRequestOption) error {
	f.merges = append(f.merges, opt)
	f.mergePR = index
	return f.mergeErr
}

func testMerger(client ForgejoPulls) *ForgejoMerger {
	return &ForgejoMerger{Client: client, Owner: "gastown", RepoName: "gastown"}
}

const mergeHead = "0123456789abcdef0123456789abcdef01234567"

func mergeWorkForTest() Work {
	return Work{BeadID: "gt-abc", Rig: "gastown", Branch: fixtureBranch, Head: mergeHead, Target: "main", Worker: "opal"}
}

// TestForgejoMergerPostsTheVerdictAndFastForwards: the merge path posts om's
// verdict as the required om / review status, opens land/<bead> -> the target,
// and merges the PR pinned to the candidate commit.
func TestForgejoMergerPostsTheVerdictAndFastForwards(t *testing.T) {
	t.Parallel()
	client := &fakePulls{}
	err := testMerger(client).Merge(context.Background(), mergeWorkForTest(), mergeHead, Verdict{Verdict: VerdictApprove, Score: 0.9})
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
	err := testMerger(client).Merge(context.Background(), mergeWorkForTest(), mergeHead, Verdict{Verdict: VerdictApprove})
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
	err := testMerger(client).Merge(context.Background(), mergeWorkForTest(), mergeHead, Verdict{Verdict: VerdictApprove})
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
	err := testMerger(client).Merge(context.Background(), mergeWorkForTest(), mergeHead, Verdict{Verdict: VerdictApprove})
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
	err := testMerger(client).Merge(context.Background(), mergeWorkForTest(), mergeHead, Verdict{Verdict: VerdictApprove})
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
	calls []mergeCall
	fn    func(w Work, head string, verdict Verdict) error
}

type mergeCall struct {
	w       Work
	head    string
	verdict Verdict
}

func (m *fakeMerger) Merge(_ context.Context, w Work, head string, verdict Verdict) error {
	m.calls = append(m.calls, mergeCall{w: w, head: head, verdict: verdict})
	if m.fn != nil {
		return m.fn(w, head, verdict)
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
	l.Candidate = &fakeCandidate{res: CandidateResult{State: CandidatePassed, Context: "ci / gate (push)"}}
	merger := &fakeMerger{fn: func(_ Work, head string, _ Verdict) error {
		// Stand in for the server-side fast-forward: the target's tip becomes
		// the candidate the worker read back.
		f.git.SetRef(t, f.origin, "refs/heads/main", head)
		return nil
	}}
	l.Merger = merger
	pushed := false
	l.afterPush = func() { pushed = true }

	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if pushed {
		t.Fatal("a cut-over rig force-pushed the target instead of merging through the pull request")
	}
	if len(f.gate.dirs) != 0 {
		t.Fatalf("the local gate ran %d time(s) on a rig landing through CI", len(f.gate.dirs))
	}
	if len(merger.calls) != 1 {
		t.Fatalf("the merger ran %d time(s), want once", len(merger.calls))
	}
	call := merger.calls[0]
	if call.head != res.LandedCommit {
		t.Fatalf("merge head = %s, want the landed %s", call.head, res.LandedCommit)
	}
	if call.verdict.Verdict != VerdictApprove {
		t.Fatalf("verdict handed to the merger = %q, want approve", call.verdict.Verdict)
	}
	if got := f.originMain(); got != res.LandedCommit {
		t.Fatalf("origin/main = %s, want the landed %s", got, res.LandedCommit)
	}
	if len(f.landingLines()) != 1 {
		t.Fatalf("landings file has %d records, want one", len(f.landingLines()))
	}
}

// TestLandMergerRefusalIsAHumanRejection: a PR Forgejo refuses as not ready to
// be merged is written to the bead as a rejection no retry can lift.
func TestLandMergerRefusalIsAHumanRejection(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	l.Candidate = &fakeCandidate{res: CandidateResult{State: CandidatePassed, Context: "ci / gate (push)"}}
	l.Merger = &fakeMerger{fn: func(Work, string, Verdict) error {
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
// is retried from the top.
func TestLandMergerOutdatedBranchIsARebuild(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	l.Candidate = &fakeCandidate{res: CandidateResult{State: CandidatePassed, Context: "ci / gate (push)"}}
	l.Merger = &fakeMerger{fn: func(Work, string, Verdict) error {
		return &RaceError{Target: "main", Rebuild: true}
	}}

	_, err := l.Land(context.Background(), f.work)
	var race *RaceError
	if !errors.As(err, &race) || !race.Rebuild {
		t.Fatalf("Land error = %T %v, want a rebuild *RaceError", err, err)
	}
	f.assertUntouched(t)
}
