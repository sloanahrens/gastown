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

// collapse folds the line wrapping the formula uses into single spaces, so a
// check for a phrase does not depend on where the source wraps.
func collapse(s string) string {
	return regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
}

// TestPolecatSelfReviewIsThePreSubmitChecklist pins gt-df9f7v: the self-review
// step is the seat's single pre-submit checklist. It parses, sits between the
// commit and build-check steps, and has nine numbered items whose first walks
// the bead's requirements, whose fifth bounds the seats' waits, and whose
// ninth reports the integration tier in the commit body where om reads it.
func TestPolecatSelfReviewIsThePreSubmitChecklist(t *testing.T) {
	t.Parallel()
	f := decodePolecatWork(t)

	// The checklist runs on committed changes: self-review's neighbours are
	// the commit step and the build check.
	ids := make([]string, len(f.Steps))
	for i, s := range f.Steps {
		ids[i] = s.ID
	}
	prev, next := "", ""
	for i, id := range ids {
		if id != "self-review" {
			continue
		}
		if i > 0 {
			prev = ids[i-1]
		}
		if i+1 < len(ids) {
			next = ids[i+1]
		}
	}
	if prev != "commit-changes" || next != "build-check" {
		t.Errorf("self-review sits between %q and %q, want commit-changes and build-check", prev, next)
	}

	step := f.step(t, "self-review")

	// Exactly nine items: the single pre-submit checklist.
	if got := len(regexp.MustCompile(`(?m)^\d+\. `).FindAllString(step, -1)); got != 9 {
		t.Fatalf("the self-review step has %d numbered items, want 9", got)
	}

	// Item 1 extends the plan written in "Before you write code" with the diff
	// line that meets each requirement; it points at that plan rather than
	// restating how to write it (R2).
	item1 := collapse(numberedItem(t, step, 1))
	for _, want := range []string{"Before you write code", "Acceptance"} {
		if !strings.Contains(item1, want) {
			t.Errorf("item 1 does not walk the requirements: lacks %q:\n%s", want, item1)
		}
	}
	if strings.Contains(item1, "Write a short plan") {
		t.Errorf("item 1 restates the plan procedure instead of pointing at it:\n%s", item1)
	}

	// Item 5 is the waits check: it names the unbounded-wait tokens and the
	// bound every hit needs.
	item5 := collapse(numberedItem(t, step, 5))
	for _, want := range []string{"time[.]Sleep", "deadline"} {
		if !strings.Contains(item5, want) {
			t.Errorf("item 5 is not the waits check: lacks %q:\n%s", want, item5)
		}
	}

	// Item 9 reports the integration tier in the commit body, where om reads
	// it, and reports a real outcome or a real reason. A canned line emitted
	// for every package reports a run that did not happen (rule 1).
	item9 := collapse(numberedItem(t, step, 9))
	for _, want := range []string{
		"go vet -tags integration",
		"near the top of the commit body",
		"not bead notes",
		"integration tier not run: <reason>",
	} {
		if !strings.Contains(item9, want) {
			t.Errorf("item 9 lacks %q:\n%s", want, item9)
		}
	}
	if strings.Contains(item9, "needs a container; the overseer runs it") {
		t.Errorf("item 9 hard-codes one reason for every package instead of the real one:\n%s", item9)
	}
}
