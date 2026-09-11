package editorial

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

	Attempt       int
	PriorFindings []PriorFinding

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
	Score    float64          `json:"score"`
	Verdict  string           `json:"verdict"`
	Findings []verdictFinding `json:"findings"`

	PriorFindings *struct {
		Resolved   []string `json:"resolved"`
		Unresolved []string `json:"unresolved"`
		Regressed  []string `json:"regressed"`
	} `json:"prior_findings,omitempty"`
}

type verdictFinding struct {
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

	tmpDir, err := os.MkdirTemp("", "gt-mq-review-*")
	if err != nil {
		return failureResult(deps, req, Tooling, fmt.Sprintf("mktemp: %v", err), 0)
	}
	defer os.RemoveAll(tmpDir)

	priorPath := filepath.Join(tmpDir, "prior.json")
	priorFindings := req.PriorFindings
	if priorFindings == nil {
		priorFindings = []PriorFinding{}
	}
	priorData, err := json.Marshal(priorFindings)
	if err != nil {
		return failureResult(deps, req, Tooling, fmt.Sprintf("marshal prior findings: %v", err), 0)
	}
	if err := os.WriteFile(priorPath, priorData, 0644); err != nil {
		return failureResult(deps, req, Tooling, fmt.Sprintf("write prior findings: %v", err), 0)
	}

	verdictPath := filepath.Join(tmpDir, "verdict.json")
	scriptPath := filepath.Join(req.RigDir, cfg.Command)
	args := []string{
		"--base", "origin/" + req.Target,
		"--head", head,
		"--mr", req.MRID,
		"--worker", req.Worker,
		"--rig", req.Rig,
		"--out", verdictPath,
		"--prior-findings", priorPath,
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
		return failureResult(deps, req, class, msg, retries)
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
		Attempt:       req.Attempt,
		ReviewedAt:    time.Now().UTC(),
	}
	if v.PriorFindings != nil {
		note.PriorFindings.Resolved = v.PriorFindings.Resolved
		note.PriorFindings.Unresolved = v.PriorFindings.Unresolved
		note.PriorFindings.Regressed = v.PriorFindings.Regressed
	}

	if note.Verdict == "approve" {
		var majors []verdictFinding
		for _, f := range v.Findings {
			if f.Severity == "major" {
				majors = append(majors, f)
			}
		}
		if len(majors) > 0 {
			ids, ferr := fileFollowups(deps.Beads, req.MRID, note.Score, majors)
			note.Followups = ids
			if ferr != nil {
				lastStderr = fmt.Sprintf("followups: %v", ferr)
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
		return failureResult(deps, req, RecordFailed, fmt.Sprintf("write note: %v", writeErr), retries)
	}
	if pushErr != nil {
		return failureResult(deps, req, RecordFailed, fmt.Sprintf("push note: %v", pushErr), retries)
	}
	if _, err := RecordReceipt(deps.Recorder, note); err != nil {
		return failureResult(deps, req, RecordFailed, fmt.Sprintf("record receipt: %v", err), retries)
	}

	if note.Verdict == "approve" {
		if err := setEditorialReviewedHead(deps.Beads, req.MRID, head); err != nil {
			return failureResult(deps, req, RecordFailed, fmt.Sprintf("update MR bead: %v", err), retries)
		}
		return ReviewResult{Exit: 0, Note: &note, Retries: retries, Stderr: lastStderr}
	}
	return ReviewResult{Exit: 1, Note: &note, Retries: retries, Stderr: lastStderr}
}

func failureResult(deps Deps, req ReviewRequest, class FailureClass, stderr string, retries int) ReviewResult {
	_, _ = RecordFailure(deps.Recorder, req.Rig, req.Worker, req.MRID, class, stderr, retries)
	return ReviewResult{Exit: 2, Class: class, Retries: retries, Stderr: stderr}
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

// Rehearse fetches origin and merges origin/<branch> onto origin/<target> in
// a throwaway detached worktree, returning the resulting head sha so the
// review sees exactly what would land. Callers that already rehearsed (or
// are re-reviewing a fixed head) pass ReviewRequest.RehearsedHead instead
// and skip this.
//
// RepoDir for the single-invocation `gt mq review` CLI path (the only
// caller that reaches this) is the refinery's live clone (refinery/rig —
// see doMQReview), not a throwaway checkout, and it may be mid-gate on its
// own branch (e.g. a batch's "temp") when this runs. The rehearsal MUST NOT
// touch g's HEAD at all: an earlier version created a temp branch and
// checked it out in g's own working directory, then tried to "restore" by
// checking out the TARGET NAME — which silently moved g's HEAD onto a stale
// local target branch instead of back to whatever it was on before, and
// left the working directory's build/test tooling running against the
// wrong tree (gt-dcku). Doing the rehearsal in a separate worktree makes
// that whole class of bug impossible: g's checkout is never touched, so
// there is nothing to restore. The batch path never calls this — it uses
// RehearseBranch directly and performs its own equivalent cleanup across
// multiple candidates before running any gate scripts.
func Rehearse(g *git.Git, target, branch string) (string, error) {
	if err := g.Fetch("origin"); err != nil {
		return "", fmt.Errorf("fetch origin: %w", err)
	}

	worktreeDir, err := os.MkdirTemp("", "gt-mq-rehearse-*")
	if err != nil {
		return "", fmt.Errorf("mktemp rehearsal worktree: %w", err)
	}
	defer os.RemoveAll(worktreeDir)

	if err := g.WorktreeAddDetached(worktreeDir, "origin/"+target); err != nil {
		return "", fmt.Errorf("add rehearsal worktree from origin/%s: %w", target, err)
	}
	defer func() { _ = g.WorktreeRemove(worktreeDir, true) }()

	wg := git.NewGit(worktreeDir)
	if err := wg.MergeNoFF("origin/"+branch, "rehearsal merge for om review"); err != nil {
		_ = wg.AbortMerge()
		return "", fmt.Errorf("merge origin/%s onto origin/%s: %w", branch, target, err)
	}
	head, err := wg.Rev("HEAD")
	if err != nil {
		return "", err
	}
	return head, nil
}

// RehearseBranch is Rehearse without the origin fetch, returning the temp
// branch name alongside the head so the caller can delete it once done.
//
// Exported so batch callers (om-gate T7) can rehearse every candidate
// sequentially — each rehearsal checks out a temp branch in the shared
// working directory, so it is not safe to call concurrently — before
// running Run itself with bounded parallelism via RehearsedHead, which
// skips this step entirely. A batch fetches origin once up front (a single
// candidate's rehearsal failing to find a just-pushed branch is no worse
// than the pre-batch single-MR path re-fetching per review) instead of
// once per candidate, and deletes each temp branch after using its head so
// a busy rig does not accumulate one gt-mq-review-* branch per candidate
// per cycle.
func RehearseBranch(g *git.Git, target, branch string) (head, tempBranch string, err error) {
	tempBranch = fmt.Sprintf("gt-mq-review-%d", time.Now().UnixNano())
	if err := g.CreateBranchFrom(tempBranch, "origin/"+target); err != nil {
		return "", "", fmt.Errorf("create rehearsal branch from origin/%s: %w", target, err)
	}
	if err := g.Checkout(tempBranch); err != nil {
		return "", "", fmt.Errorf("checkout rehearsal branch: %w", err)
	}
	if err := g.MergeNoFF("origin/"+branch, "rehearsal merge for om review"); err != nil {
		_ = g.AbortMerge()
		return "", "", fmt.Errorf("merge origin/%s onto origin/%s: %w", branch, target, err)
	}
	head, err = g.Rev("HEAD")
	if err != nil {
		return "", "", err
	}
	return head, tempBranch, nil
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
func fileFollowups(b *beads.Beads, mrID string, score float64, findings []verdictFinding) ([]string, error) {
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
