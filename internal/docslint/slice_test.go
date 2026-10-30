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

func (f fakeGit) LastCommit(p string) (int64, error) { return f[p], nil }

// failingGit answers like fakeGit, except that it fails the one path it names.
type failingGit struct {
	fakeGit
	fail string
	err  error
}

func (g failingGit) LastCommit(p string) (int64, error) {
	if p == g.fail {
		return 0, g.err
	}
	return g.fakeGit.LastCommit(p)
}

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

// TestSliceOldestFirstTiesByPath drives the tie-break with an unsorted input: the
// tiers arrive bytewise sorted, so through Slice a tie between two of their
// files never shows the comparison it is ordered by.
func TestSliceOldestFirstTiesByPath(t *testing.T) {
	t.Parallel()
	paths := []string{"docs/b.md", "docs/c.md", "docs/a.md", "AGENTS.md"}
	got, err := oldestFirst(paths, fakeGit{"AGENTS.md": 50, "docs/a.md": 100, "docs/b.md": 100, "docs/c.md": 100})
	if err != nil {
		t.Fatalf("oldestFirst(): %v", err)
	}
	want := []string{"AGENTS.md", "docs/a.md", "docs/b.md", "docs/c.md"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("oldestFirst() = %q, want %q", got, want)
	}
}

// TestSliceReportsGitErrors pins the failing branch: a lookup git cannot answer
// must stop the slice, never order its file as one no commit touched (0).
func TestSliceReportsGitErrors(t *testing.T) {
	t.Parallel()
	files := memFS{
		"AGENTS.md":         "# agents\n",
		"docs/HOOKS.md":     "# hooks\n",
		"internal/pkg/a.go": "package pkg\n",
	}
	errGit := errors.New("exit status 128")
	tests := []struct {
		name string
		fail string
	}{
		{"a lookup on the agent-facing tier", "AGENTS.md"},
		{"a lookup on the go tier", "internal/pkg/a.go"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			git := failingGit{fakeGit: fakeGit{"AGENTS.md": 100}, fail: tc.fail, err: errGit}
			got, err := Slice(files, git, 5, 5)
			if !errors.Is(err, errGit) {
				t.Fatalf("Slice() error = %v, want the lookup's %v", err, errGit)
			}
			if got != nil {
				t.Errorf("Slice() = %q, want nil alongside the error", got)
			}
		})
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
		name    string
		out     []byte
		err     error
		want    int64
		wantErr bool
	}{
		{"a committer time", []byte("1767225600\n"), nil, 1767225600, false},
		{"git knows no commit for the path", []byte(""), nil, 0, false},
		{"git fails", nil, errors.New("exit status 128"), 0, true},
		{"git answers with something that is not a time", []byte("fatal: bad revision\n"), nil, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotDir string
			var gotArgs []string
			git := GitLog{Root: "/repo", run: func(dir string, args ...string) ([]byte, error) {
				gotDir, gotArgs = dir, args
				return tc.out, tc.err
			}}
			got, err := git.LastCommit("docs/x.md")
			wantArgs := []string{"log", "-1", "--format=%ct", "--", "docs/x.md"}
			if gotDir != "/repo" || !reflect.DeepEqual(gotArgs, wantArgs) {
				t.Errorf("ran git %q in %q, want %q in /repo", gotArgs, gotDir, wantArgs)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("LastCommit() error = %v, want an error: %t", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("LastCommit() = %d, want %d", got, tc.want)
			}
		})
	}
}
