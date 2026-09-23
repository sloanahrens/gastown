package editorial

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// RekeyRequest is one auditable backfill: copy the om note for an MR onto
// the commit that actually landed — and, with SecondParent, onto that
// merge's second parent (the polecat head it brought in) — when the note's
// patch-id still matches what landed.
//
// This is the manual counterpart to the push path's copy (CopyNotesToLanded,
// editoral_gate.go). The push path can only act while it still holds the
// reviewed note; when a merge lands before the note is reachable (gt-qvxf:
// the note was keyed to a rehearsal head the merge queue discarded) the
// landed commit is left without proof, and until now the only remedy was
// somebody hand-writing a note. A backfill is the audited version of that
// hand copy: it refuses when the proof would not actually hold, and it
// stamps how the note got there.
type RekeyRequest struct {
	// MR is the merge request bead id the note belongs to. The note is
	// located by the "mr" field inside it, never by the MR bead: MR beads
	// are wisps and are routinely reaped, while the note is the durable
	// record (gt-wisp-c304 and gt-wisp-qb0y, the two backfills this command
	// exists for, are both gone from beads).
	MR string
	// Landed is the commit that landed on Target: sha, short sha, or ref.
	Landed string
	// Target is the branch Landed landed on. Landed must be reachable from
	// origin/Target or the backfill is refused — re-keying proof onto a
	// commit that never landed would be an unaudited provenance claim.
	Target string
	// SecondParent also stamps the note on Landed's second parent, the
	// polecat head a merge commit brought in.
	SecondParent bool
	// AllowAnyMR accepts, when mr has no note of its own, the note at
	// SourceHead even though its "mr" field names a different MR. Without
	// this, a resume whose editorial_reviewed_head names a note written
	// under an earlier or unrelated MR id can never find proof for its own
	// MR id and refuses forever (gt-bagu). The manual `gt mq rekey-note`
	// command leaves both false: an operator naming an MR deliberately
	// wants proof that MR's own note covers what landed.
	AllowAnyMR bool
	// SourceHead is the commit AllowAnyMR reads its borrowed note from.
	// Required whenever AllowAnyMR is set. It is deliberately a single named
	// commit, not a patch-id scan of refs/notes/om: a scan admits any
	// approve note anywhere with a matching patch-id, which is more than
	// CheckPrecondition or FindVerdictForDiff would accept — it ignores the
	// rubric sha, the om version floor, and a later request_changes verdict
	// on the same diff (gt-bagu, om review 0.55). Passing SourceHead as
	// editorial_reviewed_head — the exact head CheckPrecondition already
	// read and required to carry approve before this diff was allowed to
	// land — means the backfill can never accept proof the push itself
	// would not have.
	SourceHead string
	// Reason is the operator's justification for the copy, required. The
	// command records it verbatim next to the patch-id verification it
	// performs itself, so the note says both who claims the copy was
	// legitimate and what the machine checked.
	Reason string
	// RequestedBy optionally names who asked for the backfill (an overseer
	// bead id, for instance).
	RequestedBy string
	// Author records who ran the backfill; defaults to git user.name.
	Author string
}

// RekeyResult describes what a backfill did.
type RekeyResult struct {
	// Note is the re-keyed note as written (the last one written when
	// several targets were stamped).
	Note Note
	// SourceCommit is the commit the source note was attached to.
	SourceCommit string
	// Targets lists every commit that must carry the note: the landed
	// commit, plus its second parent when requested.
	Targets []string
	// Written lists the targets this run actually stamped. The rest already
	// carried a matching note for the MR, so the run was a republish.
	Written []string
	// PatchID is the verified patch-id shared by the note and every target.
	PatchID string
	// Pushed reports whether refs/notes/om was published to origin.
	Pushed bool
}

// RekeyNote re-keys the om note for req.MR onto the commit(s) that landed.
//
// Every check runs before anything is written, so a refusal leaves the notes
// ref untouched: the landed commit must be reachable from origin/Target, a
// note for the MR must exist under refs/notes/om, it must be an approve
// verdict, its patch-id must equal each target's own diff patch-id (with
// SecondParent, the second parent's included), and no target may already
// carry another MR's approve note for the same diff (a notes ref holds one
// note per commit, so stamping there would replace that verdict's proof).
//
// On success each target that does not already carry a matching note gets
// one, stamped with rekeyed_from, backfill, backfill_reason, backfilled_by,
// backfilled_at and patch_id_verified, and the notes ref is pushed. A push
// failure is returned as an error alongside the partial result: the notes
// exist locally but are invisible to every other clone (including the
// coverage check) until the push succeeds, which callers must report rather
// than swallow.
func RekeyNote(g *git.Git, req RekeyRequest) (*RekeyResult, error) {
	mr := strings.TrimSpace(req.MR)
	landedArg := strings.TrimSpace(req.Landed)
	target := strings.TrimSpace(req.Target)
	reason := strings.TrimSpace(req.Reason)

	if mr == "" {
		return nil, fmt.Errorf("rekey-note: merge request id is required")
	}
	if landedArg == "" {
		return nil, fmt.Errorf("rekey-note: landed commit is required")
	}
	if target == "" {
		return nil, fmt.Errorf("rekey-note: target branch is required")
	}
	if reason == "" {
		return nil, fmt.Errorf("rekey-note: a reason is required: a backfill must say why the copy is legitimate, not just that it happened")
	}

	landed, err := g.Rev(landedArg + "^{commit}")
	if err != nil {
		return nil, fmt.Errorf("rekey-note: resolve landed commit %s: %w", landedArg, err)
	}
	landed = strings.TrimSpace(landed)

	// Re-key only stamps proof onto commits that actually landed. A note on
	// an unlanded commit proves nothing about the target branch, so refuse
	// rather than publish a claim nobody can check.
	targetRef := "origin/" + target
	reachable, err := g.IsAncestor(landed, targetRef)
	if err != nil {
		return nil, fmt.Errorf("rekey-note: cannot tell whether %s landed on %s: %w (fetch the rig clone and retry)", landed, targetRef, err)
	}
	if !reachable {
		return nil, fmt.Errorf("rekey-note: refusing: %s is not reachable from %s — re-key-note only stamps commits that landed; fetch and retry, or pass the branch it landed on", landed, targetRef)
	}

	parents, err := g.Parents(landed)
	if err != nil {
		return nil, fmt.Errorf("rekey-note: parents of landed commit %s: %w", landed, err)
	}
	if len(parents) == 0 {
		return nil, fmt.Errorf("rekey-note: refusing: %s is a root commit, so there is no landed diff to match a note against", landed)
	}

	landedPatchID, err := g.PatchID(parents[0], landed)
	if err != nil {
		return nil, fmt.Errorf("rekey-note: patch-id of landed diff %s..%s: %w", parents[0], landed, err)
	}

	// plan is one entry per commit that must carry the note, each paired
	// with the parent its own diff (and so its patch-id) is taken against.
	plan := []rekeyTarget{{commit: landed, parent: parents[0], patchID: landedPatchID}}
	if req.SecondParent {
		if len(parents) < 2 {
			return nil, fmt.Errorf("rekey-note: refusing: --second-parent given but %s is not a merge commit (it has %d parent(s))", landed, len(parents))
		}
		sp := parents[1]
		spParents, err := g.Parents(sp)
		if err != nil {
			return nil, fmt.Errorf("rekey-note: parents of second parent %s: %w", sp, err)
		}
		if len(spParents) == 0 {
			return nil, fmt.Errorf("rekey-note: refusing: second parent %s is a root commit, so there is no diff to match a note against", sp)
		}
		spPatchID, err := g.PatchID(spParents[0], sp)
		if err != nil {
			return nil, fmt.Errorf("rekey-note: patch-id of second parent diff %s..%s: %w", spParents[0], sp, err)
		}
		plan = append(plan, rekeyTarget{commit: sp, parent: spParents[0], patchID: spPatchID})
	}

	found, err := loadNotesForMR(g, mr, plan, landedPatchID)
	if err != nil {
		return nil, err
	}

	var source rekeySource
	borrowedMR := ""
	switch {
	case len(found.forMR) > 0:
		// A note whose patch-id does not match the diff it would be stamped
		// on is not proof: the editorial-coverage check recomputes
		// patch-id(diff X^ X) for every first-parent commit and rejects a
		// mismatch, so copying it would publish a note that still reads as
		// uncovered. Refuse before writing anything.
		source, err = selectSourceNote(mr, landed, landedPatchID, plan, found.forMR)
		if err != nil {
			return nil, err
		}
	case req.AllowAnyMR:
		sourceHead := strings.TrimSpace(req.SourceHead)
		if sourceHead == "" {
			return nil, fmt.Errorf("rekey-note: AllowAnyMR requires SourceHead — the commit whose note is trusted as proof (normally editorial_reviewed_head)")
		}
		note, err := ReadNote(g, sourceHead)
		if err != nil {
			return nil, fmt.Errorf("rekey-note: refusing: AllowAnyMR could not read a note at source head %s: %w", shortCommit(sourceHead), err)
		}
		if note.Verdict != "approve" {
			return nil, fmt.Errorf("rekey-note: refusing: the note at source head %s is verdict %q, not approve — AllowAnyMR only borrows proof the push precondition would itself accept", shortCommit(sourceHead), note.Verdict)
		}
		if note.PatchID != landedPatchID {
			return nil, fmt.Errorf("rekey-note: refusing: patch-id mismatch — the note at source head %s carries patch-id %s, but the landed diff of %s has patch-id %s", shortCommit(sourceHead), note.PatchID, landed, landedPatchID)
		}
		source = rekeySource{commit: sourceHead, note: *note}
		borrowedMR = note.MR
	default:
		// Nothing to copy. The hint is the useful part: an operator who has
		// the landed sha but guessed the MR id is told which MRs actually
		// own this diff, and a clone whose notes ref has not caught up is
		// told how to fetch it.
		hint := ""
		if len(found.covering) > 0 {
			hint = fmt.Sprintf("; notes that do cover this diff belong to %s", strings.Join(found.covering, ", "))
		}
		return nil, fmt.Errorf("rekey-note: no om note for MR %s in refs/notes/%s — a backfill copies an existing verdict, it never invents one%s (if the note was written in another clone, fetch it first: git fetch origin '+refs/notes/%s:refs/notes/%s')",
			mr, NotesRef, hint, NotesRef, NotesRef)
	}
	if borrowedMR != "" && borrowedMR != mr {
		// The note now proves mr's landing, not the MR it was originally
		// written under — record which MR it came from (gt-bagu) and re-key
		// it so a later lookup by mr.ID finds it too.
		source.note.RekeyedFromMR = borrowedMR
		source.note.MR = mr
	}
	for _, t := range plan {
		if t.patchID != source.note.PatchID {
			return nil, fmt.Errorf("rekey-note: refusing: patch-id mismatch — the diff %s..%s has patch-id %s, but MR %s's note carries %s; what landed is not what was reviewed",
				t.parent, t.commit, t.patchID, mr, source.note.PatchID)
		}
	}

	result := &RekeyResult{
		SourceCommit: source.commit,
		PatchID:      source.note.PatchID,
		Targets:      make([]string, 0, len(plan)),
		Written:      make([]string, 0, len(plan)),
	}
	var pending []rekeyTarget
	for _, t := range plan {
		result.Targets = append(result.Targets, t.commit)
		// One note per commit per ref: writing here would replace whatever
		// is already there. Another MR's valid proof for the same diff is
		// not ours to replace.
		if other := foreignCoveringNote(found.byTarget[t.commit], source.note.PatchID, mr); other != "" {
			return nil, fmt.Errorf("rekey-note: refusing: %s already carries MR %s's approve note for the same diff — stamping MR %s there would replace that verdict's proof; re-key %s instead, or resolve it deliberately if the two MRs really are the same change",
				shortCommit(t.commit), other, mr, other)
		}
		if hasMatchingNote(found.byTarget[t.commit], source.note.PatchID) {
			continue
		}
		pending = append(pending, t)
	}

	if len(pending) == 0 {
		// Every target already carries a matching note. Nothing to stamp,
		// but still publish: the run that wrote them may have failed to
		// push, and re-publishing is the one thing a re-run can repair.
		result.Note = noteOnTarget(found.byTarget, landed, source.note.PatchID)
	} else {
		now := time.Now().UTC()
		author := strings.TrimSpace(req.Author)
		if author == "" {
			if v, err := g.ConfigGet("user.name"); err == nil {
				author = strings.TrimSpace(v)
			}
		}

		for _, t := range pending {
			n := source.note
			n.HeadSHA = t.commit
			n.RekeyedFrom = source.commit
			n.Backfill = true
			n.BackfillReason = fmt.Sprintf("%s; patch-id of git diff %s %s verified equal", reason, t.parent, t.commit)
			n.BackfilledBy = author
			backfilledAt := now
			n.BackfilledAt = &backfilledAt
			if requestedBy := strings.TrimSpace(req.RequestedBy); requestedBy != "" {
				n.RequestedBy = requestedBy
			}
			n.PatchIDVerified = true

			if err := WriteNote(g, n); err != nil {
				return result, fmt.Errorf("rekey-note: write note on %s: %w", t.commit, err)
			}
			result.Written = append(result.Written, t.commit)
			result.Note = n
		}
	}

	if err := g.PushNotes("origin", NotesRef); err != nil {
		return result, fmt.Errorf("rekey-note: note(s) written locally but NOT published: pushing refs/notes/%s to origin failed: %w — other clones, and the editorial-coverage check, cannot see this proof until the push succeeds; fetch the notes ref (git fetch origin '+refs/notes/%s:refs/notes/%s') and re-run so the note is re-published", NotesRef, err, NotesRef, NotesRef)
	}
	result.Pushed = true
	return result, nil
}

// rekeyTarget is one commit that must carry the note, with the parent its
// own diff is computed against.
type rekeyTarget struct {
	commit  string
	parent  string
	patchID string
}

// rekeySource is a candidate note to copy, with the commit it sits on.
type rekeySource struct {
	commit string
	note   Note
}

// notesForMR is what one pass over the notes ref yields for a backfill.
type notesForMR struct {
	// forMR holds the notes that belong to the requested MR.
	forMR []rekeySource
	// byTarget holds, per planned target, the notes already attached to it.
	byTarget map[string][]Note
	// covering names the other MRs with an approve note whose patch-id
	// equals the landed diff — the hints a "no note for that MR" refusal
	// hands back. AllowAnyMR does not use this: it reads RekeyRequest.
	// SourceHead directly rather than picking from this set (gt-bagu).
	covering []string
}

// loadNotesForMR reads refs/notes/om once and sorts the notes into the shape
// the backfill needs. Notes that do not parse are skipped: the notes ref is
// shared with every writer that ever touched it, and an unreadable note
// elsewhere must not make this MR's note unfindable.
func loadNotesForMR(g *git.Git, mr string, plan []rekeyTarget, landedPatchID string) (*notesForMR, error) {
	entries, err := g.NotesList(NotesRef)
	if err != nil {
		return nil, fmt.Errorf("rekey-note: list notes in refs/notes/%s: %w", NotesRef, err)
	}
	found := &notesForMR{byTarget: make(map[string][]Note, len(plan))}
	for _, e := range entries {
		var n Note
		if err := json.Unmarshal([]byte(e.Content), &n); err != nil {
			continue
		}
		// Every note on a planned target is recorded, whoever it belongs
		// to: a target already carrying another MR's proof for the same
		// diff must not be silently overwritten (see the write loop).
		for _, t := range plan {
			if t.commit == e.Annotated {
				found.byTarget[t.commit] = append(found.byTarget[t.commit], n)
			}
		}
		if n.MR == mr {
			found.forMR = append(found.forMR, rekeySource{commit: e.Annotated, note: n})
			continue
		}
		if n.Verdict == "approve" && n.PatchID == landedPatchID {
			found.covering = append(found.covering, fmt.Sprintf("%s (on %s)", n.MR, shortCommit(e.Annotated)))
		}
	}
	sort.Strings(found.covering)
	return found, nil
}

// foreignCoveringNote returns the MR id of another MR's approve note for the
// same diff that already sits on target, or "" — the case where stamping
// this MR's note would replace that verdict's proof on the commit.
func foreignCoveringNote(notes []Note, patchID, mr string) string {
	for _, n := range notes {
		if n.MR != mr && n.Verdict == "approve" && n.PatchID == patchID {
			return n.MR
		}
	}
	return ""
}

// shortCommit abbreviates a sha for an error message.
func shortCommit(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// hasMatchingNote reports whether any note on that commit is an approve
// verdict whose patch-id matches — the test the editorial-coverage check
// applies before it counts a landed commit as covered.
func hasMatchingNote(notes []Note, patchID string) bool {
	for _, n := range notes {
		if n.Verdict == "approve" && n.PatchID == patchID {
			return true
		}
	}
	return false
}

// noteOnTarget returns the matching note already attached to target, or the
// zero Note when there is none.
func noteOnTarget(byTarget map[string][]Note, target, patchID string) Note {
	for _, n := range byTarget[target] {
		if n.Verdict == "approve" && n.PatchID == patchID {
			return n
		}
	}
	return Note{}
}

// selectSourceNote picks the note to copy: the approve note for mr whose
// patch-id equals the landed diff's. A note attached to a commit we are
// about to stamp is a last resort — rekeyed_from should name the commit the
// verdict was actually written on, not the target itself — and candidates
// are ordered by commit sha so a re-run of the same command is reproducible.
func selectSourceNote(mr, landed, landedPatchID string, plan []rekeyTarget, forMR []rekeySource) (rekeySource, error) {
	var candidates []rekeySource
	var carried []string
	approved := 0
	for _, s := range forMR {
		carried = append(carried, fmt.Sprintf("patch-id %s on %s (verdict %q)", s.note.PatchID, s.commit, s.note.Verdict))
		if s.note.Verdict == "approve" {
			approved++
			if s.note.PatchID == landedPatchID {
				candidates = append(candidates, s)
			}
		}
	}
	if len(candidates) == 0 {
		// Distinguish the two refusals: a verdict that does not approve, and
		// an approve verdict that reviews something else.
		what := "patch-id mismatch: the landed diff of " + landed + " has patch-id " + landedPatchID + ", but no approve note for MR " + mr + " carries it"
		if approved == 0 {
			what = "no approve verdict for MR " + mr + " (patch-id of the landed diff is " + landedPatchID + ")"
		}
		return rekeySource{}, fmt.Errorf("rekey-note: refusing: %s; notes for this MR: %s", what, strings.Join(carried, ", "))
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].commit < candidates[j].commit })
	for _, c := range candidates {
		if !isPlannedTarget(plan, c.commit) {
			return c, nil
		}
	}
	return candidates[0], nil
}

// isPlannedTarget reports whether commit is one of the commits the backfill
// is about to stamp.
func isPlannedTarget(plan []rekeyTarget, commit string) bool {
	for _, t := range plan {
		if t.commit == commit {
			return true
		}
	}
	return false
}
