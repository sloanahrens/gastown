package editorial

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/deps"
	"github.com/steveyegge/gastown/internal/git"
)

// NotesRef is the git notes ref that carries the om verdict proof,
// attached to the reviewed head commit under refs/notes/<NotesRef>.
const NotesRef = "om"

// NoteAttempt is one verdict recorded for a reviewed head, frozen as it was
// written. Score and Verdict are what a re-review of the same diff can
// disagree with; ReviewedAt orders the history. ResolvedBackend is carried
// per-attempt, not just at the note's top level: a re-roll's score swing
// otherwise cannot be told apart from ordinary LLM nondeterminism (gt-bveg)
// if the backend that produced it changed between attempts and only the
// latest one is visible (gt-iqr6).
type NoteAttempt struct {
	Score           float64   `json:"score"`
	Verdict         string    `json:"verdict"`
	Attempt         int       `json:"attempt"`
	OMVersion       string    `json:"om_version,omitempty"`
	ResolvedBackend string    `json:"resolved_backend,omitempty"`
	FindingsCount   int       `json:"findings_count"`
	ReviewedAt      time.Time `json:"reviewed_at"`
}

// The gate's verdict schema is exactly these two, and every reader that
// validates a verdict routes through ValidVerdict rather than re-listing
// them: a verdict one reader admits and another does not is how a rejection
// gets recorded as a scored approval (gt-47nf).
const (
	VerdictApprove        = "approve"
	VerdictRequestChanges = "request_changes"
)

// ValidVerdict reports whether v is a verdict the gate can produce.
func ValidVerdict(v string) bool {
	return v == VerdictApprove || v == VerdictRequestChanges
}

// Note is the durable proof that om produced a verdict for a review. It is
// written to refs/notes/om on the reviewed head commit and survives DB
// flattens, wisp GC, and MR bead deletion — the receipt bead (see
// RecordReceipt) is for aggregation only, never proof.
type Note struct {
	OMVersion    string `json:"om_version"`
	RubricSHA256 string `json:"rubric_sha256"`
	// ResolvedBackend is om's own report of which review backend it
	// resolved and invoked for this review (verdictJSON.Backend) — e.g. the
	// backend argv or a model identifier — copied through verbatim when om
	// reports it. The backend is deliberately never pinned by the rig
	// manifest (see Manifest's doc comment): it is operator configuration
	// by design, precisely so changing it never requires a manifest
	// re-stamp. This field is instead how a swap becomes visible after the
	// fact, without pinning anything. Omitted when om's verdict carries no
	// such field — every om version before this one, and any note written
	// before this field existed — so those notes round-trip unchanged
	// (gt-iqr6).
	ResolvedBackend string `json:"resolved_backend,omitempty"`
	Rig             string `json:"rig"`
	MR              string `json:"mr"`
	Worker          string `json:"worker"`
	BaseSHA         string `json:"base_sha"`
	HeadSHA         string `json:"head_sha"`
	// ReviewedTargetTip is origin/<target>'s own tip, resolved at review
	// time — distinct from BaseSHA, which pins to the branch's own cut
	// point (the merge-base) and does not move as target advances past it.
	// A branch cut once and never rebased keeps the same merge-base no
	// matter how far target moves, so BaseSHA alone cannot answer "has
	// target moved since this review?" — the push precondition needs this
	// field to tell (gt-6bsp). Omitted when empty: a landed review has no
	// "since review" window to measure and leaves it unset, and a note
	// written before this field existed round-trips unchanged — the
	// precondition treats either case as unknown and skips the drift check
	// rather than guessing.
	ReviewedTargetTip string  `json:"reviewed_target_tip,omitempty"`
	PatchID           string  `json:"patch_id"`
	Score             float64 `json:"score"`
	// Verdict is "approve" or "request_changes".
	Verdict       string `json:"verdict"`
	FindingsCount int    `json:"findings_count"`
	// Findings is the full per-finding detail (id/severity/path/line/title)
	// behind FindingsCount, om's raw verdict output. Omitted when empty so
	// notes written before this field round-trip unchanged; a reader that
	// only needs the count keeps using FindingsCount.
	Findings      []Finding `json:"findings,omitempty"`
	PriorFindings struct {
		Resolved   []string `json:"resolved"`
		Unresolved []string `json:"unresolved"`
		Regressed  []string `json:"regressed"`
	} `json:"prior_findings"`
	Attempt    int       `json:"attempt"`
	ReviewedAt time.Time `json:"reviewed_at"`

	// Attempts is every verdict this note's diff has been given, oldest
	// first, the last entry repeating the top-level score/verdict/attempt
	// above. An om review is an LLM call, so a second invocation on an
	// unchanged diff re-rolls a near-threshold score rather than measuring
	// anything, and the verdict that comes back can differ from the one it
	// replaces (gt-bveg). Written only when there is more than one attempt,
	// so a re-roll is visible on the proof instead of silent and every
	// other note round-trips unchanged. Absent means a single recorded
	// verdict or a note predating this field; the top-level fields are the
	// latest verdict either way.
	//
	// The history is keyed by diff, not by commit: the MR path rehearses a
	// fresh merge commit on every invocation, so the verdict a re-roll
	// replaces is almost never attached to the commit the new note is
	// written on (gt-qa2p). Its entries are read from whichever note
	// FindVerdictForDiff selected as the one being replaced.
	Attempts []NoteAttempt `json:"attempts,omitempty"`

	// TimeoutSeconds records a backend-timeout override this review ran
	// with (ReviewRequest.TimeoutSeconds, the CLI's --timeout flag), so the
	// override is auditable from refs/notes/om instead of living only in
	// the invoker's shell history — and specifically so a later reader can
	// tell a review that used the rig's .om.json default apart from one
	// that was allowed longer. Omitted from the JSON when zero (no
	// override), so existing notes and default-path reviews round-trip
	// unchanged.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`

	// Followups holds the ids of the follow-up beads filed for major
	// findings on an approve verdict (DECISION 8: approval never dissolves
	// a finding). Omitted from the JSON when empty so existing notes
	// without this field still round-trip.
	Followups []string `json:"followups,omitempty"`

	// RubricRetirement is set when this review let a rubric change through
	// the criterion-deletion guard because the MR bead carried
	// RetirementLabel: the reviewed diff drops or rewrites a criterion, and
	// the note is where that survives the MR bead (gt-2oi0). Omitted when
	// false, so reviews that touched no criterion round-trip unchanged.
	RubricRetirement bool `json:"rubric_retirement,omitempty"`

	// RetroReview marks a verdict produced by a review of an already-landed
	// commit (gt mq review --landed) rather than before its merge. HeadSHA is
	// then the landed commit the note is stamped on — not a rehearsal head or
	// a branch tip — so a reader auditing what the gate saw before a merge
	// can tell this verdict was not part of that. Omitted when false, so
	// every pre-merge note round-trips unchanged.
	RetroReview bool `json:"retro_review,omitempty"`
	// ReviewHead is the branch head a retro-review diffed, when it is not
	// HeadSHA: for a merge the note must sit on the merge commit for the
	// coverage check to find it, while the change under review is the head
	// the merge brought in. Omitted otherwise — including on every review
	// written before a merge, where the two are the same commit — so a reader
	// reconstructing the reviewed range from BaseSHA and HeadSHA never has to
	// guess which of the two it is holding.
	ReviewHead string `json:"review_head,omitempty"`

	// The fields below are written only by the auditable backfill (gt mq
	// rekey-note), never by gt mq review: they record that this note was
	// copied onto a different commit than the one the review wrote it on,
	// and why that copy was legitimate. A note without Backfill is a
	// first-hand verdict; one with Backfill is a re-keyed copy whose
	// patch-id was recomputed and verified equal before the copy. All are
	// omitted when empty, so review-written notes and pre-existing
	// backfilled notes round-trip unchanged.

	// RekeyedFrom is the commit the source note was attached to — the
	// reviewed head, or a rehearsal head the merge queue discarded.
	RekeyedFrom string `json:"rekeyed_from,omitempty"`
	// RekeyedFromMR is the MR id the source note was written under, when
	// AllowAnyMR borrowed it from an MR other than the one this note is now
	// keyed to (MR, above, is overwritten to the requesting MR so lookups
	// by that MR's id find it). Empty when the source note already belonged
	// to the requesting MR, so an ordinary rekey round-trips unchanged. An
	// auditor reading the published note can otherwise not tell which MR's
	// review the borrowed proof really came from (gt-bagu).
	RekeyedFromMR string `json:"rekeyed_from_mr,omitempty"`
	// Backfill marks a note created by the backfill command rather than by
	// a review of this commit.
	Backfill bool `json:"backfill,omitempty"`
	// BackfillReason is the operator's justification for the copy, plus the
	// patch-id verification the command performed automatically.
	BackfillReason string `json:"backfill_reason,omitempty"`
	// BackfilledBy is who ran the backfill (git user.name by default).
	BackfilledBy string `json:"backfilled_by,omitempty"`
	// BackfilledAt is when the backfill ran. A pointer so a review-written
	// note omits the field entirely rather than stamping the zero time.
	BackfilledAt *time.Time `json:"backfilled_at,omitempty"`
	// RequestedBy names who asked for the backfill, when the operator knows
	// (e.g. an overseer bead id).
	RequestedBy string `json:"requested_by,omitempty"`
	// PatchIDVerified is set on every backfilled note: the command
	// recomputed patch-id(git diff <target>^ <target>) and confirmed it
	// equals PatchID before copying, which is what makes the note reachable
	// proof on that commit.
	PatchIDVerified bool `json:"patch_id_verified,omitempty"`
}

// omVersionBelowFloor reports whether v is below min, the bar a note's
// recorded om version must clear to count as proof. Empty and "dev" (the
// unset-ldflags default) are below any floor, matching AssertVersion —
// CompareVersions would otherwise silently map them to 0.0.0. An empty min is
// no floor at all.
func omVersionBelowFloor(v, min string) bool {
	if min == "" {
		return false
	}
	return v == "" || v == "dev" || deps.CompareVersions(v, min) < 0
}

// attemptOf renders a note's own top-level verdict as a history entry.
func attemptOf(n *Note) NoteAttempt {
	return NoteAttempt{
		Score:           n.Score,
		Verdict:         n.Verdict,
		Attempt:         n.Attempt,
		OMVersion:       n.OMVersion,
		ResolvedBackend: n.ResolvedBackend,
		FindingsCount:   n.FindingsCount,
		ReviewedAt:      n.ReviewedAt,
	}
}

// attemptHistory returns the ordered history to record alongside current on a
// head that already carried prev: prev's own history, or prev's top-level
// verdict when prev predates Attempts, followed by current. prev may be nil,
// in which case the result holds current alone.
func attemptHistory(prev *Note, current NoteAttempt) []NoteAttempt {
	history := make([]NoteAttempt, 0, 2)
	if prev != nil {
		if len(prev.Attempts) > 0 {
			history = append(history, prev.Attempts...)
		} else {
			history = append(history, attemptOf(prev))
		}
	}
	return append(history, current)
}

// WriteNote marshals n and attaches it as a git note on n.HeadSHA under
// NotesRef, overwriting any note already there.
func WriteNote(g *git.Git, n Note) error {
	data, err := json.Marshal(n)
	if err != nil {
		return fmt.Errorf("marshal note: %w", err)
	}
	if err := g.NotesAdd(NotesRef, n.HeadSHA, string(data)); err != nil {
		return fmt.Errorf("write note on %s: %w", n.HeadSHA, err)
	}
	return nil
}

// ReadNote reads and unmarshals the note on sha under NotesRef. It passes
// git.ErrNoNote through unchanged so callers can distinguish "not reviewed"
// from a real error.
func ReadNote(g *git.Git, sha string) (*Note, error) {
	content, err := g.NotesShow(NotesRef, sha)
	if err != nil {
		return nil, err
	}
	var n Note
	if err := json.Unmarshal([]byte(content), &n); err != nil {
		return nil, fmt.Errorf("unmarshal note on %s: %w", sha, err)
	}
	return &n, nil
}

// RecordedVerdict is a verdict found under NotesRef together with the commit
// its note is attached to — the commit a reader must name to read the
// verdict back (ReadNote) or to copy it forward (CopyNotesToLanded, which
// uses git notes copy). That commit is not necessarily the note's own
// HeadSHA field: a copy leaves the field naming the commit the verdict was
// first written on, and an annotated object need not exist at all for its
// note to be readable.
type RecordedVerdict struct {
	Commit string
	Note   Note
}

// FindVerdictForDiff returns the verdict that already answers for a diff, or
// nil when the notes ref holds none: the most recently reviewed note whose
// patch-id and deployed rubric match and whose verdict is one this pipeline
// would accept, at or above minVersion. It is recordedVerdictApplies, run
// over the whole notes ref instead of one known commit.
//
// The lookup is keyed on the diff rather than on the commit the note sits on
// because the MR path has no stable commit to key on. gt mq review rehearses
// the branch onto its target afresh on every invocation, so the rehearsal
// head — the commit a note is written on — differs on every call for a
// byte-identical diff. Keying reuse on that head left the gt-bveg guard inert
// on exactly the path it exists for: three invocations against one MR
// produced three verdicts for one patch-id (0.56, 0.62, 0.84 against a 0.60
// threshold), so a caller could roll a rejected diff until it cleared
// (gt-qa2p). Notes on rehearsal heads are also routinely unreachable from any
// branch by the time the verdict is needed, which is why the notes ref rather
// than the commit graph is what is scanned (see git.Git.NotesList).
//
// The most recently reviewed match governs, because that is the verdict the
// MR's push is authorized by: an answer that disagreed with what
// CheckPrecondition will accept would wedge the MR on a review reporting
// approve. The MR id is deliberately not part of the key — a re-minted or
// resubmitted MR carrying the same diff is the same measurement, and scoping
// the lookup to one MR would hand a caller the same roll-until-it-clears
// bypass one bead id further along. A deliberate --reroll is what supersedes
// a recorded verdict, and it says so on the proof (see Note.Attempts).
func FindVerdictForDiff(g *git.Git, patchID, rubricSHA, minVersion string) (*RecordedVerdict, error) {
	entries, err := g.NotesList(NotesRef)
	if err != nil {
		return nil, fmt.Errorf("list notes in refs/notes/%s: %w", NotesRef, err)
	}
	var best *RecordedVerdict
	for _, e := range entries {
		var n Note
		if err := json.Unmarshal([]byte(e.Content), &n); err != nil {
			// The ref is shared with every writer that ever touched it, so
			// an unreadable note elsewhere must not make this diff's
			// verdict unfindable.
			continue
		}
		if !recordedVerdictApplies(&n, patchID, rubricSHA, minVersion) {
			continue
		}
		if best == nil || reviewedLaterThan(n, best.Note) {
			best = &RecordedVerdict{Commit: e.Annotated, Note: n}
		}
	}
	return best, nil
}

// reviewedLaterThan reports whether a is a later verdict than b: by when it
// was reviewed, and by reviewed head when two were reviewed in the same
// instant (a batch reviewing the same diff from two branches), so which
// verdict governs is deterministic rather than dependent on the order the
// notes ref happens to list them in.
func reviewedLaterThan(a, b Note) bool {
	if !a.ReviewedAt.Equal(b.ReviewedAt) {
		return a.ReviewedAt.After(b.ReviewedAt)
	}
	return a.HeadSHA > b.HeadSHA
}
