//go:build integration

package docslint

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// TestIntegrationSliceOrder checks the slice end to end: OSFiles walks a real
// checkout and GitLog asks the real git. The fake cannot show that %ct comes
// back as the commit's Unix time, or that files no commit touched order first.
func TestIntegrationSliceOrder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// An empty global and system config, so a developer's gitconfig cannot
	// change what these commits record.
	gitFixture(t, root,
		[]string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null"},
		"init", "-q")

	writeFileAt(t, root, "AGENTS.md", "# agents\n")
	writeFileAt(t, root, "internal/pkg/a.go", "package pkg\n")
	commitAt(t, root, "2026-01-01T00:00:00Z", "AGENTS.md", "internal/pkg/a.go")

	writeFileAt(t, root, "README.md", "# readme\n")
	writeFileAt(t, root, "plugins/p/plugin.md", "# plugin\n")
	commitAt(t, root, "2026-02-01T00:00:00Z", "README.md", "plugins/p/plugin.md")

	writeFileAt(t, root, "docs/guides/guide.md", "# guide\n")
	writeFileAt(t, root, "cmd/x/main.go", "package main\n")
	commitAt(t, root, "2026-03-01T00:00:00Z", "docs/guides/guide.md", "cmd/x/main.go")

	// Written last and never committed, so git knows no time for them.
	writeFileAt(t, root, "docs/HOOKS.md", "# hooks\n")
	writeFileAt(t, root, "internal/pkg/b.go", "package pkg\n")

	fsys := OSFiles{Root: root}
	git := GitLog{Root: root}

	if got, want := git.LastCommit("AGENTS.md"), int64(1767225600); got != want {
		t.Errorf("LastCommit(AGENTS.md) = %d, want %d", got, want)
	}
	if got := git.LastCommit("docs/HOOKS.md"); got != 0 {
		t.Errorf("LastCommit(docs/HOOKS.md) = %d, want 0 for a file no commit touched", got)
	}

	tests := []struct {
		name string
		n, m int
		want []string
	}{
		{
			name: "the whole slice, docs then go",
			n:    9, m: 9,
			want: []string{
				"docs/HOOKS.md",
				"AGENTS.md",
				"README.md",
				"plugins/p/plugin.md", // tied with README.md, so the path decides
				"docs/guides/guide.md",
				"internal/pkg/b.go",
				"internal/pkg/a.go",
				"cmd/x/main.go",
			},
		},
		{
			name: "the limits cut each tier in its own order",
			n:    3, m: 1,
			want: []string{
				"docs/HOOKS.md",
				"AGENTS.md",
				"README.md",
				"internal/pkg/b.go",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Slice(fsys, git, tc.n, tc.m)
			if err != nil {
				t.Fatalf("Slice(%d, %d): %v", tc.n, tc.m, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Slice(%d, %d) = %q, want %q", tc.n, tc.m, got, tc.want)
			}
		})
	}
}

// writeFileAt writes rel under root, creating its directory.
func writeFileAt(t *testing.T, root, rel, contents string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("creating %s: %v", rel, err)
	}
	if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
		t.Fatalf("writing %s: %v", rel, err)
	}
}

// commitAt commits paths with the committer date fixed at date, so the slice
// reads a known time.
func commitAt(t *testing.T, root, date string, paths ...string) {
	t.Helper()
	env := []string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=docslint", "GIT_AUTHOR_EMAIL=docslint@example.com",
		"GIT_COMMITTER_NAME=docslint", "GIT_COMMITTER_EMAIL=docslint@example.com",
		"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date,
	}
	gitFixture(t, root, env, append([]string{"add", "--"}, paths...)...)
	gitFixture(t, root, env, "commit", "-q", "-m", "fixture")
}

// gitFixture runs git in root under env, and fails the test with git's output
// when it does.
func gitFixture(t *testing.T, root string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
