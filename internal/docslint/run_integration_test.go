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
		NewGit:   func(r string) Git { return GitLog{Root: r} },
	}, &out, &errb)
	if want := "docs/design/live.md\ndocs/HOOKS.md\ninternal/pkg/b.go\n"; out.String() != want || code != ExitClean {
		t.Errorf("--slice 2 1 = %q exit %d stderr %q, want %q", out.String(), code, errb.String(), want)
	}
}
