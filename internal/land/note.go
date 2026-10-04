package land

import (
	"fmt"
	"regexp"
	"strconv"
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

// rejectionBlockRE finds a rejection block's header. The marker must open a
// line: findings, reasons and gate tails are agent-supplied text, and a
// marker quoted inside one of them is content, not a block (gt-9bioi.1).
var rejectionBlockRE = regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(MergeRejectionNoteMarker) + ` \(attempt [0-9]+\): `)

// rejectionFindingRE reads one finding line of a rejection block: the
// "- id:X sev:Y path:line — title" FormatRejectionNote writes. The path is
// greedy up to the last colon, because it may contain none and the line
// number is what follows the last one.
var rejectionFindingRE = regexp.MustCompile(`^- id:(\S+) sev:(\S+) (.*):(\d+) — (.*)$`)

// ParseRejectionNote reads the last MERGE REJECTION block in notes, the one
// the live attempt wrote (a resubmission appends a new block, as with
// ParseReadyNote). ok is false when notes holds no block whose header parses.
//
// A reader that needs only which attempt a bead is on, or what the last
// refusal said, reads this rather than re-splitting the notes: the format is
// FormatRejectionNote's, and a second parser drifts from it (gt-9bioi.1).
func ParseRejectionNote(notes string) (RejectionNote, bool) {
	blocks := rejectionBlockRE.FindAllStringIndex(notes, -1)
	if len(blocks) == 0 {
		return RejectionNote{}, false
	}
	lines := strings.Split(notes[blocks[len(blocks)-1][0]:], "\n")
	var n RejectionNote
	attempt, kind, reason, ok := parseRejectionHeader(lines[0])
	if !ok {
		return RejectionNote{}, false
	}
	n.Attempt, n.Kind, n.Reason = attempt, kind, reason
	// Only the first occurrence of each field is read: the block runs to the
	// end of the notes, so later prose belongs to no rejection field.
	seen := map[string]bool{}
	inTail := false
	var tail []string
	for _, line := range lines[1:] {
		if inTail {
			if strings.HasPrefix(line, "  | ") {
				tail = append(tail, strings.TrimPrefix(line, "  | "))
				continue
			}
			inTail = false
		}
		if strings.HasPrefix(line, "  | ") {
			inTail = true
			tail = append(tail, strings.TrimPrefix(line, "  | "))
			continue
		}
		if f, ok := parseFindingLine(line); ok {
			n.Findings = append(n.Findings, f)
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if seen[key] {
			continue
		}
		switch key {
		case "branch":
			n.Branch, seen[key] = strings.TrimPrefix(value, "refs/heads/"), true
		case "target":
			n.Target, seen[key] = value, true
		case "mr":
			n.MR, seen[key] = value, true
		case "head":
			n.Head, seen[key] = value, true
		case "score":
			if score, err := strconv.ParseFloat(value, 64); err == nil {
				n.Receipt = &Receipt{Score: score}
			}
			seen[key] = true
		case "unresolved":
			if n.Receipt == nil {
				n.Receipt = &Receipt{}
			}
			for _, id := range strings.Split(value, ",") {
				if id = strings.TrimSpace(id); id != "" {
					n.Receipt.Unresolved = append(n.Receipt.Unresolved, id)
				}
			}
			seen[key] = true
		case "conflicting":
			for _, f := range strings.Split(value, ",") {
				if f = strings.TrimSpace(f); f != "" {
					n.Conflicting = append(n.Conflicting, f)
				}
			}
			seen[key] = true
		}
	}
	if len(tail) > 0 {
		n.GateTail = strings.Join(tail, "\n")
	}
	return n, true
}

// parseRejectionHeader reads the "MERGE REJECTION (attempt N): kind - reason"
// line. The class is dropped unless it is one word: the reason is free text
// and its first word is not a class (the rule gt-1jig's readers use).
func parseRejectionHeader(header string) (attempt int, kind, reason string, ok bool) {
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(header), MergeRejectionNoteMarker))
	num, after, found := strings.Cut(strings.TrimPrefix(rest, "(attempt "), ")")
	if !found {
		return 0, "", "", false
	}
	attempt, err := strconv.Atoi(strings.TrimSpace(num))
	if err != nil || attempt < 1 {
		return 0, "", "", false
	}
	reason = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(after), ":"))
	if class, text, found := strings.Cut(reason, " - "); found {
		class = strings.ToLower(strings.TrimSpace(class))
		if class != "" && !strings.ContainsAny(class, " \t") {
			kind, reason = class, strings.TrimSpace(text)
		}
	}
	return attempt, kind, reason, true
}

// parseFindingLine reads one "- id:… sev:… path:line — title" line.
func parseFindingLine(line string) (Finding, bool) {
	m := rejectionFindingRE.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return Finding{}, false
	}
	lineNo, err := strconv.Atoi(m[4])
	if err != nil {
		return Finding{}, false
	}
	return Finding{ID: m[1], Severity: m[2], Path: m[3], Line: lineNo, Title: m[5]}, true
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

// The verdict words of a shadow-mode landing record's CI fields: what the
// Forgejo candidate gate reported on the candidate, recorded beside the local
// gate's gate_result so the flip/no-flip call reads a pair (slice 8). They are
// the contract of the landings file's ci_verdict key.
const (
	// CIVerdictSuccess is the required context reporting success.
	CIVerdictSuccess = "success"
	// CIVerdictFailure is the required context reporting anything but
	// success, on a run that judged the work.
	CIVerdictFailure = "failure"
	// CIVerdictNone is no verdict at all: nothing reported within the wait
	// window, or the push, the workflow file or the API failed. Detail says
	// which.
	CIVerdictNone = "no-verdict"
)

// ciRecord is a landing record's CI fields: one candidate result rendered for
// the ci_* keys. ok is false when no candidate gate ran, the only case that
// writes none of them.
type ciRecord struct {
	// Context is the required commit status polled, e.g. "ci / gate (push)".
	Context string
	// Verdict is CIVerdictSuccess, CIVerdictFailure or CIVerdictNone.
	Verdict string
	// Detail is the failing run's status, or why no verdict was reached.
	Detail string
}

// ciRecordOf renders c for the landing record. A result carrying Err never
// reached a verdict, whatever State says (candidate.go, "A result carrying Err
// is infrastructure whatever State says"), so it records as CIVerdictNone with
// the reason.
func ciRecordOf(c *CandidateResult) (ciRecord, bool) {
	if c == nil {
		return ciRecord{}, false
	}
	rec := ciRecord{Context: NoteField(c.Context), Detail: NoteField(c.RunStatus)}
	switch {
	case c.Err != nil:
		rec.Verdict = CIVerdictNone
		rec.Detail = NoteField(elideMiddle(c.Err.Error(), reviewErrorReasonMax))
	case c.State == CandidatePassed:
		rec.Verdict = CIVerdictSuccess
	case c.State == CandidateFailed:
		rec.Verdict = CIVerdictFailure
	default:
		rec.Verdict = CIVerdictNone
	}
	return rec, true
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
	// RiskPaths are the changed paths this landing touched that matched
	// internal/land/riskpaths.txt at the base commit. They are the record the
	// attention queue's risk-path item reads, and the reason the work bead
	// carries gt:overseer-review-wanted (gt-vsct7.4). Empty on a landing that
	// touched nothing on the list.
	RiskPaths []string `json:"risk_paths,omitempty"`
	// The CI fields are the Forgejo candidate gate's verdict on this landing,
	// written only in shadow mode beside the local gate's GateResult: the
	// paired verdicts the flip/no-flip call reads (slice 8). CIContext is the
	// required commit status polled, CIVerdict is one of the CIVerdict* words,
	// and CIDetail is the failing run's status or why no verdict was reached.
	// All three are empty on a landing that ran no candidate gate.
	CIContext string `json:"ci_context,omitempty"`
	CIVerdict string `json:"ci_verdict,omitempty"`
	CIDetail  string `json:"ci_detail,omitempty"`
}

// NoteBlock is the LANDING RECORD block Land appends to the work bead.
func (r LandingRecord) NoteBlock() string {
	note := fmt.Sprintf("%s\nlanded_commit: %s\npatch_id: %s\ntarget: %s\nbase: %s\nbranch: %s\nhead: %s\ngate_result: %s\nom_verdict: %s\nom_score: %.4f\nroute: %s\nlanded_at: %s",
		LandingNoteMarker, r.LandedCommit, r.PatchID, NoteField(r.Target), r.Base, NoteField(r.Branch), r.Head,
		NoteField(r.GateResult), NoteField(r.OMVerdict), r.OMScore, NoteField(r.Route), r.LandedAt.UTC().Format(time.RFC3339))
	if r.CIVerdict != "" {
		note += fmt.Sprintf("\nci_context: %s\nci_verdict: %s", NoteField(r.CIContext), NoteField(r.CIVerdict))
		if r.CIDetail != "" {
			note += "\nci_detail: " + NoteField(r.CIDetail)
		}
	}
	return note
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
