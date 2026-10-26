package presubmit

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const (
	testModule   = "github.com/steveyegge/gastown"
	appPath      = "internal/app/app.go"
	appCoverPath = testModule + "/" + appPath
)

// addDiff is a one-file diff with no context that adds lines starting at line
// start of the new file, the shape `git diff -U0` writes.
func addDiff(file string, start int, added ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -%d,0 +%d,%d @@\n",
		file, file, file, file, start-1, start, len(added))
	for _, line := range added {
		b.WriteString("+" + line + "\n")
	}
	return b.String()
}

// coverOf is a cover profile holding the given blocks, each written in go
// test's "file:start.col,end.col stmts count" shape.
func coverOf(blocks ...string) string {
	return "mode: set\n" + strings.Join(blocks, "\n") + "\n"
}

func TestUntestedErrorBranches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		diff  string
		cover string
		want  []Finding
	}{
		{
			name:  "an added error return no test drives fails the check",
			diff:  addDiff(appPath, 12, `		return fmt.Errorf("load: %w", err)`),
			cover: coverOf(appCoverPath + ":12.3,12.44 1 0"),
			want:  []Finding{{Path: appPath, Line: 12, Text: `return fmt.Errorf("load: %w", err)`}},
		},
		{
			name:  "an error return the branch's tests drive passes",
			diff:  addDiff(appPath, 12, `		return fmt.Errorf("load: %w", err)`),
			cover: coverOf(appCoverPath + ":12.3,12.44 1 1"),
			want:  nil,
		},
		{
			name:  "a block that starts after the return on its line does not drive it",
			diff:  addDiff(appPath, 12, `		return err`),
			cover: coverOf(appCoverPath + ":12.20,13.4 1 1"),
			want:  []Finding{{Path: appPath, Line: 12, Text: "return err"}},
		},
		{
			name:  "the err variable is an error return",
			diff:  addDiff(appPath, 12, `		return err`),
			cover: coverOf(appCoverPath + ":12.3,12.12 1 0"),
			want:  []Finding{{Path: appPath, Line: 12, Text: "return err"}},
		},
		{
			name:  "an error returned beside other values is judged",
			diff:  addDiff(appPath, 12, `		return "", err`),
			cover: coverOf(appCoverPath + ":12.3,12.16 1 0"),
			want:  []Finding{{Path: appPath, Line: 12, Text: `return "", err`}},
		},
		{
			name:  "a named error value is judged",
			diff:  addDiff(appPath, 12, `		return writeErr`),
			cover: coverOf(appCoverPath + ":12.3,12.18 1 0"),
			want:  []Finding{{Path: appPath, Line: 12, Text: "return writeErr"}},
		},
		{
			name:  "an EOF return is judged",
			diff:  addDiff(appPath, 12, `		return io.EOF`),
			cover: coverOf(appCoverPath + ":12.3,12.16 1 0"),
			want:  []Finding{{Path: appPath, Line: 12, Text: "return io.EOF"}},
		},
		{
			name:  "a trailing comment does not hide the error",
			diff:  addDiff(appPath, 12, `		return err // the caller retries`),
			cover: coverOf(appCoverPath + ":12.3,12.12 1 0"),
			want:  []Finding{{Path: appPath, Line: 12, Text: "return err // the caller retries"}},
		},
		{
			name:  "a comparison that returns a bool is not an error return",
			diff:  addDiff(appPath, 12, `		return err == nil`),
			cover: coverOf(appCoverPath + ":12.3,12.20 1 0"),
			want:  nil,
		},
		{
			name:  "a call that may return an error is left unjudged",
			diff:  addDiff(appPath, 12, `		return f.Close()`),
			cover: coverOf(appCoverPath + ":12.3,12.18 1 0"),
			want:  nil,
		},
		{
			name:  "a return of no error is not judged",
			diff:  addDiff(appPath, 12, `		return nil`),
			cover: coverOf(appCoverPath + ":12.3,12.12 1 0"),
			want:  nil,
		},
		{
			name:  "a line that only mentions an error name is not a return",
			diff:  addDiff(appPath, 12, `		return log.Printf("load: %v", err)`),
			cover: coverOf(appCoverPath + ":12.3,12.40 1 1"),
			want:  nil,
		},
		{
			name:  "a test file is not a source",
			diff:  addDiff("internal/app/app_test.go", 12, `		return err`),
			cover: coverOf("github.com/steveyegge/gastown/internal/app/app_test.go:12.3,12.12 1 0"),
			want:  nil,
		},
		{
			name:  "a testdata fixture is not a source",
			diff:  addDiff("internal/app/testdata/fixture.go", 12, `		return err`),
			cover: coverOf("github.com/steveyegge/gastown/internal/app/testdata/fixture.go:12.3,12.12 1 0"),
			want:  nil,
		},
		{
			name:  "a Go file at the repo root is a source",
			diff:  addDiff("main.go", 5, `	return err`),
			cover: coverOf("github.com/steveyegge/gastown/main.go:5.2,5.10 1 0"),
			want:  []Finding{{Path: "main.go", Line: 5, Text: "return err"}},
		},
		{
			name:  "a directory the go tool skips is not a source",
			diff:  addDiff("internal/app/_build/gen.go", 5, `	return err`),
			cover: coverOf("github.com/steveyegge/gastown/internal/app/_build/gen.go:5.2,5.10 1 0"),
			want:  nil,
		},
		{
			name:  "a file the profile does not mention is unjudged",
			diff:  addDiff("internal/app/other.go", 12, `		return err`),
			cover: coverOf(appCoverPath + ":12.3,12.12 1 0"),
			want:  nil,
		},
		{
			name:  "a profile that names no file judges nothing",
			diff:  addDiff(appPath, 12, `		return err`),
			cover: coverOf(),
			want:  nil,
		},
		{
			name: "a removed error return is not an addition",
			diff: "diff --git a/internal/app/app.go b/internal/app/app.go\n" +
				"--- a/internal/app/app.go\n+++ b/internal/app/app.go\n@@ -12,1 +11,0 @@\n-\t\treturn err\n",
			cover: coverOf(appCoverPath + ":12.3,12.12 1 0"),
			want:  nil,
		},
		{
			name: "context lines advance the line number",
			diff: "diff --git a/internal/app/app.go b/internal/app/app.go\n" +
				"--- a/internal/app/app.go\n+++ b/internal/app/app.go\n" +
				"@@ -9,2 +9,3 @@ func Load() error {\n" +
				" \tctx := context.Background()\n" +
				"+\t\treturn fmt.Errorf(\"load: %w\", err)\n" +
				" \t_ = ctx\n",
			cover: coverOf(appCoverPath + ":10.3,10.40 1 0"),
			want:  []Finding{{Path: appPath, Line: 10, Text: `return fmt.Errorf("load: %w", err)`}},
		},
		{
			name: "two files in one diff are both judged",
			diff: addDiff(appPath, 12, `		return err`) +
				addDiff("internal/app/other.go", 30, `		return err`),
			cover: coverOf(
				appCoverPath+":12.3,12.12 1 0",
				"github.com/steveyegge/gastown/internal/app/other.go:30.3,30.12 1 0",
			),
			want: []Finding{
				{Path: appPath, Line: 12, Text: "return err"},
				{Path: "internal/app/other.go", Line: 30, Text: "return err"},
			},
		},
		{
			name:  "an empty diff is not judged",
			diff:  "",
			cover: coverOf(),
			want:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := UntestedErrorBranches(strings.NewReader(tt.diff), strings.NewReader(tt.cover), testModule, nil, nil)
			if err != nil {
				t.Fatalf("UntestedErrorBranches: %v", err)
			}
			if !reflect.DeepEqual(got.Findings, tt.want) {
				t.Errorf("findings = %#v, want %#v", got.Findings, tt.want)
			}
		})
	}
}

// TestUntestedErrorBranchesRefusesInputItCannotRead drives every shape the
// check refuses, so no malformed diff or profile can pass as a branch with
// nothing to judge.
func TestUntestedErrorBranchesRefusesInputItCannotRead(t *testing.T) {
	t.Parallel()
	goodCover := coverOf(appCoverPath + ":12.3,12.12 1 0")
	goodDiff := addDiff(appPath, 12, `		return err`)
	tests := []struct {
		name  string
		diff  string
		cover string
	}{
		{"an unreadable hunk header", "@@ nonsense @@\n", goodCover},
		{"a hunk line number too large for an int", "@@ -1,0 +99999999999999999999,1 @@\n", goodCover},
		{"a profile without its mode line", goodDiff, appCoverPath + ":12.3,12.12 1 0\n"},
		{"a profile line that is not a block", goodDiff, coverOf("garbage")},
		{"a profile block whose count is not a number", goodDiff, coverOf(appCoverPath + ":12.3,12.12 1 x")},
		{"a profile block with no file", goodDiff, coverOf("garbage 1 0")},
		{"a profile range whose position has no column", goodDiff, coverOf(appCoverPath + ":12,12.12 1 0")},
		{"a profile range whose line is not a number", goodDiff, coverOf(appCoverPath + ":1x.3,12.12 1 0")},
		{"a profile range whose column is not a number", goodDiff, coverOf(appCoverPath + ":12.3,12.1x 1 0")},
		{"a profile range with no comma", goodDiff, coverOf(appCoverPath + ":12.3 1 0")},
		{"a profile start position with no column", goodDiff, coverOf(appCoverPath + ":12,13.4 1 0")},
		{"a profile end position with no column", goodDiff, coverOf(appCoverPath + ":12.3,13 1 0")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := UntestedErrorBranches(strings.NewReader(tt.diff), strings.NewReader(tt.cover), testModule, nil, nil); err == nil {
				t.Error("UntestedErrorBranches accepted input it cannot read")
			}
		})
	}
}

// failingReader is a reader that always fails, the shape of a diff or profile
// on a pipe that broke mid-read.
type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestUntestedErrorBranchesReportsAReadError(t *testing.T) {
	t.Parallel()
	boom := errors.New("read failed")
	if _, err := UntestedErrorBranches(failingReader{boom}, strings.NewReader(coverOf()), testModule, nil, nil); !errors.Is(err, boom) {
		t.Errorf("diff read error = %v, want %v", err, boom)
	}
	if _, err := UntestedErrorBranches(strings.NewReader(""), failingReader{boom}, testModule, nil, nil); !errors.Is(err, boom) {
		t.Errorf("cover read error = %v, want %v", err, boom)
	}
}

// TestUntestedErrorBranchesJudgesOnlyTheNamedFile: a profile file of another package with the same
// base name, or a root file the profile names by its module alone, never judges the diff's file
// (the full name must match, not a suffix).
func TestUntestedErrorBranchesJudgesOnlyTheNamedFile(t *testing.T) {
	t.Parallel()
	diff := addDiff("errbranch.go", 7, `	return err`)
	cover := coverOf(testModule + "/internal/other/errbranch.go:7.2,7.11 1 0")
	got, err := UntestedErrorBranches(strings.NewReader(diff), strings.NewReader(cover), testModule, nil, nil)
	if err != nil {
		t.Fatalf("UntestedErrorBranches: %v", err)
	}
	if got.Judged != 0 || len(got.Findings) != 0 {
		t.Errorf("a same-named file of another package judged the root file: %+v", got)
	}
}

// TestUntestedErrorBranchesCountsWhatItJudged: the result says how many added error returns were
// judged, how many the profile did not cover, and how many sat in quoted paths.
func TestUntestedErrorBranchesCountsWhatItJudged(t *testing.T) {
	t.Parallel()
	diff := addDiff(appPath, 12, `		return fmt.Errorf("load: %w", err)`) +
		addDiff("internal/unbuilt/x.go", 3, `	return err`) +
		"diff --git a/x b/x\n--- a/x\n+++ \"b/internal/quoted name.go\"\n@@ -0,0 +4 @@\n+\treturn err\n"
	cover := coverOf(appCoverPath + ":12.3,12.44 1 0")
	got, err := UntestedErrorBranches(strings.NewReader(diff), strings.NewReader(cover), testModule, nil, nil)
	if err != nil {
		t.Fatalf("UntestedErrorBranches: %v", err)
	}
	if got.Added != 2 || got.Judged != 1 || got.Quoted != 1 || len(got.Findings) != 1 {
		t.Errorf("counts = added %d judged %d quoted %d findings %d, want 2 1 1 1", got.Added, got.Judged, got.Quoted, len(got.Findings))
	}
}

// TestUntestedErrorBranchesMatchesColumns: a return that shares a line with an executed block is
// not passed by that block; only a block whose line.column range holds the return drives it.
func TestUntestedErrorBranchesMatchesColumns(t *testing.T) {
	t.Parallel()
	diff := addDiff(appPath, 12, `		return err`)
	// The return sits at column 3. An executed block on the same line ends before it (columns 1-2),
	// and the return's own block (columns 3-10) was not executed: a line-granular match would pass it.
	cover := coverOf(appCoverPath+":12.1,12.2 1 1", appCoverPath+":12.3,12.10 1 0")
	got, err := UntestedErrorBranches(strings.NewReader(diff), strings.NewReader(cover), testModule, nil, nil)
	if err != nil {
		t.Fatalf("UntestedErrorBranches: %v", err)
	}
	if len(got.Findings) != 1 {
		t.Errorf("findings = %+v, want the uncovered return on a line with an executed block", got.Findings)
	}
}

// TestUntestedErrorBranchesFailsAnUnprofiledRootFile: an added error return in a root-module file
// the profile does not name is unjudged and reported, even when another return in the diff is judged
// and driven (gt-b9j3o.5, overseer ruling).
func TestUntestedErrorBranchesFailsAnUnprofiledRootFile(t *testing.T) {
	t.Parallel()
	diff := addDiff(appPath, 12, `		return fmt.Errorf("load: %w", err)`) + addDiff("root.go", 3, `	return err`)
	cover := coverOf(appCoverPath + ":12.3,12.44 1 1")
	got, err := UntestedErrorBranches(strings.NewReader(diff), strings.NewReader(cover), testModule, nil, nil)
	if err != nil {
		t.Fatalf("UntestedErrorBranches: %v", err)
	}
	if len(got.Unjudged) != 1 || got.Unjudged[0].Path != "root.go" || got.Judged != 1 {
		t.Errorf("unjudged = %+v judged = %d, want root.go unjudged beside the judged return", got.Unjudged, got.Judged)
	}
}

// TestUntestedErrorBranchesLeavesNestedModulesOut: a file the nested predicate names is not an added
// error return of this module at all (ACCEPTED-RISK (a) on gt-b9j3o.5).
func TestUntestedErrorBranchesLeavesNestedModulesOut(t *testing.T) {
	t.Parallel()
	diff := addDiff("tracker/x.go", 3, `	return err`)
	nested := func(p string) bool { return strings.HasPrefix(p, "tracker/") }
	got, err := UntestedErrorBranches(strings.NewReader(diff), strings.NewReader(coverOf()), testModule, nested, nil)
	if err != nil {
		t.Fatalf("UntestedErrorBranches: %v", err)
	}
	if got.Added != 0 || len(got.Unjudged) != 0 {
		t.Errorf("a nested-module return was counted: added %d unjudged %+v", got.Added, got.Unjudged)
	}
}

// TestUntestedErrorBranchesReportsExcludedFiles: an unjudged return in a file the build constraints
// exclude is reported as excluded and is not unjudged, so it does not fail (gt-b9j3o.5).
func TestUntestedErrorBranchesReportsExcludedFiles(t *testing.T) {
	t.Parallel()
	diff := addDiff("root_darwin.go", 3, `	return err`)
	excluded := func(p string) bool { return p == "root_darwin.go" }
	got, err := UntestedErrorBranches(strings.NewReader(diff), strings.NewReader(coverOf()), testModule, nil, excluded)
	if err != nil {
		t.Fatalf("UntestedErrorBranches: %v", err)
	}
	if len(got.Excluded) != 1 || len(got.Unjudged) != 0 {
		t.Errorf("excluded = %+v unjudged = %+v, want the darwin-only return excluded and nothing unjudged", got.Excluded, got.Unjudged)
	}
}
