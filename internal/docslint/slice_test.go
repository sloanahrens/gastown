package docslint

import (
	"errors"
	"io/fs"
	"reflect"
	"testing"
)

// fakeGit answers LastCommit from a table, so an order is known without git.
// A path it does not hold is one git knows no commit for.
type fakeGit map[string]int64

func (f fakeGit) LastCommit(p string) int64 { return f[p] }

func TestSliceOrder(t *testing.T) {
	t.Parallel()
	files := memFS{
		"AGENTS.md":              "# agents\n",
		"docs/HOOKS.md":          "# hooks\n",
		"README.md":              "# readme\n",
		"docs/guides/guide.md":   "# guide\n",
		"plugins/p/plugin.md":    "# plugin\n",
		"internal/pkg/a.go":      "package pkg\n",
		"internal/pkg/b.go":      "package pkg\n",
		"cmd/x/main.go":          "package main\n",
		"internal/pkg/a_test.go": "package pkg\n",
	}
	git := fakeGit{
		"AGENTS.md":            100,
		"README.md":            200,
		"plugins/p/plugin.md":  200, // tied with README.md, so the path decides
		"docs/guides/guide.md": 400,
		"internal/pkg/a.go":    100,
		"internal/pkg/b.go":    200,
		// docs/HOOKS.md and cmd/x/main.go are absent: uncommitted, so 0.
	}
	// The docs tier, oldest first: the uncommitted file, then each commit
	// time, with the 200 tie broken by path.
	docs := []string{
		"docs/HOOKS.md",
		"AGENTS.md",
		"README.md",
		"plugins/p/plugin.md",
		"docs/guides/guide.md",
	}
	goFiles := []string{"cmd/x/main.go", "internal/pkg/a.go", "internal/pkg/b.go"}
	all := append(append([]string{}, docs...), goFiles...)

	tests := []struct {
		name string
		n, m int
		want []string
	}{
		{"docs first, then go, oldest first", 5, 3, all},
		{"the doc count limits the docs", 2, 3, []string{"docs/HOOKS.md", "AGENTS.md", "cmd/x/main.go", "internal/pkg/a.go", "internal/pkg/b.go"}},
		{"the go count limits the go files", 5, 1, []string{"docs/HOOKS.md", "AGENTS.md", "README.md", "plugins/p/plugin.md", "docs/guides/guide.md", "cmd/x/main.go"}},
		{"a zero count takes none of its tier", 0, 2, []string{"cmd/x/main.go", "internal/pkg/a.go"}},
		{"both zero is an empty slice", 0, 0, nil},
		{"a negative count takes none", -1, -1, nil},
		{"a count above the tier takes the whole tier", 99, 99, all},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Slice(files, git, tc.n, tc.m)
			if err != nil {
				t.Fatalf("Slice(%d, %d): %v", tc.n, tc.m, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Slice(%d, %d) = %q, want %q", tc.n, tc.m, got, tc.want)
			}
		})
	}
}

// TestOldestFirstTiesByPath drives the tie-break with an unsorted input: the
// tiers arrive bytewise sorted, so through Slice a tie between two of their
// files never shows the comparison it is ordered by.
func TestSliceOldestFirstTiesByPath(t *testing.T) {
	t.Parallel()
	paths := []string{"docs/b.md", "docs/c.md", "docs/a.md", "AGENTS.md"}
	got := oldestFirst(paths, fakeGit{"AGENTS.md": 50, "docs/a.md": 100, "docs/b.md": 100, "docs/c.md": 100})
	want := []string{"AGENTS.md", "docs/a.md", "docs/b.md", "docs/c.md"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("oldestFirst() = %q, want %q", got, want)
	}
}

// TestSliceReportsTierErrors pins the failing branch: a tier the slice could
// not read must stop it, never yield a shorter list the audit would take for
// the whole plan.
func TestSliceReportsTierErrors(t *testing.T) {
	t.Parallel()
	files := walkErrorFS{
		memFS: memFS{
			"AGENTS.md":            "# agents\n",
			"docs/guides/guide.md": "# guide\n",
			"internal/pkg/a.go":    "package pkg\n",
		},
		failDir: "docs",
	}
	tests := []struct {
		name string
		fail string
	}{
		{"the agent-facing tier", "plugins"},
		{"the reference tier", "docs"},
		{"the go tier", "internal"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			files.failDir = tc.fail
			got, err := Slice(files, fakeGit{}, 5, 5)
			if !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("Slice() error = %v, want the walker's %v", err, fs.ErrPermission)
			}
			if got != nil {
				t.Errorf("Slice() = %q, want nil alongside the error", got)
			}
		})
	}
}

func TestSliceGitLogLastCommit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		out  []byte
		err  error
		want int64
	}{
		{"a committer time", []byte("1767225600\n"), nil, 1767225600},
		{"git knows no commit for the path", []byte(""), nil, 0},
		{"git fails", nil, errors.New("exit status 128"), 0},
		{"git answers with something that is not a time", []byte("fatal: bad revision\n"), nil, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotDir string
			var gotArgs []string
			git := GitLog{Root: "/repo", run: func(dir string, args ...string) ([]byte, error) {
				gotDir, gotArgs = dir, args
				return tc.out, tc.err
			}}
			got := git.LastCommit("docs/x.md")
			wantArgs := []string{"log", "-1", "--format=%ct", "--", "docs/x.md"}
			if gotDir != "/repo" || !reflect.DeepEqual(gotArgs, wantArgs) {
				t.Errorf("ran git %q in %q, want %q in /repo", gotArgs, gotDir, wantArgs)
			}
			if got != tc.want {
				t.Errorf("LastCommit() = %d, want %d", got, tc.want)
			}
		})
	}
}
