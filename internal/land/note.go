package land

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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

// RiskPathReviewSkippedLine opens the last line of the LANDING RECORD block of
// a landing RiskPathsRecorded left with no risk path; the label that did it
// follows in parentheses. No reader of the block parses by line (they match
// the marker and landed_commit), so the extra line breaks none (gt-g349w).
const RiskPathReviewSkippedLine = "risk-path review skipped: review-generated"

// Receipt is the om score and still-unresolved finding ids a rejection
// carries for the deacon's convergence rule (deacon.ParseEditorialReceiptFromNotes).
type Receipt struct {
	Score      float64
	Unresolved []string
}

// RejectionNote is one MERGE REJECTION block. The first part (header,
// Branch, Target, MR, findings, receipt) is the refinery's format, moved here
// unchanged so every reader keeps working (gt-s4f6). Head, Conflicting,
// GateTail and GateLog are Land's additions and are written only when set.
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
	// GateLog is where the rejected gate's whole job log is kept, for reading
	// the failure past the excerpt's cut (gt-wwqx8). Empty when none was kept.
	GateLog string
}

// FormatRejectionNote renders n. Every agent-supplied field is collapsed onto
// one line, and the fields that carry free text — the reason, each finding
// title and the gate tail — are stripped of the markers other readers split
// on, so no output can forge a block or a field (gt-zqqcr).
func FormatRejectionNote(n RejectionNote) string {
	attempt := n.Attempt
	if attempt < 1 {
		attempt = 1
	}
	header := fmt.Sprintf("%s (attempt %d): ", MergeRejectionNoteMarker, attempt)
	if class := NoteField(n.Kind); class != "" {
		header += class + " - "
	}
	note := header + defuseMarkers(NoteField(n.Reason)) +
		fmt.Sprintf("\nBranch: %s\nTarget: %s\nMR: %s", n.Branch, n.Target, n.MR)
	for _, f := range n.Findings {
		note += fmt.Sprintf("\n- id:%s sev:%s %s:%d — %s",
			NoteField(f.ID), NoteField(f.Severity), defuseFindingPath(NoteField(f.Path)), f.Line, defuseMarkers(NoteField(f.Title)))
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
	if logPath := NoteField(n.GateLog); logPath != "" {
		note += "\nGate log: " + logPath
	}
	if tail := strings.TrimRight(n.GateTail, "\n"); tail != "" {
		note += "\nGate tail:"
		for _, line := range strings.Split(tail, "\n") {
			if len(line) > gateTailLineMax {
				// Back up to a rune start: a cut inside a multibyte rune
				// leaves invalid UTF-8 in the note.
				cut := gateTailLineMax
				for cut > 0 && !utf8.RuneStart(line[cut]) {
					cut--
				}
				line = line[:cut] + " …"
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

// findingSeparatorRE is the ":N — " that ends a finding's path in a note line.
var findingSeparatorRE = regexp.MustCompile(`:[0-9]+ — `)

// defuseFindingPath keeps a path from carrying the separator that
// rejectionFindingRE ends the path at. The parser splits at the first one, so
// a path holding ":N — " would read back cut short with the rest in the title;
// the em dash is flattened to a hyphen. The rewrite is lossy on purpose: the
// note line has no escape for the separator, and a path holding one is rare
// enough that a legible near-match beats a finding cut in two.
func defuseFindingPath(path string) string {
	return findingSeparatorRE.ReplaceAllStringFunc(path, func(sep string) string {
		return strings.Replace(sep, "—", "-", 1)
	})
}

// CountRejections is the number of review rejections in notes: the review
// round the bead is on. Only a review refusal spends a rework attempt; a gate,
// policy or conflict refusal costs no om review and is fixed before the next
// round, so it does not count (gt-l3zak). A block whose header names no class
// counts as a review: a refusal nobody classified records no class at all
// (gt-1jig).
//
// It counts the same anchored headers the parser finds, so marker text quoted
// inside a reason or a finding title — anywhere but the start of a line — is
// content, not a block, and does not inflate the count (gt-zqqcr).
func CountRejections(notes string) int {
	return countKind(rejectionKinds(notes), string(RejectReview))
}

// RejectionBlocks is the number of rejection blocks in notes, whatever their
// kind: how often the landing has refused the bead, which is what a loop
// signal counts even when the refusals spend no rework attempt (gt-l3zak).
func RejectionBlocks(notes string) int {
	return len(rejectionBlockRE.FindAllStringIndex(notes, -1))
}

// reviewAttempt is the rework attempt a verdict on notes belongs to: the review
// rejections the bead carries, plus one. The reviews log and the rounds ledger
// both read it here, so a reviewed round and its record cannot disagree about
// which attempt it is (gt-yphcl).
func reviewAttempt(notes string) int {
	return CountRejections(notes) + 1
}

// rejectionKinds returns the class of every rejection block in notes, in the
// order they were written. A block with no class reads as a review: a refusal
// nobody classified records no class at all (gt-1jig).
func rejectionKinds(notes string) []string {
	blocks := rejectionBlockRE.FindAllStringIndex(notes, -1)
	kinds := make([]string, len(blocks))
	for i, b := range blocks {
		line, _, _ := strings.Cut(notes[b[0]:], "\n")
		_, kind, _, _ := parseRejectionHeader(line)
		if kind == "" {
			kind = string(RejectReview)
		}
		kinds[i] = kind
	}
	return kinds
}

// countKind is how many of kinds name kind.
func countKind(kinds []string, kind string) int {
	n := 0
	for _, k := range kinds {
		if k == kind {
			n++
		}
	}
	return n
}

// rejectionBlockRE finds a rejection block's header. The marker must open a
// line: findings, reasons and gate tails are agent-supplied text, and a
// marker quoted inside one of them is content, not a block (gt-9bioi.1).
var rejectionBlockRE = regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(MergeRejectionNoteMarker) + ` \(attempt [0-9]+\): `)

// rejectionFindingRE reads one finding line of a rejection block: the
// "- id:X sev:Y path:line — title" FormatRejectionNote writes. The path is
// lazy up to the first ":N — ": a path is a file name and never holds that
// separator, while a title is free text that may quote "file:12 — ", and
// splitting at the last one would move part of the title into the path.
var rejectionFindingRE = regexp.MustCompile(`^- id:(\S+) sev:(\S+) (.*?):(\d+) — (.*)$`)

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
		case "gate log":
			n.GateLog, seen[key] = value, true
		}
	}
	if len(tail) > 0 {
		n.GateTail = strings.Join(tail, "\n")
	}
	return n, true
}

// RejectionForAttempt returns the MERGE REJECTION block that opened attempt in
// notes: the review block carrying that attempt, or a gate or policy block of
// the same count where no review block carries it (gt-l3zak).
//
// ParseRejectionNote reads the last block of the text it is given, so each
// block is cut out up to the next header before it is parsed. The rework pass
// and the scorecard both label a recorded round by it, so it lives beside the
// format it reads rather than once per reader (gt-pfru5).
func RejectionForAttempt(notes string, attempt int) (RejectionNote, bool) {
	starts := rejectionBlockRE.FindAllStringIndex(notes, -1)
	var review, other RejectionNote
	haveReview, haveOther := false, false
	for i, s := range starts {
		end := len(notes)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		rej, parsed := ParseRejectionNote(notes[s[0]:end])
		if !parsed || rej.Attempt != attempt {
			continue
		}
		if rej.Kind == "" || rej.Kind == string(RejectReview) {
			review, haveReview = rej, true
		} else {
			other, haveOther = rej, true
		}
	}
	if haveReview {
		return review, true
	}
	return other, haveOther
}

// priorReviewFindings is the previous round's findings for the next review over
// a bead's diff: the findings in the last MERGE REJECTION block, which is the
// round that sent the work back (gt-cz07b). om classifies each as resolved,
// unresolved or regressed, so a rework round is scored on what is still wrong
// rather than on a fresh set of minors invented about an unchanged diff.
//
// Empty on a bead no refusal has rejected — which is exactly attempt 1 — and on
// a refusal that raised none, such as a gate failure: that round has nothing
// for om to classify, and the review runs cold.
func priorReviewFindings(notes string) []Finding {
	last, ok := ParseRejectionNote(notes)
	if !ok {
		return nil
	}
	return last.Findings
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
		// The lead reads the whole scan window, not only the listed commits.
		if c.Added == 0 && c.Removed > loser.Removed {
			loser = c
		}
		switch {
		case i < emptyMergeReportLimit:
			fmt.Fprintf(&b, "\n  %s %s (+%d -%d)", shortSHA(c.Commit), c.Subject, c.Added, c.Removed)
		case i == emptyMergeReportLimit:
			fmt.Fprintf(&b, "\n  ... and %d more", len(commits)-emptyMergeReportLimit)
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
	// RiskPaths are the changed paths this landing touched that matched
	// internal/land/riskpaths.txt at the base commit. They are the record the
	// attention queue's risk-path item reads, and the reason the work bead
	// carries gt:overseer-review-wanted (gt-vsct7.4). Empty on a landing that
	// touched nothing on the list.
	RiskPaths []string `json:"risk_paths,omitempty"`
	// RiskPathReviewSkippedBy is the label that left RiskPaths empty; the note
	// carries RiskPathReviewSkippedLine in place of the paths.
	RiskPathReviewSkippedBy string `json:"risk_path_review_skipped_by,omitempty"`
	// Agent is the agent the polecat's seat ran on, from its agent bead. A
	// crew landing has no seat and records none, so the field is left out of
	// the JSON and the note rather than written empty (gt-mukgk.6).
	Agent string `json:"agent,omitempty"`
	// PolecatSecs, PolecatAttempts and the two bound flags are the time the
	// bead's polecat seats spent hooked on it, read from the spans log when
	// the record is written (Append). A crew landing has no seat and a bead
	// with no spans has no entries, so all of them are left out then. A total
	// that may understate the time sets PolecatLowerBound, with the time still
	// recorded where there is any: an attempt that is still open adds no time,
	// one a dead seat closed adds the time to its close, and an unreadable log
	// leaves the time out entirely (gt-yxl22). A time read from the bead's
	// dispatch instead of a span start sets PolecatUpperBound: the interval
	// begins before the seat could have been working, so the time is at most
	// that (gt-v7ptp).
	PolecatSecs       float64          `json:"polecat_secs,omitempty"`
	PolecatAttempts   []PolecatAttempt `json:"polecat_attempts,omitempty"`
	PolecatLowerBound bool             `json:"polecat_lower_bound,omitempty"`
	PolecatUpperBound bool             `json:"polecat_upper_bound,omitempty"`
}

// PolecatAttempt is one seat's time on the landing's bead, an entry of
// polecat_attempts (gt-yxl22).
type PolecatAttempt struct {
	Seat string `json:"seat"`
	// Secs is the attempt's duration: the time to its close. An attempt its
	// seat never ended adds none. Either way LowerBound marks the duration a
	// floor — the seat's time past the open attempt or the dead close is not in
	// the record — and UpperBound marks one read from a clock that starts
	// before the seat could have been working (gt-v7ptp).
	Secs       float64 `json:"secs"`
	LowerBound bool    `json:"lower_bound,omitempty"`
	UpperBound bool    `json:"upper_bound,omitempty"`
	// From names the clock an attempt was read from when the seat wrote no
	// start event: PolecatFromSling is the bead's dispatch to the end the
	// landing trusts for the submission. It is empty for an attempt timed from
	// its own start event (gt-v7ptp).
	From string `json:"from,omitempty"`
}

// PolecatFromSling is the From of an attempt whose seat wrote no start event,
// so its duration is the interval from the bead's dispatch to the end the
// landing trusts for the submission (gt-v7ptp).
const PolecatFromSling = "sling"

// NoteBlock is the LANDING RECORD block Land appends to the work bead.
func (r LandingRecord) NoteBlock() string {
	note := fmt.Sprintf("%s\nlanded_commit: %s\npatch_id: %s\ntarget: %s\nbase: %s\nbranch: %s\nhead: %s\ngate_result: %s\nom_verdict: %s\nom_score: %.4f\nroute: %s\nlanded_at: %s",
		LandingNoteMarker, r.LandedCommit, r.PatchID, NoteField(r.Target), r.Base, NoteField(r.Branch), r.Head,
		NoteField(r.GateResult), NoteField(r.OMVerdict), r.OMScore, NoteField(r.Route), r.LandedAt.UTC().Format(time.RFC3339))
	// A crew landing has no seat, so it carries no agent line at all: an
	// empty "agent:" would read as a seat nobody could name (gt-mukgk.6).
	if r.Agent != "" {
		note += "\nagent: " + NoteField(r.Agent)
	}
	if r.RiskPathReviewSkippedBy != "" {
		note += fmt.Sprintf("\n%s (label %s)", RiskPathReviewSkippedLine, NoteField(r.RiskPathReviewSkippedBy))
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

// ParseLandingRecord returns the last LANDING RECORD block in notes, and
// whether the block names a landed commit. The last, because a reworked bead
// carries one block per landing and only the newest says what its branch
// landed as.
func ParseLandingRecord(notes string) (LandingRecord, bool) {
	idx := strings.LastIndex(notes, LandingNoteMarker+"\n")
	if idx < 0 {
		return LandingRecord{}, false
	}
	var rec LandingRecord
	body := notes[idx+len(LandingNoteMarker)+1:]
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			break
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "landed_commit":
			rec.LandedCommit = value
		case "patch_id":
			rec.PatchID = value
		case "target":
			rec.Target = value
		case "base":
			rec.Base = value
		case "branch":
			rec.Branch = value
		case "head":
			rec.Head = value
		case "gate_result":
			rec.GateResult = value
		case "om_verdict":
			rec.OMVerdict = value
		case "om_score":
			rec.OMScore, _ = strconv.ParseFloat(value, 64)
		case "route":
			rec.Route = value
		case "landed_at":
			if landedAt, err := time.Parse(time.RFC3339, value); err == nil {
				rec.LandedAt = landedAt
			}
		case "agent":
			rec.Agent = value
		}
	}
	return rec, rec.LandedCommit != ""
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
