package land

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/checkpoint"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/git"
)

// Beads is the part of beads.Client Land writes the work bead through
// (ADR 0001: bd only, never the store).
type Beads interface {
	Show(id string) (*beads.Issue, error)
	Update(id string, opts beads.UpdateOptions) error
	ForceCloseWithReason(reason string, ids ...string) error
	AppendNotes(id, note string) error
	// Children and CloseWithReason are what closing the molecule a landed
	// bead carries needs: the walk down to its step wisps, then an unforced
	// close of each (gt-oqz0r).
	Children(parentID string) ([]*beads.Issue, error)
	CloseWithReason(reason string, ids ...string) error
}

var _ Beads = beads.Client(nil)

// Repo is the part of *git.Git Land works through: the lander's clone, and
// each throwaway worktree added from it (docs/testing.md, "Seams for external
// tools"). Tests pass gitfake.
type Repo interface {
	FetchRefspecWithTimeout(remote, refspec string, timeout time.Duration) error
	Rev(ref string) (string, error)
	IsAncestor(ancestor, descendant string) (bool, error)
	PushRemoteBranchTip(remote, branch string) (string, error)
	TreesIdentical(a, b string) (bool, error)
	CommitMessages(base, head string) ([]git.CommitMessage, error)
	CommitLineStatsInRange(revRange string, limit int) ([]git.CommitLineStats, error)
	// DiffNameOnly is the paths a change touches, and ShowFileAtRev reads one
	// file at a revision. RiskPaths uses both to label a landing that touched
	// a risk path without holding it (gt-vsct7.4).
	DiffNameOnly(base, head string) ([]string, error)
	ShowFileAtRev(ref, path string) (string, error)
	PatchID(base, head string) (string, error)
	WorktreeAddDetached(path, ref string) error
	WorktreeRemove(path string, force bool) error
	WorktreePrune() error
	MergeNoFF(branch, message string) error
	MergeSquash(branch, message string) error
	GetConflictingFiles() ([]string, error)
	AbortMerge() error
	// Push updates remote's branch to the refspec's source, force-updating it:
	// the merge candidate is the worker's own branch and every retry rebuilds
	// it.
	Push(remote, refspec string, force bool) error
	VerifyPushedCommit(remote, branch, commit string) error
}

var _ Repo = (*git.Git)(nil)

// Lander lands work for one rig. It holds no state between calls; the caller
// (the daemon's landing worker, gt-v4ssj.2) runs one Land at a time per rig.
type Lander struct {
	// Repo is a clone of the rig's repository that throwaway worktrees are
	// added from. It is never checked out onto anything by Land.
	Repo string
	// Remote is the remote to fetch from and push to; "" means origin. The
	// landing worker sets it to the rig's configured landing remote
	// (gt-fn9e6.9).
	Remote string
	// WorkRoot is a private directory (0700) the throwaway worktrees live in.
	WorkRoot string
	// Route names who landed, for the record ("daemon" when empty).
	Route string

	// Candidate is the Forgejo gate that runs on the merged tree: the tree is
	// pushed as land/<bead> and its CI verdict decides. It is required —
	// merge_queue.forgejo is mandatory for any rig that lands, so a Lander
	// without one is a rig that can no longer be landed and must fail closed
	// (gt-fn9e6.32).
	Candidate Candidate
	// Merger lands the merged tree through a Forgejo pull request: it posts
	// om's verdict, opens land/<bead> -> the target and merges it pinned to
	// the candidate commit. It is required beside Candidate; the local
	// force-push the two replaced is gone (gt-fn9e6.32).
	Merger   Merger
	Reviewer Reviewer
	Beads    Beads
	Landings *LandingsFile
	Out      io.Writer
	Now      func() time.Time
	// RangeChecks run on base..head before the merge (the landing worker
	// passes AttributionCheck). A returned Rejection is written to the bead
	// like any other.
	RangeChecks []RangeCheck
	// ReviewErrorLands lands a green merged tree whose om review could not
	// run (binary missing, malformed verdict, timeout), recording the verdict
	// as "error:<reason>", so om infrastructure never blocks the queue. When
	// false, such a landing stops with an *InfraError instead.
	ReviewErrorLands bool
	// ReviewErrorRejects, when ReviewErrorLands is false, rejects a green
	// merged tree whose om review produced no verdict (after the reviewer's
	// own retries) to gt:needs-human, instead of stopping as an *InfraError
	// that the next pass would retry and pay om for again (gt-b5ugw).
	ReviewErrorRejects bool
	// ReviewErrorLandsLabels names labels whose beads land a green tree with
	// om_verdict error when om returns no verdict, whatever
	// ReviewErrorRejects says: the red-main owner's reverts, so an om outage
	// never keeps main red (gt-b5ugw review).
	ReviewErrorLandsLabels []string
	// OMDiffTooLargeLines is the merged-tree change size (lines added plus
	// removed) past which an om execution error is reported as the diff being
	// too large for om. Zero means DefaultOMDiffTooLargeLines (gt-hhid7).
	OMDiffTooLargeLines int
	// Slow reports a gate or om stage that runs past its threshold; nil
	// reports nothing (gt-lcu5p).
	Slow *SlowAlarm
	// Stage, when set, is told each stage as the landing enters it, by the
	// StageCI/StageOM names. It is how the daemon judges a pass by the stage
	// it is running rather than by the pipeline as a whole; the work before
	// the CI wait has no stage of its own and reports nothing (gt-84gcp).
	Stage func(beadID, stage string)
	// CIFailure, when set, is told every red candidate-gate verdict as the
	// gate reads it, before the rejection is written: the daemon's CI-failure
	// watch turns a test failing on a second bead into an escalation and a
	// repair bead (gt-xvw20). It runs on the landing's goroutine and must
	// return promptly; the watch does its alerting off it.
	CIFailure func(f CIFailure)

	openRepo func(dir string) Repo // test seam: opens git at dir; nil means *git.Git
}

// Result describes a landing.
type Result struct {
	LandedCommit string
	PatchID      string
	// Base is the target tip the merge was built on and the lease expected.
	Base    string
	Gate    GateResult
	Verdict Verdict
	// RiskPaths are the changed paths the landing touched that matched
	// RiskPathsFile at Base. They label the work bead and are written to the
	// landing record; they never reject or delay the landing (gt-vsct7.4).
	RiskPaths []string
}

// RejectionKind says what about the work stopped it from landing.
type RejectionKind string

const (
	RejectConflict  RejectionKind = "conflict"
	RejectGate      RejectionKind = "gate"
	RejectReview    RejectionKind = "review"
	RejectEmpty     RejectionKind = "empty"
	RejectPolicy    RejectionKind = "policy"
	RejectNotPushed RejectionKind = "not_pushed"
	// RejectTimeout is a gate stage that outlived its own timeout (gt-b5ugw).
	RejectTimeout RejectionKind = "timeout"
	// RejectMergeRefused is a cut-over rig's PR the API refused as "not ready
	// to be merged" (405) with the base unmoved: a required status is missing
	// or red, which a retry never fixes, so a human owns it (gt-fn9e6.5).
	RejectMergeRefused RejectionKind = "merge_refused"
	// RejectForgedStatus is a cut-over rig's candidate that failed the merge's
	// creator check (verifyCreators): a required status was posted by an
	// account the merge does not trust. Tampering with the gate is escalated,
	// never merged (gt-fn9e6.7).
	RejectForgedStatus RejectionKind = "forged_status"
)

// Rejection is a landing refused because of the work itself. Land has
// already written it to the work bead (a MERGE REJECTION block), set the bead
// open and unassigned, and swapped the ready label for rework (or
// gt:needs-human when Rework is false). RecordErr is set when that write
// failed; the rejection still stands.
type Rejection struct {
	Kind        RejectionKind
	Reason      string
	GateTail    string
	Findings    []Finding
	Conflicting []string
	// ReviewSummary and ReviewScore are om's, when om asked for changes.
	ReviewSummary string
	ReviewScore   float64
	// Rework is false when only a human can lift the refusal (no_merge), and
	// past MaxReworkAttempts whatever the kind was: the loop, not the refusal,
	// is what the ceiling stops.
	Rework    bool
	RecordErr error
}

func (r *Rejection) Error() string {
	msg := fmt.Sprintf("landing rejected (%s): %s", r.Kind, r.Reason)
	if r.RecordErr != nil {
		msg += fmt.Sprintf(" [recording the rejection failed: %v]", r.RecordErr)
	}
	return msg
}

// RaceError means the target moved between the merge and the write, so the
// lease refused the push — or, on a cut-over rig, Forgejo's outdated-branch
// guard refused the merge. Nothing was written; land again from the top.
type RaceError struct {
	Target   string
	Expected string
	Actual   string
	// Rebuild is set when a cut-over rig's merge was refused because the
	// target moved after the candidate was built. The candidate is cut from
	// the target, so a busy target makes this the normal case, not a failure:
	// the next attempt rebuilds it on the new tip (design, "Risks").
	Rebuild bool
}

func (e *RaceError) Error() string {
	if e.Rebuild {
		// The outdated-branch guard reports no tips, only that the merge is
		// refused: the worker rebuilds rather than comparing.
		return fmt.Sprintf("landing lost the race: %s moved after the candidate was built; it must be rebuilt on the new tip", e.Target)
	}
	return fmt.Sprintf("landing lost the race: %s moved from %s to %s during the gate; nothing pushed", e.Target, shortSHA(e.Expected), shortSHA(e.Actual))
}

// MergeRefusedError is Forgejo refusing a PR merge with 405 "not ready to be
// merged" when the base has not moved: a required status is missing or red, so
// the PR cannot merge and a retry never will. It is separate from the
// outdated-branch race (a 409), which the worker answers by rebuilding; Land
// turns this one into a rejection for a human (gt-fn9e6.5 review note).
type MergeRefusedError struct {
	Err error
}

func (e *MergeRefusedError) Error() string { return "the merge was refused: " + e.Err.Error() }
func (e *MergeRefusedError) Unwrap() error { return e.Err }

// ForgedStatusError is the forgery check refusing the candidate: a required
// status on it was posted by an account the merge does not trust. No retry and
// no author lifts it, so Land leaves it for a human (gt-fn9e6.7).
type ForgedStatusError struct {
	// Reason names the status, the commit and the creator that failed, for the
	// rejection note.
	Reason string
}

func (e *ForgedStatusError) Error() string {
	return "a required status on the candidate failed the creator check: " + e.Reason
}

// InfraError is a failure of the landing machinery (git, bd, the gate's
// tooling, om) that says nothing about the work. Nothing was written to the
// work bead.
type InfraError struct {
	Stage string
	Err   error
}

func (e *InfraError) Error() string { return fmt.Sprintf("landing failed at %s: %v", e.Stage, e.Err) }
func (e *InfraError) Unwrap() error { return e.Err }

// ErrReadBack means the push reported success but the target's tip is not
// the landed commit. Nothing is recorded: whether the work landed is unknown
// until a human or the next read says so.
var ErrReadBack = errors.New("target tip is not the landed commit after the push")

// RecordError means the work landed (Result is valid, the target's tip was
// read back) but the landing record could not be completed.
type RecordError struct {
	Result Result
	Err    error
}

func (e *RecordError) Error() string {
	return fmt.Sprintf("landed %s but the landing record is incomplete: %v", shortSHA(e.Result.LandedCommit), e.Err)
}
func (e *RecordError) Unwrap() error { return e.Err }

// The landing pipeline's stages, as Lander reports them to Stage and as the
// slow-landing alarm labels them (gt-lcu5p). Only the two the CI wait and the
// review run have a timeout of their own; the rest are bounded by the
// landing's overall budget.
const (
	StageCI = "ci"
	StageOM = "om"
)

// ciGateContext is the required status context the candidate gate polled, read
// from the CI step it recorded (a CI StepResult carries the context in
// Command). A merge with no context to verify is a misconfiguration
// (gt-fn9e6.7).
func ciGateContext(g GateResult) string {
	for _, st := range g.Steps {
		if st.Name == StageCI {
			return st.Command
		}
	}
	return ""
}

// candidateGate runs the Forgejo gate and renders its verdict as a GateResult,
// the shape the red path and the landed record read. Silence is not a verdict:
// it comes back as an *InfraError, the path every other no-verdict failure
// takes.
//
// The run's own result comes back beside the verdict: a red or infra outcome
// ends the landing without a merge, and the candidate branch has to go with it
// (gt-k796q), so Land needs the branch the run pushed.
func (l *Lander) candidateGate(ctx context.Context, wt Repo, dir string, w Work, merged string) (CandidateResult, GateResult, error) {
	start := time.Now()
	cres := l.Candidate.Run(ctx, wt, dir, w, merged)
	step := StepResult{Name: StageCI, Command: cres.Context, Elapsed: time.Since(start), Tail: cres.Tail}
	if cres.Err != nil {
		return cres, GateResult{}, &InfraError{Stage: StageCI, Err: cres.Err}
	}
	switch cres.State {
	case CandidatePassed:
		return cres, GateResult{Passed: true, Steps: []StepResult{step}}, nil
	case CandidateFailed:
		step.ExitCode = 1
		l.ciFailure(w, cres)
		l.logf("%s: %s failed on the candidate %s (%s); the job log tail goes with the rework", w.BeadID, cres.Context, shortSHA(cres.SHA), cres.Branch)
		return cres, GateResult{Steps: []StepResult{step}}, nil
	default:
		return cres, GateResult{}, &InfraError{Stage: StageCI, Err: fmt.Errorf(
			"%w: %s reported nothing on the candidate %s (%s) within its wait window",
			ErrCISilence, cres.Context, shortSHA(cres.SHA), cres.Branch)}
	}
}

// ciFailure hands a red candidate verdict to the CI-failure watch, with the
// failing job's log tail parsed for the tests it names.
func (l *Lander) ciFailure(w Work, cres CandidateResult) {
	if l.CIFailure == nil {
		return
	}
	l.CIFailure(CIFailure{Bead: w.BeadID, Tail: cres.Tail, Tests: ParseTestFailures(cres.Tail)})
}

// discardCandidate deletes the candidate branch the gate pushed this run,
// after a terminal red or infra outcome ends the landing without a merge: the
// branch has no owner left, and only a later attempt's force-push would
// replace it, so a bead that is rejected or abandoned keeps one land/<bead>
// branch on the remote forever (gt-k796q). Nothing is discarded for a green
// gate: the branch is the one the land PR opens on and, on the 409 rebuild
// path, the one the retry's pullFor reuses.
func (l *Lander) discardCandidate(ctx context.Context, w Work, cres *CandidateResult) {
	if cres == nil {
		return
	}
	l.Candidate.Discard(ctx, w, *cres)
}

// Merger lands the merged candidate through a Forgejo pull request, the write
// path a cut-over rig replaces the force-push with. It posts om's verdict as
// the om / review commit status, verifies the candidate's required statuses
// carry the creators the merge trusts, opens land/<bead> -> the target, and
// merges the PR pinned to the candidate commit (design, "om review and the
// om / review status" and "The PR, the merge and the forgery check").
// *ForgejoMerger is the production implementation; a Lander with no Merger
// keeps the local force-push.
type Merger interface {
	// Merge lands req. A 409 from the outdated-branch guard comes back as a
	// *RaceError with Rebuild set; a required status whose creator fails the
	// forgery check comes back as a *ForgedStatusError.
	Merge(ctx context.Context, req MergeRequest) error
}

// MergeRequest is what Land hands its Merger once the gate is green and om has
// a verdict.
type MergeRequest struct {
	// Work is the landing: its bead, its pushed branch and its target.
	Work Work
	// Head is the merged commit CI gated and om reviewed, and is the merge's
	// head_commit_id.
	Head string
	// Verdict is om's decision, posted as the om / review status.
	Verdict Verdict
	// GateContext is the required status context the candidate gate polled,
	// such as "ci / gate (push)". The creator check reads that context's
	// statuses, so it is required.
	GateContext string
}

// ForgejoPulls is the part of the Forgejo client the land PR uses.
// *forgejo.Client implements it; tests pass a fake.
type ForgejoPulls interface {
	PostStatus(ctx context.Context, owner, repo, sha string, req forgejo.StatusRequest) (*forgejo.CommitStatus, error)
	CombinedStatus(ctx context.Context, owner, repo, ref string) (*forgejo.CombinedStatus, error)
	OpenPulls(ctx context.Context, owner, repo string) ([]forgejo.PullRequest, error)
	CreatePull(ctx context.Context, owner, repo string, opt forgejo.CreatePullRequestOption) (*forgejo.PullRequest, error)
	MergePull(ctx context.Context, owner, repo string, index int64, opt forgejo.MergePullRequestOption) error
	DeleteBranch(ctx context.Context, owner, repo, branch string) error
}

// ForgejoMerger is the production Merger. The daemon builds it from the rig's
// merge_queue.forgejo block, beside the CandidateGate and from the same client
// (daemon/landing_worker.go), so the bot that pushes the candidate is the bot
// that posts the review and merges.
type ForgejoMerger struct {
	// Client is the Forgejo API.
	Client ForgejoPulls
	// Owner and RepoName name the Forgejo repository that holds the PR.
	Owner, RepoName string
	// BotLogin is the landing bot's Forgejo login: the only account whose
	// om / review status the merge trusts. The daemon takes it from the rig's
	// merge_queue.forgejo.bots, and refuses to build the merger without it,
	// because an unknown bot is a check that can never pass.
	BotLogin string
	// CallTimeout bounds one Forgejo call; a zero means
	// DefaultCandidateCallTimeout.
	CallTimeout time.Duration
	// Out, when set, receives one line per stage.
	Out io.Writer
}

// Merge posts the review status, verifies the candidate's creators, opens the
// land PR (reusing the one an earlier attempt left) and merges it pinned to
// head.
func (m *ForgejoMerger) Merge(parent context.Context, req MergeRequest) error {
	ctx, cancel := context.WithTimeout(parent, nonZero(m.CallTimeout, DefaultCandidateCallTimeout))
	defer cancel()
	if err := m.postVerdict(ctx, req.Head, req.Verdict); err != nil {
		return err
	}
	// The creator check runs after the verdict is posted and before anything
	// is opened or merged: it reads the om / review status the post above
	// created, and a candidate that fails it never reaches the PR.
	if err := m.verifyCreators(ctx, req); err != nil {
		return err
	}
	w := req.Work
	head := req.Head
	pr, err := m.pullFor(ctx, w, head)
	if err != nil {
		return err
	}
	err = m.Client.MergePull(ctx, m.Owner, m.RepoName, pr.Number, forgejo.MergePullRequestOption{
		// The candidate already is the merge of the work onto the target, so a
		// fast-forward lands that exact commit — the one CI gated and om
		// reviewed — as the target's tip. A merge commit made here would test
		// one tree and land another.
		Style:        forgejo.MergeStyleFastForward,
		HeadCommitID: head,
	})
	if err != nil {
		var apiErr *forgejo.APIError
		switch {
		case errors.As(err, &apiErr) && apiErr.IsConflict():
			// block_on_outdated_branch: the target moved after the candidate was
			// built, so this candidate is stale. Not a rejection and not a lost
			// race the author pays for — the next attempt rebuilds it (design,
			// "The worker must treat the rebuild as the normal case").
			m.logf("%s: %s moved before the merge; the candidate must be rebuilt", w.BeadID, w.Target)
			return &RaceError{Target: w.Target, Rebuild: true}
		case errors.As(err, &apiErr) && apiErr.IsNotReadyToMerge():
			// "not ready to be merged" with no moved base means a required
			// status is missing or red, which a retry never fixes: a renamed
			// workflow, a context that never reported, a status branch
			// protection refused. A human owns it (gt-fn9e6.5 review note).
			m.logf("%s: %s refused the merge as not ready", w.BeadID, m.RepoName)
			return &MergeRefusedError{Err: err}
		}
		return &InfraError{Stage: "merge pull request", Err: err}
	}
	m.logf("%s: merged %s on %s through pull request #%d", w.BeadID, shortSHA(head), w.Target, pr.Number)
	m.deleteCandidate(ctx, w)
	return nil
}

// deleteCandidate removes the land/<bead> candidate branch once the PR is
// merged. Forgejo applies a repository's delete-branch-after-merge setting only
// to a merge made in the web UI, never to one made through the API, so without
// this the candidate branches accumulate on every landing (gt-fn9e6.21).
//
// The deletion runs as the landing bot: the one account the landing path
// pushes the candidate with. A failure is logged and ignored — the landing has
// already succeeded and a later landing for the same bead force-updates the
// branch anyway — and a merge that did not happen never reaches here, so the
// branch stays for the retry.
func (m *ForgejoMerger) deleteCandidate(ctx context.Context, w Work) {
	branch := w.Candidate()
	if err := m.Client.DeleteBranch(ctx, m.Owner, m.RepoName, branch); err != nil {
		m.logf("%s: could not delete %s after the merge: %v", w.BeadID, branch, err)
		return
	}
	m.logf("%s: deleted %s after the merge", w.BeadID, branch)
}

// postVerdict posts om's verdict as the required om / review status on the
// candidate commit.
func (m *ForgejoMerger) postVerdict(ctx context.Context, head string, verdict Verdict) error {
	if _, err := m.Client.PostStatus(ctx, m.Owner, m.RepoName, head, OMVerdictStatus(verdict)); err != nil {
		return &InfraError{Stage: "post om review status", Err: err}
	}
	return nil
}

// verifyCreators is the forgery check: any write-access user can post a commit
// status, so the merge trusts a required status only when its creator is the
// one that context demands. The gate status must carry no user creator — it
// comes from the Actions run — and om / review must be posted by the landing
// bot. Every status carrying a context is checked, so one a user posted cannot
// hide behind the one a workflow posted; a failure comes back as a
// *ForgedStatusError, never a merge and never a retry (gt-fn9e6.7).
//
// The "no user creator" half is a Forgejo behavior rather than a contract
// (verified on 16.0.5), so a version that starts attributing Actions statuses
// to a user blocks every merge here rather than letting one through (design,
// "Where the epic cannot be followed exactly").
func (m *ForgejoMerger) verifyCreators(ctx context.Context, req MergeRequest) error {
	combined, err := m.Client.CombinedStatus(ctx, m.Owner, m.RepoName, req.Head)
	if err != nil {
		return &InfraError{Stage: "read the candidate's statuses", Err: err}
	}
	// The candidate gate read the gate context as a success moments ago, so an
	// absent one is an anomaly rather than a verdict: it is retried, never
	// merged, and never reported as a forgery it did not see.
	gate := combined.StatusesFor(req.GateContext)
	if len(gate) == 0 {
		return &InfraError{Stage: "read the candidate's statuses",
			Err: fmt.Errorf("no %s status is on the candidate %s", req.GateContext, shortSHA(req.Head))}
	}
	for _, st := range gate {
		if st.HasUserCreator() {
			return &ForgedStatusError{Reason: fmt.Sprintf(
				"the %s status on the candidate %s was posted by %s, but only the workflow run posts it: a status with a user creator is a forged gate, so the landing is escalated to a human and never merged",
				req.GateContext, shortSHA(req.Head), creatorDesc(&st))}
		}
	}
	review := combined.StatusesFor(OMStatusContext)
	if len(review) == 0 {
		return &InfraError{Stage: "read the candidate's statuses",
			Err: fmt.Errorf("no %s status is on the candidate %s, though the worker just posted one", OMStatusContext, shortSHA(req.Head))}
	}
	for _, st := range review {
		if !st.PostedBy(m.BotLogin) {
			return &ForgedStatusError{Reason: fmt.Sprintf(
				"the %s status on the candidate %s was posted by %s, not by the landing bot %q: the review the merge is pinned to is not the worker's, so the landing is escalated to a human and never merged",
				OMStatusContext, shortSHA(req.Head), creatorDesc(&st), NoteField(m.BotLogin))}
		}
	}
	return nil
}

// creatorDesc names who posted a status, for a rejection note. NoteField keeps
// an account's chosen display name from injecting a line into the note.
func creatorDesc(s *forgejo.CommitStatus) string {
	if login := s.CreatorLogin(); login != "" {
		return "user " + NoteField(fmt.Sprintf("%q", login))
	}
	return "no user"
}

// pullFor is the open PR for w's candidate branch, or a new one. A rebuild
// after the outdated-branch guard leaves the first attempt's PR open — the
// branch is force-updated and the PR follows it — so the retry reuses that PR
// at its new head rather than failing to open a second one for the same pair
// of branches.
func (m *ForgejoMerger) pullFor(ctx context.Context, w Work, head string) (*forgejo.PullRequest, error) {
	branch := w.Candidate()
	pulls, err := m.Client.OpenPulls(ctx, m.Owner, m.RepoName)
	if err != nil {
		return nil, &InfraError{Stage: "list pull requests", Err: err}
	}
	for i := range pulls {
		if pulls[i].Head.Ref == branch {
			return &pulls[i], nil
		}
	}
	pr, err := m.Client.CreatePull(ctx, m.Owner, m.RepoName, forgejo.CreatePullRequestOption{
		Title: NoteField(fmt.Sprintf("land: %s (%s)", w.BeadID, w.Branch)),
		Body: fmt.Sprintf("Landing %s from %s at %s onto %s.\nCI gated this commit; the %s status records the review the merge is pinned to.\n",
			w.BeadID, NoteField(w.Branch), shortSHA(head), w.Target, OMStatusContext),
		Head: branch,
		Base: w.Target,
	})
	if err != nil {
		return nil, &InfraError{Stage: "open the land pull request", Err: err}
	}
	return pr, nil
}

func (m *ForgejoMerger) logf(format string, args ...any) {
	if m.Out != nil {
		_, _ = fmt.Fprintf(m.Out, "[land] "+format+"\n", args...)
	}
}

// stage reports the landing's current stage to the Stage hook, if one is set.
func (l *Lander) stage(w Work, stage string) {
	if l.Stage != nil {
		l.Stage(w.BeadID, stage)
	}
}

func (l *Lander) remote() string {
	if l.Remote == "" {
		return "origin"
	}
	return l.Remote
}

func (l *Lander) logf(format string, args ...any) {
	if l.Out != nil {
		_, _ = fmt.Fprintf(l.Out, "[land] "+format+"\n", args...)
	}
}

func (l *Lander) open(dir string) Repo {
	if l.openRepo != nil {
		return l.openRepo(dir)
	}
	return git.NewGit(dir)
}

func (l *Lander) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Land lands w: the declared head merged onto a throwaway worktree of the
// target, the merged tree gated by CI on the pushed candidate, then om review,
// then the target written through a Forgejo pull request — an om / review
// status, a creator check on the candidate's required statuses, and a
// land/<bead> PR merged with head_commit_id. The target's tip is read back,
// then the landings file, the LANDING RECORD block and the close.
//
// A 409 from the PR merge means the target moved and the candidate is rebuilt
// through the race path; a 405 is a refusal for a human, never a race; a
// required status a user posted is a rejection for a human, never a merge.
//
// A red gate is a rejection: nothing lands on a red candidate (gt-fn9e6.32).
//
// Errors: *Rejection (written to the bead), *RaceError, *InfraError (nothing
// written), ErrNotReady, *RecordError (landed, record incomplete).
func (l *Lander) Land(ctx context.Context, w Work) (Result, error) {
	if err := l.validate(w); err != nil {
		return Result{}, err
	}
	issue, err := l.Beads.Show(w.BeadID)
	if err != nil {
		return Result{}, &InfraError{Stage: "read bead", Err: err}
	}
	remote := l.remote()
	g := l.open(l.Repo)
	// A landing whose bead record was left incomplete (RecordError) is
	// finished from the landings file, never landed twice. This runs before
	// the readiness checks, because the partial record may already have
	// taken the ready label off.
	if repaired, err := l.repairRecord(g, w); err != nil {
		if repaired != nil {
			return *repaired, err
		}
		return Result{}, err
	} else if repaired != nil {
		return *repaired, nil
	}
	// IsActionable, not IsTerminal: a defer applied while the gate ran parks
	// the bead under a worker that has already read it (gt-y7n1u).
	if !beads.IssueStatus(strings.TrimSpace(issue.Status)).IsActionable() {
		return Result{}, fmt.Errorf("%w: %s is %s", ErrNotReady, w.BeadID, issue.Status)
	}
	if !beads.HasLabel(issue, LabelReadyToLand) {
		return Result{}, fmt.Errorf("%w: %s has no %s label", ErrNotReady, w.BeadID, LabelReadyToLand)
	}
	if rej := policyRejection(issue); rej != nil {
		return Result{}, l.reject(issue, w, rej, nil)
	}

	if err := l.fetchTarget(g, w.Target); err != nil {
		return Result{}, &InfraError{Stage: "fetch target", Err: err}
	}
	base, err := g.Rev(remote + "/" + w.Target)
	if err != nil {
		return Result{}, &InfraError{Stage: "resolve target", Err: err}
	}
	if rej, err := l.checkHeadPushed(g, w); err != nil || rej != nil {
		if rej != nil {
			return Result{}, l.reject(issue, w, rej, nil)
		}
		return Result{}, err
	}
	if already, err := g.IsAncestor(w.Head, base); err != nil {
		return Result{}, &InfraError{Stage: "ancestry", Err: err}
	} else if already {
		// It reached the target by a route that left no landing record (an
		// operator push): not the author's to rework.
		return Result{}, l.reject(issue, w, &Rejection{Kind: RejectEmpty, Rework: false,
			Reason: fmt.Sprintf("empty merge: head %s is already reachable from %s/%s (%s) with no landing record; nothing to land", shortSHA(w.Head), remote, w.Target, shortSHA(base))}, nil)
	}
	for _, check := range l.RangeChecks {
		rej, err := check(g, base, w.Head)
		if err != nil {
			return Result{}, &InfraError{Stage: "range check", Err: err}
		}
		if rej != nil {
			return Result{}, l.reject(issue, w, rej, nil)
		}
	}
	// Risk paths are the label this landing carries, never a hold: a read
	// that fails leaves the landing unlabelled rather than stopping it, so a
	// broken globs file can never wedge the queue (gt-vsct7.4).
	riskPaths, err := RiskPaths(g, base, w.Head)
	if err != nil {
		l.logf("%s: reading risk paths: %v; landing unlabelled", w.BeadID, err)
		riskPaths = nil
	}
	if same, err := g.TreesIdentical(base, w.Head); err != nil {
		return Result{}, &InfraError{Stage: "compare trees", Err: err}
	} else if same {
		return Result{}, l.reject(issue, w, &Rejection{Kind: RejectEmpty, Rework: true, Reason: EmptyMergeReason(g, EmptyMerge{
			Target: w.Target, Base: base, Head: w.Head, Stage: "before merge",
			Comparison: fmt.Sprintf("%s/%s and %s have identical trees", remote, w.Target, shortSHA(w.Head)),
		})}, nil)
	}

	dir, id, cleanup, err := l.addWorktree(g, base)
	if err != nil {
		return Result{}, &InfraError{Stage: "worktree", Err: err}
	}
	defer cleanup()
	ctx = WithLandingID(ctx, id)
	wt := l.open(dir)

	merged, rej, err := mergeWork(wt, w, base)
	if err != nil {
		return Result{}, err
	}
	if rej != nil {
		return Result{}, l.reject(issue, w, rej, nil)
	}
	if same, err := wt.TreesIdentical(base, "HEAD"); err != nil {
		return Result{}, &InfraError{Stage: "compare trees", Err: err}
	} else if same {
		return Result{}, l.reject(issue, w, &Rejection{Kind: RejectEmpty, Rework: true, Reason: EmptyMergeReason(wt, EmptyMerge{
			Target: w.Target, Base: base, Head: w.Head, Stage: "after merge",
			Comparison: fmt.Sprintf("the merge result and %s/%s have identical trees", remote, w.Target),
		})}, nil)
	}
	patchID, err := wt.PatchID(base, merged)
	if err != nil {
		return Result{}, &InfraError{Stage: "patch-id", Err: err}
	}

	l.logf("%s: merged %s onto %s/%s (%s) as %s; pushing the merge candidate and waiting for its CI verdict, then om review", w.BeadID, shortSHA(w.Head), remote, w.Target, shortSHA(base), shortSHA(merged))
	// The candidate's CI runs the same stages the gate always did (lint, then
	// tests), and om runs only after it passes: om is the costly stage, and
	// work that fails lint or tests never pays for it (gt-b5ugw).
	l.stage(w, StageCI)
	gateCtx, gateDone := l.Slow.watch(ctx, l, w, dir, StageCI)
	var (
		gateRes   GateResult
		gateErr   error
		ciContext string
		// candRes is the candidate gate's run: the branch it pushed is
		// discarded if the gate ends the landing.
		candRes *CandidateResult
	)
	w.CandidateBranch, w.CandidateHead = w.Candidate(), merged
	run, ciVerdict, err := l.candidateGate(gateCtx, wt, dir, w, merged)
	candRes, gateRes, gateErr = &run, ciVerdict, err
	ciContext = ciGateContext(gateRes)
	gateDone()
	if gateErr != nil {
		l.discardCandidate(ctx, w, candRes)
		return Result{}, gateErr
	}
	res := Result{LandedCommit: merged, PatchID: patchID, Base: base, Gate: gateRes, RiskPaths: riskPaths}
	if gateRes.Err != nil {
		return Result{}, &InfraError{Stage: "gate", Err: gateRes.Err}
	}
	if !gateRes.Passed {
		reason, tail := "gate failed on the merged tree: "+gateRes.Summary(), gateRes.FailureTail()
		if len(gateRes.Steps) > 0 {
			reason = fmt.Sprintf("the candidate gate %s failed on the merged tree", gateRes.Steps[0].Command)
		}
		l.logf("%s: %s", w.BeadID, stageTimes(gateRes, 0, false))
		l.discardCandidate(ctx, w, candRes)
		rej := &Rejection{Kind: RejectGate, Rework: true, Reason: reason, GateTail: tail}
		return Result{}, l.reject(issue, w, rej, nil)
	}
	var (
		verdict   Verdict
		reviewErr error
	)
	if OverseerReviewed(issue, w.Head) {
		// The overseer reviewed this exact head in place of om, which could
		// not return a verdict (gt-g8t3m): record that and do not pay for om
		// again. The gate above still ran.
		verdict = Verdict{Verdict: VerdictOverseerPrefix + shortSHA(w.Head)}
		l.logf("%s: %s, om skipped (overseer-reviewed)", w.BeadID, stageTimes(gateRes, 0, false))
	} else {
		reviewStart := time.Now()
		l.stage(w, StageOM)
		omCtx, omDone := l.Slow.watch(ctx, l, w, dir, StageOM)
		verdict, reviewErr = l.Reviewer.Review(omCtx, dir, base, merged)
		// One flaky om run must not cost an escalation and a hand review
		// (gt-q241r): an execution error on an ordinary tree is retried once,
		// inside this same om stage. A timeout, an infra error and a verdict
		// that arrived are final, and so is an execution error on a tree past
		// the size bound, which is deterministic and would fail the same way
		// again.
		omRetried := errors.Is(reviewErr, ErrOMExecution) && omCtx.Err() == nil && !l.omDiffPastBound(g, base, merged)
		if omRetried {
			l.logf("%s: om review returned no verdict (%s); retrying once", w.BeadID, reviewErrorReason(reviewErr))
			verdict, reviewErr = l.Reviewer.Review(omCtx, dir, base, merged)
		}
		omDone()
		l.logf("%s: %s", w.BeadID, stageTimes(gateRes, time.Since(reviewStart), omRetried))
	}
	res.Verdict = verdict
	if reviewErr == nil && verdict.Verdict != VerdictApprove && verdict.Verdict != VerdictRequestChanges && verdict.Verdict != VerdictSkipped && !strings.HasPrefix(verdict.Verdict, VerdictOverseerPrefix) {
		reviewErr = fmt.Errorf("reviewer returned no verdict (%q)", verdict.Verdict)
	}
	if reviewErr != nil {
		if ctx.Err() != nil || (!l.ReviewErrorLands && !l.ReviewErrorRejects) {
			return Result{}, &InfraError{Stage: "review", Err: reviewErr}
		}
		if !l.ReviewErrorLands && !l.landsUnreviewed(issue) {
			rej := &Rejection{Kind: RejectReview, Rework: false,
				Reason: l.reviewErrorRejectionReason(wt, base, merged, reviewErr)}
			return Result{}, l.reject(issue, w, rej, nil)
		}
		verdict = Verdict{Verdict: VerdictErrorPrefix + reviewErrorReason(reviewErr)}
		res.Verdict = verdict
		l.logf("%s: WARNING om review did not run (%v); the merged tree is green, so it lands with om_verdict %q", w.BeadID, reviewErr, verdict.Verdict)
	} else if verdict.Verdict == VerdictRequestChanges {
		rej := &Rejection{Kind: RejectReview, Rework: true, Reason: fmt.Sprintf("om requested changes (score %.2f, %d finding(s))", verdict.Score, len(verdict.Findings))}
		return Result{}, l.reject(issue, w, rej, &verdict)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, &InfraError{Stage: "push", Err: err}
	}

	// The rig lands through Forgejo: the om status is posted and the PR merged
	// with head_commit_id, so a candidate that moved after the verdict cannot
	// land on it (design, "The PR, the merge and the forgery check"). The merge
	// is a fast-forward, so the target's tip is the candidate commit the
	// read-back checks.
	//
	// The merge verifies the candidate's status creators first, so it needs the
	// context CI reported: a candidate gate that reported none is a
	// misconfiguration rather than a landing.
	if ciContext == "" {
		return Result{}, &InfraError{Stage: "merge pull request",
			Err: errors.New("the candidate gate reported no status context, so the merge's creator check has nothing to verify")}
	}
	if err := l.Merger.Merge(ctx, MergeRequest{Work: w, Head: merged, Verdict: verdict, GateContext: ciContext}); err != nil {
		var refused *MergeRefusedError
		var forged *ForgedStatusError
		switch {
		case errors.As(err, &forged):
			// Tampering with the gate is nobody's to rework and no retry
			// lifts it: the bead goes to a human, and the worker escalates
			// it (classification reads Rework=false as outRejectedHuman).
			rej := &Rejection{Kind: RejectForgedStatus, Rework: false, Reason: forged.Reason}
			return Result{}, l.reject(issue, w, rej, nil)
		case errors.As(err, &refused):
			rej := &Rejection{Kind: RejectMergeRefused, Rework: false,
				Reason: fmt.Sprintf("Forgejo refused to merge %s into %s (405, not ready to be merged): %s. A required status is missing or red, so a retry cannot converge: check that branch protection's required contexts match the gate workflow's, and that the %s status was accepted. A human decides.",
					w.Candidate(), w.Target, elideMiddle(NoteField(refused.Err.Error()), reviewErrorReasonMax), OMStatusContext)}
			return Result{}, l.reject(issue, w, rej, nil)
		}
		return Result{}, err
	}
	if err := wt.VerifyPushedCommit(remote, w.Target, merged); err != nil {
		return Result{}, &InfraError{Stage: "read-back", Err: fmt.Errorf("%w: %v", ErrReadBack, err)}
	}
	l.logf("%s: landed %s on %s/%s (patch-id %s)", w.BeadID, shortSHA(merged), remote, w.Target, shortSHA(patchID))

	if err := l.record(w, res); err != nil {
		return res, &RecordError{Result: res, Err: err}
	}
	return res, nil
}

const fetchTimeout = 2 * time.Minute

func (l *Lander) fetchTarget(g Repo, target string) error {
	remote := l.remote()
	return g.FetchRefspecWithTimeout(remote, fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", target, remote, target), fetchTimeout)
}

// repairRecord finishes the record of a landing the landings file holds for
// w when its commit is on the target. It returns nil, nil when there is
// nothing to repair. An error reading the file or the target stops Land: a
// landing it cannot rule out must not be landed a second time.
func (l *Lander) repairRecord(g Repo, w Work) (*Result, error) {
	rec, found, err := l.Landings.Find(w.BeadID, w.Head)
	if err != nil {
		return nil, &InfraError{Stage: "read landings file", Err: err}
	}
	if !found {
		return nil, nil
	}
	if err := l.fetchTarget(g, w.Target); err != nil {
		return nil, &InfraError{Stage: "fetch target", Err: err}
	}
	landed, err := g.IsAncestor(rec.LandedCommit, l.remote()+"/"+w.Target)
	if err != nil {
		return nil, &InfraError{Stage: "ancestry of recorded landing", Err: err}
	}
	if !landed {
		// A record whose commit is not on the target (the target was rewound)
		// is history, not proof: land normally.
		return nil, nil
	}
	l.logf("%s: already landed as %s; finishing its record", w.BeadID, shortSHA(rec.LandedCommit))
	res := &Result{LandedCommit: rec.LandedCommit, PatchID: rec.PatchID, Base: rec.Base,
		Verdict: Verdict{Verdict: rec.OMVerdict, Score: rec.OMScore}, RiskPaths: rec.RiskPaths}
	if err := l.recordBead(w, rec); err != nil {
		return res, &RecordError{Result: *res, Err: err}
	}
	return res, nil
}

func (l *Lander) validate(w Work) error {
	switch {
	case l.Repo == "" || l.WorkRoot == "":
		return errors.New("lander: Repo and WorkRoot are required")
	case l.Candidate == nil || l.Merger == nil:
		return errors.New("lander: Candidate and Merger are required; merge_queue.forgejo is mandatory for any rig that lands")
	case l.Reviewer == nil || l.Beads == nil || l.Landings == nil:
		return errors.New("lander: Reviewer, Beads and Landings are required")
	case w.BeadID == "" || w.Branch == "" || w.Head == "" || w.Target == "":
		return fmt.Errorf("%w: work %+v is missing its bead, branch, head or target", ErrNotReady, w)
	case w.Branch == w.Target:
		return fmt.Errorf("%w: branch %s is the target", ErrNotReady, w.Branch)
	}
	return nil
}

// policyRejection applies the work bead's own policy: a non-concrete bead,
// no_merge / review_only / local work and unchecked acceptance criteria never
// land.
func policyRejection(issue *beads.Issue) *Rejection {
	if reason := beads.ConcreteWorkIssueRejectReason(issue); reason != "" {
		return &Rejection{Kind: RejectPolicy, Rework: false, Reason: "not a concrete work bead: " + reason}
	}
	if reason := CloseBlockReason(issue); reason != "" {
		return &Rejection{Kind: RejectPolicy, Rework: false, Reason: reason + " work never lands on the target; a human decides what happens to the branch"}
	}
	if n := beads.HasUncheckedCriteria(issue); n > 0 {
		return &Rejection{Kind: RejectPolicy, Rework: true, Reason: fmt.Sprintf("%d unchecked acceptance criteria", n)}
	}
	return nil
}

// checkHeadPushed asserts <remote>/<branch> — the rig's configured landing
// remote (gt-fn9e6.9), origin by default — carries the declared head, so Land
// never merges a commit the author did not push (gt-sda9).
func (l *Lander) checkHeadPushed(g Repo, w Work) (*Rejection, error) {
	remote := l.remote()
	tip, err := g.PushRemoteBranchTip(remote, w.Branch)
	if err != nil {
		return nil, &InfraError{Stage: "read branch tip", Err: err}
	}
	if tip == "" {
		return &Rejection{Kind: RejectNotPushed, Rework: true, Reason: fmt.Sprintf("%s has no branch %s, so it cannot carry head %s", remote, w.Branch, shortSHA(w.Head))}, nil
	}
	if err := g.FetchRefspecWithTimeout(remote, fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", w.Branch, remote, w.Branch), fetchTimeout); err != nil {
		return nil, &InfraError{Stage: "fetch branch", Err: err}
	}
	if _, err := g.Rev(w.Head + "^{commit}"); err != nil {
		return &Rejection{Kind: RejectNotPushed, Rework: true, Reason: fmt.Sprintf("head %s is not on %s/%s (tip %s)", shortSHA(w.Head), remote, w.Branch, shortSHA(tip))}, nil
	}
	ok, err := g.IsAncestor(w.Head, remote+"/"+w.Branch)
	if err != nil {
		return nil, &InfraError{Stage: "ancestry", Err: err}
	}
	if !ok {
		return &Rejection{Kind: RejectNotPushed, Rework: true, Reason: fmt.Sprintf("head %s is not reachable from %s/%s (tip %s)", shortSHA(w.Head), remote, w.Branch, shortSHA(tip))}, nil
	}
	return nil, nil
}

// landingIDKey carries a landing's ID on the context Land hands its gate.
type landingIDKey struct{}

// LandingID is the ID of the landing whose gate or rerun ctx belongs to,
// unique per landing (the name of its land-* directory under WorkRoot): what
// keeps two landings' logs apart now that every landing checks out at the
// same path. "" outside a landing.
func LandingID(ctx context.Context) string {
	id, _ := ctx.Value(landingIDKey{}).(string)
	return id
}

// WithLandingID returns ctx carrying id as its LandingID: what Land does for
// its gate, for a caller (or a test) that runs a gate outside Land.
func WithLandingID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, landingIDKey{}, id)
}

// landWorktreeName is the one worktree directory every landing of a rig uses,
// under WorkRoot. A fixed path is what lets Go's build cache hit across
// landings: its keys include each package's directory, so a new random path
// per landing recompiled every package and wrote a whole new cache
// generation each time (gt-2ycne.2, ~470 GB). The worker lands one bead at a
// time per rig, so one path per WorkRoot is never shared.
const landWorktreeName = "wt"

// addWorktree adds a detached worktree at base at WorkRoot/wt, clearing one a
// dead landing left there, and returns it with this landing's ID.
func (l *Lander) addWorktree(g Repo, base string) (dir, id string, cleanup func(), err error) {
	if err := os.MkdirAll(l.WorkRoot, 0o700); err != nil {
		return "", "", nil, err
	}
	if err := os.Chmod(l.WorkRoot, 0o700); err != nil {
		return "", "", nil, err
	}
	// The land-* directory is only the landing's identity now: unique, and
	// the name its logs go under.
	parent, err := os.MkdirTemp(l.WorkRoot, "land-*")
	if err != nil {
		return "", "", nil, err
	}
	dir = filepath.Join(l.WorkRoot, landWorktreeName)
	removeWorktree := func() {
		_ = g.WorktreeRemove(dir, true)
		_ = os.RemoveAll(dir)
		_ = g.WorktreePrune()
	}
	if _, statErr := os.Lstat(dir); statErr == nil {
		removeWorktree()
	}
	if err := g.WorktreeAddDetached(dir, base); err != nil {
		_ = os.RemoveAll(parent)
		return "", "", nil, err
	}
	return dir, filepath.Base(parent), func() {
		removeWorktree()
		_ = os.RemoveAll(parent)
	}, nil
}

// mergeWork merges the declared head into the worktree at base: --no-ff, or
// a squash when the range still holds auto-save commits so none reach the
// target (the refinery's stackMerge behavior).
func mergeWork(wt Repo, w Work, base string) (string, *Rejection, error) {
	msg := fmt.Sprintf("land: %s (%s) onto %s (%s)\n\nWork: %s", w.Branch, shortSHA(w.Head), w.Target, shortSHA(base), w.BeadID)
	hasAutoSave, err := hasAutoSaveCommits(wt, base, w.Head)
	if err != nil {
		return "", nil, &InfraError{Stage: "inspect range", Err: err}
	}
	if hasAutoSave {
		err = wt.MergeSquash(w.Head, msg)
	} else {
		err = wt.MergeNoFF(w.Head, msg)
	}
	if err != nil {
		files, filesErr := wt.GetConflictingFiles()
		_ = wt.AbortMerge()
		if filesErr == nil && len(files) > 0 {
			return "", &Rejection{Kind: RejectConflict, Rework: true, Conflicting: files,
				Reason: fmt.Sprintf("merging %s into %s conflicts in %d file(s); rebase onto %s and resolve", shortSHA(w.Head), w.Target, len(files), w.Target)}, nil
		}
		return "", nil, &InfraError{Stage: "merge", Err: err}
	}
	merged, err := wt.Rev("HEAD")
	if err != nil {
		return "", nil, &InfraError{Stage: "merge", Err: err}
	}
	return merged, nil, nil
}

// hasAutoSaveCommits reports whether base..head, the commits the landing
// brings onto the target, holds a machine-generated auto-save commit.
func hasAutoSaveCommits(g Repo, base, head string) (bool, error) {
	msgs, err := g.CommitMessages(base, head)
	if err != nil {
		return false, fmt.Errorf("listing commits %s..%s: %w", shortSHA(base), shortSHA(head), err)
	}
	for _, m := range msgs {
		subject, _, _ := strings.Cut(m.Message, "\n")
		if checkpoint.IsAutoSaveSubject(subject) {
			return true, nil
		}
	}
	return false, nil
}

// landsUnreviewed reports whether issue carries one of
// ReviewErrorLandsLabels.
func (l *Lander) landsUnreviewed(issue *beads.Issue) bool {
	for _, label := range l.ReviewErrorLandsLabels {
		if beads.HasLabel(issue, label) {
			return true
		}
	}
	return false
}

// stageTimes is one line naming each gate stage's wall time and, when om
// ran, its own, under the host load the stages ran on: "stages: lint 18s,
// gate 92s, om 2m31s (load1 7.4)". A host that reports no load leaves the
// line in its old form (gt-a025o).
func stageTimes(g GateResult, om time.Duration, omRetried bool) string {
	return stageTimesUnderLoad(g, om, omRetried, hostLoad1)
}

// stageTimesUnderLoad is stageTimes with the load reading passed in, so a test
// pins the sample without swapping a package variable.
func stageTimesUnderLoad(g GateResult, om time.Duration, omRetried bool, load1 func() (float64, bool)) string {
	parts := make([]string, 0, len(g.Steps)+1)
	for _, st := range g.Steps {
		t := st.Elapsed.Round(time.Second).String()
		if st.TimedOut {
			t += " (timed out)"
		} else if st.ExitCode != 0 {
			t += fmt.Sprintf(" (exit %d)", st.ExitCode)
		}
		parts = append(parts, st.Name+" "+t)
	}
	if om > 0 {
		t := om.Round(time.Second).String()
		if omRetried {
			t += " (retried)"
		}
		parts = append(parts, "om "+t)
	}
	line := "stages: " + strings.Join(parts, ", ")
	if sample, ok := load1(); ok {
		line += fmt.Sprintf(" (load1 %.1f)", sample)
	}
	return line
}

// reviewErrorReasonMax bounds the review error recorded in the rejection note
// and the verdict string.
const reviewErrorReasonMax = 200

// reviewErrorReason is err on one bounded line, for the recorded verdict. The
// bound keeps both ends, because an om error opens with the command that failed
// and closes with the backend's own stderr while the middle is boilerplate: the
// head-only cut this replaced kept a config warning and dropped the cause
// (gt-hhid7).
func reviewErrorReason(err error) string {
	return elideMiddle(NoteField(err.Error()), reviewErrorReasonMax)
}

// elideMiddle shortens s to at most max runes by dropping from the middle,
// marking the drop with an ellipsis. Counted in runes, not bytes, so a cut
// never splits a multi-byte character.
func elideMiddle(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	const marker = " … "
	head := (max - len([]rune(marker))) / 2
	tail := max - len([]rune(marker)) - head
	return string(r[:head]) + marker + string(r[len(r)-tail:])
}

// DefaultOMDiffTooLargeLines is the changed-line bound past which an om
// execution error is read as the diff being too large for om. Ordinary landings
// are a small fraction of it, so the phrase stays rare and means what it says
// (gt-hhid7).
const DefaultOMDiffTooLargeLines = 5000

// omDiffScanLimit bounds the commit walk that totals the merged tree's changed
// lines. A landing branch is a handful of commits (a squashed branch is one),
// so the walk is never cut short on a real branch.
const omDiffScanLimit = 1000

// omDiffTooLargeLines is the configured bound, or the default when unset.
func (l *Lander) omDiffTooLargeLines() int {
	if l.OMDiffTooLargeLines > 0 {
		return l.OMDiffTooLargeLines
	}
	return DefaultOMDiffTooLargeLines
}

// omDiffPastBound reports whether the merged tree changes more lines than the om
// size bound, so an execution error on it is deterministic and a retry would
// only buy the same failure (gt-q241r). A size the history could not be read for
// reports false: not knowing is not evidence the diff is too large.
func (l *Lander) omDiffPastBound(g Repo, base, merged string) bool {
	lines, ok := mergedDiffLines(g, base, merged)
	return ok && lines > l.omDiffTooLargeLines()
}

// reviewErrorRejectionReason is the RejectReview reason for a green merged tree
// on which om returned no verdict. An execution error on a tree past the stated
// bound names the size, because the reader's next move — an overseer review,
// not a rework — differs from a bare execution error's (gt-hhid7).
func (l *Lander) reviewErrorRejectionReason(g Repo, base, merged string, reviewErr error) string {
	const preface = "om review returned no verdict on a green merged tree, so it does not land unreviewed: "
	if !errors.Is(reviewErr, ErrOMExecution) {
		return preface + reviewErrorReason(reviewErr)
	}
	lines, ok := mergedDiffLines(g, base, merged)
	bound := l.omDiffTooLargeLines()
	if !ok || lines <= bound {
		return preface + reviewErrorReason(reviewErr)
	}
	return fmt.Sprintf("diff too large for om; overseer review needed: the merged tree changes %d lines, past the %d-line bound, and om exited with an execution error: %s",
		lines, bound, reviewErrorReason(reviewErr))
}

// mergedDiffLines totals the lines base..head changed (added plus removed), the
// size the om bound is stated in. ok is false when the history could not be
// read; the caller then reports the bare execution error rather than guess.
func mergedDiffLines(g Repo, base, head string) (int, bool) {
	stats, err := g.CommitLineStatsInRange(base+".."+head, omDiffScanLimit)
	if err != nil {
		return 0, false
	}
	total := 0
	for _, s := range stats {
		total += s.Added + s.Removed
	}
	return total, true
}

// MaxReworkAttempts is the rejection count that ends the rework loop: a bead
// rejected this many times goes to a human instead of back to a polecat
// (gt-28ibg). Each round costs a polecat session and a gate run, and by the
// third the rounds are repeating a refusal rather than converging on a
// landing, so the loop is the failure a human has to look at. It matches the
// town's other circuit-breaker threshold, config.DefaultRecoveryMaxBeadRespawns.
const MaxReworkAttempts = 3

// reject writes rej to the work bead and returns it.
func (l *Lander) reject(issue *beads.Issue, w Work, rej *Rejection, verdict *Verdict) error {
	attempt := CountRejections(issue.Notes) + 1
	// The attempt ceiling (gt-28ibg): the rejection count decides, before the
	// note is written, whether this round is the last one a polecat gets.
	capped := rej.Rework && attempt >= MaxReworkAttempts
	reason := rej.Reason
	if capped {
		reason = fmt.Sprintf("%s (attempt %d of %d: the loop is escalated, not reworked again)",
			reason, attempt, MaxReworkAttempts)
	}
	note := RejectionNote{
		Attempt:     attempt,
		Kind:        string(rej.Kind),
		Reason:      reason,
		Branch:      w.Branch,
		Target:      w.Target,
		MR:          w.BeadID,
		Head:        w.Head,
		Conflicting: rej.Conflicting,
		GateTail:    rej.GateTail,
	}
	if verdict != nil {
		rej.Findings = verdict.Findings
		rej.ReviewSummary = verdict.Summary
		rej.ReviewScore = verdict.Score
		note.Findings = verdict.Findings
		note.Receipt = &Receipt{Score: verdict.Score}
	}
	l.logf("%s: rejected (%s): %s", w.BeadID, rej.Kind, reason)
	if err := l.Beads.AppendNotes(w.BeadID, FormatRejectionNote(note)); err != nil {
		rej.RecordErr = fmt.Errorf("appending the rejection note: %w", err)
		return rej
	}
	// The gate can run for an hour. A bead that changed hands meanwhile (no
	// longer ready, closed, or claimed by someone else) keeps the note but is
	// not reopened or unassigned: that would take work from whoever holds it.
	now, err := l.Beads.Show(w.BeadID)
	if err != nil {
		rej.RecordErr = fmt.Errorf("re-reading the bead before reopening it: %w", err)
		return rej
	}
	if !beads.HasLabel(now, LabelReadyToLand) || beads.IssueStatus(strings.TrimSpace(now.Status)).IsTerminal() || now.Assignee != issue.Assignee {
		rej.RecordErr = fmt.Errorf("%s changed during the landing (status %s, assignee %q, ready=%v); the rejection is noted and the bead left as it is",
			w.BeadID, now.Status, now.Assignee, beads.HasLabel(now, LabelReadyToLand))
		return rej
	}
	// The cap's other half: the landing worker routes the outcome by Rework,
	// and its human branch is the one that escalates. Writing gt:needs-human
	// without flipping this would leave a capped bead labeled for a human with
	// nobody told (gt-28ibg).
	if capped {
		rej.Rework = false
		rej.Reason = reason
	}
	label := LabelRework
	if !rej.Rework {
		label = LabelNeedsHuman
	}
	// The two refusal labels are exclusive. A bead can move between them — a
	// resubmission rejected for a different reason, or one that hits the
	// attempt cap — and carrying both would leave the record claiming the
	// author and a human own it at once.
	other := LabelNeedsHuman
	if label == LabelNeedsHuman {
		other = LabelRework
	}
	remove := []string{LabelReadyToLand, other}
	open, unassigned := string(beads.StatusOpen), ""
	if err := l.Beads.Update(w.BeadID, beads.UpdateOptions{
		Status:       &open,
		Assignee:     &unassigned,
		AddLabels:    []string{label},
		RemoveLabels: remove,
		// The author's claim is over: gt done handed the work to the landing
		// worker, and a rejection hands it back to dispatch. The re-read
		// above established nobody else took it since.
		Force: true,
	}); err != nil {
		rej.RecordErr = fmt.Errorf("reopening the bead: %w", err)
	}
	return rej
}

// gateRecord is the gate line of a landing record: the gate's own summary and
// the warnings it printed about the host it ran on, so a slow landing says
// why where it is recorded.
func gateRecord(res Result) string {
	return strings.Join(append([]string{res.Gate.Summary()}, res.Gate.Warnings()...), "; ")
}

// record writes the landing: the rig's landings file first (the record that
// survives any bd failure), then the bead's LANDING RECORD block, then the
// close carrying landed_commit and patch_id.
func (l *Lander) record(w Work, res Result) error {
	route := l.Route
	if route == "" {
		route = "daemon"
	}
	rec := LandingRecord{
		BeadID: w.BeadID, Rig: w.Rig, Branch: w.Branch, Head: w.Head, Target: w.Target, Base: res.Base,
		LandedCommit: res.LandedCommit, PatchID: res.PatchID,
		GateResult: gateRecord(res), OMVerdict: res.Verdict.Verdict, OMScore: res.Verdict.Score,
		Route: route, LandedAt: l.now().UTC(), RiskPaths: res.RiskPaths,
	}
	if err := l.Landings.Append(rec); err != nil {
		return fmt.Errorf("landings file: %w", err)
	}
	return l.recordBead(w, rec)
}

// recordBead writes the landing onto the work bead: the LANDING RECORD block,
// the ready label off, the overseer-review-wanted label on when the landing
// touched a risk path, and the close carrying landed_commit and patch_id. It
// is idempotent, so a repair after a partial write finishes the same record.
func (l *Lander) recordBead(w Work, rec LandingRecord) error {
	issue, err := l.Beads.Show(w.BeadID)
	if err != nil {
		return fmt.Errorf("reading %s: %w", w.BeadID, err)
	}
	if !strings.Contains(issue.Notes, LandingNoteMarker+"\nlanded_commit: "+rec.LandedCommit) {
		if err := l.Beads.AppendNotes(w.BeadID, rec.NoteBlock()); err != nil {
			return fmt.Errorf("landing record note on %s: %w", w.BeadID, err)
		}
	}
	// The label goes on in the same update that takes the ready label off: a
	// risky landing is one state change, not two a crash can split.
	update := beads.UpdateOptions{}
	if beads.HasLabel(issue, LabelReadyToLand) {
		update.RemoveLabels = []string{LabelReadyToLand}
	}
	if len(rec.RiskPaths) > 0 && !beads.HasLabel(issue, LabelOverseerReviewWanted) {
		update.AddLabels = []string{LabelOverseerReviewWanted}
	}
	if len(update.RemoveLabels) > 0 || len(update.AddLabels) > 0 {
		if err := l.Beads.Update(w.BeadID, update); err != nil {
			return fmt.Errorf("updating %s after landing: %w", w.BeadID, err)
		}
	}
	if !beads.IssueStatus(strings.TrimSpace(issue.Status)).IsTerminal() {
		if err := l.Beads.ForceCloseWithReason(rec.CloseReason(), w.BeadID); err != nil {
			return fmt.Errorf("closing %s: %w", w.BeadID, err)
		}
	}
	// The work is closed, so the workflow that carried it ends here too.
	l.closeAttachedMolecule(w.BeadID)
	return nil
}
