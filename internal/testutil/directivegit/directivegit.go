// Package directivegit builds real bare repos for the central-directive tests:
// a bare repo whose commits carry a directives/ tree. The config and cmd test
// packages share it. It imports nothing from the gastown module, so config's
// own tests can use it without an import cycle (testutil imports config).
package directivegit

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Entry is one file in a commit's directives/ tree. Symlink makes it a symlink
// (mode 120000) whose target text is Content.
type Entry struct {
	Content string
	Symlink bool
}

// NewBare creates an empty bare repo at dir and returns dir.
func NewBare(t testing.TB, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatalf("create parent of %s: %v", dir, err)
	}
	git(t, "", nil, nil, "init", "--bare", "-q", dir)
	return dir
}

// Commit adds one commit to the bare repo at repo whose tree holds files under
// directives/ and a root README. parent is the commit to build on, or "" for a
// root commit. It returns the new commit's id.
func Commit(t testing.TB, repo, parent string, files map[string]Entry) string {
	t.Helper()
	index := filepath.Join(t.TempDir(), "index")
	env := []string{
		"GIT_INDEX_FILE=" + index,
		"GIT_AUTHOR_NAME=directivegit", "GIT_AUTHOR_EMAIL=directivegit@example.invalid",
		"GIT_COMMITTER_NAME=directivegit", "GIT_COMMITTER_EMAIL=directivegit@example.invalid",
	}
	readme := hashObject(t, repo, "readme\n")
	git(t, repo, env, nil, "update-index", "--add", "--cacheinfo", "100644,"+readme+",README")
	for name, e := range files {
		mode := "100644"
		if e.Symlink {
			mode = "120000"
		}
		sha := hashObject(t, repo, e.Content)
		git(t, repo, env, nil, "update-index", "--add", "--cacheinfo", mode+","+sha+",directives/"+name)
	}
	tree := git(t, repo, env, nil, "write-tree")
	args := []string{"commit-tree", tree, "-m", "directives"}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	return git(t, repo, env, nil, args...)
}

// hashObject writes content into repo's object store and returns its id.
func hashObject(t testing.TB, repo, content string) string {
	t.Helper()
	return git(t, repo, nil, strings.NewReader(content), "hash-object", "-w", "--stdin")
}

// git runs one git command and returns its trimmed stdout. repo is the bare
// repo it runs against, or "" for the test's working directory; stdin, when
// non-nil, is fed to the command.
func git(t testing.TB, repo string, env []string, stdin *strings.Reader, args ...string) string {
	t.Helper()
	if repo != "" {
		args = append([]string{"--git-dir=" + repo}, args...)
	}
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), env...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(stdout.String())
}
