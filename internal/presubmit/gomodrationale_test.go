package presubmit

import (
	"strings"
	"testing"
)

// TestMissingRationales judges the rule: a commit changing a go.mod or go.sum
// file needs a rationale line after its subject, and a commit changing neither
// is not judged at all (gt-b9j3o.6).
func TestMissingRationales(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		commit     Commit
		wantJudged int
		wantFiles  []string
	}{
		{
			name:       "go.mod without a rationale fails",
			commit:     Commit{SHA: "a1", Message: "bump x (gt-1)", Changed: []string{"go.mod", "internal/x/x.go"}},
			wantJudged: 1,
			wantFiles:  []string{"go.mod"},
		},
		{
			name:       "go.sum without a rationale fails",
			commit:     Commit{SHA: "a2", Message: "bump y (gt-1)\n\nSome prose.", Changed: []string{"go.sum"}},
			wantJudged: 1,
			wantFiles:  []string{"go.sum"},
		},
		{
			name:       "a rationale line passes",
			commit:     Commit{SHA: "a3", Message: "bump z (gt-1)\n\nRationale: z v1.2 fixes the parser panic.", Changed: []string{"go.mod"}},
			wantJudged: 1,
		},
		{
			name:       "the rationale marker is read in any case",
			commit:     Commit{SHA: "a4", Message: "bump z (gt-1)\n\nRATIONALE: z v1.2 fixes the parser panic.", Changed: []string{"go.mod"}},
			wantJudged: 1,
		},
		{
			name:       "a rationale on the subject is not one",
			commit:     Commit{SHA: "a5", Message: "Rationale: bump z (gt-1)", Changed: []string{"go.mod"}},
			wantJudged: 1,
			wantFiles:  []string{"go.mod"},
		},
		{
			name:       "a bare marker with no text is not a rationale",
			commit:     Commit{SHA: "a6", Message: "bump z (gt-1)\n\nRationale:   ", Changed: []string{"go.mod"}},
			wantJudged: 1,
			wantFiles:  []string{"go.mod"},
		},
		{
			name:       "a change without a module file is not judged",
			commit:     Commit{SHA: "a7", Message: "fix the parser (gt-1)", Changed: []string{"internal/x/x.go", "docs/x.md"}},
			wantJudged: 0,
		},
		{
			name:       "a nested module's go.mod counts",
			commit:     Commit{SHA: "a8", Message: "bump tracker (gt-1)", Changed: []string{"tracker/go.sum"}},
			wantJudged: 1,
			wantFiles:  []string{"tracker/go.sum"},
		},
		{
			name:       "a file merely named like a module file is not one",
			commit:     Commit{SHA: "a9", Message: "docs (gt-1)", Changed: []string{"docs/go.mod.md", "internal/go.sub"}},
			wantJudged: 0,
		},
		{
			name:       "both module files of one commit are named once each",
			commit:     Commit{SHA: "b1", Message: "bump q (gt-1)", Changed: []string{"go.sum", "go.mod", "go.sum"}},
			wantJudged: 1,
			wantFiles:  []string{"go.sum", "go.mod"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := MissingRationales([]Commit{tt.commit})
			if res.Judged != tt.wantJudged {
				t.Errorf("Judged = %d, want %d", res.Judged, tt.wantJudged)
			}
			if len(tt.wantFiles) == 0 {
				if len(res.Missing) != 0 {
					t.Fatalf("Missing = %v, want none", res.Missing)
				}
				return
			}
			if len(res.Missing) != 1 {
				t.Fatalf("Missing = %v, want one finding", res.Missing)
			}
			if got := res.Missing[0]; got.SHA != tt.commit.SHA {
				t.Errorf("finding names %q, want %q", got.SHA, tt.commit.SHA)
			}
			if got := res.Missing[0].Files; strings.Join(got, ",") != strings.Join(tt.wantFiles, ",") {
				t.Errorf("finding names files %v, want %v", got, tt.wantFiles)
			}
		})
	}
}

// TestMissingRationalesCountsEveryCommit: one unexplained module change among
// commits that are explained or touch no module file fails on its own, and the
// count says how many commits were judged (gt-b9j3o.6).
func TestMissingRationalesCountsEveryCommit(t *testing.T) {
	t.Parallel()
	res := MissingRationales([]Commit{
		{SHA: "c1", Message: "fix x (gt-1)", Changed: []string{"internal/x/x.go"}},
		{SHA: "c2", Message: "bump y (gt-1)\n\nRationale: y v2 drops the broken API.", Changed: []string{"go.mod"}},
		{SHA: "c3", Message: "bump z (gt-1)", Changed: []string{"go.sum"}},
	})
	if res.Judged != 2 {
		t.Errorf("Judged = %d, want 2", res.Judged)
	}
	if len(res.Missing) != 1 || res.Missing[0].SHA != "c3" {
		t.Fatalf("Missing = %v, want only c3", res.Missing)
	}
	if want := "bump z (gt-1)"; res.Missing[0].Subject != want {
		t.Errorf("Subject = %q, want %q", res.Missing[0].Subject, want)
	}
}

// TestMissingRationalesOnAnEmptyBranch: a branch with no commits judges none
// and reports no finding, which the command prints as the counts it judged.
func TestMissingRationalesOnAnEmptyBranch(t *testing.T) {
	t.Parallel()
	res := MissingRationales(nil)
	if res.Judged != 0 || len(res.Missing) != 0 {
		t.Errorf("result = %+v, want nothing judged and no finding", res)
	}
}
