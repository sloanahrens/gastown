package land

import (
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/git"
)

// MergeRejectionNoteMarker opens every rejection block on a work bead. The
// polecat work formula, the deacon's redispatch and gt done's
// unchanged-since-rejection refusal all read it (dispatch owns the constant).
const MergeRejectionNoteMarker = dispatch.MergeRejectionNoteMarker

// LandingNoteMarker opens the landing record block on a work bead.
const LandingNoteMarker = "LANDING RECORD"

// Receipt is the om score and still-unresolved finding ids a rejection
// carries for the deacon's convergence rule (deacon.ParseEditorialReceiptFromNotes).
type Receipt struct {
	Score      float64
	Unresolved []string
}

// RejectionNote is one MERGE REJECTION block. The first part (header,
// Branch, Target, MR, findings, receipt) is the refinery's format, moved here
// unchanged so every reader keeps working (gt-s4f6). Head, Conflicting and
// GateTail are Land's additions and are written only when set.
type RejectionNote struct {
	Attempt  int
	Kind     string // failure class; empty means unclassified (gt-1jig)
	Reason   string
	Branch   string
	Target   string
	MR       string
	Findings []Finding
	Receipt  *Receipt

	// Head is the exact tip that was rejected. gt done compares a resubmission
	// against it (unchanged-since-rejection), so a rejection with no MR bead
	// still names its content.
	Head        string
	Conflicting []string
	GateTail    string
}

// FormatRejectionNote renders n. Every agent-supplied field is collapsed onto
// one line, and the gate tail is indented and stripped of the markers other
// readers split on, so no output line can forge a block or a field.
func FormatRejectionNote(n RejectionNote) string {
	attempt := n.Attempt
	if attempt < 1 {
		attempt = 1
	}
	header := fmt.Sprintf("%s (attempt %d): ", MergeRejectionNoteMarker, attempt)
	if class := NoteField(n.Kind); class != "" {
		header += class + " - "
	}
	note := header + NoteField(n.Reason) +
		fmt.Sprintf("\nBranch: %s\nTarget: %s\nMR: %s", n.Branch, n.Target, n.MR)
	for _, f := range n.Findings {
		note += fmt.Sprintf("\n- id:%s sev:%s %s:%d — %s",
			NoteField(f.ID), NoteField(f.Severity), NoteField(f.Path), f.Line, NoteField(f.Title))
	}
	if n.Receipt != nil {
		note += fmt.Sprintf("\nScore: %.4f", n.Receipt.Score)
		if len(n.Receipt.Unresolved) > 0 {
			note += fmt.Sprintf("\nUnresolved: %s", strings.Join(n.Receipt.Unresolved, ","))
		}
	}
	if head := NoteField(n.Head); head != "" {
		note += "\nHead: " + head
	}
	if len(n.Conflicting) > 0 {
		files := make([]string, len(n.Conflicting))
		for i, f := range n.Conflicting {
			files[i] = NoteField(f)
		}
		note += "\nConflicting: " + strings.Join(files, ", ")
	}
	if tail := strings.TrimRight(n.GateTail, "\n"); tail != "" {
		note += "\nGate tail:"
		for _, line := range strings.Split(tail, "\n") {
			if len(line) > gateTailLineMax {
				line = line[:gateTailLineMax] + " …"
			}
			note += "\n  | " + defuseMarkers(line)
		}
	}
	return note
}

// gateTailLineMax bounds each quoted output line, so one runaway line (a
// dumped blob, a minified file) cannot bloat the bead's notes.
const gateTailLineMax = 400

// defuseMarkers keeps a quoted output line from carrying a block marker that
// another reader splits the notes on.
func defuseMarkers(line string) string {
	line = strings.ReplaceAll(line, MergeRejectionNoteMarker, "MERGE-REJECTION")
	line = strings.ReplaceAll(line, ReadyNoteMarker, "READY-TO-LAND")
	return strings.ReplaceAll(line, LandingNoteMarker, "LANDING-RECORD")
}

// CountRejections is the number of MERGE REJECTION blocks in notes, the
// attempt counter the formula and the deacon use.
func CountRejections(notes string) int {
	return strings.Count(notes, MergeRejectionNoteMarker+" (attempt")
}

// EmptyMerge is one empty-merge refusal's evidence (gt-j5cc).
type EmptyMerge struct {
	// Target is the branch being merged into, for the message.
	Target string
	// Base is the ref the branch's own commits are measured against. It must
	// be one the merge has not moved.
	Base string
	// Head is the submitted branch head, whose commits are blamed.
	Head string
	// Stage says where the check ran: a branch already empty when submitted
	// reads differently from a merge that turned out to be empty.
	Stage string
	// Comparison names the two refs found to hold identical trees.
	Comparison string
}

const (
	// emptyMergeScanLimit bounds how many of the branch's own commits the
	// refusal inspects. The commit that lost the payload is always one of the
	// most recent that remove content, so a window from the tip is enough.
	emptyMergeScanLimit = 20
	// emptyMergeReportLimit caps how many commits the refusal lists before it
	// counts the rest.
	emptyMergeReportLimit = 8
)

// CommitStatser is the part of *git.Git EmptyMergeReason reads.
type CommitStatser interface {
	CommitLineStatsInRange(revRange string, limit int) ([]git.CommitLineStats, error)
}

// EmptyMergeReason builds the refusal text for a merge that changes nothing.
// Its first line stands alone, because that is the part a close reason or a
// log line keeps.
func EmptyMergeReason(g CommitStatser, ev EmptyMerge) string {
	var b strings.Builder
	fmt.Fprintf(&b, "empty merge (%s): %s, so this MR changes nothing in %s",
		ev.Stage, ev.Comparison, ev.Target)

	commits, err := g.CommitLineStatsInRange(ev.Base+".."+ev.Head, emptyMergeScanLimit)
	if err != nil {
		fmt.Fprintf(&b, " (the branch's own commits could not be read: %v)", err)
		return b.String()
	}
	if len(commits) == 0 {
		fmt.Fprintf(&b, "; the branch has no commits %s does not already have", ev.Base)
		return b.String()
	}

	b.WriteString("; the branch's own commits, newest first:")
	var loser git.CommitLineStats
	for i, c := range commits {
		if i == emptyMergeReportLimit {
			fmt.Fprintf(&b, "\n  ... and %d more", len(commits)-emptyMergeReportLimit)
			break
		}
		fmt.Fprintf(&b, "\n  %s %s (+%d -%d)", shortSHA(c.Commit), c.Subject, c.Added, c.Removed)
		if c.Added == 0 && c.Removed > loser.Removed {
			loser = c
		}
	}
	// A commit that only removes lines is the shape the incident had. Naming
	// the largest one is a lead to check, not a verdict.
	if loser.Commit != "" {
		fmt.Fprintf(&b, "\n%s removes %d lines and adds none — check it first",
			shortSHA(loser.Commit), loser.Removed)
	}
	return b.String()
}

// CloseBlockReason says why a work bead must not be closed as landed:
// "no_merge", "review_only" or "merge_strategy:local", or "" when it may be.
func CloseBlockReason(issue *beads.Issue) string {
	if fields := beads.ParseAttachmentFields(issue); fields != nil {
		switch {
		case fields.NoMerge:
			return "no_merge"
		case fields.ReviewOnly:
			return "review_only"
		case strings.EqualFold(strings.TrimSpace(fields.MergeStrategy), "local"):
			return "merge_strategy:local"
		}
	}
	return ""
}

// LandingRecord is the durable record of one landing. It is written to the
// rig's landings file as JSON and to the work bead as a LANDING RECORD notes
// block (until be-u20 gives beads a typed landing verb).
type LandingRecord struct {
	BeadID       string    `json:"bead"`
	Rig          string    `json:"rig"`
	Branch       string    `json:"branch"`
	Head         string    `json:"head"`
	Target       string    `json:"target"`
	Base         string    `json:"base"`
	LandedCommit string    `json:"landed_commit"`
	PatchID      string    `json:"patch_id"`
	GateResult   string    `json:"gate_result"`
	OMVerdict    string    `json:"om_verdict"`
	OMScore      float64   `json:"om_score"`
	Route        string    `json:"route"`
	LandedAt     time.Time `json:"landed_at"`
}

// NoteBlock is the LANDING RECORD block Land appends to the work bead.
func (r LandingRecord) NoteBlock() string {
	return fmt.Sprintf("%s\nlanded_commit: %s\npatch_id: %s\ntarget: %s\nbase: %s\nbranch: %s\nhead: %s\ngate_result: %s\nom_verdict: %s\nom_score: %.4f\nroute: %s\nlanded_at: %s",
		LandingNoteMarker, r.LandedCommit, r.PatchID, NoteField(r.Target), r.Base, NoteField(r.Branch), r.Head,
		NoteField(r.GateResult), NoteField(r.OMVerdict), r.OMScore, NoteField(r.Route), r.LandedAt.UTC().Format(time.RFC3339))
}

// CloseReason is the work bead's close reason. target_branch and commit_sha
// keep the shape the refinery's merged close used, so existing readers of
// close reasons still find the landed commit.
func (r LandingRecord) CloseReason() string {
	return fmt.Sprintf("Landed on %s\ntarget_branch: %s\ncommit_sha: %s\nlanded_commit: %s\npatch_id: %s",
		NoteField(r.Target), NoteField(r.Target), r.LandedCommit, r.LandedCommit, r.PatchID)
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
