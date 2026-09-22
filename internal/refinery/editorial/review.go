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

// Run rehearses (or accepts an already-rehearsed) head, asserts the harness
// version, invokes the rig's editorial gate script, classifies the outcome,
// retries once for transient classes, and on a verdict writes the note and
// receipt (and, on an approve carrying major findings, one follow-up bead
// per finding — DECISION 8: approval never dissolves a finding).
func Run(ctx context.Context, req ReviewRequest, deps Deps) ReviewResult {
	cfg := req.Config.WithDefaults()

	manifest, err := LoadManifest(req.RigDir)
	if err != nil {
		return failureResult(deps, req, BinaryMissing, err.Error(), 0)
	}
	if err := AssertVersion(manifest, cfg, req.RigDir, req.RepoDir); err != nil {
		var ce *ClassifiedError
		class := VersionMismatch
		if errors.As(err, &ce) {
			class = ce.Class
		}
		return failureResult(deps, req, class, err.Error(), 0)
	}

	head := req.RehearsedHead
	if head == "" {
		rehearsed, err := Rehearse(deps.Git, req.Target, req.Branch)
		if err != nil {
			return failureResult(deps, req, Tooling, fmt.Sprintf("rehearsal failed: %v", err), 0)
		}
		head = rehearsed
	} else {
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

	mergeBase, err := deps.Git.MergeBase("origin/"+req.Target, head)
	if err != nil {
		return failureResult(deps, req, Tooling, fmt.Sprintf("merge-base: %v", err), 0)
	}
	patchID, err := deps.Git.PatchID(mergeBase, head)
	if err != nil {
		return failureResult(deps, req, Tooling, fmt.Sprintf("patch-id: %v", err), 0)
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

	// The verdict this review answers for or replaces, read before the
	// decision to review at all: it answers the diff outright when it still
	// applies, and is carried into the new note's attempt history when it
	// does not.
	prior, err := priorVerdict(deps.Git, head, patchID, manifest.Rubric.SHA256, cfg.MinVersion)
	if err != nil {
		// Not knowing whether this diff was already reviewed is not a state
		// to re-roll from: an unreadable note would be silently replaced.
		return failureResult(deps, req, Tooling, fmt.Sprintf("read recorded verdict for diff %s: %v", patchID, err), 0)
	}
	if prior != nil && !req.Reroll && recordedVerdictApplies(&prior.Note, patchID, manifest.Rubric.SHA256, cfg.MinVersion) {
		if prior.Note.Verdict == "approve" {
			// Record the head the note was found on — the reviewed head —
			// not this invocation's rehearsal commit. No note exists on the
			// latter (that is why the lookup had to scan), and the push
			// precondition finds the note by reading editorial_reviewed_head
			// (CheckPrecondition), so pointing it at a commit with no note
			// would refuse the push on a review that had just approved.
			if err := ensureEditorialReviewedHead(deps.Beads, req.MRID, prior.Commit); err != nil {
				return failureResult(deps, req, RecordFailed, fmt.Sprintf("update MR bead: %v", err), 0)
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
		"--mr", req.MRID,
		"--worker", req.Worker,
		"--rig", req.Rig,
		"--out", verdictPath,
		"--prior-findings", priorPath,
	}
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
		stderr, exitCode, execErr := deps.Exec(ctx, scriptPath, args, req.RepoDir)
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
		OMVersion:     manifest.OMBinary.Version,
		RubricSHA256:  manifest.Rubric.SHA256,
		Rig:           req.Rig,
		MR:            req.MRID,
		Worker:        req.Worker,
		BaseSHA:       mergeBase,
		HeadSHA:       head,
		PatchID:       patchID,
		Score:         v.Score,
		Verdict:       v.Verdict,
		FindingsCount: len(v.Findings),
		Findings:      v.Findings,
		Attempt:       req.Attempt,
		ReviewedAt:    time.Now().UTC(),
		// Recorded only when the review actually ran with an override, so
		// the note distinguishes "used the rig default" from "was allowed
		// N seconds" (see Note.TimeoutSeconds).
		TimeoutSeconds: req.TimeoutSeconds,
		// True only when this review carried a retirement through the rubric
		// guard (see Note.RubricRetirement).
		RubricRetirement: retiredRubric,
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
		if err := setEditorialReviewedHead(deps.Beads, req.MRID, head); err != nil {
			return failureResultWithRawOutput(deps, req, RecordFailed, fmt.Sprintf("update MR bead: %v", err), retries, tmpDir)
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

func failureResult(deps Deps, req ReviewRequest, class FailureClass, stderr string, _ int) ReviewResult {
	_, _ = RecordFailure(deps.Recorder, req.Rig, req.Worker, req.MRID, class, stderr, 0)
	return ReviewResult{Exit: 2, Class: class, Retries: 0, Stderr: stderr}
}

// failureResultWithRawOutput is like failureResult but captures the raw backend
// output (stderr + verdict.json) before the temp dir is cleaned.
func failureResultWithRawOutput(deps Deps, req ReviewRequest, class FailureClass, stderr string, retries int, tmpDir string) ReviewResult {
	rawOutput := captureRawOutput(tmpDir, stderr)
	_, _ = RecordFailure(deps.Recorder, req.Rig, req.Worker, req.MRID, class, rawOutput, retries)
	return ReviewResult{Exit: 2, Class: class, Retries: retries, Stderr: rawOutput}
}

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
	if v.Verdict != "approve" && v.Verdict != "request_changes" {
		return nil, MalformedVerdict, fmt.Errorf("verdict field is %q, want approve or request_changes", v.Verdict)
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
	dir, err := os.MkdirTemp("", "gt-mq-rehearse-*")
	if err != nil {
		return nil, fmt.Errorf("mktemp rehearsal worktree: %w", err)
	}
	if err := g.WorktreeAddDetached(dir, "origin/"+target); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("add rehearsal worktree from origin/%s: %w", target, err)
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
	if prev.Verdict != "approve" && prev.Verdict != "request_changes" {
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
		if err := b.AddComment(mrID, comment); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("commenting followups on %s: %w", mrID, err)
		}
	}
	return ids, firstErr
}
