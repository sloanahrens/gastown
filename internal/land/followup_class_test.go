package land

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestFollowupClass(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		FindingClassComment: true, FindingClassTestStyle: true, FindingClassBehaviour: false,
		FindingClassDesign: false, FindingClassMissingTest: false, FindingClassProcess: false, "": false,
	}
	for class, want := range cases {
		if got := followupNoted(class); got != want {
			t.Errorf("followupNoted(%q) = %v, want %v", class, got, want)
		}
	}
}

func TestSplitNoted(t *testing.T) {
	t.Parallel()
	filed, noted := splitNoted([]Finding{
		{Title: "a behaviour", Class: FindingClassBehaviour},
		{Title: "a comment", Class: FindingClassComment},
		{Title: "a design", Class: FindingClassDesign},
		{Title: "a test style", Class: FindingClassTestStyle},
		{Title: "a missing test", Class: FindingClassMissingTest},
		{Title: "a process", Class: FindingClassProcess},
		{Title: "an old finding", Class: ""},
	})
	if got, want := findingTitles(noted), []string{"a comment", "a test style"}; !slices.Equal(got, want) {
		t.Errorf("noted = %v, want %v", got, want)
	}
	if got, want := findingTitles(filed), []string{"a behaviour", "a design", "a missing test", "a process", "an old finding"}; !slices.Equal(got, want) {
		t.Errorf("filed = %v, want %v", got, want)
	}
}

func findingTitles(findings []Finding) []string {
	titles := make([]string, len(findings))
	for i, f := range findings {
		titles[i] = f.Title
	}
	return titles
}

func TestFollowupNoteComment(t *testing.T) {
	t.Parallel()
	noted := make([]Finding, 25)
	for i := range noted {
		noted[i] = Finding{Title: fmt.Sprintf("issue %d", i+1), Path: "a.go", Line: i + 1, Class: FindingClassComment}
	}
	got := followupNoteComment(Work{BeadID: "gt-abc"}, Result{
		LandedCommit: "0123456789abcdef",
		Verdict:      Verdict{Score: 0.82},
	}, noted)
	header := "minor notes om raised on gt-abc (approved at 0.82, landed 01234567), filed as no bead:"
	if !strings.HasPrefix(got, header+"\n") || !strings.HasSuffix(got, "\nand 5 more") {
		t.Errorf("note = %q, want it to open with %q and end with \"and 5 more\"", got, header)
	}
	if n := strings.Count(got, "\n- "); n != 20 || !strings.Contains(got, "\n- issue 20 (a.go:20, comment)") || strings.Contains(got, "issue 21") {
		t.Errorf("note names %d findings, want exactly issues 1-20:\n%s", n, got)
	}
}
