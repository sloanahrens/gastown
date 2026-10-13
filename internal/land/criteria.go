package land

import (
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

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
