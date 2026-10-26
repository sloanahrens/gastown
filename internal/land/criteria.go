package land

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/beads"
)

// acceptedRiskMarker opens a bead comment that records a decision already
// settled: a review that re-raises one spends a round on a question the bead
// has answered.
const acceptedRiskMarker = "ACCEPTED-RISK:"

// acceptedRiskCap bounds AcceptedRisksText in bytes, the size om keeps of a
// --context-file before it truncates (om-pp3).
const acceptedRiskCap = 8 << 10

// headerFieldCap bounds the author and the date each in an entry's attribution
// line, so a long field cannot push the record past its cap by itself.
const headerFieldCap = 64

// acceptedRiskExcerptCap bounds the excerpt an audit entry carries, in runes:
// long enough to name the decision, short enough for a one-line landing note.
const acceptedRiskExcerptCap = 80

// AcceptedRisksText is the accepted-risk record the work bead's comments carry,
// which om is given through --context-file so it reports a finding that only
// restates a settled decision as info rather than refusing the change on it
// (gt-lauly).
//
// A comment counts when its text opens with "ACCEPTED-RISK:" and its author is
// on the allowlist (isAcceptedRiskAuthor): an allowlist, not a denylist, so an
// author shape nobody listed is ignored rather than trusted. That allowlist is
// the first of three layers, and none of them changes bd: a polecat session is
// refused at the tap guard when it posts an ACCEPTED-RISK comment or sets a
// comment author (gt-z71j0), and the landing that uses one leaves a note on the
// bead and hands the attention sink, when one is set, an item so a forged or
// mistaken waiver is seen when it is used (Lander.recordAcceptedRisks,
// gt-tvhg8). Entries are joined oldest first, each under a line naming its
// author and date, and the join is capped at acceptedRiskCap bytes by dropping
// the oldest: the newest entry is the one the running review is judged
// against. When any comment's date does not parse, the order is the list's own,
// since no date can say where that comment belongs. Empty means the bead
// records no accepted risk, and the review runs without the flag.
func AcceptedRisksText(comments []beads.Comment) string {
	kept := acceptedRiskComments(comments)
	entries := make([]string, len(kept))
	for i, c := range kept {
		entries[i] = acceptedRiskEntry(c.comment.Author, c.comment.CreatedAt, c.text)
	}
	return capAcceptedRisks(entries)
}

// AcceptedRisk is one accepted-risk comment a review used, as the audit layer
// names it (gt-tvhg8): the comment's id, its author and date ready to print on
// one line, and an excerpt of its text.
type AcceptedRisk struct {
	CommentID string
	Author    string
	Date      string
	Excerpt   string
}

// AcceptedRisksUsed reports every comment AcceptedRisksText records, as entries
// an audit can name: the same qualification and the same order, read through
// the same filter (acceptedRiskComments), so the record om weighs and the audit
// cannot drift apart. The byte cap is AcceptedRisksText's alone — it bounds the
// file om reads, while an audit that dropped the oldest waivers would defeat
// the reason it exists — so every qualifying comment is reported here. Empty
// means the bead records no accepted risk.
func AcceptedRisksUsed(comments []beads.Comment) []AcceptedRisk {
	kept := acceptedRiskComments(comments)
	used := make([]AcceptedRisk, len(kept))
	for i, c := range kept {
		used[i] = AcceptedRisk{
			CommentID: c.comment.ID,
			Author:    headerField(c.comment.Author),
			Date:      headerField(c.comment.CreatedAt),
			Excerpt:   acceptedRiskExcerpt(c.text),
		}
	}
	return used
}

// acceptedRiskComment is one comment that records an accepted risk: the comment
// itself, its text with the surrounding whitespace trimmed, and the moment it
// was written.
type acceptedRiskComment struct {
	comment beads.Comment
	text    string
	at      int64
}

// acceptedRiskComments filters comments to the ones that record an accepted
// risk and orders them oldest first. AcceptedRisksText and AcceptedRisksUsed
// both read this filter, so they qualify and order comments identically.
func acceptedRiskComments(comments []beads.Comment) []acceptedRiskComment {
	kept := make([]acceptedRiskComment, 0, len(comments))
	dated := true
	for _, c := range comments {
		text := strings.TrimSpace(c.Text)
		if !strings.HasPrefix(text, acceptedRiskMarker) || !isAcceptedRiskAuthor(c.Author) {
			continue
		}
		at, ok := commentTime(c.CreatedAt)
		dated = dated && ok
		kept = append(kept, acceptedRiskComment{comment: c, text: text, at: at})
	}
	if dated {
		sort.SliceStable(kept, func(i, j int) bool { return kept[i].at < kept[j].at })
	}
	return kept
}

// commentTime reads a comment's creation time in unix seconds. It reports false
// for a date that does not parse.
func commentTime(created string) (int64, bool) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(created))
	if err != nil {
		return 0, false
	}
	return t.Unix(), true
}

// acceptedRiskEntry renders one comment under the line that attributes it: om
// weighs a named author's decision, and an assertion with no one behind it is
// not a decision to weigh.
func acceptedRiskEntry(author, created, text string) string {
	author, created = headerField(author), headerField(created)
	if created == "" {
		created = "an unrecorded date"
	}
	return fmt.Sprintf("[ACCEPTED-RISK from %s, %s]\n%s", author, created, text)
}

// headerField makes a comment's author or date safe to print on the attribution
// line: control characters and newlines are dropped, so nothing in it can open
// a second line, and the rest is capped at headerFieldCap bytes.
func headerField(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return cutToBytes(strings.TrimSpace(s), headerFieldCap)
}

// acceptedRiskExcerpt returns text's first acceptedRiskExcerptCap runes with
// its newlines flattened to spaces, so an entry fits on one line of a landing
// note without splitting a multi-byte character.
func acceptedRiskExcerpt(text string) string {
	flat := strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(text)
	runes := []rune(flat)
	if len(runes) <= acceptedRiskExcerptCap {
		return flat
	}
	return string(runes[:acceptedRiskExcerptCap])
}

// isAcceptedRiskAuthor reports whether an author may record an accepted risk:
// the shared human identity or the overseer, matched exactly after trimming and
// case-sensitively. Every other author, polecats included, is ignored (gt-lauly).
//
// This allowlist is a policy fence, not proof of authorship: bd records no
// session, all writers share one OS user, and a comment's author is whatever
// the writer typed. The layers that catch a bypass are the tap guard refusing a
// polecat's ACCEPTED-RISK write (gt-z71j0) and the audit of every use
// (gt-tvhg8).
func isAcceptedRiskAuthor(author string) bool {
	switch strings.TrimSpace(author) {
	case "Sloan Ahrens", "town/crew/overseer":
		return true
	}
	return false
}

// capAcceptedRisks joins entries oldest first and keeps the longest run of the
// newest that fits acceptedRiskCap.
func capAcceptedRisks(entries []string) string {
	return capEntries(entries, acceptedRiskCap)
}

// capEntries joins entries oldest first and keeps the longest run of the newest
// that fits limit bytes, in one pass from the newest entry back. A newest entry
// larger than the limit is cut rather than dropped: om truncates the file at
// that size anyway, and half the newest record is worth more than a whole older
// one. The cut lands on a rune boundary.
func capEntries(entries []string, limit int) string {
	const sep = "\n\n"
	size, keep := 0, len(entries)
	for keep > 0 {
		grown := size + len(entries[keep-1])
		if keep < len(entries) {
			grown += len(sep)
		}
		if keep < len(entries) && grown > limit {
			break
		}
		size = grown
		keep--
	}
	return cutToBytes(strings.Join(entries[keep:], sep), limit)
}

// cutToBytes returns s's first n bytes, stopping on a rune boundary so a
// multi-byte character is never split.
func cutToBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// CriteriaText is the work bead's acceptance text, the criteria om reads to
// judge a diff against what the bead asked for (OMReviewer gives it to om
// through --criteria-file, gt-yl7n4). It is beads.AcceptanceText, the same
// text policyRejection and gt done count unchecked boxes in, so the review
// and the refusal never read different criteria.
//
// The whole text comes back, checked and unchecked items alike: what an
// unchecked item means is policyRejection's business. Empty means the bead
// carries no acceptance text, and the review runs without the flag.
func CriteriaText(issue *beads.Issue) string {
	return beads.AcceptanceText(issue)
}
