package land

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/authorkind"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/checkpoint"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land/policy"
)

// Beads is the part of beads.Client Land writes the work bead through
// (ADR 0001: bd only, never the store).
type Beads interface {
	Show(id string) (*beads.Issue, error)
	Update(id string, opts beads.UpdateOptions) error
	ForceCloseWithReason(reason string, ids ...string) error
	AppendNotes(id, note string) error
	// Comments is how reject tells a requeued rejection from one an
	// interrupted pass left: both leave the bead ready with the refusal label
	// gone, and only the requeue comment separates them (gt-en9gs), and how
	// the verdict comment is checked for one this attempt already wrote
	// (gt-uebmq).
	Comments(id string) ([]beads.Comment, error)
	// AddComment writes the review record every landing leaves on its work
	// bead: a rejection's note is not the only trace a review should leave,
	// and an approved or waived landing has none otherwise (gt-uebmq), and the
	// line a repeat of an already-filed finding leaves on its bead (gt-s01pn).
	AddComment(id, text string) error
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
	// UpdateRef points a full ref name at commit, creating or moving it: the
	// local half of refs/landed/<bead> (gt-as9v8).
	UpdateRef(ref, commit string) error
	IsAncestor(ancestor, descendant string) (bool, error)
	// MergeBase is the best common ancestor of a and b, and it fails when the
	// two share none (git merge-base exits 1 with no output): the point a
	// squash of head onto base staged the head's change from, which is what
	// the recovery of an unrecorded squash landing reads the head's own patch
	// against (gt-064oh).
	MergeBase(a, b string) (string, error)
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
	// PatchIDZeroContext is PatchID over a diff with no context lines, so an
	// edit near the change does not alter it (gt-r5uwc).
	PatchIDZeroContext(base, head string) (string, error)
	WorktreeAddDetached(path, ref string) error
	WorktreeRemove(path string, force bool) error
	WorktreePrune() error
	MergeNoFF(branch, message string) error
	MergeSquash(branch, message string) error
	GetConflictingFiles() ([]string, error)
	AbortMerge() error
	// ResetHard puts the worktree's HEAD, index and files back at ref, which
	// is how the landing undoes a probe merge that committed.
	ResetHard(ref string) error
	// Push updates remote's branch to the refspec's source, force-updating it:
	// the merge candidate is the worker's own branch and every retry rebuilds
	// it.
	Push(remote, refspec string, force bool) error
	// PushForceWithLease and ListRemoteRefsWithHashes are how refs/landed/<bead>
	// is moved on the remote: read what it holds, then overwrite exactly that
	// value (gt-as9v8).
	PushForceWithLease(remote, refspec, branchRef, expectedSHA string) error
	ListRemoteRefsWithHashes(remote, prefix string) ([]git.RemoteRef, error)
	VerifyPushedCommit(remote, branch, commit string) error
	// VerifyPushedCommitReachableFromPushTarget is the read-back a shared
	// target needs: another actor's landing may have advanced the tip between
	// the merge and the read, so the landed commit has to be found in the
	// branch's history rather than at its tip (gt-mdet3).
	VerifyPushedCommitReachableFromPushTarget(remote, branch, commit string) error
	// FindCommitMatchingSubject is the newest commit reachable from ref whose
	// subject begins with prefix, which is how a landing's own merge commit
	// is found on the target when no record of it was written. The subject,
	// never the whole message: a revert of a landing quotes its subject, and
	// reading that as the landing records the revert (gt-mdet3, gt-h7trd).
	FindCommitMatchingSubject(ref, prefix string) (string, error)
	// Parents is a commit's parents, for the base of a landing read back out
	// of the target's history.
	Parents(commit string) ([]string, error)
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
	// Reviews, when set, receives one line per om verdict that judged a tree,
	// the log the dashboard's rework table reads. Nil means off (gt-mctxx).
	Reviews *ReviewsFile
	// RoundsDir, when set, holds one rounds-<bead>.json per landing: the
	// findings each om round raised and what became of them, the history
	// gt-3l5qy hands om through --rounds-file (gt-yphcl). Empty means off.
	RoundsDir string
	// roundsWarnMu guards roundsWarned: a bead whose ledger is unreadable is
	// named once in the log, not on every round (gt-yphcl).
	roundsWarnMu sync.Mutex
	roundsWarned map[string]bool
	Out          io.Writer
	Now          func() time.Time
	// Followups, when set, is the beads surface the post-landing follow-up
	// feed files through: a landing om approved at or above the rig's bar gets
	// shaped beads for the minor findings that were not worth blocking on, or a
	// comment on the bead an earlier landing filed for one (gt-s01pn). Nil
	// means off, and off means nothing in the feed runs.
	Followups FollowupBeads
	// FollowupBackpressure is the open om-followup count at which the feed
	// stops filing. Zero means DefaultFollowupBackpressure (gt-4ty22).
	FollowupBackpressure int
	// FollowupAlert, when set, is told every time the backpressure bound keeps
	// the feed from filing, so the findings lost to it are visible. It runs on
	// the feed's own goroutine, which the feed's timeout can abandon; the
	// lander holds no state between landings, so how often the loss reaches a
	// human is the sink's own business (gt-4ty22). Nil reports nothing.
	FollowupAlert func(message string)
	// AcceptedRiskAlert, when set, raises the attention item the overseer sees
	// for a review that was given an accepted risk (gt-tvhg8): key names the
	// work bead and the head reviewed, so a repeat of the same review keeps
	// one item. An error is logged and never fails the landing. Nil reports
	// nothing.
	AcceptedRiskAlert func(key, message string) error
	// BirdOwned, when set, reports whether a bead's assignee is a bird — a
	// crew or operator identity, not a polecat seat — and so keeps the bead
	// when its landing is rejected (gt-h4cvi). Nil reports no bead bird-owned,
	// which is the polecat behavior: every rejection resets the bead to open
	// and unassigned.
	BirdOwned func(assignee string) bool
	// RejectionAlert, when set, raises the attention item a bird-owned
	// rejection leaves for the bead's owner (gt-h4cvi): the key names the bead,
	// so a delivery repeated on a retried pass updates the one item. An error
	// leaves the bead ready for that retry and is reported on the Rejection as
	// RecordErr: the owner's only notice must not fail silently. Nil reports
	// nothing.
	RejectionAlert func(key, message string) error
	// auditTimeout overrides commentTimeout for the accepted-risk writes, so a
	// test can reach the timeout without waiting it out. Zero keeps
	// commentTimeout.
	auditTimeout time.Duration
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
	// own retries) to gt:needs-overseer, instead of stopping as an *InfraError
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
	// authorOf is the test seam for the head's author, for a handle that cannot
	// record authors (gitfake). nil reads the author through the handle.
	authorOf func(w Work) (Identity, error)
	// landingEnv is the test seam that is told the environment the merge handle
	// is opened with, so a unit test can assert the merge commit's identity even
	// when openRepo replaces the handle (gt-vuv2n).
	landingEnv func(env []string)
}

// Result describes a landing.
type Result struct {
	LandedCommit string
	PatchID      string
	// Base is the target tip the merge was built on and the lease expected.
	Base string
	// Gate is the merged tree's gate result, and GateNote is the gate line the
	// record carries in its place for a landing whose gate result was never
	// recorded (gt-mdet3).
	Gate     GateResult
	GateNote string
	Verdict  Verdict
	// RiskPaths are the changed paths the landing touched that matched
	// RiskPathsFile at Base. They label the work bead and are written to the
	// landing record; they never reject or delay the landing (gt-vsct7.4).
	RiskPaths []string
	// RiskPathReviewSkippedBy is the label that left RiskPaths empty (RiskPathsRecorded).
	RiskPathReviewSkippedBy string
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
// already written it to the work bead (a MERGE REJECTION block), swapped the
// ready label for rework (or gt:needs-overseer when Rework is false), and
// released the bead to dispatch by reopening and unassigning it — except a
// bird-owned bead, which keeps its assignee and status and hears of the
// refusal from a rejected:<bead> attention item (BirdOwned, RejectionAlert).
// RecordErr is set when the bead write failed, or when a bird-owned bead's
// attention item could not be delivered — that pass leaves the bead ready so
// the next one raises the item; either way the rejection still stands.
type Rejection struct {
	Kind     RejectionKind
	Reason   string
	GateTail string
	// GateLog is where the rejected gate's whole job log is kept; the note
	// names it so the excerpt can be checked against the full log (gt-wwqx8).
	GateLog     string
	Findings    []Finding
	Conflicting []string
	// ReviewSummary and ReviewScore are om's, when om asked for changes.
	ReviewSummary string
	ReviewScore   float64
	// ReviewVerdict is the verdict om reached, when the refusal came from one:
	// request_changes for om's own refusal, approve for a landing the finding
	// rules refused. A reader told request_changes beside the approve score of
	// 0.90 would be told the opposite of what om said (gt-oi4je).
	ReviewVerdict string
	// Rework is false when only a human can lift the refusal (no_merge), and at
	// MaxReworkAttempts refusals of its kind, whatever that kind is: the loop,
	// not the refusal, is what the ceiling stops (gt-phftg).
	Rework bool
	// TargetTip is the target commit the merge was built on, set on a conflict.
	// The rejection line carries it because a conflicted merge writes no merge
	// line, and the conflict scorecard reads a rejection's target from that
	// line (gt-0nvhe).
	TargetTip string
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
	// TargetHasCommit reports whether a commit is on the merge target. A
	// MergePull whose response was lost — a client-side timeout the server may
	// have completed — is decided by it, because only the target says whether
	// the merge happened. Land sets it from its Repo; a nil one leaves such a
	// timeout reported as the failure it may be (gt-mdet3).
	TargetHasCommit func(commit string) (bool, error)
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
// head. Each Forgejo call carries its own deadline, so one slow call neither
// cancels the next nor turns a merge the server completed into a failure it
// did not (gt-mdet3).
func (m *ForgejoMerger) Merge(parent context.Context, req MergeRequest) error {
	if err := m.postVerdict(parent, req.Head, req.Verdict); err != nil {
		return err
	}
	// The creator check runs after the verdict is posted and before anything
	// is opened or merged: it reads the om / review status the post above
	// created, and a candidate that fails it never reaches the PR.
	if err := m.verifyCreators(parent, req); err != nil {
		return err
	}
	w := req.Work
	head := req.Head
	pr, err := m.pullFor(parent, w, head)
	if err != nil {
		return err
	}
	ctx, cancel := m.callCtx(parent)
	err = m.Client.MergePull(ctx, m.Owner, m.RepoName, pr.Number, forgejo.MergePullRequestOption{
		// The candidate already is the merge of the work onto the target, so a
		// fast-forward lands that exact commit — the one CI gated and om
		// reviewed — as the target's tip. A merge commit made here would test
		// one tree and land another.
		Style:        forgejo.MergeStyleFastForward,
		HeadCommitID: head,
	})
	cancel()
	if err != nil {
		if mergeResponseLost(err, parent) {
			return m.mergeAfterTimeout(parent, req, err)
		}
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
	m.deleteCandidate(parent, w)
	return nil
}

// callCtx bounds one Forgejo call: long enough for a healthy instance, short
// enough that a stuck call does not hold the landing.
func (m *ForgejoMerger) callCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, nonZero(m.CallTimeout, DefaultCandidateCallTimeout))
}

// mergeResponseLost reports whether err is a MergePull that timed out on this
// side, leaving whether the server merged unknown. A timeout that is the
// caller's own cancellation is not one: nothing about the landing is unknown
// there, and the caller is already stopping.
func mergeResponseLost(err error, parent context.Context) bool {
	if parent.Err() != nil {
		return false
	}
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout())
}

// mergeAfterTimeout decides a MergePull whose response was lost: if the target
// carries the candidate the server merged and the landing continues, and
// otherwise the timeout is reported as the failure it is. Without a verifier
// the outcome stays unknown, which is reported as an error rather than read as
// either one.
func (m *ForgejoMerger) mergeAfterTimeout(parent context.Context, req MergeRequest, cause error) error {
	if req.TargetHasCommit == nil {
		return &InfraError{Stage: "merge pull request", Err: cause}
	}
	landed, err := req.TargetHasCommit(req.Head)
	if err != nil {
		return &InfraError{Stage: "verify the merge after a timeout",
			Err: fmt.Errorf("the merge call timed out (%v) and the target could not be read to tell whether it took: %w", cause, err)}
	}
	if !landed {
		return &InfraError{Stage: "merge pull request", Err: cause}
	}
	m.logf("%s: the merge call timed out, but %s carries %s; the merge took", req.Work.BeadID, req.Work.Target, shortSHA(req.Head))
	m.deleteCandidate(parent, req.Work)
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
func (m *ForgejoMerger) deleteCandidate(parent context.Context, w Work) {
	branch := w.Candidate()
	ctx, cancel := m.callCtx(parent)
	defer cancel()
	if err := m.Client.DeleteBranch(ctx, m.Owner, m.RepoName, branch); err != nil {
		m.logf("%s: could not delete %s after the merge: %v", w.BeadID, branch, err)
		return
	}
	m.logf("%s: deleted %s after the merge", w.BeadID, branch)
}

// postVerdict posts om's verdict as the required om / review status on the
// candidate commit.
func (m *ForgejoMerger) postVerdict(parent context.Context, head string, verdict Verdict) error {
	ctx, cancel := m.callCtx(parent)
	defer cancel()
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
func (m *ForgejoMerger) verifyCreators(parent context.Context, req MergeRequest) error {
	ctx, cancel := m.callCtx(parent)
	defer cancel()
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
func (m *ForgejoMerger) pullFor(parent context.Context, w Work, head string) (*forgejo.PullRequest, error) {
	branch := w.Candidate()
	listCtx, listCancel := m.callCtx(parent)
	pulls, err := m.Client.OpenPulls(listCtx, m.Owner, m.RepoName)
	listCancel()
	if err != nil {
		return nil, &InfraError{Stage: "list pull requests", Err: err}
	}
	for i := range pulls {
		if pulls[i].Head.Ref == branch {
			return &pulls[i], nil
		}
	}
	createCtx, createCancel := m.callCtx(parent)
	defer createCancel()
	pr, err := m.Client.CreatePull(createCtx, m.Owner, m.RepoName, forgejo.CreatePullRequestOption{
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

// review runs the landing's reviewer over the merged tree, naming the work bead
// when the reviewer can carry it, so om can echo the id back and a mismatch
// reaches the landing log (gt-pwvv4), and carrying the bead's acceptance text
// so om judges the diff against what the bead asked for (gt-yl7n4) and the
// previous round's findings so a rework round is scored against them
// (gt-cz07b). A reviewer that implements only Review is called exactly as
// before.
func (l *Lander) review(ctx context.Context, in ReviewInput) (Verdict, error) {
	in.Logf = l.logf
	if br, ok := l.Reviewer.(BeadReviewer); ok {
		return br.ReviewBead(ctx, in)
	}
	return l.Reviewer.Review(ctx, in.Dir, in.Base, in.Head)
}

func (l *Lander) open(dir string) Repo {
	if l.openRepo != nil {
		return l.openRepo(dir)
	}
	return CommittingGit(dir)
}

// openLanding opens the merge candidate's worktree through a handle whose every
// call carries env (CommittingEnv), so the merge commit's committer and author
// are set explicitly (gt-dc4y3, gt-vuv2n).
func (l *Lander) openLanding(dir string, env []string) Repo {
	if l.landingEnv != nil {
		l.landingEnv(env)
	}
	if l.openRepo != nil {
		return l.openRepo(dir)
	}
	return git.NewGit(dir).WithEnv(env)
}

// landingCommitEnv is the environment the landing's merge commit is made under:
// the landing committer and the author of the head, the seat that did the
// work (gt-vuv2n). The author is read from the head commit itself, so a head
// whose author is missing or unreadable is an error and no commit is made.
// The landing never falls back to git config.
func (l *Lander) landingCommitEnv(g Repo, w Work) ([]string, error) {
	author, err := l.headAuthor(g, w)
	if err != nil {
		return nil, err
	}
	return CommittingEnv(author)
}

// headAuthor reads the author of the head commit through the handle.
func (l *Lander) headAuthor(g Repo, w Work) (Identity, error) {
	if l.authorOf != nil {
		return l.authorOf(w)
	}
	ar, ok := g.(authorReader)
	if !ok {
		return Identity{}, fmt.Errorf("reading the author: the Git handle %T cannot read commit authors", g)
	}
	name, email, err := ar.CommitIdentity(w.Head)
	if err != nil {
		return Identity{}, fmt.Errorf("reading the author: %w", err)
	}
	return Identity{Name: name, Email: email}, nil
}

// rescoreMargin is how far below the rig's bar a rejection may score, with no
// major or critical finding, and still earn a second om run on the same head
// (gt-cz07b). Reviewer noise is real at this distance: the same head scored
// 0.78 in the daemon's run and 0.80 in the overseer's, so a diff whose true
// score sits this close to the bar is decided by which run happened.
const rescoreMargin = 0.03

// rescoreEpsilon absorbs the float subtraction in the margin test. om's scores
// and a bar are two-decimal values, and 0.8-0.77 is 0.030000000000000027:
// without it a score exactly on the margin reads as outside it.
const rescoreEpsilon = 1e-9

// borderlineRejection reports whether v is a rejection a second om run may
// flip: a request for changes with no major or critical finding that scored
// within rescoreMargin of the rig's bar (gt-cz07b). A major finding is a
// judgment, not noise, so it never earns a rerun, and bar <= 0 means the tree
// named no bar for a score to be near.
func borderlineRejection(v Verdict, bar float64) bool {
	if v.Verdict != VerdictRequestChanges || bar <= 0 || v.Score >= bar {
		return false
	}
	if bar-v.Score > rescoreMargin+rescoreEpsilon {
		return false
	}
	return !hasMajorFinding(v.Findings)
}

// hasMajorFinding reports whether findings carry one om refuses work on: a
// major or critical.
func hasMajorFinding(findings []Finding) bool {
	return blockingSeverity(findings) != ""
}

// BlocksLanding reports whether findings carry a major or critical, the
// severities om refuses work on. It is hasMajorFinding for readers outside
// this package.
func BlocksLanding(findings []Finding) bool {
	return hasMajorFinding(findings)
}

// blockingSeverity names the severity of the finding that refuses the work
// whatever its score: critical when there is one, major otherwise, and "" when
// there is neither (gt-oi4je). Info, or a string this review does not know, is
// not a severity that blocks.
func blockingSeverity(findings []Finding) string {
	blocking := ""
	for _, f := range findings {
		switch omSeverity(f.Severity) {
		case omSeverityCritical:
			return omSeverityCritical
		case omSeverityMajor:
			blocking = omSeverityMajor
		}
	}
	return blocking
}

// maxMinorFindings is how many minor findings an approving review may carry
// into a landing (gt-oi4je). A sixth is the review saying the diff is not
// ready, and one that landed would spend a follow-up bead on each.
const maxMinorFindings = 5

// findingsRejection is the refusal an approving review earns from its findings
// alone, or nil when they let it land. om scores a lone major inside the
// approving band, so the score and the findings are two decisions and this is
// the findings': an approval carries no major or critical finding and no more
// than maxMinorFindings minors (gt-oi4je). A verdict that is not an approval is
// refused or waived elsewhere, and this leaves it alone.
func findingsRejection(v Verdict) *Rejection {
	if !v.Approved() {
		return nil
	}
	if severity := blockingSeverity(v.Findings); severity != "" {
		return &Rejection{Kind: RejectReview, Rework: true,
			Reason: fmt.Sprintf("a %s finding blocks the landing at any score", severity)}
	}
	if minorFindings(v.Findings) > maxMinorFindings {
		return &Rejection{Kind: RejectReview, Rework: true,
			Reason: fmt.Sprintf("more than %d minor findings: fix the most important and resubmit", maxMinorFindings)}
	}
	return nil
}

// minorFindings counts the findings om graded minor, the one severity a
// passing review may carry up to maxMinorFindings of. Info, and a severity
// this review does not know, are not minors and fill no count.
func minorFindings(findings []Finding) int {
	n := 0
	for _, f := range findings {
		if omSeverity(f.Severity) == omSeverityMinor {
			n++
		}
	}
	return n
}

// rescoreBorderline runs om once more over the same head when v is a
// borderline rejection, and returns the verdict the landing stands on: the
// higher-scoring of the two runs, since either is as true as the other. A
// rerun that reaches no verdict leaves the first standing, because a review
// that produced one decided the landing and the rerun is a second opinion
// (gt-cz07b).
//
// It is not a rework attempt. Nothing reaches the bead until the landing is
// decided, so a rerun that stays a rejection records the one refusal the
// landing would have recorded anyway, and the three-attempt cap counts it
// once.
func (l *Lander) rescoreBorderline(ctx context.Context, v Verdict, in ReviewInput) Verdict {
	bar, err := OMThresholdAt(in.Repo, in.Base)
	if err != nil {
		l.logf("%s: could not read the review bar to decide whether to re-score the rejection (%v); the rejection stands", in.BeadID, err)
		return v
	}
	if !borderlineRejection(v, bar) {
		return v
	}
	l.logf("%s: om requested changes at %.2f with no major finding, within %.2f of the %.2f bar; om runs once more on the same head",
		in.BeadID, v.Score, rescoreMargin, bar)
	rerun, err := l.review(ctx, in)
	if err != nil {
		l.logf("%s: the second om run reached no verdict (%s); the first stands at %.2f", in.BeadID, reviewErrorReason(err), v.Score)
		return v
	}
	// A run whose verdict is not one of om's two decisions judged nothing, the
	// way a failure does: it is not a second opinion to weigh, and the first
	// run's stands.
	if rerun.Verdict != VerdictApprove && rerun.Verdict != VerdictRequestChanges {
		l.logf("%s: the second om run returned no verdict (%q); the first stands at %.2f", in.BeadID, rerun.Verdict, v.Score)
		return v
	}
	stands := v
	word := "the first"
	if rerun.Score > v.Score {
		stands, word = rerun, "the second"
	}
	l.logf("%s: the second om run scored %.2f (%s) against %.2f (%s); the landing stands on %s",
		in.BeadID, rerun.Score, rerun.Verdict, v.Score, v.Verdict, word)
	stands.Rescore = &Rescore{First: v.Score, Rerun: rerun.Score}
	return stands
}

// logReview appends the verdict to the reviews log when it is an om decision,
// approve or request_changes. The attempt is the one a rejection would be
// recorded as, so a bead's lines count up with its MERGE REJECTION blocks: for
// a verdict the landing refuses, that is the block's own attempt, ceiling and
// all, so the rework pass can match the line to the block it judges by number
// (gt-k10bq). rejected is whether the landing refuses the work on this verdict,
// which an approval can earn from its findings alone (gt-vu18r). The record
// carries the author's branch and seat kind, and the prompt its bead's stamp
// names, so a per-kind view needs no other file (gt-b9j3o.16), and each finding
// whole, so a follow-up bead filed later is written from the record rather than
// from a verdict nobody kept (gt-3arxj). A failed
// append never holds a landing, but the line it loses is a hole in the rework
// table, so it is also written on the work bead, where the table's reader can
// find it.
func (l *Lander) logReview(issue *beads.Issue, w Work, comments []beads.Comment, repo Repo, base string, v Verdict, rejected bool) {
	if l.Reviews == nil || (v.Verdict != VerdictApprove && v.Verdict != VerdictRequestChanges) {
		return
	}
	// An unreadable bar is logged as 0, which the table names unknown. The bar
	// is the base's, the one the review was held to (gt-uhuw9).
	bar, err := OMThresholdAt(repo, base)
	if err != nil {
		l.logf("%s: could not read the review bar for the reviews log (%v); it is logged as 0", w.BeadID, err)
	}
	attempt := reviewAttempt(issue.Notes)
	// A refused round's block stops counting at the ceiling (gt-ern0y), so the
	// line names that same attempt: the rework pass matches a record to its
	// block by number and skips a record with no block (gt-k10bq). An approval
	// writes no block, so its line is its own round.
	if v.Verdict == VerdictRequestChanges || rejected {
		attempt = attemptAtCeiling(attempt)
	}
	rec := ReviewRecord{BeadID: w.BeadID, Rig: w.Rig, Attempt: attempt,
		Score: v.Score, Verdict: v.Verdict, Rejected: rejected, Agent: w.Agent, Findings: len(v.Findings), Bar: bar, At: l.now().UTC(),
		Branch: w.Branch, AuthorKind: authorkind.Kind(w.Branch), PromptSHA: promptStampSHA(comments, w),
		FindingText: reviewFindingTexts(v.Findings)}
	if err := l.Reviews.Append(rec); err != nil {
		l.logf("%s: could not append to the reviews log (%v); the landing is unaffected", w.BeadID, err)
		l.commentReviewLogFailure(w, rec, err)
	}
}

// logRounds records this round's findings in the landing's rounds ledger, the
// history gt-3l5qy hands om through --rounds-file. The round is counted from
// the same read as the reviews log's attempt but stops at no ceiling: an
// over-ceiling round is still a round, and clamping it would overwrite the
// ledger's record of the round it clamped to (gt-k10bq). A ledger write never
// holds a landing: the record is the landing's history, not its precondition
// (gt-yphcl).
func (l *Lander) logRounds(issue *beads.Issue, w Work, v Verdict) {
	if l.RoundsDir == "" || (v.Verdict != VerdictApprove && v.Verdict != VerdictRequestChanges) {
		return
	}
	ledger := RoundsLedger{OutDir: l.RoundsDir, Bead: w.BeadID, Now: l.now}
	if err := ledger.Append(reviewAttempt(issue.Notes), w.Head, v.Findings); err != nil {
		if errors.Is(err, ErrRoundsCorrupt) && !l.firstRoundsWarning(w.BeadID) {
			return
		}
		l.logf("%s: could not record the round in the rounds ledger (%v); the landing is unaffected, and om reviews without the rounds", w.BeadID, err)
	}
}

// firstRoundsWarning reports whether bead has not been named for an unreadable
// rounds ledger yet, and marks it named. The lander lives across beads, so the
// marks are kept per bead (gt-yphcl).
func (l *Lander) firstRoundsWarning(bead string) bool {
	l.roundsWarnMu.Lock()
	defer l.roundsWarnMu.Unlock()
	if l.roundsWarned == nil {
		l.roundsWarned = map[string]bool{}
	}
	if l.roundsWarned[bead] {
		return false
	}
	l.roundsWarned[bead] = true
	return true
}

// beadComments reads the bead's comments once for the review: the work-item
// file om reads (gt-0sztd) and the prompt stamp the review record carries
// (gt-b9j3o.16). Comments that cannot be read leave both without them; the
// review still judges the diff.
func (l *Lander) beadComments(issue *beads.Issue) []beads.Comment {
	comments, err := l.Beads.Comments(issue.ID)
	if err != nil {
		l.logf("%s: could not read the bead's comments for the work-item file (%v); om reads the brief without them, and the review record carries no prompt", issue.ID, err)
	}
	return comments
}

// roundsText is the bead's rounds ledger as om reads it, or "" when there is no
// ledger to hand over. An unreadable ledger is named once for the bead and the
// review runs without rounds (gt-yphcl, gt-3l5qy).
func (l *Lander) roundsText(bead string) string {
	if l.RoundsDir == "" {
		return ""
	}
	ledger := RoundsLedger{OutDir: l.RoundsDir, Bead: bead, Now: l.now}
	data, err := ledger.Render(DefaultRoundsCap)
	if err != nil {
		if !errors.Is(err, ErrRoundsCorrupt) || l.firstRoundsWarning(bead) {
			l.logf("%s: could not read the rounds ledger (%v); om reviews without rounds", bead, err)
		}
		return ""
	}
	return string(data)
}

func (l *Lander) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// acceptedRisks reads the work bead's comments, once per landing, and returns
// the accepted-risk record the review is given through --context-file
// (AcceptedRisksText) and the entries that record names, for the audit
// (AcceptedRisksUsed, gt-tvhg8). A bead whose comments cannot be read is
// reviewed without them, as one that records none: the review still judges the
// diff.
func (l *Lander) acceptedRisks(id string) (string, []AcceptedRisk) {
	comments, err := l.Beads.Comments(id)
	if err != nil {
		l.logf("%s: could not read the bead's comments (%v); om reviews without accepted risks", id, err)
		return "", nil
	}
	return AcceptedRisksText(comments), AcceptedRisksUsed(comments)
}

// AcceptedRiskUsedMarker opens the landing note a review leaves when it was
// given accepted risks (gt-tvhg8). It is a note, not a comment, so the
// accepted-risk filter never reads it back as a decision.
const AcceptedRiskUsedMarker = "ACCEPTED-RISK USED"

// recordAcceptedRisks leaves the audit of a review that was given accepted
// risks (gt-tvhg8): one landing note on the work bead naming every entry, and
// one attention item through AcceptedRiskAlert. A review given none leaves
// neither. The two writes run apart, so a note that hangs never holds up the
// item, and the pair shares acceptedRiskTimeout, so a slow bead costs a log
// line and never fails the landing.
func (l *Lander) recordAcceptedRisks(w Work, used []AcceptedRisk) {
	if len(used) == 0 {
		return
	}
	note := acceptedRiskNote(w.Head, used)
	writes := make(chan error, 2)
	go func() {
		writes <- wrapErr("the landing note", l.writeAcceptedRiskNote(w.BeadID, note))
	}()
	go func() {
		var alert error
		if l.AcceptedRiskAlert != nil {
			alert = l.AcceptedRiskAlert(acceptedRiskKey(w), acceptedRiskMessage(w, used))
		}
		writes <- wrapErr("the attention item", alert)
	}()
	limit := l.acceptedRiskTimeout()
	timeout := time.After(limit)
	var errs []error
	for range 2 {
		select {
		case err := <-writes:
			errs = append(errs, err)
		case <-timeout:
			l.logf("%s: recording the accepted risks this review used did not finish in %s; the landing is unaffected", w.BeadID, limit)
			return
		}
	}
	if err := errors.Join(errs...); err != nil {
		l.logf("%s: could not record the accepted risks this review used (%v); the landing is unaffected", w.BeadID, err)
	}
}

// writeAcceptedRiskNote appends note to id unless the bead already carries it.
// A review that is recorded again for the same head and the same entries, as a
// landing retried after a failure past the review is, leaves its audit once. A
// bead whose notes cannot be read gets the note anyway: a second copy costs
// less than a missing audit.
func (l *Lander) writeAcceptedRiskNote(id, note string) error {
	if issue, err := l.Beads.Show(id); err == nil && strings.Contains(issue.Notes, note) {
		return nil
	}
	return l.Beads.AppendNotes(id, note)
}

// acceptedRiskTimeout bounds the accepted-risk writes: commentTimeout, unless a
// test has shortened it through auditTimeout.
func (l *Lander) acceptedRiskTimeout() time.Duration {
	if l.auditTimeout > 0 {
		return l.auditTimeout
	}
	return commentTimeout
}

// wrapErr names which write failed, and passes a nil error through unchanged.
func wrapErr(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", what, err)
}

// acceptedRiskNote renders the landing note: the head reviewed, then one line
// per entry naming its comment, author, date and excerpt. Every field is
// collapsed onto its line, and the marker defusing keeps an excerpt from
// opening a record another reader splits on.
func acceptedRiskNote(head string, used []AcceptedRisk) string {
	lines := []string{fmt.Sprintf("%s: %d accepted risk(s) reviewed at %s", AcceptedRiskUsedMarker, len(used), shortSHA(head))}
	for _, u := range used {
		lines = append(lines, fmt.Sprintf("- comment %s by %s on %s: %s",
			NoteField(u.CommentID), NoteField(u.Author), NoteField(u.Date), defuseMarkers(NoteField(u.Excerpt))))
	}
	return strings.Join(lines, "\n")
}

// acceptedRiskKey is the attention item's identity: the work bead and the head
// its review judged, so the overseer sees one item per reviewed head.
func acceptedRiskKey(w Work) string {
	return "accepted-risk:" + w.BeadID + ":" + shortSHA(w.Head)
}

// acceptedRiskMessage is the one-line summary the overseer reads on the item.
func acceptedRiskMessage(w Work, used []AcceptedRisk) string {
	return fmt.Sprintf("om review of %s at %s used %d accepted risk(s); see the bead's ACCEPTED-RISK USED note",
		w.BeadID, shortSHA(w.Head), len(used))
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
	already, err := g.IsAncestor(w.Head, base)
	if err != nil {
		return Result{}, &InfraError{Stage: "ancestry", Err: err}
	}
	// A landing this lander merged but never recorded — the daemon died
	// between the merge and the record, or the MergePull response was lost —
	// is finished, not landed again. The land merge's subject names this
	// branch, head and target, so a search of the target finds it, and the
	// landing is not the author's work to redo.
	//
	// The search runs only where the target already carries this head's work:
	// the head is on the target (a --no-ff merge contains it, so the ancestry
	// check above saw it), the two trees are identical, or a re-merge writes
	// nothing (a squash, which mergeWork makes of a range that still holds
	// auto-save commits, is a single-parent commit that never contains the
	// head, so neither cheap test can see it). Those are the three dead ends
	// the pass would otherwise end on, and each one asks the recovery before
	// it gives up. A landing that reaches none of them is a landing to make,
	// and never walks the target's history for a land merge naming a head
	// nothing merged (gt-064oh). A fourth shape — a retry whose merge
	// conflicts with a later landing that edited the head's own lines — still
	// ends as a conflict rejection with rework (gt-01yt6).
	if already {
		if res, handled, err := l.finishedUnrecordedLanding(g, issue, w); handled {
			return res, err
		}
		// The head is on the target with no land merge of this lander's to
		// account for it: it reached the target by a route that left no
		// landing record (an operator push), which is not the author's to
		// rework (gt-mdet3).
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
	// A bead the review machinery filed records no risk path, so its landing
	// asks for no risk-path review of the review's own output (gt-g349w).
	riskPaths, skippedBy := RiskPathsRecorded(issue, riskPaths)
	if skippedBy != "" {
		l.logf("%s: risk-path review skipped: the review machinery filed this bead (%s); recording no risk path", w.BeadID, skippedBy)
	}
	if same, err := g.TreesIdentical(base, w.Head); err != nil {
		return Result{}, &InfraError{Stage: "compare trees", Err: err}
	} else if same {
		// Identical trees before the merge are the other shape of a lost
		// record rather than an empty range: the squash of a head that forked
		// at the target writes exactly the head's tree, so the target and the
		// head compare equal and the author would be sent to rework work that
		// already landed (gt-064oh). The range checks above already ran; they
		// read the base..head the head's own first pass read and passed, so
		// they do not stand between a lost record and its recovery here.
		if res, handled, err := l.finishedUnrecordedLanding(g, issue, w); handled {
			return res, err
		}
		return Result{}, l.reject(issue, w, &Rejection{Kind: RejectEmpty, Rework: true, Reason: EmptyMergeReason(g, EmptyMerge{
			Target: w.Target, Base: base, Head: w.Head, Stage: "before merge",
			Comparison: fmt.Sprintf("%s/%s and %s have identical trees", remote, w.Target, shortSHA(w.Head)),
		})}, nil)
	}

	// The merge commit's identity is read before the worktree is cut, so an
	// unreadable head author fails the landing with no commit made (gt-vuv2n).
	env, err := l.landingCommitEnv(g, w)
	if err != nil {
		return Result{}, &InfraError{Stage: "head author", Err: fmt.Errorf("head %s: %w", shortSHA(w.Head), err)}
	}
	dir, id, cleanup, err := l.addWorktree(g, base)
	if err != nil {
		return Result{}, &InfraError{Stage: "worktree", Err: err}
	}
	defer cleanup()
	ctx = WithLandingID(ctx, id)
	wt := l.openLanding(dir, env)

	out, err := mergeWork(wt, w, base)
	if err != nil {
		// The last shape of a lost record: a squash whose head the target
		// already carries stages nothing, so mergeWork's commit fails with an
		// empty index and every pass reports the merge as infra over a landing
		// that already took (gt-064oh). mergeWork has aborted the merge, so
		// the worktree is clean whether or not the recovery finds a landing,
		// and the merge's own error is the one to report when it does not.
		// A lookup that fails says nothing about whether the landing exists,
		// so the pass ends as the infra error the worker retries; rejecting
		// here would send the author to rework work that may have landed
		// (gt-xqpmc).
		if res, handled, cerr := l.finishedUnrecordedLanding(g, issue, w); handled {
			return res, cerr
		}
		// No land merge accounts for the head's content on the target, and
		// merging it changes nothing: the work is already there by a route
		// that left no record, so there is nothing for the author to land
		// and nothing for an operator to repair (gt-sq2oe).
		if out.EmptySquash {
			return Result{}, l.reject(issue, w, &Rejection{Kind: RejectEmpty, Rework: true, Reason: EmptyMergeReason(wt, EmptyMerge{
				Target: w.Target, Base: base, Head: w.Head, Stage: "after merge",
				Comparison: fmt.Sprintf("merging %s changes nothing on %s/%s (its changes are already there, and no land merge of this lander's accounts for them)", shortSHA(w.Head), remote, w.Target),
			})}, nil)
		}
		return Result{}, err
	}
	merged, rej := out.Commit, out.Rejection
	if rej != nil {
		// A conflict with the head's own landed squash is the landing meeting
		// itself, not the author's to resolve; with no land merge naming this
		// head on the target the recovery declines and the rejection stands
		// (gt-01yt6).
		if rej.Kind == RejectConflict {
			if res, handled, err := l.finishedUnrecordedLanding(g, issue, w); handled {
				return res, err
			}
		}
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
	res := Result{LandedCommit: merged, PatchID: patchID, Base: base, Gate: gateRes,
		RiskPaths: riskPaths, RiskPathReviewSkippedBy: skippedBy}
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
		rej := &Rejection{Kind: RejectGate, Rework: true, Reason: reason, GateTail: tail, GateLog: candRes.GateLog}
		return Result{}, l.reject(issue, w, rej, nil)
	}
	var (
		verdict   Verdict
		reviewErr error
		comments  []beads.Comment
	)
	// reviewStart opens the window the review leaves on the work bead: it
	// covers the overseer check and the om attempt alike, so a waiver is
	// timed like a review (gt-uebmq).
	reviewStart := time.Now()
	if OverseerReviewed(issue, w.Head) {
		// The overseer reviewed this exact head in place of om, which could
		// not return a verdict (gt-g8t3m): record that and do not pay for om
		// again. The gate above still ran.
		verdict = Verdict{Verdict: VerdictOverseerPrefix + shortSHA(w.Head)}
		l.logf("%s: %s, om skipped (overseer-reviewed)", w.BeadID, stageTimes(gateRes, 0, false))
	} else {
		// A landing that edits the root .om.json changes the bar om judges
		// the head against, so its head waits for the overseer before om runs
		// (gt-93hf4). A waived head skipped this branch above.
		if rej := l.omConfigRejection(g, base, merged, w.Head, w.BeadID); rej != nil {
			return Result{}, l.reject(issue, w, rej, nil)
		}
		l.stage(w, StageOM)
		omCtx, omDone := l.Slow.watch(ctx, l, w, dir, StageOM)
		accepted, used := l.acceptedRisks(w.BeadID)
		comments = l.beadComments(issue)
		in := ReviewInput{Dir: dir, Base: base, Head: merged, Repo: g, BeadID: w.BeadID,
			CriteriaText: CriteriaText(issue), PriorFindings: priorReviewFindings(issue.Notes),
			AcceptedRisksText: accepted,
			BriefText:         BeadBrief(issue.Description, comments), PolicyText: policy.PolicyBlock(),
			Round: reviewAttempt(issue.Notes), RoundsText: l.roundsText(w.BeadID)}
		verdict, reviewErr = l.review(omCtx, in)
		// One flaky om run must not cost an escalation and a hand review
		// (gt-q241r): an execution error on an ordinary tree is retried once,
		// inside this same om stage. A timeout, an infra error and a verdict
		// that arrived are final, and so is an execution error on a tree past
		// the size bound, which is deterministic and would fail the same way
		// again.
		omRetried := errors.Is(reviewErr, ErrOMExecution) && omCtx.Err() == nil && !l.omDiffPastBound(g, base, merged)
		if omRetried {
			l.logf("%s: om review returned no verdict (%s); retrying once", w.BeadID, reviewErrorReason(reviewErr))
			verdict, reviewErr = l.review(omCtx, in)
		}
		if reviewErr == nil {
			verdict = l.rescoreBorderline(omCtx, verdict, in)
		}
		omDone()
		l.recordAcceptedRisks(w, used)
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
		verdict = Verdict{Verdict: VerdictErrorPrefix + reviewErrorReason(reviewErr), Version: verdict.Version}
		res.Verdict = verdict
		l.logf("%s: WARNING om review did not run (%v); the merged tree is green, so it lands with om_verdict %q", w.BeadID, reviewErr, verdict.Verdict)
	}
	// The review is over and this is its verdict, whatever the branches below
	// do with the landing: the work bead gets the one line that ties it to the
	// review, written before either the rejection or the merge (gt-uebmq). The
	// findings decide the approval's fate before the line is written, so an
	// approval they bounce is logged as the rejection it is (gt-vu18r).
	findingsRej := findingsRejection(verdict)
	l.logReview(issue, w, comments, g, base, verdict, findingsRej != nil)
	l.logRounds(issue, w, verdict)
	l.commentVerdict(w, verdict, reviewStart, time.Now())
	if verdict.Verdict == VerdictRequestChanges {
		rej := &Rejection{Kind: RejectReview, Rework: true, Reason: fmt.Sprintf("om requested changes (score %.2f, %d finding(s))", verdict.Score, len(verdict.Findings))}
		return Result{}, l.reject(issue, w, rej, &verdict)
	}
	// An approving verdict is a score and a set of findings, and the findings
	// refuse the landing too: a major inside the approving band would otherwise
	// land on its score alone (gt-oi4je).
	if rej := findingsRej; rej != nil {
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
	if err := l.Merger.Merge(ctx, MergeRequest{Work: w, Head: merged, Verdict: verdict, GateContext: ciContext,
		TargetHasCommit: l.targetHasCommit(g, w.Target)}); err != nil {
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
	if err := wt.VerifyPushedCommitReachableFromPushTarget(remote, w.Target, merged); err != nil {
		return Result{}, &InfraError{Stage: "read-back", Err: fmt.Errorf("%w: %v", ErrReadBack, err)}
	}
	l.logf("%s: landed %s on %s/%s (patch-id %s)", w.BeadID, shortSHA(merged), remote, w.Target, shortSHA(patchID))

	if err := l.record(g, w, res); err != nil {
		return res, &RecordError{Result: res, Err: err}
	}
	// The landing is recorded, so the feed runs after it and cannot change it
	// (gt-s01pn).
	l.fileFollowups(g, base, w, res)
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
	// A repair finishes the same write the landing started, ref first, so a
	// crash after the landings file leaves the ref too (gt-as9v8).
	if err := l.writeLandedRef(g, rec); err != nil {
		return res, &RecordError{Result: *res, Err: err}
	}
	if err := l.recordBead(w, rec); err != nil {
		return res, &RecordError{Result: *res, Err: err}
	}
	return res, nil
}

// landMergeMarker is the literal mergeWork writes at the start of the land
// merge commit's subject for w: the subject up to the base, which no other
// landing's subject carries because it names this branch, head and target.
// Only the subject is matched, so a commit that quotes the marker — a revert
// of the landing — is not the landing (gt-h7trd).
func landMergeMarker(w Work) string {
	return fmt.Sprintf("land: %s (%s) onto %s (", w.Branch, shortSHA(w.Head), w.Target)
}

// finishedUnrecordedLanding is completeUnrecordedLanding consulted at a dead
// end: handled reports that the pass is over, with res and err the values Land
// returns — the landing it finished, its RecordError, or the recovery's own
// InfraError. A lookup that fails is itself an answer, because without it the
// pass cannot tell a lost record from a landing to make (gt-064oh). Both
// dead ends and the post-merge failure consult it, so the guarantee that a
// failed lookup comes back as an *InfraError lives here and not in each
// caller (gt-z0qzo).
func (l *Lander) finishedUnrecordedLanding(g Repo, issue *beads.Issue, w Work) (res Result, handled bool, err error) {
	completed, err := l.completeUnrecordedLanding(g, issue, w)
	if completed != nil {
		return *completed, true, err
	}
	return Result{}, err != nil, asInfraError("recover the unrecorded landing", err)
}

// asInfraError returns err unchanged when it already is, or wraps, an
// *InfraError, and wraps it at stage otherwise; nil stays nil. Land documents
// that a pass with no verdict ends as an *InfraError, and the landing worker
// reads the failing stage off that type, so a recovery that returns some other
// error would be reported without one.
func asInfraError(stage string, err error) error {
	var infra *InfraError
	if err == nil || errors.As(err, &infra) {
		return err
	}
	return &InfraError{Stage: stage, Err: err}
}

// completeUnrecordedLanding finishes a landing this lander merged but never
// recorded: it finds the land merge it made for w's head on the target, writes
// the record and returns the result. It returns nil, nil when the target
// carries no such merge — or carries one whose patch is not this head's change
// — which means the head arrived by a route that left no record of this
// lander's and is not a landing to complete.
//
// The target's history is the whole evidence: om's verdict and the gate's
// result were not kept, so the record says so rather than inventing them.
func (l *Lander) completeUnrecordedLanding(g Repo, issue *beads.Issue, w Work) (*Result, error) {
	remote := l.remote()
	merged, err := g.FindCommitMatchingSubject(remote+"/"+w.Target, landMergeMarker(w))
	if err != nil {
		return nil, &InfraError{Stage: "find the landing on the target", Err: err}
	}
	if merged == "" {
		return nil, nil
	}
	parents, err := g.Parents(merged)
	if err != nil {
		return nil, &InfraError{Stage: "read the landing's base", Err: err}
	}
	if len(parents) == 0 {
		return nil, &InfraError{Stage: "read the landing's base",
			Err: fmt.Errorf("the land merge %s on %s/%s has no parent to base it on", shortSHA(merged), remote, w.Target)}
	}
	base := parents[0]
	patchID, err := g.PatchID(base, merged)
	if err != nil {
		return nil, &InfraError{Stage: "patch-id of the landing", Err: err}
	}
	// The squash check compares changed lines only. A patch-id hashes context
	// lines too, so the ids above, taken over diffs of different bases, differ
	// whenever the target edited a line within three of the head's hunk, and
	// the landing was never finished though its work is on the target
	// (gt-r5uwc). patchID above stays the recorded one.
	//
	// The price is that the check no longer sees where a change sits: the same
	// lines added to or removed from the same file match wherever in it they
	// land. That is enough here because the subject marker has already named
	// the head's bead, so only another head of that bead whose change is
	// identical but for its position could match wrongly (gt-qaelo).
	mergedChange, err := g.PatchIDZeroContext(base, merged)
	if err != nil {
		return nil, &InfraError{Stage: "patch-id of the landing", Err: err}
	}
	// A --no-ff merge holds the head among its ancestors, so the marker and
	// the head agree on which work landed. A squash holds it nowhere, and the
	// marker is then the only thing naming the head: the patch the commit
	// wrote has to be the head's own change, or the subject matched a
	// different head's landing and this head's work is not on the target to
	// record.
	//
	// The head's own change is read from its merge base with the base the
	// landing merged onto, because that is the diff git merge --squash staged
	// (gt-064oh). Reading it from the landing's base instead — diff base..head
	// — also folds in every change the target made between the fork and the
	// landing, reversed: a branch that forked before that base is then read as
	// a patch that is not the head's change, and its landing is never
	// finished.
	if containsHead, err := g.IsAncestor(w.Head, merged); err != nil {
		return nil, &InfraError{Stage: "ancestry of the landing", Err: err}
	} else if !containsHead {
		fork, err := g.MergeBase(base, w.Head)
		if err != nil {
			return nil, &InfraError{Stage: "merge base of the head", Err: err}
		}
		headPatchID, err := g.PatchIDZeroContext(fork, w.Head)
		if err != nil {
			return nil, &InfraError{Stage: "patch-id of the head", Err: err}
		}
		if headPatchID != mergedChange {
			l.logf("%s: %s begins a land merge's subject but its patch is not %s's change; not finishing it",
				w.BeadID, shortSHA(merged), shortSHA(w.Head))
			return nil, nil
		}
	}
	// The risk paths are recomputed rather than carried: the label a landed
	// path puts on the bead is the same read Land does on a live landing.
	riskPaths, err := RiskPaths(g, base, merged)
	if err != nil {
		l.logf("%s: reading risk paths: %v; landing unlabelled", w.BeadID, err)
		riskPaths = nil
	}
	riskPaths, skippedBy := RiskPathsRecorded(issue, riskPaths)
	if skippedBy != "" {
		l.logf("%s: risk-path review skipped: the review machinery filed this bead (%s); recording no risk path", w.BeadID, skippedBy)
	}
	res := &Result{LandedCommit: merged, PatchID: patchID, Base: base,
		Verdict: Verdict{Verdict: VerdictUnrecorded}, RiskPaths: riskPaths,
		RiskPathReviewSkippedBy: skippedBy,
		GateNote:                "not recorded: this landing merged on the target before its pass could record the gate result (gt-mdet3)"}
	l.logf("%s: %s merged %s on %s/%s but its record was never written; finishing the landing", w.BeadID, remote, shortSHA(merged), remote, w.Target)
	if err := l.record(g, w, *res); err != nil {
		return res, &RecordError{Result: *res, Err: err}
	}
	return res, nil
}

// targetHasCommit is the MergeRequest.TargetHasCommit Land hands its merger:
// whether commit is on the target now, which is the question a MergePull whose
// response was lost is answered by.
func (l *Lander) targetHasCommit(g Repo, target string) func(commit string) (bool, error) {
	return func(commit string) (bool, error) {
		if err := l.fetchTarget(g, target); err != nil {
			return false, err
		}
		tip, err := g.Rev(l.remote() + "/" + target)
		if err != nil {
			return false, err
		}
		return g.IsAncestor(commit, tip)
	}
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
	if n := len(beads.UncheckedAcceptanceText(issue)); n > 0 {
		return &Rejection{Kind: RejectPolicy, Rework: true, Reason: fmt.Sprintf("%d unchecked acceptance criteria", n)}
	}
	return nil
}

// omConfigRejection refuses a landing whose merge edits the root .om.json, the
// config om reviews under: its rubric, context, depth and threshold keys set
// the bar om judges the head against, so the head waits for the overseer's
// review of that head (the OVERSEER REVIEWED waiver) before om runs (gt-93hf4).
// Only the root file counts; a nested file of that name is not the review
// config. A diff that cannot be read is refused too: a diff nobody read cannot
// be shown to leave the config alone (fail closed). The git error is logged,
// not written into the refusal, since git's stderr is captured command output.
func (l *Lander) omConfigRejection(g Repo, base, merged, head, beadID string) *Rejection {
	paths, err := g.DiffNameOnly(base, merged)
	if err != nil {
		l.logf("%s: reading the paths this landing changes for the %s check: %v", beadID, omConfigFile, err)
		return omConfigRefusal("the paths this landing changes could not be read, so it cannot be shown to leave "+omConfigFile+" alone", head)
	}
	if !slices.Contains(paths, omConfigFile) {
		return nil
	}
	return omConfigRefusal("this landing edits "+omConfigFile+", the config om reviews under", head)
}

// omConfigRefusal is the policy refusal for a head that waits on the overseer.
// Rework is false: the author's branch is not what is wrong, so no rework
// round is spent, and the bead goes to gt:needs-overseer for the overseer to
// review the head and write the waiver line the reason quotes.
func omConfigRefusal(what, head string) *Rejection {
	return &Rejection{Kind: RejectPolicy, Rework: false, Reason: fmt.Sprintf(
		"%s; the overseer must review head %s before om runs: to waive that review, the overseer writes the note %q and adds the %s label",
		what, head, OverseerReviewedMarker+" "+head, LabelOverseerReviewed)}
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

// mergeOutcome is what mergeWork learned from a merge: the merged commit, or
// the Rejection that stopped it, or, beside an error, EmptySquash for a squash
// that failed because it staged nothing, the head's changes being already on
// the base.
type mergeOutcome struct {
	Commit      string
	Rejection   *Rejection
	EmptySquash bool
}

// mergeWork merges the declared head into the worktree at base: --no-ff, or
// a squash when the range still holds auto-save commits so none reach the
// target (the refinery's stackMerge behavior). A merge that fails ends as an
// InfraError.
func mergeWork(wt Repo, w Work, base string) (mergeOutcome, error) {
	msg := fmt.Sprintf("land: %s (%s) onto %s (%s)\n\nWork: %s", w.Branch, shortSHA(w.Head), w.Target, shortSHA(base), w.BeadID)
	hasAutoSave, err := hasAutoSaveCommits(wt, base, w.Head)
	if err != nil {
		return mergeOutcome{}, &InfraError{Stage: "inspect range", Err: err}
	}
	// Both merge commits are made through wt, which openLanding opened with
	// CommittingEnv: the landing committer and the head's author (gt-vuv2n).
	if hasAutoSave {
		err = wt.MergeSquash(w.Head, msg)
	} else {
		err = wt.MergeNoFF(w.Head, msg)
	}
	if err != nil {
		files, filesErr := wt.GetConflictingFiles()
		_ = wt.AbortMerge()
		if filesErr == nil && len(files) > 0 {
			return mergeOutcome{Rejection: &Rejection{Kind: RejectConflict, Rework: true, Conflicting: files, TargetTip: base,
				Reason: fmt.Sprintf("merging %s into %s conflicts in %d file(s); rebase onto %s and resolve", shortSHA(w.Head), w.Target, len(files), w.Target)}}, nil
		}
		empty := hasAutoSave && filesErr == nil && mergeChangesNothing(wt, w, base, msg)
		return mergeOutcome{EmptySquash: empty}, &InfraError{Stage: "merge", Err: err}
	}
	merged, err := wt.Rev("HEAD")
	if err != nil {
		return mergeOutcome{}, &InfraError{Stage: "merge", Err: err}
	}
	return mergeOutcome{Commit: merged}, nil
}

// mergeChangesNothing answers whether merging the head onto base would change
// the target, after a squash of it failed without a conflict. The question is
// put to git by merging the head without squashing: a conflict-free merge
// whose tree is the base's is the head's work already there, the same
// comparison the "after merge" check makes. Any other outcome — a merge that
// fails too, or one that writes something — leaves the squash's own failure
// to stand. The probe leaves the worktree where it found it: a merge that
// failed is aborted and one that committed is reset to base, so the caller
// reads the tree it had before asking. A reset that fails leaves that
// unknown, and the answer is then no.
func mergeChangesNothing(wt Repo, w Work, base, msg string) bool {
	if wt.MergeNoFF(w.Head, msg) != nil {
		_ = wt.AbortMerge()
		return false
	}
	same, err := wt.TreesIdentical(base, "HEAD")
	if wt.ResetHard(base) != nil {
		return false
	}
	return err == nil && same
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

// MaxReworkAttempts is the number of refusals of one kind that ends the rework
// loop: a bead refused this many times by one kind goes to a human instead of
// back to a polecat (gt-28ibg). Each round costs a polecat session
// and a gate run, and by the third the rounds are repeating a refusal rather
// than converging on a landing, so the loop is the failure a human has to look
// at. A review is one kind; a gate or policy refusal is another and spends no
// review attempt (gt-l3zak), but three of them are still a loop and reach the
// same ceiling (gt-phftg). It matches the town's other circuit-breaker
// threshold, config.DefaultRecoveryMaxBeadRespawns.
const MaxReworkAttempts = 3

// attemptAtCeiling is the number a review refusal past MaxReworkAttempts is
// recorded under. The MERGE REJECTION block's header stops at the ceiling and
// the reviews log names that same number, so the rework pass can match a line
// to the block it judges by number (gt-ern0y, gt-k10bq). The rounds ledger
// keeps its own count and calls nothing here: an over-ceiling round is still a
// round (gt-yphcl).
func attemptAtCeiling(attempt int) int {
	return min(attempt, MaxReworkAttempts)
}

// rejectionAlreadyRecorded reports whether the last MERGE REJECTION block in
// issue's notes is the one rej would write — same head, same kind — left
// unfinished by an earlier pass. A block is unfinished
// when the bead still looks pre-rejection: the pass that wrote it failed
// before reopening the bead, and reopening is what takes the ready label off
// and puts a refusal label on. After a completed rejection the author
// resubmits, which re-adds the ready label but leaves the refusal label
// behind, so the block is history and a new one is written (gt-zqqcr).
//
// A requeue restores the ready label and drops the refusal label (gt-3e1z4),
// leaving the same shape, so the labels alone cannot tell a requeued block from
// an interrupted one. The requeue comment names the attempt it undid: when it
// names this block, the block is history and this rejection is the next attempt
// (gt-en9gs).
//
// The attempt alone does not always settle it: a refusal past the ceiling is
// clamped to MaxReworkAttempts (gt-ern0y), the number a re-queue of the capped
// rejection also names, so a block a pass writes after the re-queue carries the
// same number as the one the re-queue undid. The comment's block count tells
// them apart — a block added since puts the notes above the count it names
// (gt-kwe1l).
func (l *Lander) rejectionAlreadyRecorded(issue *beads.Issue, w Work, rej *Rejection) (bool, error) {
	if w.Head == "" || !beads.HasLabel(issue, LabelReadyToLand) ||
		beads.HasLabel(issue, LabelRework) || HasNeedsPerson(issue) {
		return false, nil
	}
	last, ok := ParseRejectionNote(issue.Notes)
	if !ok || last.Head != w.Head || last.Kind != string(rej.Kind) {
		return false, nil
	}
	comments, err := l.Beads.Comments(w.BeadID)
	if err != nil {
		return false, fmt.Errorf("reading %s's comments: %w", w.BeadID, err)
	}
	if q, ok := NewestRequeue(comments); ok && q.Attempt == last.Attempt {
		// The re-queue undid this block and nothing followed it; a comment that
		// names no count leaves the attempt to decide (gt-kwe1l).
		if q.Blocks == 0 || RejectionBlocks(issue.Notes) <= q.Blocks {
			return false, nil
		}
	}
	return true, nil
}

// reject writes rej to the work bead and returns it. A rejection whose block
// an earlier pass already wrote and then failed to finish is completed, never
// recorded twice: appending a second identical block would burn a rework
// attempt the author never saw, and at MaxReworkAttempts the bead is escalated
// with no rework comment ever posted (gt-zqqcr).
func (l *Lander) reject(issue *beads.Issue, w Work, rej *Rejection, verdict *Verdict) error {
	repeat, err := l.rejectionAlreadyRecorded(issue, w, rej)
	if err != nil {
		rej.RecordErr = err
		return rej
	}
	kind := string(rej.Kind)
	// The number this refusal is on within its kind: the blocks of that kind
	// the bead already carries, plus itself. A block an interrupted pass
	// already wrote is this refusal's own, already counted among them, so a
	// repeat completes it in place rather than counting it twice (gt-zqqcr).
	// A review numbers among reviews, and every other kind among its own, so
	// two gate refusals never write the same header and neither claims a
	// review's number (gt-l3zak).
	attempt := countKind(rejectionKinds(issue.Notes), kind)
	if !repeat {
		attempt++
	}
	// The count stops at the ceiling (gt-ern0y): a bead resubmitted after it
	// is escalated keeps accumulating notes, but there is no attempt four of a
	// three-round budget, so the header and the escalation text below never
	// name a review round above MaxReworkAttempts.
	if kind == string(RejectReview) {
		attempt = attemptAtCeiling(attempt)
	}
	// The attempt ceiling (gt-28ibg): a refusal at MaxReworkAttempts decides,
	// before the note is written, whether this round is the last one a polecat
	// gets, whatever its kind. A gate or policy refusal spends no review
	// attempt (gt-l3zak), but a loop of them is still a loop, so three gate or
	// three policy refusals reach the same ceiling (gt-phftg).
	capped := rej.Rework && attempt >= MaxReworkAttempts
	reason := rej.Reason
	if capped {
		reason = fmt.Sprintf("%s (attempt %d of %d: the loop is escalated, not reworked again)",
			reason, min(attempt, MaxReworkAttempts), MaxReworkAttempts)
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
		GateLog:     rej.GateLog,
	}
	if verdict != nil {
		rej.Findings = verdict.Findings
		rej.ReviewSummary = verdict.Summary
		rej.ReviewScore = verdict.Score
		rej.ReviewVerdict = verdict.Verdict
		note.Findings = verdict.Findings
		note.Receipt = &Receipt{Score: verdict.Score}
	}
	// The target tip rides the rejection line itself: a conflicted merge writes
	// no merge line, so this is the only record of what the conflict was built
	// on (gt-0nvhe).
	line := fmt.Sprintf("%s: rejected (%s): %s", w.BeadID, rej.Kind, reason)
	if rej.TargetTip != "" {
		line += fmt.Sprintf(" (target tip %s)", rej.TargetTip)
	}
	l.logf("%s", line)
	if !repeat {
		if err := l.Beads.AppendNotes(w.BeadID, FormatRejectionNote(note)); err != nil {
			rej.RecordErr = fmt.Errorf("appending the rejection note: %w", err)
			return rej
		}
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
	// and its human branch is the one that escalates. Writing gt:needs-overseer
	// without flipping this would leave a capped bead labeled for a person with
	// nobody told (gt-28ibg).
	if capped {
		rej.Rework = false
		rej.Reason = reason
	}
	label := LabelRework
	if !rej.Rework {
		label = LabelNeedsOverseer
	}
	// The refusal labels are exclusive. A bead can move between them — a
	// resubmission rejected for a different reason, or one that hits the
	// attempt cap — and carrying both would leave the record claiming the
	// author and a person own it at once. A swap clears every person label the
	// bead still carries (gt:needs-human from before the rename included), not
	// only the one it writes (gt-zjel5).
	remove := []string{LabelReadyToLand}
	for _, other := range append([]string{LabelRework}, needsPersonLabels...) {
		if other != label {
			remove = append(remove, other)
		}
	}
	update := beads.UpdateOptions{
		AddLabels:    []string{label},
		RemoveLabels: remove,
		// gt done handed the claim to the landing worker, and the re-read above
		// established nobody else took it since, so the write may change the
		// bead's labels whatever its assignee says.
		Force: true,
	}
	// A bird keeps the bead it claimed: a crew or operator submitted the
	// branch itself, so re-queuing the bead for dispatch would hand the
	// rework to a polecat seat that never wrote it, and the owner would learn
	// of the refusal only by asking (gt-h4cvi). The owner reads it as the
	// attention item raised below instead. A polecat's claim ends at gt done,
	// so its bead goes back to the queue.
	birdOwned := l.BirdOwned != nil && l.BirdOwned(now.Assignee)
	// A bird hears of the refusal only through the attention item, so it is
	// raised BEFORE the write that takes the bead out of the ready queue: once
	// the write lands, nothing runs the rejection again, and an item raised
	// after it could be lost for good. A failed raise leaves the bead ready
	// (its rejection note already stands), so the worker's next pass retries
	// it; the item is keyed by the bead, so the retry updates the one item
	// instead of minting a second (gt-h4cvi).
	if birdOwned {
		if err := l.raiseRejectionAlert(w, rej, now.Assignee, now.Status); err != nil {
			// Keep the claim and surface the delivery failure: the worker
			// backs off and runs the rejection again, which is the owner's
			// only path to hearing of it (gt-h4cvi).
			rej.RecordErr = fmt.Errorf("raising the rejection attention item: %w", err)
			return rej
		}
	}
	if !birdOwned {
		open := string(beads.StatusOpen)
		update.Status = &open
		// A human-only bead goes back to the holder that submitted it, not to
		// dispatch: only a person can produce its branch, and an unassigned one
		// is a bead the next tick slings onto a seat that cannot land it
		// (gt-me800). The crew that holds it reads the rejection and asks the
		// human for a new branch. An overseer-only or operator-only bead is
		// kept with its holder the same way (gt-4rivs).
		if !dispatch.HumanHeld(now.Labels) {
			unassigned := ""
			update.Assignee = &unassigned
		}
	}
	if err := l.Beads.Update(w.BeadID, update); err != nil {
		rej.RecordErr = fmt.Errorf("reopening the bead: %w", err)
		return rej
	}
	// The bead is back with its author or a person, so the submission this
	// refusal answered was the seat's turn end and the attempt its gt done left
	// open is ended here, under the conditions the landing path ends one: the
	// seat, the ready note's stamp and the host's first sight of that head. A
	// refusal that is not a turn end — a bead that changed hands above, or a
	// submission the host never saw — leaves the attempt open, and the pane
	// keeps reading its time as a lower bound (gt-fhjdh).
	if l.Landings != nil {
		l.Landings.closeAttempt(w.BeadID, w.Head, w.seat(), w.Submitted, l.now())
	}
	return rej
}

// raiseRejectionAlert tells the owner that their landing was refused: the
// attention queue is where a bird reads the town's unresolved items, so the
// item names the bead as its key and the owner and refusal in its text
// (gt-h4cvi). The key makes the raise idempotent — a retried pass updates the
// one item — and the delivery error goes back to the caller, which keeps the
// bead ready to retry rather than logging the failure and moving on.
func (l *Lander) raiseRejectionAlert(w Work, rej *Rejection, owner, status string) error {
	if l.RejectionAlert == nil {
		return nil
	}
	msg := fmt.Sprintf("Landing of %s (%s) was rejected (%s): %s; the bead stays with %s (status %s)",
		w.BeadID, w.Branch, rej.Kind, alertField(rej.Reason), owner, strings.TrimSpace(status))
	if summary := alertField(rej.ReviewSummary); summary != "" {
		msg += fmt.Sprintf("; om: %s", summary)
	}
	return l.RejectionAlert("rejected:"+w.BeadID, msg)
}

// rejectionAlertMax bounds the refusal text an attention item carries: the
// queue is a headline the owner reads at a glance, while the whole reason and
// the gate's output stay in the bead's rejection note, which already bounds
// each quoted line (gt-h4cvi).
const rejectionAlertMax = 300

// alertField makes s one line of bounded length for an attention item: a
// refusal reason can be a gate step's whole command line, and the item is read
// in a terminal, so a runaway reason is cut at a rune boundary rather than
// pasted whole.
func alertField(s string) string {
	s = NoteField(s)
	if len(s) <= rejectionAlertMax {
		return s
	}
	cut := rejectionAlertMax
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + " …"
}

// gateRecord is the gate line of a landing record: the gate's own summary and
// the warnings it printed about the host it ran on, so a slow landing says
// why where it is recorded.
func gateRecord(res Result) string {
	if res.GateNote != "" {
		return res.GateNote
	}
	return strings.Join(append([]string{res.Gate.Summary()}, res.Gate.Warnings()...), "; ")
}

// record writes the landing: refs/landed/<bead>, then the rig's landings
// file (the record that survives any bd failure), then the bead's LANDING
// RECORD block, then the close carrying landed_commit and patch_id. The ref
// precedes the file because the file marks the bead done for the
// leftover-branch sweep: a crash between them leaves a kept branch the ref
// answers ancestry for, never a swept branch nothing proves landed
// (gt-as9v8).
func (l *Lander) record(g Repo, w Work, res Result) error {
	route := l.Route
	if route == "" {
		route = "daemon"
	}
	rec := LandingRecord{
		BeadID: w.BeadID, Rig: w.Rig, Branch: w.Branch, Head: w.Head, Target: w.Target, Base: res.Base,
		LandedCommit: res.LandedCommit, PatchID: res.PatchID,
		GateResult: gateRecord(res), OMVerdict: res.Verdict.Verdict, OMScore: res.Verdict.Score,
		Route: route, Agent: w.Agent, LandedAt: l.now().UTC(), RiskPaths: res.RiskPaths,
		RiskPathReviewSkippedBy: res.RiskPathReviewSkippedBy,
	}
	if err := l.writeLandedRef(g, rec); err != nil {
		return err
	}
	if err := l.Landings.AppendSubmitted(rec, w.seat(), w.Submitted); err != nil {
		return fmt.Errorf("landings file: %w", err)
	}
	return l.recordBead(w, rec)
}

// recordBead writes the landing onto the work bead: the LANDING RECORD block,
// the ready label off, the overseer-review-wanted label on when the record
// carries a risk path, and the close carrying landed_commit and patch_id. It
// is idempotent, so a repair after a partial write finishes the same record.
func (l *Lander) recordBead(w Work, rec LandingRecord) error {
	issue, err := l.Beads.Show(w.BeadID)
	if err != nil {
		return fmt.Errorf("reading %s: %w", w.BeadID, err)
	}
	// The note and the label changes go out in one update: one Dolt commit,
	// and a crash cannot leave the record without its label changes or the
	// reverse. A risky landing is one state change, not two.
	update := beads.UpdateOptions{}
	if !strings.Contains(issue.Notes, LandingNoteMarker+"\nlanded_commit: "+rec.LandedCommit) {
		update.AppendNotes = rec.NoteBlock()
	}
	if beads.HasLabel(issue, LabelReadyToLand) {
		update.RemoveLabels = []string{LabelReadyToLand}
	}
	if len(rec.RiskPaths) > 0 && !beads.HasLabel(issue, LabelOverseerReviewWanted) {
		update.AddLabels = []string{LabelOverseerReviewWanted}
	}
	if update.AppendNotes != "" || len(update.RemoveLabels) > 0 || len(update.AddLabels) > 0 {
		if err := l.Beads.Update(w.BeadID, update); err != nil {
			return fmt.Errorf("recording the landing on %s: %w", w.BeadID, err)
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

// LandedRef is the ref a landing leaves for its bead: the head that landed,
// kept after the polecat branch is deleted so a worktree still holding the
// branch stays answerable by ancestry (gt-as9v8).
func LandedRef(beadID string) string {
	return "refs/landed/" + beadID
}

// writeLandedRef points refs/landed/<bead> at the head the landing record
// names, in the rig's own repository and on the remote. refs/landed/ is the
// landing worker's namespace alone, so the local update is unconditional. The
// push is a plain one while the remote has no ref or already holds this head;
// when the remote holds another head (a re-landing of the same bead after a
// revert) the move is logged and made with a lease on the value just read, so
// a ref some other writer moved in between is refused rather than clobbered.
//
// Every failure is returned. record runs this before the landings file and
// the close, so a landing whose ref could not be written stays unrecorded and
// the next pass finishes it (completeUnrecordedLanding), instead of closing
// the bead on a landing nothing can answer by ancestry. A record that names
// no head is a caller bug, not a landing without a ref.
func (l *Lander) writeLandedRef(g Repo, rec LandingRecord) error {
	ref := LandedRef(rec.BeadID)
	if rec.Head == "" {
		return fmt.Errorf("setting %s: the landing record names no head", ref)
	}
	if err := g.UpdateRef(ref, rec.Head); err != nil {
		return fmt.Errorf("setting %s: %w", ref, err)
	}
	remote := l.remote()
	held, err := remoteRefTip(g, remote, ref)
	if err != nil {
		return fmt.Errorf("reading %s on %s: %w", ref, remote, err)
	}
	refspec := rec.Head + ":" + ref
	if held == "" || held == rec.Head {
		if err := g.Push(remote, refspec, false); err != nil {
			return fmt.Errorf("pushing %s to %s: %w", ref, remote, err)
		}
		return nil
	}
	l.logf("%s: repairing %s on %s from %s to %s", rec.BeadID, ref, remote, shortSHA(held), shortSHA(rec.Head))
	if err := g.PushForceWithLease(remote, refspec, ref, held); err != nil {
		return fmt.Errorf("moving %s on %s from %s: %w", ref, remote, shortSHA(held), err)
	}
	return nil
}

// remoteRefTip is the commit remote holds at the full ref name, "" when it
// holds none. ls-remote matches its pattern as a path suffix, so the listing
// is filtered to the exact name.
func remoteRefTip(g Repo, remote, ref string) (string, error) {
	refs, err := g.ListRemoteRefsWithHashes(remote, ref)
	if err != nil {
		return "", err
	}
	for _, r := range refs {
		if r.Name == ref {
			return r.Hash, nil
		}
	}
	return "", nil
}
