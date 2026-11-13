package land

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// LocalRoundMarker opens a LOCAL ROUND block: the om round the polecat ran on
// its own diff before landing (gt-buay5a). It is not a MERGE REJECTION block.
// No rejection reader matches it, and a test holds the rejection readers to the
// same answers with and without one in the notes.
const LocalRoundMarker = "LOCAL ROUND"

// localRoundHeaderPrefix opens a block header. A reply line starts with
// "LOCAL ROUND REPLY:", which this prefix does not match.
const localRoundHeaderPrefix = LocalRoundMarker + " ("

// localRoundFindingLeader opens a finding line. It is not the "- " bullet a
// MERGE REJECTION finding uses: a rejection block runs to the next rejection
// header, so a bullet line here would be read as a finding of that rejection.
const localRoundFindingLeader = "LOCAL ROUND FINDING "

// localRoundReplyLeader opens a reply line.
const localRoundReplyLeader = LocalRoundMarker + " REPLY: "

// The kinds a reply carries: the polecat fixed the finding, or disputes it.
const (
	LocalRoundFixed    = "fixed"
	LocalRoundDisputed = "disputed"
)

var (
	localRoundHeaderRE  = regexp.MustCompile(`^LOCAL ROUND \((\d+)\): head=(.*?) at=(\S+)$`)
	localRoundFindingRE = regexp.MustCompile(`^` + regexp.QuoteMeta(localRoundFindingLeader) +
		`id:(\S*) sev:(\S*) (.*?):(\d+) —(?: (.*))?$`)
	localRoundReplyRE = regexp.MustCompile(`^` + regexp.QuoteMeta(localRoundReplyLeader) +
		`id:(\S*) (` + regexp.QuoteMeta(LocalRoundFixed) + `|` + regexp.QuoteMeta(LocalRoundDisputed) + `) -(?: (.*))?$`)
)

// LocalRound is one LOCAL ROUND block: the round number, the head the round
// reviewed, when it ran and the findings it raised. Finding.Detail is not
// written, as a rejection note does not write it either.
type LocalRound struct {
	Round    int
	Head     string
	At       time.Time
	Findings []Finding
}

// Reply is one LOCAL ROUND REPLY line: the polecat's answer to one finding of a
// round. Kind is LocalRoundFixed or LocalRoundDisputed.
type Reply struct {
	FindingID string
	Kind      string
	Text      string
}

// FormatLocalRound renders one LOCAL ROUND block: the header, then one line per
// finding. A round number below 1 is written as 1, since no block with a smaller
// number parses back. The head and every finding field are collapsed onto one
// line, and the marker text a reader splits on is defused, so no field can open
// a block or a finding line of its own. The block has no trailing newline; the
// caller adds the separator. at is written to the second, in UTC.
func FormatLocalRound(n int, head string, findings []Finding, at time.Time) string {
	if n < 1 {
		n = 1
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s%d): head=%s at=%s", localRoundHeaderPrefix, n, NoteField(head), at.UTC().Format(time.RFC3339))
	for _, f := range findings {
		fmt.Fprintf(&b, "\n%sid:%s sev:%s %s:%d — %s", localRoundFindingLeader,
			localRoundToken(f.ID), localRoundToken(f.Severity), defuseFindingPath(NoteField(f.Path)), f.Line,
			defuseLocalRoundText(f.Title))
	}
	return b.String()
}

// FormatLocalRoundReply renders one reply to a finding of the round it follows:
// "LOCAL ROUND REPLY: id:<id> <kind> - <text>". The kind's whitespace is
// collapsed, but its words are not checked. ParseLocalRoundReplies reads only
// LocalRoundFixed and LocalRoundDisputed, so any other kind is not a reply and
// its finding stays unanswered.
func FormatLocalRoundReply(findingID, kind, text string) string {
	return fmt.Sprintf("%sid:%s %s - %s", localRoundReplyLeader, localRoundToken(findingID),
		NoteField(kind), defuseLocalRoundText(text))
}

// ParseLastLocalRound reads the last LOCAL ROUND block in notes. ok is false
// when notes holds no block, and also when the last block does not parse: its
// header or one of its finding lines is malformed. An earlier block is never
// read in place of a malformed later one, and no finding of a block that is
// read is dropped.
func ParseLastLocalRound(notes string) (LocalRound, bool) {
	lines := strings.Split(notes, "\n")
	last := -1
	for i, line := range lines {
		if strings.HasPrefix(line, localRoundHeaderPrefix) {
			last = i
		}
	}
	if last < 0 {
		return LocalRound{}, false
	}
	r, ok := parseLocalRoundHeader(lines[last])
	if !ok {
		return LocalRound{}, false
	}
	for _, line := range lines[last+1:] {
		if !strings.HasPrefix(line, localRoundFindingLeader) {
			break
		}
		f, ok := parseLocalRoundFinding(line)
		if !ok {
			return LocalRound{}, false
		}
		r.Findings = append(r.Findings, f)
	}
	return r, true
}

// ParseLocalRoundReplies returns the replies to the findings of round afterRound,
// in the order they were written. A reply belongs to the round whose block most
// recently precedes it, up to the next LOCAL ROUND header, so a reply written
// before round afterRound's block, or after the block of the round that follows
// it, is not one of its replies. A reply line that does not parse is skipped, and
// its finding stays unanswered. A round number below 1 has no replies.
func ParseLocalRoundReplies(notes string, afterRound int) []Reply {
	if afterRound < 1 {
		return nil
	}
	var out []Reply
	inRound := false
	for _, line := range strings.Split(notes, "\n") {
		if strings.HasPrefix(line, localRoundHeaderPrefix) {
			r, ok := parseLocalRoundHeader(line)
			inRound = ok && r.Round == afterRound
			continue
		}
		if !inRound || !strings.HasPrefix(line, localRoundReplyLeader) {
			continue
		}
		if rep, ok := parseLocalRoundReply(line); ok {
			out = append(out, rep)
		}
	}
	return out
}

// parseLocalRoundHeader reads a "LOCAL ROUND (n): head=<sha> at=<RFC3339>" line.
// The round number must be at least 1 and the time must parse.
func parseLocalRoundHeader(line string) (LocalRound, bool) {
	m := localRoundHeaderRE.FindStringSubmatch(line)
	if m == nil {
		return LocalRound{}, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		return LocalRound{}, false
	}
	at, err := time.Parse(time.RFC3339, m[3])
	if err != nil {
		return LocalRound{}, false
	}
	return LocalRound{Round: n, Head: m[2], At: at}, true
}

// parseLocalRoundFinding reads one "LOCAL ROUND FINDING id:… sev:… path:line — title"
// line. The path is lazy up to the first ":N — ", as a rejection finding's is.
func parseLocalRoundFinding(line string) (Finding, bool) {
	m := localRoundFindingRE.FindStringSubmatch(line)
	if m == nil {
		return Finding{}, false
	}
	lineNo, err := strconv.Atoi(m[4])
	if err != nil {
		return Finding{}, false
	}
	return Finding{ID: m[1], Severity: m[2], Path: m[3], Line: lineNo, Title: m[5]}, true
}

// parseLocalRoundReply reads one "LOCAL ROUND REPLY: id:… <kind> - <text>" line.
func parseLocalRoundReply(line string) (Reply, bool) {
	m := localRoundReplyRE.FindStringSubmatch(line)
	if m == nil {
		return Reply{}, false
	}
	return Reply{FindingID: m[1], Kind: m[2], Text: m[3]}, true
}

// localRoundToken collapses s onto one token for a field that holds one word,
// an id or a severity. Whitespace is dropped, so the field cannot split the line.
func localRoundToken(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// defuseLocalRoundText is defuseMarkers plus the LOCAL ROUND marker, for free
// text that a reader might split on.
func defuseLocalRoundText(s string) string {
	s = defuseMarkers(NoteField(s))
	return strings.ReplaceAll(s, LocalRoundMarker, "LOCAL-ROUND")
}
