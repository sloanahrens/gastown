//go:build integration

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/bumpversion"
)

// gitIn runs git in dir under the test's process environment, which
// isolateGitEnv has already replaced, and returns its trimmed standard output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// isolateGitEnv gives this test process the environment the real runGit and
// runProcess inherit: no GIT_* variable of the caller (GIT_DIR, say), no global or
// system git configuration, a home under the test directory, and an explicit
// identity. The runners read the process environment, so this has to be the process
// environment, not a copy; internal/bumpversion's integration tests hold the same
// rule with a private child environment (a cmd test cannot import their helper).
func isolateGitEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_COMMON_DIR"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "bump-version test")
	t.Setenv("GIT_AUTHOR_EMAIL", "bump-version@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "bump-version test")
	t.Setenv("GIT_COMMITTER_EMAIL", "bump-version@example.invalid")
}

// TestIntegrationProductionWiringRunsTheCommand drives the command through run with
// systemEnv(root): the production file system, the production runGit and runProcess,
// the production root and home resolution. Only make and the installed gt are faked
// at the process seam, and nix is made absent. The run starts from a directory that
// is not the repository, so every file is found through the root. The three version
// files are edited, the commit and tag are made by the real git, and no push is run.
func TestIntegrationProductionWiringRunsTheCommand(t *testing.T) {
	isolateGitEnv(t)
	root := t.TempDir()
	t.Chdir(t.TempDir())

	gitIn(t, root, "init", "-q")
	files := map[string]string{
		bumpversion.VersionGoPath:   "package cmd\n\nvar (\n\tVersion = \"1.2.1\"\n)\n",
		bumpversion.PackageJSONPath: "{\n  \"version\": \"1.2.1\"\n}\n",
		bumpversion.ChangelogPath:   "# Changelog\n\n## [Unreleased]\n\n- Initial work.\n",
	}
	for name, text := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", "fixture")

	env := systemEnv(root)
	var makes int
	realRun := env.Exec.Run
	env.Exec.Run = func(dir string, extra []string, name string, args ...string) ([]byte, []byte, error) {
		switch name {
		case "make":
			makes++
			return nil, nil, nil
		case "gt":
			return []byte("gt version 1.3.0\n"), nil, nil
		}
		return realRun(dir, extra, name, args...)
	}
	realLook := env.Exec.LookPath
	env.Exec.LookPath = func(file string) (string, error) {
		if file == "nix" {
			return "", exec.ErrNotFound
		}
		return realLook(file)
	}
	realGitRun := env.Git.Run
	env.Git.Run = func(dir string, args ...string) (int, error) {
		if len(args) > 0 && args[0] == "push" {
			t.Errorf("the run ran git %s; the command never pushes", strings.Join(args, " "))
		}
		return realGitRun(dir, args...)
	}
	var out, errw bytes.Buffer
	env.Out = &out

	code := run([]string{"1.3.0", "--commit", "--tag", "--install"}, env, &errw)
	if code != 0 {
		t.Fatalf("run = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out.String(), errw.String())
	}
	if makes != 1 {
		t.Errorf("make install ran %d times, want 1", makes)
	}
	if !strings.HasSuffix(out.String(), "Summary:\nCommitted and tagged v1.3.0.\n") {
		t.Errorf("output does not end with the summary:\n%s", out.String())
	}
	for name, want := range map[string]string{
		bumpversion.VersionGoPath:   `Version = "1.3.0"`,
		bumpversion.PackageJSONPath: `"version": "1.3.0"`,
		bumpversion.ChangelogPath:   "## [1.3.0] - ",
	} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), want) {
			t.Errorf("%s after the run lacks %q:\n%s", name, want, data)
		}
	}
	if got := gitIn(t, root, "cat-file", "-t", "v1.3.0"); got != "tag" {
		t.Errorf("tag object type = %q, want an annotated tag object", got)
	}
	if got := gitIn(t, root, "status", "--porcelain"); got != "" {
		t.Errorf("working tree after the commit = %q, want clean", got)
	}
	if got := gitIn(t, root, "remote"); got != "" {
		t.Errorf("git remote = %q, want no remote to push to", got)
	}
}
