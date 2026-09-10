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

	// Followups holds the ids of the follow-up beads filed for major
	// findings on an approve verdict (DECISION 8: approval never dissolves
	// a finding). Omitted from the JSON when empty so existing notes
	// without this field still round-trip.
	Followups []string `json:"followups,omitempty"`
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
