package refinery

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/steveyegge/gastown/internal/plugin"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
)

// ReviewedMR is one batch candidate's editorial review outcome, reported so
// `gt mq batch run --json` can show what happened to every reviewed MR, not
// just the ones that made it into the stack.
type ReviewedMR struct {
	ID      string `json:"id"`
	Exit    int    `json:"exit"`
	Verdict string `json:"verdict,omitempty"`
}

// EjectedMR is a batch member removed from the stack after review — its
// content changed once it landed alongside the rest of the stack, so the
// verdict it was reviewed under no longer applies. Left queued and
// untouched: not a conflict, not a test-failure culprit.
type EjectedMR struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// reviewBatchCandidates runs the om editorial gate on every batch candidate
// before stacking, bounded by cfg.ReviewParallelism concurrent invocations.
// It is a no-op — returning candidates unchanged and a nil reviewed list —
// when the rig has not set merge_queue.editorial.required, so batches on
// rigs that never configured editorial keep upstream (pre-gate) behavior.
//
// Members whose review does not come back Exit 0 (approve) are dropped from
// the returned batch and left untouched in the queue — they aren't recorded
// as conflicts or culprits, since nothing about their branch or merge
// eligibility failed. editorial.Run already persists the note/receipt (or
// failure receipt) for every outcome, so there is nothing further to record
// here beyond the summary in reviewed.
//
// notes carries each approved member's editorial note (keyed by MR ID), so
// the caller can later recompute its patch-id once the member is actually
// stacked and eject it if the two no longer match (ejectPatchIDChanged).
func (e *Engineer) reviewBatchCandidates(ctx context.Context, candidates []*MRInfo, target string) (approved []*MRInfo, reviewed []ReviewedMR, notes map[string]*editorial.Note) {
	if e.config.Editorial == nil || !e.config.Editorial.Required || len(candidates) == 0 {
		return candidates, nil, nil
	}
	cfg := e.config.Editorial.WithDefaults()

	townRoot := filepath.Dir(e.rig.Path)
	var notesMu sync.Mutex
	deps := editorial.Deps{
		Git:      e.git,
		Beads:    e.beads,
		Recorder: plugin.NewRecorder(townRoot),
		Exec:     e.editorialExec,
		// Shared across every concurrent Run call below: serializes the
		// WriteNote+PushNotes tail so two goroutines never race a `git
		// notes add` against a `git push refs/notes/om` on the same ref
		// (lost update, or a spurious non-fast-forward rejection).
		NotesMu: &notesMu,
	}

	// Rehearse every candidate sequentially first: a rehearsal checks out a
	// temp branch in the shared working directory, so running it
	// concurrently across candidates would have them stomp on each other's
	// checkouts. Everything after (manifest checks, patch-id, the gate
	// script exec) operates on fixed SHAs or an external process and is
	// safe to run concurrently, bounded below by ReviewParallelism.
	//
	// Fetch origin once for the whole batch (rather than once per
	// candidate) and delete each temp branch as soon as its head is in
	// hand, so a busy rig doesn't pay one fetch and accumulate one
	// gt-mq-review-* branch per candidate per cycle.
	rehearsedHeads := make([]string, len(candidates))
	rehearsalErrs := make([]error, len(candidates))
	if fetchErr := e.git.Fetch("origin"); fetchErr != nil {
		for i := range candidates {
			rehearsalErrs[i] = fmt.Errorf("fetch origin: %w", fetchErr)
		}
	} else {
		var prevTempBranch string
		for i, mr := range candidates {
			head, tempBranch, err := editorial.RehearseBranch(e.git, target, mr.Branch)
			rehearsedHeads[i], rehearsalErrs[i] = head, err
			if prevTempBranch != "" {
				_ = e.git.DeleteBranch(prevTempBranch, true)
			}
			prevTempBranch = ""
			if err == nil {
				prevTempBranch = tempBranch
			}
		}
		if prevTempBranch != "" {
			_ = e.git.Checkout(target)
			_ = e.git.DeleteBranch(prevTempBranch, true)
		}
	}

	results := make([]editorial.ReviewResult, len(candidates))
	sem := make(chan struct{}, cfg.ReviewParallelism)
	var wg sync.WaitGroup
	for i, mr := range candidates {
		if err := rehearsalErrs[i]; err != nil {
			stderr := fmt.Sprintf("rehearsal failed: %v", err)
			_, _ = editorial.RecordFailure(deps.Recorder, e.rig.Name, mr.Worker, mr.ID, editorial.Tooling, stderr, 0)
			results[i] = editorial.ReviewResult{Exit: 2, Class: editorial.Tooling, Stderr: stderr}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, mr *MRInfo) {
			defer wg.Done()
			defer func() { <-sem }()
			attempt := mr.RetryCount + 1
			req := editorial.ReviewRequest{
				RigDir:        e.rig.Path,
				RepoDir:       e.workDir,
				MRID:          mr.ID,
				Worker:        mr.Worker,
				Rig:           e.rig.Name,
				Target:        target,
				Branch:        mr.Branch,
				RehearsedHead: rehearsedHeads[i],
				Attempt:       attempt,
				PriorFindings: editorial.BuildPriorFindings(e.beads, mr.SourceIssue, attempt),
				Config:        cfg,
			}
			results[i] = editorial.Run(ctx, req, deps)
		}(i, mr)
	}
	wg.Wait()

	approved = make([]*MRInfo, 0, len(candidates))
	reviewed = make([]ReviewedMR, 0, len(candidates))
	notes = make(map[string]*editorial.Note, len(candidates))
	for i, mr := range candidates {
		r := results[i]
		verdict := ""
		if r.Note != nil {
			verdict = r.Note.Verdict
		}
		reviewed = append(reviewed, ReviewedMR{ID: mr.ID, Exit: r.Exit, Verdict: verdict})
		if r.Exit == 0 {
			// The om-gate T6 push precondition reads
			// mr.EditorialReviewedHead off this same in-memory MRInfo
			// (editorial_gate.go's buildLandedMRs) — editorial.Run only
			// persists the reviewed head onto the MR bead
			// (setEditorialReviewedHead), which this batch's own MRInfo
			// pointer never re-reads. Without this, T6 refuses to push
			// every batch member reviewed for the first time here.
			mr.EditorialReviewedHead = r.Note.HeadSHA
			approved = append(approved, mr)
			notes[mr.ID] = r.Note
		} else {
			_, _ = fmt.Fprintf(e.output, "[Batch] MR %s: editorial review exit=%d, dropped from batch (left queued)\n", mr.ID, r.Exit)
		}
	}
	return approved, reviewed, notes
}

// ejectPatchIDChanged recomputes each stacked member's range patch-id as it
// actually sits on the built stack (target ← M1 ← M2 ← ... in first-parent
// order) and compares it to the patch-id its editorial note was reviewed
// under. A mismatch — e.g. a clean but auto-resolved three-way merge whose
// content ends up different from the standalone branch reviewed earlier —
// means the review no longer applies: the member is ejected (removed from
// the returned stack, reported in ejected, left queued and untouched — not
// a conflict, not a test-failure culprit).
//
// It is a no-op — returning stacked unchanged — when the rig has not set
// merge_queue.editorial.required. The caller must rebuild the stack from
// the returned kept list (e.g. via resetAndRebuildStack) before running
// gates whenever len(ejected) > 0, since this leaves the working tree as
// BuildRebaseStack built it — still containing any ejected member's merge.
//
// This compares each member's ON-STACK first-parent diff (target ←
// previous stack tip) to the patch-id its note was reviewed under — not
// the same range editorialPrecondition/CheckPrecondition (om-gate T6)
// checks at push time (mergeBase..submittedHead, off the standalone
// branch). The two can disagree: an earlier stack member touching nearby
// context lines can shift this range's diff even though the member's own
// branch is untouched. That is intentional here — this check exists to
// catch exactly that on-stack drift — but it means a member can pass one
// check and fail the other.
func (e *Engineer) ejectPatchIDChanged(stacked []*MRInfo, notes map[string]*editorial.Note, target string) (kept []*MRInfo, ejected []EjectedMR, err error) {
	if e.config.Editorial == nil || !e.config.Editorial.Required || len(stacked) == 0 {
		return stacked, nil, nil
	}

	n := len(stacked)
	tips, err := e.git.FirstParentLog("origin/"+target, "HEAD")
	if err != nil {
		return nil, nil, fmt.Errorf("first-parent log for %s: %w", target, err)
	}
	if len(tips) != n {
		return nil, nil, fmt.Errorf("first-parent log for %s: found %d commit(s), expected %d for %d MR(s)", target, len(tips), n, n)
	}
	baseSHA, err := e.git.Rev("origin/" + target)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving stack base: %w", err)
	}

	kept = make([]*MRInfo, 0, n)
	pre := baseSHA
	for i, mr := range stacked {
		post := tips[i]
		patchID, perr := e.git.PatchID(pre, post)
		if perr != nil {
			return nil, nil, fmt.Errorf("patch-id for %s: %w", mr.ID, perr)
		}
		note := notes[mr.ID]
		if note == nil || patchID != note.PatchID {
			_, _ = fmt.Fprintf(e.output, "[Batch] MR %s: patch-id changed on stack, ejecting (left queued)\n", mr.ID)
			ejected = append(ejected, EjectedMR{ID: mr.ID, Reason: "patch_id_changed_on_stack"})
		} else {
			kept = append(kept, mr)
		}
		pre = post
	}
	return kept, ejected, nil
}
