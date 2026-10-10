package cmd

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// gt bead review records an overseer review in the one form the attention
// queue reads (gt-eorhf). The queue answers a risk-path item by the exact head
// the landing record names (land.HasOverseerReviewNote), so a review written
// as a comment, or against the merge sha, leaves the item open. The head here
// is read from that landing record and never taken from the caller.

var beadReviewSHA, beadReviewReason string

var beadReviewCmd = &cobra.Command{
	Use:   "review <id> PASS|FAIL|WAIVED|AUDITED",
	Short: "Record an overseer review of a bead's landed head",
	Long: `Record an overseer review of a landed bead as the note the attention
queue reads: "OVERSEER REVIEW <head> PASS|FAIL", appended to the bead's
notes. Both verdicts clear the bead's risk-path item — a FAIL says a human
looked and is filing the follow-up.

WAIVED and AUDITED clear it for a landing that was reviewed another way (a
file-level audit) or consciously not reviewed, and need --reason: the note is
"OVERSEER REVIEW <head> WAIVED: <reason>". Without a reason nothing is
written. --reason is refused with PASS and FAIL.

<head> is the head the bead's landing record names, never a sha you pass.
--sha picks which head when the bead landed more than once; without it the
newest landing is reviewed. A --sha that is not a full 40-hex head in one of
that bead's landing records is refused.

A bead whose notes already carry a review of that head is left alone, so
recording the same verdict twice writes one line.

Examples:
  gt bead review gt-abc PASS
  gt bead review gt-abc FAIL --sha=<40-hex head of an earlier landing>
  gt bead review gt-abc AUDITED --reason "file-level audit of the diff, no findings"
  gt bead review gt-abc WAIVED --reason "docs-only change under a risk path"`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return newBeadVerbs(cmd).review(args[0], args[1], beadReviewSHA, beadReviewReason)
	},
}

// fullHead matches a git head as the landing records and the review note
// carry it: lowercase 40-hex.
var fullHead = regexp.MustCompile(`^[0-9a-f]{40}$`)

// review writes the overseer review line for the bead's landed head, or
// reports the bead as already reviewed. WAIVED and AUDITED carry reason into
// the line; PASS and FAIL take none.
func (v *beadVerbs) review(id, verdict, sha, reason string) error {
	verdict = strings.ToUpper(strings.TrimSpace(verdict))
	// NoteField folds newlines, so a reason cannot start a second note line.
	reason = land.NoteField(reason)
	switch verdict {
	case "PASS", "FAIL":
		if reason != "" {
			return fmt.Errorf("--reason applies to WAIVED and AUDITED, not %s", verdict)
		}
	case "WAIVED", "AUDITED":
		if reason == "" {
			return fmt.Errorf("a %s review needs --reason: say why the head is not getting a PASS", verdict)
		}
	default:
		return fmt.Errorf("review verdict must be PASS, FAIL, WAIVED or AUDITED, not %q", verdict)
	}
	sha = strings.ToLower(strings.TrimSpace(sha))
	if sha != "" && !fullHead.MatchString(sha) {
		return fmt.Errorf("--sha must be a full 40-hex head, not %q", sha)
	}
	is, err := v.client.Show(id)
	if err != nil {
		return err
	}
	head, err := v.reviewHead(id, sha)
	if err != nil {
		return err
	}
	line := land.OverseerReviewMarker + " " + head + " " + verdict
	if reason != "" {
		line += ": " + reason
	}
	if land.HasOverseerReviewNote(is.Notes, head) {
		_, err := fmt.Fprintf(v.out, "✓ %s already records %s\n", id, line)
		return err
	}
	if err := v.client.AppendNotes(id, line); err != nil {
		return err
	}
	_, err = fmt.Fprintf(v.out, "✓ Recorded %s on %s\n", line, id)
	return err
}

// landedOverseerHead returns the head an overseer review of bead id names:
// the bead's newest landing record, or the record whose head is sha when sha
// is given. It reads the same landings file the attention collector's
// risk-path item does and refuses when no record matches, because a bead with
// no landed head has none for the queue to answer.
func landedOverseerHead(townRoot, id, sha string) (string, error) {
	prefix := beads.ExtractPrefix(id)
	rig := beads.GetRigNameForPrefix(townRoot, prefix)
	if rig == "" {
		return "", fmt.Errorf("cannot tell which rig landed %s: no route for prefix %q in %s", id, prefix, townRoot)
	}
	lf, err := land.RigLandingsFile(townRoot, rig)
	if err != nil {
		return "", err
	}
	// Since(time.Time{}) reads every record: a review may come long after the
	// landing, so the queue's risk-path window is not this verb's. Records
	// come oldest first, so the last match is the newest landing of the bead.
	recs, err := lf.Since(time.Time{})
	if err != nil {
		return "", err
	}
	head := ""
	for _, rec := range recs {
		if rec.BeadID != id || rec.Head == "" || (sha != "" && rec.Head != sha) {
			continue
		}
		head = rec.Head
	}
	if head == "" {
		if sha != "" {
			return "", fmt.Errorf("%s has no landing record for head %s in %s", id, sha, lf.Path)
		}
		return "", fmt.Errorf("%s has no landing record in %s: a review names the head that landed", id, lf.Path)
	}
	return head, nil
}
