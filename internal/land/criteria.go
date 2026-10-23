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

// AcceptedRisksText is the accepted-risk record the work bead's comments carry,
// which om is given through --context-file so it reports a finding that only
// restates a settled decision as info rather than refusing the change on it
// (gt-lauly).
//
// A comment counts when its text opens with "ACCEPTED-RISK:" and its author is
// on the allowlist (isAcceptedRiskAuthor): an allowlist, not a denylist, so an
// author shape nobody listed is ignored rather than trusted, and a polecat can
// waive nothing on its own work. Entries are joined oldest first, each under a
// line naming its author and date, and the join is capped at acceptedRiskCap
// bytes by dropping the oldest: the newest entry is the one the running review
// is judged against. When any comment's date does not parse, the order is the
// list's own, since no date can say where that comment belongs. Empty means the
// bead records no accepted risk, and the review runs without the flag.
func AcceptedRisksText(comments []beads.Comment) string {
	risks := make([]acceptedRisk, 0, len(comments))
	dated := true
	for _, c := range comments {
		text := strings.TrimSpace(c.Text)
		if !strings.HasPrefix(text, acceptedRiskMarker) || !isAcceptedRiskAuthor(c.Author) {
			continue
		}
		at, ok := commentTime(c.CreatedAt)
		dated = dated && ok
		risks = append(risks, acceptedRisk{at: at,
			entry: acceptedRiskEntry(c.Author, c.CreatedAt, text)})
	}
	if dated {
		sort.SliceStable(risks, func(i, j int) bool { return risks[i].at < risks[j].at })
	}
	entries := make([]string, len(risks))
	for i, r := range risks {
		entries[i] = r.entry
	}
	return capAcceptedRisks(entries)
}

// acceptedRisk is one qualifying comment: the entry it renders to, and the
// moment it was written, which orders the join when every comment has one.
type acceptedRisk struct {
	at    int64
	entry string
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

// isAcceptedRiskAuthor reports whether an author may record an accepted risk:
// the shared human identity or the overseer, matched exactly after trimming and
// case-sensitively. Every other author, polecats included, is ignored (gt-lauly).
func isAcceptedRiskAuthor(author string) bool {
	switch strings.TrimSpace(author) {
	case "Sloan Ahrens", "town/crew/overseer":
		return true
	}
	return false
}

// capAcceptedRisks joins entries oldest first and keeps the longest run of the
// newest that fits acceptedRiskCap, in one pass from the newest entry back. A
// newest entry larger than the cap is cut rather than dropped: om truncates the
// file at that size anyway, and half the newest record is worth more than a
// whole older one.
func capAcceptedRisks(entries []string) string {
	const sep = "\n\n"
	size, keep := 0, len(entries)
	for keep > 0 {
		grown := size + len(entries[keep-1])
		if keep < len(entries) {
			grown += len(sep)
		}
		if keep < len(entries) && grown > acceptedRiskCap {
			break
		}
		size = grown
		keep--
	}
	return cutToBytes(strings.Join(entries[keep:], sep), acceptedRiskCap)
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
// through --criteria-file, gt-yl7n4).
//
// The bead's own acceptance field wins when it is set, but the spec dispatcher
// shapes a work bead as one description carrying the "## Acceptance" template
// section and leaves the field empty, so the description's section is the
// source nearly every landing has. The whole section comes back, checked and
// unchecked items alike: what an unchecked item means is policyRejection's
// business and is unchanged here.
//
// Empty means the bead carries no acceptance text, and the review runs without
// the flag.
func CriteriaText(issue *beads.Issue) string {
	if issue == nil {
		return ""
	}
	if text := strings.TrimSpace(issue.AcceptanceCriteria); text != "" {
		return text
	}
	return acceptanceSection(issue.Description)
}

// acceptanceSection is the body under the description's "## Acceptance"
// heading, up to the next "## " heading or the end of the description. The
// heading itself is not part of the text; the heading must be the section's
// whole title, so "## Acceptance" matches and "## Accepted trade-offs" does
// not.
func acceptanceSection(description string) string {
	var body []string
	inside := false
	for _, line := range strings.Split(description, "\n") {
		trimmed := strings.TrimSpace(line)
		// A deeper heading ("### a criterion") is body text: only a "## "
		// line opens or closes a section.
		if strings.HasPrefix(trimmed, "## ") {
			if inside {
				break
			}
			inside = strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(trimmed, "## ")), "acceptance")
			continue
		}
		if inside {
			body = append(body, line)
		}
	}
	return strings.TrimSpace(strings.Join(body, "\n"))
}
