package dashboard

import (
	"strings"
	"testing"
)

// TestHealthPillNamesTheFirstCause reads the embedded page: the header pill is
// what answers "why is the town not green" at a glance, so it must render the
// verdict's worst cause beside the verdict and keep the rest in its title
// (gt-70aa6). The state is data-driven, so the page's wiring is what this
// checks.
func TestHealthPillNamesTheFirstCause(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	start := strings.Index(page, "function renderHealth(h)")
	if start < 0 {
		t.Fatal("index.html has no renderHealth")
	}
	body := page[start:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end]
	}
	for _, want := range []string{"causes[0]", "p.title", "causeText"} {
		if !strings.Contains(body, want) {
			t.Errorf("renderHealth does not mention %s", want)
		}
	}
}
