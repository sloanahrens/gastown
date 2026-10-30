//go:build integration

package bumpversion

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// isolatedGitEnv is the environment every git child in this test runs with. It is
// built from scratch, so no GIT_* variable the caller has set (GIT_DIR, say) reaches
// git, and no global or system config does: git acts on the throwaway repository and
// nothing else. The identity is set explicitly, so the commit does not depend on the
// host.
func isolatedGitEnv(t *testing.T) []string {
	home := t.TempDir()
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=bump-version test",
		"GIT_AUTHOR_EMAIL=bump-version@example.invalid",
		"GIT_COMMITTER_NAME=bump-version test",
		"GIT_COMMITTER_EMAIL=bump-version@example.invalid",
	}
}

// realGit is the git seam backed by the git binary, run in dir with env. Its output
// is discarded, as the steps under test never read it.
func realGit(env []string) Git {
	return Git{Run: func(dir string, args ...string) (int, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		err := cmd.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), nil
		}
		return 0, err
	}}
}

// gitOut runs git in dir with env and returns its trimmed standard output, failing
// the test when git exits non-zero.
func gitOut(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// diskFS is the FS seam over a directory on disk, so the steps edit the fixture
// tree the test wrote into its own temporary repository.
type diskFS struct{ root string }

func (d diskFS) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(d.root, name))
}

func (d diskFS) WriteFile(name string, data []byte) error {
	return os.WriteFile(filepath.Join(d.root, name), data, 0o644)
}

// TestIntegrationCommitAndTagInAThrowawayRepo: the dirty-tree rule passes on a clean
// fixture repository, the version files are edited and committed with the release
// message, and the tag is annotated. The repository has no remote, so nothing could
// have been pushed.
func TestIntegrationCommitAndTagInAThrowawayRepo(t *testing.T) {
	// An inherited GIT_DIR points at a repository that is not the throwaway one. The
	// steps must not reach it: the commit and tag land in root, and elsewhere stays
	// empty.
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.git")
	initCmd := exec.Command("git", "init", "--bare", "-q", elsewhere)
	initCmd.Env = isolatedGitEnv(t)
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	t.Setenv("GIT_DIR", elsewhere)

	root := t.TempDir()
	env := isolatedGitEnv(t)
	git := realGit(env)
	fsys := diskFS{root: root}

	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.name", "bump-version test"},
		{"config", "user.email", "bump-version@example.invalid"},
		{"config", "commit.gpgsign", "false"},
		{"config", "tag.gpgsign", "false"},
	} {
		if _, err := git.Run(root, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}

	// The fixture tree is the script's three version files at 1.2.1, committed so
	// the tree is clean before the bump.
	fixtures := map[string]string{
		VersionGoPath:   "version.go.before",
		PackageJSONPath: "package.json.before",
		ChangelogPath:   "CHANGELOG.md.before",
	}
	for name, src := range fixtures {
		data, err := os.ReadFile(filepath.Join("testdata", src))
		if err != nil {
			t.Fatalf("read fixture %s: %v", src, err)
		}
		dst := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "fixture"}} {
		if _, err := git.Run(root, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	var checkOut bytes.Buffer
	if err := CheckTree(git, root, Options{Commit: true}, &checkOut); err != nil {
		t.Fatalf("CheckTree on a clean fixture tree: %v", err)
	}
	if checkOut.Len() != 0 {
		t.Errorf("CheckTree on a clean tree wrote %q, want nothing", checkOut.String())
	}

	const oldVersion, newVersion = "1.2.1", "1.3.0"
	if err := EditFile(fsys, VersionGoPath, func(s string) string { return SetVersionGo(s, oldVersion, newVersion) }); err != nil {
		t.Fatal(err)
	}
	if err := EditFile(fsys, PackageJSONPath, func(s string) string { return SetPackageJSON(s, oldVersion, newVersion) }); err != nil {
		t.Fatal(err)
	}
	if warning, err := UpdateChangelogFile(fsys, newVersion, "2026-10-30"); err != nil || warning != "" {
		t.Fatalf("UpdateChangelogFile: warning %q, err %v", warning, err)
	}

	if err := Commit(git, fsys, root, Release{Old: oldVersion, New: newVersion}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := Tag(git, root, newVersion); err != nil {
		t.Fatalf("Tag: %v", err)
	}

	wantMessage := "chore: Bump version to 1.3.0\n\n" +
		"Updated all component versions:\n" +
		"- gt CLI: 1.2.1 → 1.3.0\n" +
		"- npm package: 1.2.1 → 1.3.0\n\n" +
		"Generated by bump-version"
	if got := gitOut(t, env, root, "log", "-1", "--format=%B"); got != wantMessage {
		t.Errorf("commit message = %q, want %q", got, wantMessage)
	}
	wantFiles := "CHANGELOG.md\ninternal/cmd/version.go\nnpm-package/package.json"
	if got := gitOut(t, env, root, "show", "--name-only", "--format=", "HEAD"); got != wantFiles {
		t.Errorf("files in the commit = %q, want %q", got, wantFiles)
	}
	if got := gitOut(t, env, root, "tag", "-l", "v"+newVersion); got != "v"+newVersion {
		t.Errorf("git tag -l = %q, want v%s", got, newVersion)
	}
	if got := gitOut(t, env, root, "cat-file", "-t", "v"+newVersion); got != "tag" {
		t.Errorf("tag object type = %q, want an annotated tag object", got)
	}
	if got := gitOut(t, env, root, "remote"); got != "" {
		t.Errorf("git remote = %q, want no remote to push to", got)
	}
	if got := gitOut(t, env, root, "status", "--porcelain"); got != "" {
		t.Errorf("working tree after the commit = %q, want clean", got)
	}
	if got := gitOut(t, env, elsewhere, "rev-list", "--all", "--count"); got != "0" {
		t.Errorf("inherited GIT_DIR repository has %s commits, want 0: the steps reached outside the throwaway repository", got)
	}
}
