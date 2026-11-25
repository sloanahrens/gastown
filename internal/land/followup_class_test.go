package land

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestFollowupNoted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		f    Finding
		want bool
	}{
		{"a comment anywhere", Finding{Title: "a comment", Path: "internal/land/x.go", Class: FindingClassComment}, true},
		{"test style anywhere", Finding{Title: "a test style", Path: "internal/land/x_test.go", Class: FindingClassTestStyle}, true},
		{"behaviour in code", Finding{Title: "a behaviour", Path: "internal/land/x.go", Class: FindingClassBehaviour}, false},
		{"behaviour under docs", Finding{Title: "a behaviour", Path: "docs/x.md", Class: FindingClassBehaviour}, false},
		{"behaviour under directives", Finding{Title: "a behaviour", Path: "directives/x.md", Class: FindingClassBehaviour}, false},
		{"design under docs", Finding{Title: "a design", Path: "docs/x.md", Class: FindingClassDesign}, true},
		{"design under directives", Finding{Title: "a design", Path: "directives/x.md", Class: FindingClassDesign}, true},
		{"empty class under docs", Finding{Title: "an old finding", Path: "docs/x.md"}, true},
		{"empty class under directives", Finding{Title: "an old finding", Path: "directives/x.md"}, true},
		{"missing test under docs", Finding{Title: "a missing test", Path: "docs/x.md", Class: FindingClassMissingTest}, true},
		{"process under directives", Finding{Title: "a process", Path: "directives/x.md", Class: FindingClassProcess}, true},
		{"design in code", Finding{Title: "a design", Path: "internal/land/x.go", Class: FindingClassDesign}, false},
		{"empty class in code", Finding{Title: "an old finding", Path: "internal/land/x.go"}, false},
		{"a look-alike prefix", Finding{Title: "a design", Path: "docsite/x.md", Class: FindingClassDesign}, false},
		{"a docs dir below the root", Finding{Title: "a design", Path: "internal/docs/x.md", Class: FindingClassDesign}, false},
		{"a cleaned docs path", Finding{Title: "a design", Path: "./docs/x.md", Class: FindingClassDesign}, true},
		{"the docs directory itself", Finding{Title: "a design", Path: "docs", Class: FindingClassDesign}, true},
		{"no path", Finding{Title: "a design", Class: FindingClassDesign}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := followupNoted(c.f); got != c.want {
				t.Errorf("followupNoted(%+v) = %v, want %v", c.f, got, c.want)
			}
		})
	}
}

func TestSplitNoted(t *testing.T) {
	t.Parallel()
	filed, noted := splitNoted([]Finding{
		{Title: "a behaviour", Path: "internal/land/x.go", Class: FindingClassBehaviour},
		{Title: "a comment", Path: "internal/land/x.go", Class: FindingClassComment},
		{Title: "a design", Path: "internal/land/x.go", Class: FindingClassDesign},
		{Title: "a test style", Path: "internal/land/x_test.go", Class: FindingClassTestStyle},
		{Title: "a docs test style", Path: "docs/x.md", Class: FindingClassTestStyle},
		{Title: "a missing test", Path: "internal/land/x.go", Class: FindingClassMissingTest},
		{Title: "a process", Path: "internal/land/x.go", Class: FindingClassProcess},
		{Title: "an old finding", Path: "internal/land/x.go", Class: ""},
		{Title: "docs design", Path: "docs/x.md", Class: FindingClassDesign},
		{Title: "directive design", Path: "directives/x.md", Class: FindingClassDesign},
		{Title: "docs behaviour", Path: "docs/x.md", Class: FindingClassBehaviour},
		{Title: "a docs comment", Path: "docs/x.md", Class: FindingClassComment},
	})
	if got, want := findingTitles(noted), []string{"a comment", "a test style", "a docs test style", "docs design", "directive design", "a docs comment"}; !slices.Equal(got, want) {
		t.Errorf("noted = %v, want %v", got, want)
	}
	if got, want := findingTitles(filed), []string{"a behaviour", "a design", "a missing test", "a process", "an old finding", "docs behaviour"}; !slices.Equal(got, want) {
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
