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
// disagree with; ReviewedAt orders the history.
type NoteAttempt struct {
	Score         float64   `json:"score"`
	Verdict       string    `json:"verdict"`
	Attempt       int       `json:"attempt"`
	OMVersion     string    `json:"om_version,omitempty"`
	FindingsCount int       `json:"findings_count"`
	ReviewedAt    time.Time `json:"reviewed_at"`
}

// Note is the durable proof that om produced a verdict for a review. It is
// written to refs/notes/om on the reviewed head commit and survives DB
// flattens, wisp GC, and MR bead deletion — the receipt bead (see
// RecordReceipt) is for aggregation only, never proof.
type Note struct {
	OMVersion    string  `json:"om_version"`
	RubricSHA256 string  `json:"rubric_sha256"`
	Rig          string  `json:"rig"`
	MR           string  `json:"mr"`
	Worker       string  `json:"worker"`
	BaseSHA      string  `json:"base_sha"`
	HeadSHA      string  `json:"head_sha"`
	PatchID      string  `json:"patch_id"`
	Score        float64 `json:"score"`
	// Verdict is "approve" or "request_changes".
	Verdict       string `json:"verdict"`
	FindingsCount int    `json:"findings_count"`
	PriorFindings struct {
		Resolved   []string `json:"resolved"`
		Unresolved []string `json:"unresolved"`
		Regressed  []string `json:"regressed"`
	} `json:"prior_findings"`
	Attempt    int       `json:"attempt"`
	ReviewedAt time.Time `json:"reviewed_at"`

	// Attempts is every verdict recorded for this head, oldest first, the
	// last entry repeating the top-level score/verdict/attempt above. An
	// om review is an LLM call, so a second invocation on an unchanged diff
	// re-rolls a near-threshold score rather than measuring anything, and
	// the verdict that comes back can differ from the one it replaces
	// (gt-bveg). Written only when there is more than one attempt, so a
	// re-roll is visible on the proof instead of silent and every other
	// note round-trips unchanged. Absent means a single recorded verdict or
	// a note predating this field; the top-level fields are the latest
	// verdict either way.
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
		Score:         n.Score,
		Verdict:       n.Verdict,
		Attempt:       n.Attempt,
		OMVersion:     n.OMVersion,
		FindingsCount: n.FindingsCount,
		ReviewedAt:    n.ReviewedAt,
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
