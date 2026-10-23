package land

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
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

// AcceptedRisksText is the accepted-risk record the work bead's comments carry,
// which om is given through --context-file so it reports a finding that only
// restates a settled decision as info rather than refusing the change on it
// (gt-lauly).
//
// A comment counts when its text opens with "ACCEPTED-RISK:" and its author is
// not a polecat, so a polecat can waive nothing on its own work. Entries are
// joined oldest first, each under a line naming its author and date, and the
// join is capped at acceptedRiskCap bytes by dropping the oldest: the newest
// entry is the one the running review is judged against. Empty means the bead
// records no accepted risk, and the review runs without the flag.
func AcceptedRisksText(comments []beads.Comment) string {
	risks := make([]acceptedRisk, 0, len(comments))
	for _, c := range comments {
		text := strings.TrimSpace(c.Text)
		if !strings.HasPrefix(text, acceptedRiskMarker) || isPolecatAuthor(c.Author) {
			continue
		}
		risks = append(risks, acceptedRisk{at: commentTime(c.CreatedAt),
			entry: acceptedRiskEntry(c.Author, c.CreatedAt, text)})
	}
	sort.SliceStable(risks, func(i, j int) bool { return risks[i].at < risks[j].at })
	entries := make([]string, len(risks))
	for i, r := range risks {
		entries[i] = r.entry
	}
	return capAcceptedRisks(entries)
}

// acceptedRisk is one qualifying comment: the entry it renders to, and the
// moment it was written, which orders the join and decides what a cap drops.
type acceptedRisk struct {
	at    int64
	entry string
}

// commentTime reads a comment's creation time in unix seconds, and the oldest
// representable moment for a date that does not parse: a timestamp nothing can
// read is the entry a cap drops first.
func commentTime(created string) int64 {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(created))
	if err != nil {
		return math.MinInt64
	}
	return t.Unix()
}

// acceptedRiskEntry renders one comment under the line that attributes it: om
// weighs a named author's decision, and an assertion with no one behind it is
// not a decision to weigh.
func acceptedRiskEntry(author, created, text string) string {
	author, created = strings.TrimSpace(author), strings.TrimSpace(created)
	if author == "" {
		author = "an unnamed author"
	}
	if created == "" {
		created = "an unrecorded date"
	}
	return fmt.Sprintf("[ACCEPTED-RISK from %s, %s]\n%s", author, created, text)
}

// isPolecatAuthor reports whether an address names a polecat
// ("gastown/polecats/capable"), read by path segment so a rig or pool whose name
// merely contains the word is not one.
func isPolecatAuthor(author string) bool {
	for _, segment := range strings.Split(strings.ToLower(author), "/") {
		if strings.TrimSpace(segment) == "polecats" {
			return true
		}
	}
	return false
}

// capAcceptedRisks joins entries oldest first and drops the oldest until the
// join fits acceptedRiskCap. A newest entry larger than the cap is cut rather
// than dropped: om truncates the file at that size anyway, and half the newest
// record is worth more than a whole older one.
func capAcceptedRisks(entries []string) string {
	joined := strings.Join(entries, "\n\n")
	for len(entries) > 1 && len(joined) > acceptedRiskCap {
		entries = entries[1:]
		joined = strings.Join(entries, "\n\n")
	}
	if len(joined) > acceptedRiskCap {
		joined = cutToBytes(joined, acceptedRiskCap)
	}
	return joined
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
