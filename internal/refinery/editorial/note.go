package editorial

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// NotesRef is the git notes ref that carries the om verdict proof,
// attached to the reviewed head commit under refs/notes/<NotesRef>.
const NotesRef = "om"

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
