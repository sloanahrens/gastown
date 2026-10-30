package land

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/authorkind"
	"github.com/steveyegge/gastown/internal/beads"
)

const (
	// LabelReadyToLand marks a work bead whose branch is pushed and waiting
	// for the landing worker. gt done sets it; Land removes it on every
	// outcome that writes the bead.
	LabelReadyToLand = "gt:ready-to-land"
	// LabelRework marks a work bead Land rejected for a reason its author can
	// fix. Dispatch already routes on it (sling_pool.go).
	LabelRework = "rework"
	// LabelNeedsHuman marks a work bead whose landing only a human can settle:
	// Land refused it on policy (no_merge), or the bead hit
	// MaxReworkAttempts and the rework loop is the failure (gt-28ibg).
	LabelNeedsHuman = "gt:needs-human"
	// LabelNeedsOverseer is the overseer-first spelling of LabelNeedsHuman: a
	// landing left for a person is written with it, because the overseer is
	// the first step of every escalation (gt-zjel5). LabelNeedsHuman stays
	// readable for beads written before the rename.
	LabelNeedsOverseer = "gt:needs-overseer"
	// LabelNeedsOperator is the operator's spelling of the same hold. Landing
	// never writes it; it is read and cleared like LabelNeedsHuman (gt-zjel5).
	LabelNeedsOperator = "gt:needs-operator"

	// LabelOverseerReviewed marks a work bead whose head the overseer
	// reviewed in place of om; it counts only beside an OverseerReviewedMarker
	// note naming that exact head (gt-g8t3m). gt done removes it on every
	// submission, so a new head is never waved through on an old review.
	LabelOverseerReviewed = "gt:overseer-reviewed"
	// LabelOverseerReviewWanted marks a work bead whose landing touched a
	// risk path (RiskPathsFile) and so wants a post-landing look at that head
	// (gt-vsct7.4). It is deliberately not LabelOverseerReviewed: that one
	// says the overseer stood in for om, this one asks for a human. Land adds
	// it and nothing strips it — a review does not remove it, and the
	// attention item it feeds clears on the review note (OverseerReviewMarker).
	LabelOverseerReviewWanted = "gt:overseer-review-wanted"

	// OverseerReviewMarker opens the note line "OVERSEER REVIEW <full head
	// sha> PASS|FAIL" that records a review of a risk-path item's head: it
	// names the head a human looked at. A PASS answers the item; a FAIL keeps
	// it open until a later PASS for the head. The line
	// "OVERSEER REVIEW <head> WAIVED: <reason>" or "... AUDITED: <reason>"
	// answers it too, for a head reviewed another way or consciously not
	// reviewed; the reason is required. It is not
	// OverseerReviewedMarker — that marker says om was bypassed, and must not
	// satisfy a risk-path item (gt-vsct7.4).
	OverseerReviewMarker = "OVERSEER REVIEW"
	// OverseerReviewedMarker opens the note line "OVERSEER REVIEWED <full
	// head sha>" that binds the overseer's review to one head.
	OverseerReviewedMarker = "OVERSEER REVIEWED"

	// PolecatReviewMarker opens the verdict line a polecat's review document
	// ends with: "POLECAT REVIEW <full head sha> PASS" or "POLECAT REVIEW
	// <full head sha> FAIL: <the defect>". Unlike the overseer's line it is
	// written in docs/reviews, not in the bead's notes, and it records a
	// second look at a head that already landed.
	PolecatReviewMarker = "POLECAT REVIEW"

	// ReadyNoteMarker opens the notes block gt done writes to say what to land.
	ReadyNoteMarker = "READY TO LAND"
)

// needsPersonLabels are every label that leaves a work bead for a person. The
// readers ask HasNeedsPerson rather than one label, so the old spelling keeps
// its meaning beside the new ones.
var needsPersonLabels = []string{LabelNeedsHuman, LabelNeedsOverseer, LabelNeedsOperator}

// HasNeedsPerson reports whether issue carries any label that leaves it for a
// person (gt-zjel5).
func HasNeedsPerson(issue *beads.Issue) bool {
	for _, label := range needsPersonLabels {
		if beads.HasLabel(issue, label) {
			return true
		}
	}
	return false
}

// Work is one landing request: the work bead and the pushed branch head it
// declares.
type Work struct {
	BeadID string
	Rig    string
	Branch string
	// Head is the commit gt done pushed and verified on the rig's landing
	// remote (gt-fn9e6.9), origin by default. Land merges this commit, never
	// whatever the branch holds later.
	Head   string
	Target string
	Worker string
	// Agent is the agent the polecat's seat ran on, read from its agent bead
	// and kept after the session ends (gt-mukgk.6). A crew landing has no
	// seat and carries none, and every record written for it leaves the field
	// empty rather than guessing the role's default.
	Agent string
	// Submitted is when the bead was submitted for landing, and is what the
	// landing worker orders the ready queue by: a comment on the bead is not
	// a submission and does not move it (gt-t2jhf). Zero on a bead submitted
	// before the note carried one.
	Submitted time.Time
	// CandidateBranch and CandidateHead name the merge candidate: the branch
	// Land pushes it as and the merged commit on it. A READY TO LAND block
	// carries neither — Candidate derives the branch from BeadID — and Land
	// fills both for the run, so a log, a note or a repair names what CI
	// tested rather than recomputing it.
	CandidateBranch string
	CandidateHead   string
}

// Candidate is the branch w's merge candidate is pushed as for the Forgejo
// gate: the CandidateBranch field when one is set, else "land/<bead>".
func (w Work) Candidate() string {
	if w.CandidateBranch != "" {
		return w.CandidateBranch
	}
	return "land/" + w.BeadID
}

// PolecatBranchPrefix opens the branch a polecat's work is pushed as:
// polecat/<polecat>/<bead>, sometimes with a +<mutation> suffix. It is the
// prefix the author-kind rule reads a seat from (authorkind.PolecatPrefix).
const PolecatBranchPrefix = authorkind.PolecatPrefix

// seat is the seat a landing's spans belong to, or "" when the landing has no
// polecat seat. It is read from the branch, which the landing fetched from the
// remote and verified carries the head it merged, rather than from the worker
// line of the ready note: that line is the seat's own word, and a seat must not
// be able to name the attempt its end closes (gt-8yk78). A crew branch has no
// seat, and neither has a branch that is not a polecat's.
func (w Work) seat() string {
	rest, ok := strings.CutPrefix(w.Branch, PolecatBranchPrefix)
	if !ok || w.Rig == "" {
		return ""
	}
	polecat, _, ok := strings.Cut(rest, "/")
	if !ok || polecat == "" {
		return ""
	}
	return w.Rig + "/" + polecat
}

// refusalSeat is the seat a refusal may end an attempt for: the polecat the
// verified branch names, or "" when the branch is not a polecat's, the landing
// names no rig, or the ready note's worker line disagrees with it.
func refusalSeat(w Work) string {
	if w.Rig == "" || authorkind.Kind(w.Branch) != authorkind.Polecat {
		return ""
	}
	polecat := authorkind.Seat(w.Branch)
	if w.Worker != "" && w.Worker != polecat {
		return ""
	}
	return w.Rig + "/" + polecat
}

// FormatReadyNote renders the READY TO LAND block for w. Every value is
// collapsed onto one line, so no field can inject a line the parser reads.
// Submitted is left out while it is zero.
func FormatReadyNote(w Work) string {
	note := fmt.Sprintf("%s\nBranch: %s\nHead: %s\nTarget: %s\nWorker: %s",
		ReadyNoteMarker, NoteField(w.Branch), NoteField(w.Head), NoteField(w.Target), NoteField(w.Worker))
	if !w.Submitted.IsZero() {
		note += "\nSubmitted: " + w.Submitted.UTC().Format(time.RFC3339)
	}
	return note
}

// ParseReadyNote reads the last READY TO LAND block in notes. A rework
// resubmission appends a new block, so the last one is the live request.
// BeadID and Rig are not part of the note and come back empty.
func ParseReadyNote(notes string) (Work, bool) {
	idx := strings.LastIndex(notes, ReadyNoteMarker+"\n")
	if idx < 0 {
		return Work{}, false
	}
	var w Work
	seen := map[string]bool{}
	for _, line := range strings.Split(notes[idx+len(ReadyNoteMarker)+1:], "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if seen[key] {
			break
		}
		seen[key] = true
		value = strings.TrimSpace(value)
		switch key {
		case "branch":
			w.Branch = value
		case "head":
			w.Head = value
		case "target":
			w.Target = value
		case "worker":
			w.Worker = value
		case "submitted":
			// A time this code cannot read orders as if the block carried
			// none: a bad stamp is not a reason to refuse to land the work.
			if at, err := time.Parse(time.RFC3339, value); err == nil {
				w.Submitted = at
			}
		default:
			return finishReady(w)
		}
	}
	return finishReady(w)
}

func finishReady(w Work) (Work, bool) {
	if w.Branch == "" || w.Head == "" || w.Target == "" {
		return Work{}, false
	}
	return w, true
}

// SubmittedAt is when issue was submitted for landing: the READY TO LAND
// block's time, which gt done stamps and no comment moves. A bead submitted
// before the block carried one falls back to its last update, the only
// ordering it ever had (gt-t2jhf). Zero when neither can be read.
func SubmittedAt(issue *beads.Issue) time.Time {
	if issue == nil {
		return time.Time{}
	}
	if w, ok := ParseReadyNote(issue.Notes); ok && !w.Submitted.IsZero() {
		return w.Submitted
	}
	at, err := time.Parse(time.RFC3339, issue.UpdatedAt)
	if err != nil {
		return time.Time{}
	}
	return at
}

// ReadyBlockTime returns the time on the bead's last READY TO LAND block, zero
// when the notes hold no complete block or the block carries no readable
// submission time. Unlike SubmittedAt it does not fall back to the bead's last
// update: that time moves with every write to the bead, so a clock dated from
// it would restart with each comment a seat adds (gt-chd44).
func ReadyBlockTime(notes string) time.Time {
	if w, ok := ParseReadyNote(notes); ok {
		return w.Submitted
	}
	return time.Time{}
}

// ErrNotReady means the bead does not carry a complete landing request.
var ErrNotReady = errors.New("work bead is not ready to land")

// WorkFromBead builds the landing request from a work bead: it must carry the
// ready label and a READY TO LAND block.
func WorkFromBead(issue *beads.Issue, rig string) (Work, error) {
	if issue == nil {
		return Work{}, fmt.Errorf("%w: no bead", ErrNotReady)
	}
	if !beads.HasLabel(issue, LabelReadyToLand) {
		return Work{}, fmt.Errorf("%w: %s has no %s label", ErrNotReady, issue.ID, LabelReadyToLand)
	}
	w, ok := ParseReadyNote(issue.Notes)
	if !ok {
		return Work{}, fmt.Errorf("%w: %s has no complete %s block in its notes", ErrNotReady, issue.ID, ReadyNoteMarker)
	}
	w.BeadID = issue.ID
	w.Rig = rig
	return w, nil
}

// NoteField collapses a free-text note field onto one line. Branch names,
// titles, paths and reasons are agent-supplied, and a newline in one of them
// would inject whole lines into the notes: a forged finding, a forged
// "MERGE REJECTION (attempt" marker, or a forged Head (gt-s4f6).
func NoteField(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// OverseerReviewed reports whether the overseer reviewed head in place of om:
// the bead carries LabelOverseerReviewed and a notes line
// "OVERSEER REVIEWED <sha>" whose sha is the full head. A label alone, or a
// review of another head, is not one (gt-g8t3m).
func OverseerReviewed(issue *beads.Issue, head string) bool {
	if issue == nil || head == "" || !beads.HasLabel(issue, LabelOverseerReviewed) {
		return false
	}
	for _, line := range strings.Split(issue.Notes, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), OverseerReviewedMarker+" ")
		if ok && strings.TrimSpace(rest) == head {
			return true
		}
	}
	return false
}

// overseerReviewLine matches "OVERSEER REVIEW <sha> PASS", "OVERSEER REVIEW
// <sha> FAIL" with an optional " findings=<bead>[,<bead>]" naming the finding
// beads that failed the landing, or "OVERSEER REVIEW <sha> WAIVED|AUDITED:
// <reason>", the note lines that record a review of a risk-path item's head.
// The trailing \b keeps a word from extending a verdict ("PASSED" is not
// PASS); WAIVED and AUDITED answer only with a reason after the colon, and the
// sha is compared whole, so a review of another head does not answer. A FAIL
// written before the suffix existed has no findings= and still parses, and a
// suffix that is not a clean comma-separated bead-id list reads as none: the
// list must run to whitespace or the end of the line, so a malformed suffix is
// never partly trusted (gt-86uzs). It deliberately does not match "OVERSEER
// REVIEWED ...": that marker says om was bypassed, not that a human reviewed
// this head (gt-vsct7.4).
var overseerReviewLine = regexp.MustCompile(`^OVERSEER REVIEW ([0-9a-f]{7,64}) (?:(PASS|FAIL)\b(?: findings=([A-Za-z0-9._-]+(?:,[A-Za-z0-9._-]+)*)(?:\s|$))?|(WAIVED|AUDITED): +\S)`)

// The verdicts a review line records. FAIL found a defect, PASS cleared the
// head, and WAIVED and AUDITED say the head was left unreviewed or reviewed
// another way, each with its reason.
const (
	OverseerReviewPass = "PASS"
	OverseerReviewFail = "FAIL"
)

// HasOverseerReviewNote reports whether notes answer a risk-path item for
// head: the latest review line naming exactly head is a PASS, or a WAIVED or
// AUDITED with its reason. A FAIL does not answer: the review found a defect,
// so the item stays until a later PASS (or WAIVED or AUDITED) for that head.
// WAIVED and AUDITED say the head was consciously left unreviewed or reviewed
// another way, and the reason is the record (gt-r5ne9).
func HasOverseerReviewNote(notes, head string) bool {
	verdict, ok := OverseerReviewVerdict(notes, head)
	return ok && verdict != OverseerReviewFail
}

// OverseerReviewVerdict returns the verdict (PASS, FAIL, WAIVED or AUDITED)
// of the latest review line in notes that names exactly head, and whether there
// is one. Every reader of a head's verdict takes the latest line, not the
// first, so a PASS written after a FAIL supersedes it (gt-pe01w).
func OverseerReviewVerdict(notes, head string) (string, bool) {
	if head == "" {
		return "", false
	}
	verdict, found := "", false
	for _, line := range strings.Split(notes, "\n") {
		if m := overseerReviewLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil && m[1] == head {
			verdict, found = m[2]+m[4], true
		}
	}
	return verdict, found
}

// OverseerReviewFindings returns the finding beads the latest review line in
// notes naming exactly head names, in the order the line carries them, and nil
// when it names none: a FAIL written before the suffix existed, or a PASS
// (gt-86uzs). Like OverseerReviewVerdict it reads the latest line, so a PASS
// written over a FAIL leaves no findings.
func OverseerReviewFindings(notes, head string) []string {
	if head == "" {
		return nil
	}
	var out []string
	for _, line := range strings.Split(notes, "\n") {
		m := overseerReviewLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil || m[1] != head {
			continue
		}
		out = nil
		for _, id := range strings.Split(m[3], ",") {
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, id)
			}
		}
	}
	return out
}

// polecatReviewLine matches a review document's verdict line: the head it
// reviewed and PASS or FAIL. A FAIL's defects follow a colon as prose, so they
// are never parsed; the trailing \b keeps "PASSED" from reading as PASS.
var polecatReviewLine = regexp.MustCompile(`^POLECAT REVIEW ([0-9a-f]{7,64}) (PASS|FAIL)\b`)

// PolecatReviewVerdict returns the verdict (PASS or FAIL) of the latest verdict
// line in doc that names head, and whether there is one. The line may name the
// head by an abbreviation of at least 7 hex digits, as a review document
// commonly does. A document naming only another head reviewed a different
// commit, so it says nothing about head.
func PolecatReviewVerdict(doc, head string) (string, bool) {
	if head == "" {
		return "", false
	}
	verdict, found := "", false
	for _, line := range strings.Split(doc, "\n") {
		if m := polecatReviewLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil && namesHead(m[1], head) {
			verdict, found = m[2], true
		}
	}
	return verdict, found
}

// minHeadAbbrev is the shortest abbreviation namesHead matches: the seven hex
// digits git's own abbreviation starts at. A shorter one names many commits,
// so it cannot show that a review read the head.
const minHeadAbbrev = 7

// namesHead reports whether sha and head name the same commit, either whole or
// as the other's prefix of at least seven digits.
func namesHead(sha, head string) bool {
	if len(sha) < minHeadAbbrev || len(head) < minHeadAbbrev {
		return false
	}
	return strings.HasPrefix(head, sha) || strings.HasPrefix(sha, head)
}
