//go:build integration

package docslint

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitAt runs git in dir with a clean environment, so the machine's git config and
// the environment a hook runs under cannot change the result, and with the
// commit date given.
func gitAt(t *testing.T, dir, date string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_DATE=" + date,
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// TestIntegrationSliceOrdersByCommitDate builds a throwaway repository with
// commits at known dates and checks --slice reads the real git log: least
// recently committed first, docs then go files, ties by path.
func TestIntegrationSliceOrdersByCommitDate(t *testing.T) {
	root := t.TempDir()
	for name, content := range fixtureTree() {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitAt(t, root, "2026-01-01T00:00:00Z", "init", "-q")
	commit := func(date string, paths ...string) {
		gitAt(t, root, date, append([]string{"add", "--"}, paths...)...)
		gitAt(t, root, date, "commit", "-q", "--allow-empty", "-m", strings.Join(paths, " "))
	}
	commit("2026-01-01T00:00:00Z", "docs/design/live.md", "internal/pkg/b.go")
	commit("2026-02-01T00:00:00Z", "docs/HOOKS.md", "internal/pkg/a.go")
	commit("2026-03-01T00:00:00Z", ".")

	var out, errb bytes.Buffer
	code := Run([]string{"--slice", "2", "1"}, Env{
		Getenv:   func(k string) string { return map[string]string{"DOCS_LINT_ROOT": root}[k] },
		TopLevel: func() (string, error) { return root, nil },
		NewFS:    func(r string) FS { return OSFiles{Root: r} },
		NewGit:   func(r string) Git { return fixtureGitLog(r) },
	}, &out, &errb)
	if want := "docs/design/live.md\ndocs/HOOKS.md\ninternal/pkg/b.go\n"; out.String() != want || code != ExitClean {
		t.Errorf("--slice 2 1 = %q exit %d stderr %q, want %q", out.String(), code, errb.String(), want)
	}
}

// useFixtureEnv makes the process environment the fixture environment for the
// rest of t: every GIT_ variable removed, and git's global and system config
// off. The production GitLog runner inherits the process environment, so this
// keeps its lookups on the fixture repository whatever the caller exported.
func useFixtureEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "GIT_") {
			t.Setenv(key, "")
			os.Unsetenv(key)
		}
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
}

// TestIntegrationProductionGitLogAsksGit runs the production GitLog, the exec
// path the docs-lint command takes, against a fixture repository. The other
// integration tests inject their own runner, so without this nothing runs the
// default runner's exec.
func TestIntegrationProductionGitLogAsksGit(t *testing.T) {
	root := t.TempDir()
	gitAt(t, root, "2026-01-01T00:00:00Z", "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitAt(t, root, "2026-01-01T00:00:00Z", "add", "--", "a.go")
	gitAt(t, root, "2026-01-01T00:00:00Z", "commit", "-q", "-m", "a.go")
	useFixtureEnv(t)

	git := GitLog{Root: root}
	committed, err := git.LastCommit("a.go")
	if err != nil {
		t.Fatalf("LastCommit(a.go): %v", err)
	}
	if want := int64(1767225600); committed != want {
		t.Errorf("LastCommit(a.go) = %d, want %d", committed, want)
	}
	untouched, err := git.LastCommit("never.go")
	if err != nil {
		t.Fatalf("LastCommit(never.go): %v", err)
	}
	if untouched != 0 {
		t.Errorf("LastCommit(never.go) = %d, want 0 for a file no commit touched", untouched)
	}
}

// TestIntegrationProductionGitLogFailsOutsideRepo checks the production runner
// reports an error, not a 0, when git cannot answer: a directory that is not a
// repository.
func TestIntegrationProductionGitLogFailsOutsideRepo(t *testing.T) {
	useFixtureEnv(t)
	if _, err := (GitLog{Root: t.TempDir()}).LastCommit("a.go"); err == nil {
		t.Error("LastCommit outside a repository = nil error, want an error")
	}
}
