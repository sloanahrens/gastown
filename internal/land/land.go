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
	PushForceWithLease(remote, refspec, branchRef, expectedSHA string) error
	VerifyPushedCommit(remote, branch, commit string) error
}

var _ Repo = (*git.Git)(nil)

// Lander lands work for one rig. It holds no state between calls; the caller
// (the daemon's landing worker, gt-v4ssj.2) runs one Land at a time per rig.
type Lander struct {
	// Repo is a clone of the rig's repository that throwaway worktrees are
	// added from. It is never checked out onto anything by Land.
	Repo string
	// Remote is the remote to fetch from and push to; "" means origin.
	Remote string
	// WorkRoot is a private directory (0700) the throwaway worktrees live in.
	WorkRoot string
	// Route names who landed, for the record ("daemon" when empty).
	Route string

	// Gate runs on the merged tree. The landing worker passes
	// WithSlot(LandGate(tree, rigMergeQueueConfig), townRoot, role): LandGate
	// reads the rig's merge_queue.gate, and WithSlot holds the container-gate
	// slot for the `make test` fallback only. Land does not take the slot
	// itself.
	Gate     Gate
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
	// Rerun reruns only pkgs' tests, once, in the merged tree at dir: the
	// flake policy's rerun (flake.go). nil means a red gate is final.
	Rerun func(ctx context.Context, dir string, pkgs []string) GateResult
	// GateBeads files the flake policy's beads: one per flaky test (or one
	// per package when MinPackageFlakeTests or more of its tests failed
	// together), one per package over the test budget. nil logs them only.
	GateBeads GateBeads
	// Slow reports a gate or om stage that runs past its threshold; nil
	// reports nothing (gt-lcu5p).
	Slow *SlowAlarm
	// Stage, when set, is told each stage as the landing enters it, by the
	// StageGate/StageOM names. It is how the daemon judges a pass by the stage
	// it is running rather than by the pipeline as a whole; the work before
	// the gate has no stage of its own and reports nothing (gt-84gcp).
	Stage func(beadID, stage string)

	afterPush func()                // test seam: runs between the push and the read-back
	openRepo  func(dir string) Repo // test seam: opens git at dir; nil means *git.Git
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
	// Rerun and Flaky are set when the gate was red and the flake policy's
	// rerun of the failed packages passed.
	Rerun *GateResult
	Flaky []Flake
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

// RaceError means the target moved between the merge and the push, so the
// lease refused the push. Nothing was written; land again from the top.
type RaceError struct {
	Target   string
	Expected string
	Actual   string
}

func (e *RaceError) Error() string {
	return fmt.Sprintf("landing lost the race: %s moved from %s to %s during the gate; nothing pushed", e.Target, shortSHA(e.Expected), shortSHA(e.Actual))
}

// ErrLintTimeout is the cause of an *InfraError whose lint stage outlived its
// timeout. The landing worker counts a run of them per bead and escalates
// (gt-j8ade); it is never a rejection.
var ErrLintTimeout = errors.New("lint stage timed out")

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
// slow-landing alarm labels them (gt-lcu5p). Only the two the gate and the
// review run have a timeout of their own; the rest are bounded by the
// landing's overall budget.
const (
	StageGate = "gate"
	StageOM   = "om"
)

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
// target, the merged tree gated while om reviews the same range, a
// --force-with-lease push against the tip the merge was built on, a read-back
// of that tip, then the landings file, the LANDING RECORD block and the close.
//
// A red gate goes through the flake policy (flake.go) before it is a
// rejection.
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

	l.logf("%s: merged %s onto %s/%s (%s) as %s; gating the merged tree, then om review", w.BeadID, shortSHA(w.Head), remote, w.Target, shortSHA(base), shortSHA(merged))
	// The gate's stages (lint, then tests) run in order, and om only after
	// they pass: om is the costly stage, and work that fails lint or tests
	// never pays for it (gt-b5ugw).
	l.stage(w, StageGate)
	gateCtx, gateDone := l.Slow.watch(ctx, l, w, dir, StageGate)
	gateRes := l.Gate.Run(gateCtx, dir)
	gateDone()
	res := Result{LandedCommit: merged, PatchID: patchID, Base: base, Gate: gateRes, RiskPaths: riskPaths}
	if step, ok := gateRes.TimedOutStep(); ok && ctx.Err() == nil {
		l.logf("%s: %s", w.BeadID, stageTimes(gateRes, 0, false))
		if step.Name == "lint" {
			// A slow lint is almost always one waiting on golangci-lint's
			// module lock behind another run: not a verdict on the tree. The
			// next pass retries it (gt-b5ugw review).
			return Result{}, &InfraError{Stage: "gate", Err: fmt.Errorf("%w: %s did not finish within its %s timeout; nothing was judged", ErrLintTimeout, step.Command, step.Timeout)}
		}
		// A test stage over its bound may be a hang in the work or a loaded
		// host; the author cannot tell which by editing, so it goes to a
		// human rather than back as rework (gt-b5ugw review).
		rej := &Rejection{Kind: RejectTimeout, Rework: false, GateTail: gateRes.FailureTail(),
			Reason: fmt.Sprintf("gate stage %s (%s) did not finish within its %s timeout on the merged tree", step.Name, step.Command, step.Timeout)}
		return Result{}, l.reject(issue, w, rej, nil)
	}
	if gateRes.Err != nil {
		return Result{}, &InfraError{Stage: "gate", Err: gateRes.Err}
	}
	if !gateRes.Passed {
		fv, err := l.applyFlakePolicy(ctx, dir, w, merged, gateRes)
		if err != nil {
			return Result{}, err
		}
		if len(fv.flakes) == 0 {
			reason, tail := "gate failed on the merged tree: "+gateRes.Summary(), gateRes.FailureTail()
			if fv.rerun != nil {
				reason += "; the rerun of the failed package(s) failed too: " + fv.rerun.Summary()
				tail = fv.rerun.FailureTail()
			}
			if names := gateRes.ShellTierFailures(); len(names) > 0 {
				reason += "; the shell tier failed: " + strings.Join(names, " ")
			}
			l.logf("%s: %s", w.BeadID, stageTimes(gateRes, 0, false))
			rej := &Rejection{Kind: RejectGate, Rework: true, Reason: reason, GateTail: tail}
			return Result{}, l.reject(issue, w, rej, nil)
		}
		res.Rerun, res.Flaky = fv.rerun, fv.flakes
		l.logf("%s: the failed package(s) passed their rerun; landing with %d flake(s) filed", w.BeadID, len(fv.flakes))
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

	if err := wt.PushForceWithLease(remote, "HEAD:refs/heads/"+w.Target, "refs/heads/"+w.Target, base); err != nil {
		tip, tipErr := wt.PushRemoteBranchTip(remote, w.Target)
		if tipErr == nil && tip != "" && tip != base {
			return Result{}, &RaceError{Target: w.Target, Expected: base, Actual: tip}
		}
		return Result{}, &InfraError{Stage: "push", Err: err}
	}
	if l.afterPush != nil {
		l.afterPush()
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
	case l.Gate == nil || l.Reviewer == nil || l.Beads == nil || l.Landings == nil:
		return errors.New("lander: Gate, Reviewer, Beads and Landings are required")
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

// checkHeadPushed asserts origin/<branch> carries the declared head, so Land
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
