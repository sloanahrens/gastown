package presubmit

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// onlyGit strips the -c core.quotePath=false pair RedBaseOnlyTestsAndDocs puts before the subcommand, so fakeGit answers the diff.
type onlyGit struct{ fakeGit }

func (g *onlyGit) run(args []string) ([]byte, error) {
	if len(args) > 1 && args[0] == "-c" {
		args = args[2:]
	}
	return g.fakeGit.run(args)
}

func TestRedBaseOnly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		diff    string
		diffErr error
		base    string
		want    bool
		wantErr string
	}{
		{"test files with a markdown document give true", "pkg/a_test.go\x00other/b_test.go\x00docs/x.md\x00", nil, "main", true, ""},
		{"a Go source file among them gives false", "pkg/a_test.go\x00pkg/impl.go\x00", nil, "main", false, ""},
		{"a Makefile among them gives false", "docs/x.md\x00Makefile\x00", nil, "main", false, ""},
		{"a testdata file among them gives false", "pkg/a_test.go\x00pkg/testdata/case.json\x00", nil, "main", false, ""},
		{"a deleted source file among them gives false", "pkg/a_test.go\x00pkg/gone.go\x00", nil, "main", false, ""},
		{"no changed file gives false", "", nil, "main", false, ""},
		{"an empty base is returned", "", nil, "", false, "empty base"},
		{"a failed diff is returned", "", fmt.Errorf("boom"), "main", false, "git diff"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := onlyGit{fakeGit{diff: tt.diff, diffErr: tt.diffErr}}
			got, err := RedBaseOnlyTestsAndDocs(g.run, tt.base)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("RedBaseOnlyTestsAndDocs(%q) error = %v, want one mentioning %q", tt.base, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("RedBaseOnlyTestsAndDocs() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("RedBaseOnlyTestsAndDocs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRedBaseOnlyArgs(t *testing.T) {
	t.Parallel()
	g := onlyGit{fakeGit{diff: "pkg/a_test.go\x00"}}
	if _, err := RedBaseOnlyTestsAndDocs(g.run, "main"); err != nil {
		t.Fatalf("RedBaseOnlyTestsAndDocs() error = %v", err)
	}
	args := g.diffArgs
	if len(args) == 0 || args[len(args)-1] != "main...HEAD" || slices.Contains(args, "--") || !slices.Contains(args, "-z") || !slices.Contains(args, "--no-renames") || !slices.Contains(args, "--end-of-options") {
		t.Errorf("diff args = %v, want every path, NUL separated, renames off, no pathspec, the revision last", args)
	}
}
