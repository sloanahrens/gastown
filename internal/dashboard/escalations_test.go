package dashboard

import (
	"regexp"
	"testing"
)

// A critical escalation must not read like a low one: the page colours each of
// the four severities, and no two share a colour. The mapping lives in the page
// (SEV_CLASS) and its colours in the stylesheet, so both are read here.
func TestEscalationSeverityColoursAreDistinct(t *testing.T) {
	t.Parallel()
	page := string(indexHTML)

	seen := map[string]string{}
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if !regexp.MustCompile(`\b` + sev + `: "sev-` + sev + `"`).MatchString(page) {
			t.Errorf("index.html does not map severity %q to its class", sev)
		}
		m := regexp.MustCompile(`\.sev-` + sev + `\{color:var\(--(\w+)\)\}`).FindStringSubmatch(page)
		if m == nil {
			t.Errorf(".sev-%s has no colour in index.html", sev)
			continue
		}
		seen[sev] = m[1]
	}
	distinct := map[string]bool{}
	for _, colour := range seen {
		distinct[colour] = true
	}
	if len(seen) == 4 && len(distinct) != 4 {
		t.Errorf("the four severities share colours: %v", seen)
	}
}
