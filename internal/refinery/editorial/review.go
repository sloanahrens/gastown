package editorial

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/plugin"
	"github.com/steveyegge/gastown/internal/slot"
)

// PriorFinding is one prior MERGE REJECTION finding carried forward into
// this review, so the reviewer classifies it resolved/unresolved/regressed
// instead of rediscovering it from scratch. The exact provenance (parsing
// the source issue's rejection notes) is refined in om-gate T10; this is
// the wire format `--prior-findings` accepts today.
type PriorFinding struct {
	ID       string `json:"id"`
	Severity string `json:"severity,omitempty"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Title    string `json:"title,omitempty"`
	Attempt  int    `json:"attempt,omitempty"`
}

// ReviewRequest describes one gt mq review invocation.
type ReviewRequest struct {
	// RigDir is the rig root directory: home to the harness manifest and
	// the gate script named by Config.Command.
	RigDir string
	// RepoDir is the git working directory the rehearsal and rubric-file
	// checks run against.
	RepoDir string

	MRID   string
	Worker string
	Rig    string
	Target string // target branch, e.g. "main"
	Branch string // source branch being reviewed

	// RehearsedHead, when set, is reviewed directly instead of rehearsing
	// Branch onto Target first (the CLI's --rehearsed flag).
	RehearsedHead string

	// Landed, when set, reviews a commit that already landed (the CLI's
	// --landed flag) instead of a submitted branch: no rehearsal, no MR
	// bead, and the note is stamped on the landed commit itself. Mutually
	// exclusive with RehearsedHead.
	Landed *LandedRange

	// TimeoutSeconds, when > 0, overrides the rig's .om.json backend
	// timeout for this one review (the CLI's --timeout flag). It is
	// appended to the gate-script args as --timeout, which the gate script
	// forwards to `om review --timeout`, and recorded on the note so the
	// override is auditable. Zero means "use the rig's configured
	// timeout" and passes no flag at all, leaving the default path byte
	// for byte what it was.
	TimeoutSeconds int

	Attempt       int
	PriorFindings []PriorFinding

	// Reroll re-reviews a diff that already carries a recorded verdict,
	// replacing it. False everywhere by default, including every batch
	// member: the gate is an LLM, so re-invoking it on an unchanged diff
	// re-rolls a near-threshold score instead of measuring anything, and a
	// caller free to re-roll can roll a defective diff until it clears the
	// threshold — the fail-open path through the town's headline gate
	// (gt-bveg). A deliberate re-roll is recorded in the note's attempt
	// history either way (see Note.Attempts).
	Reroll bool

	// RubricRetirement marks an MR whose stated purpose is to retire rubric
	// criteria — the MR bead carries RetirementLabel — and lets it through
	// the criterion-deletion guard in Run. False everywhere else, including
	// every batch member: retiring a criterion must be a decision someone
	// took, never one an unattended review made.
	RubricRetirement bool

	// Config is the rig's resolved merge_queue.editorial config.
	Config config.EditorialConfig
}

// ReviewResult is the outcome of one Run call.
type ReviewResult struct {
	Exit    int // 0 approve, 1 request_changes, 2 infra failure
	Note    *Note
	Class   FailureClass // set only when Exit == 2
	Retries int
	Stderr  string

	// Reused reports that Note is the diff's already-recorded verdict,
	// returned without invoking om — see ReviewRequest.Reroll. Only the
	// invocation differs; Exit carries the same meaning either way.
	Reused bool
}

// ExecFunc runs the gate script at path with args in dir and returns its
// captured stderr and exit code. err is set only when the process could not
// be launched or waited on (e.g. binary missing) — a non-zero exit on its
// own is reported via exitCode, not err. Tests substitute a stub standing in
// for om-gate.sh; production uses RunGateScript.
type ExecFunc func(ctx context.Context, path string, args []string, dir string) (stderr string, exitCode int, err error)

// Deps are Run's collaborators, all substitutable for testing.
type Deps struct {
	Git      *git.Git
	Beads    *beads.Beads
	Recorder *plugin.Recorder
	Exec     ExecFunc

	// NotesMu, when set, is locked around the WriteNote+PushNotes tail of
	// Run. A single Deps value (and so a single mutex) shared across
	// concurrent Run calls on the same RepoDir — the batch path's bounded
	// parallelism (om-gate T7) — serializes that tail so two goroutines
	// never race a `git notes add` against a `git push refs/notes/om` on
	// the same ref: the second writer would otherwise silently overwrite
	// the first's note (lost update) or have its push rejected as
	// non-fast-forward, spuriously dropping a batch member. Nil for the
	// single-invocation `gt mq review` CLI path, which has no concurrent
	// sibling to race.
	NotesMu *sync.Mutex
}

// RunGateScript is the production ExecFunc: it execs path with args, capping
// captured stderr so a runaway process cannot exhaust memory.
func RunGateScript(ctx context.Context, path string, args []string, dir string) (string, int, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stderr.String(), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stderr.String(), exitErr.ExitCode(), nil
	}
	// Launch failure (binary missing, permission denied, ctx deadline, ...):
	// not a process exit, so the caller must classify it itself.
	return stderr.String(), -1, err
}

func isExecNotFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}

// verdictJSON is the shape gt mq review reads from --out. Its finding and
// prior_findings shapes track om's internal/verdict package (om-gate T12);
// gt mq review only needs the fields it routes on.
type verdictJSON struct {
	Score    float64   `json:"score"`
	Verdict  string    `json:"verdict"`
	Findings []Finding `json:"findings"`

	// Backend, when om reports it, is copied through onto Note.ResolvedBackend
	// unchanged — see that field's doc comment for why this is a record, not
	// a pin (gt-iqr6). Absent from every om version that predates this, in
	// which case it unmarshals to the zero value and the note simply carries
	// none.
	Backend string `json:"backend,omitempty"`

	PriorFindings *struct {
		Resolved   []string `json:"resolved"`
		Unresolved []string `json:"unresolved"`
		Regressed  []string `json:"regressed"`
	} `json:"prior_findings,omitempty"`
}

// Finding is one om verdict finding, exported so it can travel beyond this
// package onto Note.Findings and, from there, forward into a merge
// rejection's deadWorkerRecoveryRequest.Findings (om-gate T10) — the same
// shape the gate script itself emits.
type Finding struct {
	ID       string `json:"id,omitempty"`
	Severity string `json:"severity,omitempty"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Title    string `json:"title,omitempty"`
}

// reviewMarkerSuffix is the role tail gt slot status, the dashboard's Gate
// panel and plugins/rebuild-gt key on to recognize an om review in flight
// (gt-97cm).
const reviewMarkerSuffix = "om-review"

// reviewMarkerRole names the role an in-flight review of rig holds, e.g.
// "gastown/om-review".
func reviewMarkerRole(rig string) string {
	return rig + "/" + reviewMarkerSuffix
}

// reviewMarkerName keys a review's marker on the work being reviewed — its MR,
// or the commit a landed review is answering for. Two invocations of the same
// review therefore collide on one name and the second is refused, while the
// batch's parallel members (ReviewParallelism) each hold their own.
func reviewMarkerName(req ReviewRequest) string {
	key := req.MRID
	if key == "" && req.Landed != nil {
		key = req.Landed.Commit
	}
	if key == "" {
		key = req.Branch
	}
	if key == "" {
		key = "unkeyed"
	}
	return reviewMarkerSuffix + "-" + key
}

// acquireReviewMarker takes this review's in-flight marker and returns a func
// that releases it, a no-op when RigDir is empty since there is then no town
// for the marker to be visible in.
func acquireReviewMarker(req ReviewRequest) (func(), error) {
	if req.RigDir == "" {
		return func() {}, nil
	}
	h, err := slot.AcquireMarker(filepath.Dir(req.RigDir), reviewMarkerName(req), reviewMarkerRole(req.Rig))
	if err != nil {
		return nil, err
	}
	return func() { _ = h.Release() }, nil
}

// Run rehearses (or accepts an already-rehearsed) head, asserts the harness
// version, invokes the rig's editorial gate script, classifies the outcome,
// retries once for transient classes, and on a verdict writes the note and
// receipt (and, on an approve carrying major findings, one follow-up bead
// per finding — DECISION 8: approval never dissolves a finding).
func Run(ctx context.Context, req ReviewRequest, deps Deps) ReviewResult {
	// Acquired before the manifest load so a version-assert refusal still
	// shows its hold, and released on every exit path below, panics included
	// (gt-97cm).
	release, err := acquireReviewMarker(req)
	if err != nil {
		// A duplicate of a running review is refused as such; any other way the
		// marker could not be taken (an unwritable lock directory) is Tooling,
		// so the underlying error is not reported as a review in flight.
		class := Tooling
		var held *slot.MarkerHeldError
		if errors.As(err, &held) {
			class = ReviewInFlight
		}
		return failureResult(deps, req, class, err.Error(), 0)
	}
	defer release()

	cfg := req.Config.WithDefaults()

	manifest, err := LoadManifest(req.RigDir)
	if err != nil {
		return failureResult(deps, req, BinaryMissing, err.Error(), 0)
	}

	if req.Landed != nil && req.RehearsedHead != "" {
		return failureResult(deps, req, ConfigError, "--rehearsed and --landed are mutually exclusive: a landed review derives its range from the commit graph", 0)
	}

	head := req.RehearsedHead
	mergeBase := ""
	switch {
	case req.Landed != nil:
		head = req.Landed.Head
		mergeBase = req.Landed.Base
	case head == "":
		rehearsed, err := Rehearse(deps.Git, req.Target, req.Branch)
		if err != nil {
			return failureResult(deps, req, Tooling, fmt.Sprintf("rehearsal failed: %v", err), 0)
		}
		head = rehearsed
	default:
		// The CLI's --rehearsed flag accepts a ref NAME (e.g. a temp branch),
		// not necessarily a sha. Resolve it now: everything downstream
		// (Note.HeadSHA, setEditorialReviewedHead's editorial_reviewed_head)
		// is documented and consumed as an immutable commit sha, and a ref
		// name recorded there stops resolving to the reviewed commit the
		// moment that branch moves or is deleted.
		resolved, err := deps.Git.Rev(head)
		if err != nil {
			return failureResult(deps, req, Tooling, fmt.Sprintf("resolve rehearsed head %q: %v", head, err), 0)
		}
		head = resolved
	}

	if mergeBase == "" {
		mergeBase, err = deps.Git.MergeBase("origin/"+req.Target, head)
		if err != nil {
			return failureResult(deps, req, Tooling, fmt.Sprintf("merge-base: %v", err), 0)
		}
	}

	// Whether this diff touches the deployed rubric file has to be known
	// before AssertVersion runs, not just before the criterion guard below:
	// AssertVersion's ordinary path hashes the rubric off req.RepoDir's
	// working tree, which the single-review CLI path has already checked out
	// onto the reviewed head — the branch's own rubric, if the branch
	// touched it. Asserting against that would let a branch grade itself
	// under whatever rubric it proposes, so a touch routes the version
	// assertion (and, below, the gate script's own cwd) onto the target's
	// committed rubric instead (gt-7bvf).
	rubricTouched, rubricRel, err := RubricTouched(deps.Git, req.RepoDir, manifest.Rubric.Path, mergeBase, head)
	if err != nil {
		return failureResult(deps, req, Tooling, fmt.Sprintf("rubric touch check: %v", err), 0)
	}

	reviewDir := req.RepoDir
	if rubricTouched {
		// The pre-change rubric has to be read from a ref that does NOT
		// already carry this diff. For an ordinary (non-landed) review,
		// target has not merged the branch yet, so origin/<target> IS the
		// pre-change state. A landed/retro review is different: the landed
		// commit is already on origin/<target>'s first-parent chain, so that
		// ref carries the very rubric this diff introduced — reading it there
		// would grade the change under its own rules, exactly the
		// self-grading this gate exists to prevent. mergeBase (the landed
		// range's Base, the merge point before this diff landed) is the
		// pre-change ref in that case instead (gt-7bvf).
		preChangeRef := "origin/" + req.Target
		preChangeDesc := preChangeRef
		if req.Landed != nil {
			preChangeRef = mergeBase
			preChangeDesc = fmt.Sprintf("pre-landing base %s", mergeBase)
		}
		targetRubric, err := deps.Git.ShowFile(preChangeRef, rubricRel)
		if err != nil {
			return failureResult(deps, req, Tooling, fmt.Sprintf("read pre-change rubric %s:%s: %v", preChangeDesc, rubricRel, err), 0)
		}
		if err := AssertVersionWithRubric(manifest, cfg, req.RigDir, targetRubric); err != nil {
			return failureResult(deps, req, classOf(err, VersionMismatch), err.Error(), 0)
		}
		// The gate script must never read the rubric off a checkout of the
		// reviewed head: om-gate.sh runs `om review` at its cwd, so a diff
		// that changes .om.json would otherwise be scored against its own
		// proposed criteria. A fresh detached worktree of preChangeRef
		// carries the deployed (pre-change) rubric regardless of what
		// req.RepoDir has checked out, and (being a full worktree, not a
		// shallow one) still resolves --base/--head against mergeBase/head
		// below by sha.
		rehearsal, err := BeginRehearsalAt(deps.Git, preChangeRef, req.Target)
		if err != nil {
			return failureResult(deps, req, Tooling, fmt.Sprintf("rubric review worktree: %v", err), 0)
		}
		defer func() {
			if closeErr := rehearsal.Close(); closeErr != nil {
				_, _ = fmt.Fprintf(os.Stderr, "[editorial] warning: %v\n", closeErr)
			}
		}()
		reviewDir = rehearsal.dir
	} else if err := AssertVersion(manifest, cfg, req.RigDir, req.RepoDir); err != nil {
		return failureResult(deps, req, classOf(err, VersionMismatch), err.Error(), 0)
	}

	// targetTip is target's own tip at review time — see Note.ReviewedTargetTip
	// for why this must be resolved separately from mergeBase. A landed review
	// has no "since review" window to measure (the review IS the after-the-fact
	// look at what landed), so it leaves this unset.
	targetTip := ""
	if req.Landed == nil {
		targetTip, err = deps.Git.Rev("origin/" + req.Target)
		if err != nil {
			return failureResult(deps, req, Tooling, fmt.Sprintf("resolve origin/%s: %v", req.Target, err), 0)
		}
	}
	// The diff's identity, and so the key the verdict is recorded under. A
	// landed review keys it on the landed diff (ResolveLandedRange.PatchID):
	// that is what the coverage check recomputes from the commit the note sits
	// on, and the branch's own range hashes differently whenever the target
	// moved lines inside the branch's hunk context — a note keyed on the range
	// would answer for nothing.
	patchID := ""
	if req.Landed != nil {
		patchID = req.Landed.PatchID
	} else {
		patchID, err = deps.Git.PatchID(mergeBase, head)
		if err != nil {
			return failureResult(deps, req, Tooling, fmt.Sprintf("patch-id: %v", err), 0)
		}
	}

	// The rubric guard runs before the gate script, not after a verdict: the
	// script reviews the diff against the DEPLOYED rubric, so a branch that
	// swaps a criterion out is scored by the criteria it did not touch and
	// comes back approve. Nothing later re-reads the proposed rubric (gt-2oi0).
	rubricDeltas, err := DiffRubricAt(deps.Git, req.RepoDir, manifest.Rubric.Path, mergeBase, head)
	if err != nil {
		return failureResult(deps, req, Tooling, fmt.Sprintf("rubric check: %v", err), 0)
	}
	if len(rubricDeltas) > 0 && !req.RubricRetirement {
		return failureResult(deps, req, RubricRegression, FormatRubricRegression(rubricDeltas), 0)
	}
	// Recorded on the note only when a retirement was actually exercised, so
	// the note answers "was a criterion given up here?" for every review.
	retiredRubric := len(rubricDeltas) > 0

	// The commit a landed review stamps, and the only one whose note can
	// answer for it: the landed commit. Every other review stamps the head it
	// reviewed.
	noteCommit := head
	var retroReview bool
	var reviewHead string
	if req.Landed != nil {
		noteCommit = req.Landed.Commit
		retroReview = true
		if head != noteCommit {
			reviewHead = head
		}
	}

	// The verdict this review answers for or replaces, read before the
	// decision to review at all: it answers the diff outright when it still
	// applies, and is carried into the new note's attempt history when it
	// does not.
	prior, err := priorVerdict(deps.Git, noteCommit, patchID, manifest.Rubric.SHA256, cfg.MinVersion)
	if err != nil {
		// Not knowing whether this diff was already reviewed is not a state
		// to re-roll from: an unreadable note would be silently replaced.
		return failureResult(deps, req, Tooling, fmt.Sprintf("read recorded verdict for diff %s: %v", patchID, err), 0)
	}
	// A landed review answers from a recorded verdict only when the note it
	// found sits on the landed commit. priorVerdict falls back to a
	// notes-ref-wide patch-id scan (FindVerdictForDiff), and a match found that
	// way was written against a rehearsal head the merge queue discarded:
	// reusing it would report approve while stamping nothing, leaving the
	// landed commit as uncovered as it was — the gap this mode exists to close
	// (gt-ljn8). Falling through re-reviews and writes the note.
	reuseApplies := prior != nil && !req.Reroll &&
		(!retroReview || prior.Commit == noteCommit) &&
		recordedVerdictApplies(&prior.Note, patchID, manifest.Rubric.SHA256, cfg.MinVersion)
	if reuseApplies {
		// A recorded approve can be stale the same way CheckPrecondition's own
		// drift check (targetDriftedMaterially, precondition.go) judges a push
		// stale: target may have moved onto files this MR's own diff also
		// touches since prior.Note.ReviewedTargetTip was recorded, even though
		// the MR's own diff (patchID, just matched above) is unchanged.
		// Reusing such a note here is what livelocked ReasonTargetDriftMaterial
		// (gt-6bsp): the precondition refuses the push on drift and queues the
		// MR on the promise that the next review writes a fresh note, but this
		// reuse path is the only thing standing between "next review" and
		// "same stale note again" — every later cycle found the same
		// ReviewedTargetTip and refused forever. Falling through re-reviews and
		// writes a note with a current ReviewedTargetTip, same as any other
		// reuse-disqualifying condition above.
		drifted, err := targetDriftedMaterially(deps.Git, prior.Note.ReviewedTargetTip, targetTip, mergeBase, head)
		if err != nil {
			return failureResult(deps, req, Tooling, fmt.Sprintf("target drift check: %v", err), 0)
		}
		reuseApplies = !drifted
	}
	if reuseApplies {
		if prior.Note.Verdict == "approve" {
			// A landed review has no MR bead to record it on, and no push to
			// precondition: the note is already on the landed commit, which
			// is where the coverage check reads it.
			if !retroReview {
				// Record the head the note was found on — the reviewed head
				// — not this invocation's rehearsal commit. The push
				// precondition (CheckPrecondition) finds the note by reading
				// editorial_reviewed_head and comparing patch-id; it never
				// requires the reviewed head to be reachable from anything
				// (git notes read by sha regardless of ancestry — see
				// ReadNote), so a found-on head from an earlier rehearsal,
				// or an earlier MR for the same diff, answers just as well
				// as this invocation's own head (gt-bagu).
				if err := ensureEditorialReviewedHead(deps.Beads, req.MRID, prior.Commit); err != nil {
					return failureResult(deps, req, RecordFailed, fmt.Sprintf("update MR bead: %v", err), 0)
				}
			}
			return ReviewResult{Exit: 0, Note: &prior.Note, Reused: true}
		}
		return ReviewResult{Exit: 1, Note: &prior.Note, Reused: true}
	}

	tmpDir, err := os.MkdirTemp("", "gt-mq-review-*")
	if err != nil {
		return failureResult(deps, req, Tooling, fmt.Sprintf("mktemp: %v", err), 0)
	}
	// Defer cleanup but capture raw output first on failure.
	// This ensures the verdict.json and stderr are preserved for diagnosis.
	tmpDirCleanup := func() { _ = os.RemoveAll(tmpDir) }
	defer tmpDirCleanup()

	// If this function returns a failure after tmpDir is created, capture
	// the raw output before cleanup.
	defer func() {
		if recoverPanic := recover(); recoverPanic != nil {
			// Capture raw output before cleanup when panic occurs
			rawOutput := captureRawOutput(tmpDir, fmt.Sprintf("panic: %v", recoverPanic))
			panic(rawOutput)
		}
	}()

	priorPath := filepath.Join(tmpDir, "prior.json")
	priorFindings := req.PriorFindings
	if priorFindings == nil {
		priorFindings = []PriorFinding{}
	}
	priorData, err := json.Marshal(priorFindings)
	if err != nil {
		return failureResultWithRawOutput(deps, req, Tooling, fmt.Sprintf("marshal prior findings: %v", err), 0, tmpDir)
	}
	if err := os.WriteFile(priorPath, priorData, 0644); err != nil {
		return failureResultWithRawOutput(deps, req, Tooling, fmt.Sprintf("write prior findings: %v", err), 0, tmpDir)
	}

	verdictPath := filepath.Join(tmpDir, "verdict.json")
	scriptPath := filepath.Join(req.RigDir, cfg.Command)
	// The diff base is the merge-base sha, not origin/<target>: the target
	// can move while a rehearsed head waits for its gate, and a diff from the
	// moved target shows the target's own new commits as deletions on the
	// branch (two phantom rejections on 2026-09-19, gt-x1x3). The merge-base
	// pins the review to exactly what the branch changed.
	args := []string{
		"--base", mergeBase,
		"--head", head,
	}
	// Omitted rather than passed empty: a landed review names no MR bead, and
	// the gate script reads an absent --mr as "do not route this verdict to a
	// bead", which is what a verdict about a commit whose MR is gone should do.
	if req.MRID != "" {
		args = append(args, "--mr", req.MRID)
	}
	// --worker is passed whether or not it has a value: every MR-path run
	// before landed reviews existed sent it, and the deployed gate script
	// parses its arguments strictly and fails closed, so a shape it has never
	// seen is a refusal, not a default.
	args = append(args, "--worker", req.Worker)
	args = append(args, "--rig", req.Rig, "--out", verdictPath, "--prior-findings", priorPath)
	// Only appended when the caller asked for an override: the deployed
	// gate script parses its args with a strict case and fails closed
	// (exit 2) on any argument it does not know, so the default path must
	// stay exactly as it was for a script that predates --timeout support.
	if req.TimeoutSeconds > 0 {
		args = append(args, "--timeout", strconv.Itoa(req.TimeoutSeconds))
	}

	var v *verdictJSON
	var class FailureClass
	var lastStderr string
	retries := 0
	for attempt := 0; ; attempt++ {
		stderr, exitCode, execErr := deps.Exec(ctx, scriptPath, args, reviewDir)
		lastStderr = stderr
		v, class, err = classifyOutcome(execErr, exitCode, stderr, verdictPath)
		if class == "" {
			break // verdict obtained
		}
		if attempt == 0 && class.Retryable() {
			retries = 1
			continue
		}
		break
	}

	if class != "" {
		msg := lastStderr
		if msg == "" && err != nil {
			msg = err.Error()
		}
		// Capture raw output before temp dir cleanup for diagnostic purposes.
		return failureResultWithRawOutput(deps, req, class, msg, retries, tmpDir)
	}

	note := Note{
		OMVersion:         manifest.OMBinary.Version,
		RubricSHA256:      manifest.Rubric.SHA256,
		ResolvedBackend:   v.Backend,
		Rig:               req.Rig,
		MR:                req.MRID,
		Worker:            req.Worker,
		BaseSHA:           mergeBase,
		ReviewedTargetTip: targetTip,
		HeadSHA:           noteCommit,
		PatchID:           patchID,
		Score:             v.Score,
		Verdict:           v.Verdict,
		FindingsCount:     len(v.Findings),
		Findings:          v.Findings,
		Attempt:           req.Attempt,
		ReviewedAt:        time.Now().UTC(),
		// Recorded only when the review actually ran with an override, so
		// the note distinguishes "used the rig default" from "was allowed
		// N seconds" (see Note.TimeoutSeconds).
		TimeoutSeconds: req.TimeoutSeconds,
		// True only when this review carried a retirement through the rubric
		// guard (see Note.RubricRetirement).
		RubricRetirement: retiredRubric,
		// Set only on a landed review, where the note is stamped on the
		// landed commit and the diff that was scored is the branch's own
		// head (see Note.RetroReview, Note.ReviewHead).
		RetroReview: retroReview,
		ReviewHead:  reviewHead,
	}
	if v.PriorFindings != nil {
		note.PriorFindings.Resolved = v.PriorFindings.Resolved
		note.PriorFindings.Unresolved = v.PriorFindings.Unresolved
		note.PriorFindings.Regressed = v.PriorFindings.Regressed
	}
	// This note replaces the verdict prior named, so it carries that verdict
	// forward: the diff a re-review scored is often byte-identical to the one
	// it replaces, and a replaced score with no trace of the score it
	// replaced is what made the gate's nondeterminism invisible (gt-bveg).
	var priorNote *Note
	if prior != nil {
		priorNote = &prior.Note
	}
	if history := attemptHistory(priorNote, attemptOf(&note)); len(history) > 1 {
		note.Attempts = history
	}

	// resultStderr, unlike lastStderr (the raw gate-script output, kept
	// around for the failure-path diagnostics above), is what ReviewResult.Stderr
	// reports on a verdict: callers (mq_review.go, batch_editorial.go) print it
	// as "approved with warning", so it must carry only genuine non-fatal
	// follow-up trouble, not the gate script's routine stderr chatter — a
	// clean approve where the script merely logged to stderr is not a warning.
	var resultStderr string
	if note.Verdict == "approve" {
		var majors []Finding
		for _, f := range v.Findings {
			if f.Severity == "major" {
				majors = append(majors, f)
			}
		}
		if len(majors) > 0 {
			ids, ferr := fileFollowups(deps.Beads, req.MRID, note.Score, majors)
			note.Followups = ids
			if ferr != nil {
				resultStderr = fmt.Sprintf("followups: %v", ferr)
			}
		}
	}

	if deps.NotesMu != nil {
		deps.NotesMu.Lock()
	}
	writeErr := WriteNote(deps.Git, note)
	var pushErr error
	if writeErr == nil {
		// The note is written into RepoDir's own refs/notes/om, which
		// readers in other clones (e.g. the editorial-coverage doctor
		// check, run from mayor/rig) never see unless it reaches the
		// shared origin remote.
		pushErr = deps.Git.PushNotes("origin", NotesRef)
	}
	if deps.NotesMu != nil {
		deps.NotesMu.Unlock()
	}
	if writeErr != nil {
		return failureResultWithRawOutput(deps, req, RecordFailed, fmt.Sprintf("write note: %v", writeErr), retries, tmpDir)
	}
	if pushErr != nil {
		return failureResultWithRawOutput(deps, req, RecordFailed, fmt.Sprintf("push note: %v", pushErr), retries, tmpDir)
	}
	if _, err := RecordReceipt(deps.Recorder, note); err != nil {
		return failureResultWithRawOutput(deps, req, RecordFailed, fmt.Sprintf("record receipt: %v", err), retries, tmpDir)
	}

	if note.Verdict == "approve" {
		if !retroReview {
			// A submitted branch's push is authorized by its note, which the
			// refinery finds by reading editorial_reviewed_head off the MR
			// bead. A landed review has no such bead — and no push to
			// precondition — so it skips this write.
			if err := setEditorialReviewedHead(deps.Beads, req.MRID, head); err != nil {
				return failureResultWithRawOutput(deps, req, RecordFailed, fmt.Sprintf("update MR bead: %v", err), retries, tmpDir)
			}
		}
		return ReviewResult{Exit: 0, Note: &note, Retries: retries, Stderr: resultStderr}
	}
	return ReviewResult{Exit: 1, Note: &note, Retries: retries, Stderr: resultStderr}
}

// captureRawOutput reads the verdict.json file from tmpDir (if it exists) and
// returns it along with stderr for diagnostic purposes. This is called when a
// failure occurs to preserve the raw backend output before the temp dir is cleaned.
func captureRawOutput(tmpDir, stderr string) string {
	output := stderr
	verdictPath := filepath.Join(tmpDir, "verdict.json")
	if data, err := os.ReadFile(verdictPath); err == nil {
		output = strings.TrimSpace(stderr)
		if output != "" {
			output += "\n\n"
		}
		output += "=== verdict.json content ===\n" + strings.TrimSpace(string(data))
	}
	return output
}

// classOf returns the FailureClass err was classified with, falling back when
// it carries none. A classless ClassifiedError keeps the caller's fallback
// instead of overwriting it with an empty class: RecordFailure refuses an
// empty class, so letting one through would leave the failure recorded
// nowhere at all (gt-47nf).
func classOf(err error, fallback FailureClass) FailureClass {
	var ce *ClassifiedError
	if errors.As(err, &ce) && ce.Class != "" {
		return ce.Class
	}
	return fallback
}

func failureResult(deps Deps, req ReviewRequest, class FailureClass, stderr string, _ int) ReviewResult {
	_, recErr := RecordFailure(deps.Recorder, req.Rig, req.Worker, req.MRID, class, stderr, 0)
	stderr = receiptRefusalNotice(stderr, recErr)
	return ReviewResult{Exit: 2, Class: class, Retries: 0, Stderr: stderr}
}

// failureResultWithRawOutput is like failureResult but captures the raw backend
// output (stderr + verdict.json) before the temp dir is cleaned.
func failureResultWithRawOutput(deps Deps, req ReviewRequest, class FailureClass, stderr string, retries int, tmpDir string) ReviewResult {
	rawOutput := captureRawOutput(tmpDir, stderr)
	_, recErr := RecordFailure(deps.Recorder, req.Rig, req.Worker, req.MRID, class, rawOutput, retries)
	rawOutput = receiptRefusalNotice(rawOutput, recErr)
	return ReviewResult{Exit: 2, Class: class, Retries: retries, Stderr: rawOutput}
}

// receiptRefusalNotice appends a refused-receipt notice to the failure's
// captured output and echoes it to stderr, returning output unchanged when
// the receipt was recorded.
//
// A refused receipt leaves this failure with no durable record at all, so
// the refusal has to travel with the result rather than be dropped on the
// floor by a discarded error return. The exit code stays 2 either way: the
// review is still fail-closed, and escalating the missing receipt through
// exit 2's own witness path is what the notice is for (gt-47nf).
func receiptRefusalNotice(output string, recErr error) string {
	if recErr == nil {
		return output
	}
	msg := fmt.Sprintf("[editorial] warning: failure receipt refused: %v", recErr)
	_, _ = fmt.Fprintln(os.Stderr, msg)
	if output == "" {
		return msg
	}
	return output + "\n\n" + msg
}

// gateUsageErrorMarker is what the rig's gate script writes to stderr when its
// own argument parser rejects an invocation: om-gate.sh's usage_error() logs
// "usage error: <detail>" and exits 2. That parser prints its whole flag list
// on rejection, and the list advertises "--timeout", so the BackendTimeout
// search below would read a caller/harness disagreement as a backend timeout —
// a Retryable class — and run the gate twice on a deterministic error
// (gt-o6xh).
const gateUsageErrorMarker = "usage error:"

// classifyOutcome maps one gate-script invocation's outcome to either a
// parsed verdict (class == "") or a FailureClass, in the fail-closed
// table's order: BinaryMissing, ConfigError, BackendTimeout, MalformedVerdict,
// Tooling. VersionMismatch is asserted before invocation and never reaches
// here. Routes on exit code and stderr markers only, never on log prose
// beyond the documented markers.
func classifyOutcome(execErr error, exitCode int, stderr, verdictPath string) (*verdictJSON, FailureClass, error) {
	if execErr != nil {
		if isExecNotFound(execErr) {
			return nil, BinaryMissing, execErr
		}
		return nil, Tooling, execErr
	}
	if strings.HasPrefix(stderr, "om: config error:") {
		return nil, ConfigError, errors.New(strings.TrimSpace(stderr))
	}
	lowerStderr := strings.ToLower(stderr)
	if exitCode == 2 && strings.Contains(lowerStderr, gateUsageErrorMarker) {
		return nil, ConfigError, errors.New(strings.TrimSpace(stderr))
	}
	if exitCode == 2 && (strings.Contains(lowerStderr, "timeout") || strings.Contains(lowerStderr, "timed out")) {
		return nil, BackendTimeout, errors.New(strings.TrimSpace(stderr))
	}

	info, statErr := os.Stat(verdictPath)
	if statErr != nil || info.Size() == 0 {
		return nil, MalformedVerdict, fmt.Errorf("verdict file missing or empty at %s", verdictPath)
	}
	data, err := os.ReadFile(verdictPath)
	if err != nil {
		return nil, MalformedVerdict, err
	}
	var v verdictJSON
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, MalformedVerdict, fmt.Errorf("parsing verdict JSON: %w", err)
	}
	if !ValidVerdict(v.Verdict) {
		return nil, MalformedVerdict, fmt.Errorf("verdict field is %q, want %s or %s", v.Verdict, VerdictApprove, VerdictRequestChanges)
	}
	if exitCode != 0 && exitCode != 1 {
		return nil, Tooling, fmt.Errorf("gate script exited %d unexpectedly (stderr: %s)", exitCode, stderr)
	}
	return &v, "", nil
}

// Rehearsal is a throwaway detached worktree that review rehearsals run in, so
// the refinery's live clone is never checked out onto anything: the clone the
// single-review CLI path hands over (refinery/rig — see doMQReview) may be
// mid-gate on its own branch, and a rehearsal that touches its HEAD leaves that
// gate building the wrong tree (gt-dcku, gt-kmul).
//
// One Rehearsal serves a whole batch: Branch resets the worktree to
// origin/<target> first, so the batch's sequential rehearsals cannot stack on
// each other, and the batch pays one `git worktree add` per cycle instead of
// one per candidate.
type Rehearsal struct {
	live   *git.Git // the live clone, which owns the worktree registration
	dir    string   // the throwaway worktree's path
	wt     *git.Git // a handle rooted at dir
	target string   // the base every rehearsal merges onto
}

// BeginRehearsal creates the throwaway detached worktree a Rehearsal runs in,
// seeded (detached) at origin/<target>. Close must be called to remove it.
func BeginRehearsal(g *git.Git, target string) (*Rehearsal, error) {
	return BeginRehearsalAt(g, "origin/"+target, target)
}

// BeginRehearsalAt is BeginRehearsal for a caller whose seed ref is not
// origin/<target> itself — a landed/retro review of a diff that touched the
// rubric, whose pre-change state has already been folded into origin/<target>
// by the very commit under review (gt-7bvf). target only records what a
// later Branch call would merge onto; it does not affect the seed.
func BeginRehearsalAt(g *git.Git, ref, target string) (*Rehearsal, error) {
	dir, err := os.MkdirTemp("", "gt-mq-rehearse-*")
	if err != nil {
		return nil, fmt.Errorf("mktemp rehearsal worktree: %w", err)
	}
	if err := g.WorktreeAddDetached(dir, ref); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("add rehearsal worktree from %s: %w", ref, err)
	}
	return &Rehearsal{live: g, dir: dir, wt: git.NewGit(dir), target: target}, nil
}

// Branch merges origin/<branch> onto origin/<target> in the rehearsal worktree
// and returns the resulting head sha, so the review sees exactly what would
// land. An error means this branch is not reviewable — in practice, a merge
// conflict — and its message is what the caller records.
//
// The reset to origin/<target> is load-bearing twice: it is what lets one
// Rehearsal serve several candidates without one stacking on the last, and it
// is why no failure here needs cleanup of its own — the next call wipes
// whatever an aborted merge left behind.
func (r *Rehearsal) Branch(branch string) (string, error) {
	if err := r.wt.ResetHard("origin/" + r.target); err != nil {
		return "", fmt.Errorf("reset rehearsal worktree to origin/%s: %w", r.target, err)
	}
	// A merge that conflicts and is aborted can leave the index dirty with
	// conflict entries; untracked files can only come from the merge itself.
	// Clearing both means the merge below always starts from a pristine
	// checkout, whatever the previous candidate did.
	if err := r.wt.CleanForce(); err != nil {
		return "", fmt.Errorf("clean rehearsal worktree: %w", err)
	}
	if err := r.wt.MergeNoFF("origin/"+branch, "rehearsal merge for om review"); err != nil {
		_ = r.wt.AbortMerge()
		return "", fmt.Errorf("merge origin/%s onto origin/%s: %w", branch, r.target, err)
	}
	head, err := r.wt.Rev("HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve rehearsal head: %w", err)
	}
	return head, nil
}

// Close removes the rehearsal worktree. A failure is returned rather than
// swallowed: a worktree that fails to remove stays registered in the live clone
// with its directory still on disk, and discarding that silently is how they
// accumulate unnoticed (gt-evk4). Nothing downstream depends on the removal
// having happened.
func (r *Rehearsal) Close() error {
	removeErr := r.live.WorktreeRemove(r.dir, true)
	// WorktreeRemove leaves the directory behind when it held untracked
	// files, so nothing outlives the registration.
	if err := os.RemoveAll(r.dir); err != nil && removeErr == nil {
		return fmt.Errorf("remove rehearsal worktree dir %s: %w", r.dir, err)
	}
	if removeErr != nil {
		return fmt.Errorf("remove rehearsal worktree %s: %w", r.dir, removeErr)
	}
	return nil
}

// Rehearse fetches origin and merges origin/<branch> onto origin/<target> in
// a throwaway detached worktree, returning the resulting head sha so the
// review sees exactly what would land. Callers that already rehearsed (or
// are re-reviewing a fixed head) pass ReviewRequest.RehearsedHead instead
// and skip this. It is the one-candidate wrapper over BeginRehearsal +
// Branch + Close, and never touches g's checkout.
//
// The batch path calls BeginRehearsal directly instead: one fetch and one
// worktree serve every candidate, rather than one of each per candidate.
func Rehearse(g *git.Git, target, branch string) (string, error) {
	if err := g.Fetch("origin"); err != nil {
		return "", fmt.Errorf("fetch origin: %w", err)
	}
	r, err := BeginRehearsal(g, target)
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()
	return r.Branch(branch)
}

// priorVerdict returns the verdict a review of this diff answers for or
// replaces, or nil when the diff carries none.
//
// The note on head itself wins when there is one, applicable or not: it is
// the verdict recorded for exactly this commit, which is what a re-review
// replaces and what a reuse answers from. Only when head carries no note at
// all does the search widen to the whole notes ref — the MR path's normal
// case, since it rehearses a new merge commit every invocation, so the head a
// verdict was written on is never the head a later invocation computes
// (gt-qa2p).
//
// A note that cannot be read is an error, not an empty result: a caller must
// not re-roll a diff whose recorded verdict it could not see.
func priorVerdict(g *git.Git, head, patchID, rubricSHA, minVersion string) (*RecordedVerdict, error) {
	note, err := ReadNote(g, head)
	switch {
	case err == nil:
		return &RecordedVerdict{Commit: head, Note: *note}, nil
	case errors.Is(err, git.ErrNoNote):
		return FindVerdictForDiff(g, patchID, rubricSHA, minVersion)
	default:
		return nil, err
	}
}

// recordedVerdictApplies reports whether prev still answers for a head:
// same diff (patch-id), same deployed rubric, a verdict this pipeline would
// itself have accepted, and an om version still above the configured floor.
// A note failing any of the four is not a recorded verdict for this review —
// the diff, the criteria it was scored against, or the bar it must clear has
// moved — so the review proceeds and replaces it. The version floor is the
// push precondition's, not a second one: answering from a note the
// precondition then refuses would wedge the MR on a review that reports
// approve.
func recordedVerdictApplies(prev *Note, patchID, rubricSHA, minVersion string) bool {
	if prev == nil {
		return false
	}
	if !ValidVerdict(prev.Verdict) {
		return false
	}
	if prev.PatchID != patchID || prev.RubricSHA256 != rubricSHA {
		return false
	}
	return !omVersionBelowFloor(prev.OMVersion, minVersion)
}

// ensureEditorialReviewedHead records head on the MR bead unless it is
// already there. The review that wrote the note has normally recorded it
// already, and a bead write costs a Dolt commit, so the reused-verdict path
// asks first rather than re-recording the same head. On a reuse the head is
// the commit the note was found on, which is what the field is for: the push
// precondition reads it to find the note without scanning.
func ensureEditorialReviewedHead(b *beads.Beads, mrID, head string) error {
	if issue, err := b.Show(mrID); err == nil {
		if fields := beads.ParseMRFields(issue); fields != nil && fields.EditorialReviewedHead == head {
			return nil
		}
	}
	return setEditorialReviewedHead(b, mrID, head)
}

// setEditorialReviewedHead records the reviewed head on the MR bead so the
// push precondition (om-gate T6) can find the matching note without
// re-deriving it.
func setEditorialReviewedHead(b *beads.Beads, mrID, head string) error {
	issue, err := b.Show(mrID)
	if err != nil {
		return fmt.Errorf("show %s: %w", mrID, err)
	}
	fields := beads.ParseMRFields(issue)
	if fields == nil {
		fields = &beads.MRFields{}
	}
	fields.EditorialReviewedHead = head
	newDesc := beads.SetMRFields(issue, fields)
	return b.Update(mrID, beads.UpdateOptions{Description: &newDesc})
}

// fileFollowups files one P2 bug bead per major finding on an approve
// verdict (label om-followup) and appends their ids as a comment on the MR
// bead, so a review that approves-with-caveats still leaves a paper trail —
// approval never dissolves a finding (DECISION 8).
func fileFollowups(b *beads.Beads, mrID string, score float64, findings []Finding) ([]string, error) {
	var ids []string
	var firstErr error
	for _, f := range findings {
		title := fmt.Sprintf("%s (om major on %s approved at %g)", f.Title, mrID, score)
		desc := f.Title
		if f.Path != "" {
			desc = fmt.Sprintf("%s\n\n%s:%d", f.Title, f.Path, f.Line)
		}
		issue, err := b.Create(beads.CreateOptions{
			Title:       title,
			Labels:      []string{"gt:bug", "om-followup"},
			Priority:    2,
			Description: desc,
		})
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("filing followup for finding %q: %w", f.Title, err)
			}
			continue
		}
		ids = append(ids, issue.ID)
	}
	if len(ids) > 0 {
		comment := fmt.Sprintf("om approve carried %d major finding(s); follow-ups filed: %s", len(ids), strings.Join(ids, ", "))
		// An empty mrID is a retro review of a landed commit: the follow-up
		// beads are the record, and there is no MR bead to comment on —
		// commenting on the landed commit's (nonexistent) MR bead would fail
		// and poison an otherwise clean approve.
		if mrID != "" {
			if err := b.AddComment(mrID, comment); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("commenting followups on %s: %w", mrID, err)
			}
		}
	}
	return ids, firstErr
}
