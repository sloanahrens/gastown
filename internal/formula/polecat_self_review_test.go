package formula

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// numberedItem returns the text of item n in the self-review step, from its
// "n. " line up to the next numbered item or the end of the step. It fails
// when the step has no such item.
func numberedItem(t *testing.T, step string, n int) string {
	t.Helper()
	lines := strings.Split(step, "\n")
	startRe := regexp.MustCompile(fmt.Sprintf(`^%d\. `, n))
	nextRe := regexp.MustCompile(fmt.Sprintf(`^%d\. `, n+1))
	start := -1
	for i, line := range lines {
		if start < 0 {
			if startRe.MatchString(line) {
				start = i
			}
			continue
		}
		if nextRe.MatchString(line) {
			return strings.Join(lines[start:i], "\n")
		}
	}
	if start < 0 {
		t.Fatalf("the self-review step has no item %d", n)
	}
	return strings.Join(lines[start:], "\n")
}

// TestPolecatSelfReviewIsThePreSubmitChecklist pins gt-df9f7v: the self-review
// step is the single pre-submit checklist, laid out in nine items. It opens
// with a requirement walk and checks unbounded waits, and it tells the seat to
// put the integration-tier line in the commit body, where om reads it, not in
// the bead notes.
func TestPolecatSelfReviewIsThePreSubmitChecklist(t *testing.T) {
	t.Parallel()
	f := decodePolecatWork(t)
	step := f.step(t, "self-review")

	for n := 1; n <= 9; n++ {
		if !regexp.MustCompile(fmt.Sprintf(`(?m)^%d\. `, n)).MatchString(step) {
			t.Fatalf("the self-review step has no numbered item %d", n)
		}
	}
	if regexp.MustCompile(`(?m)^10\. `).MatchString(step) {
		t.Error("the self-review step has a tenth item, want nine")
	}

	if got := numberedItem(t, step, 1); !strings.Contains(got, "Requirements (9, 12)") {
		t.Errorf("item 1 does not name Requirements (9, 12):\n%s", got)
	}
	if got := numberedItem(t, step, 5); !strings.Contains(got, "Waits (13)") {
		t.Errorf("item 5 does not name Waits (13):\n%s", got)
	}

	item9 := numberedItem(t, step, 9)
	for _, want := range []string{
		"go vet -tags integration",
		"near the top of the commit body",
		"not bead notes",
	} {
		if !strings.Contains(item9, want) {
			t.Errorf("item 9 lacks %q:\n%s", want, item9)
		}
	}
}
