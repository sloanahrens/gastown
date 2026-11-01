//go:build integration

package makegate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Sandbox is a throwaway town root for the seed cases, ported from the shell
// fixture's helpers: temp git checkouts laid out as <town>/<rig>/mayor/rig, a
// pins file and the seed output directory. Every path is under the test's own
// temp directory, so no case reads the host's town tree, a network remote or a
// writable path outside the test.
type Sandbox struct {
	t    *testing.T
	root string
	repo string
}

// seedSandboxRig is POLE_SEED_RIGS for a Sandbox: one pinned rig beside the
// build rig, as the shell fixture seeds.
const seedSandboxRig = "fx"

// newSandbox returns a Sandbox under a fresh temp directory. The checkout under
// test is read once, so every real make call lands in the tree the tests were
// compiled in rather than in the working directory.
func newSandbox(t *testing.T) *Sandbox {
	t.Helper()
	return &Sandbox{t: t, root: t.TempDir(), repo: repoRoot(t)}
}

// Path returns the sandbox path of elem.
func (s *Sandbox) Path(elem ...string) string {
	return filepath.Join(append([]string{s.root}, elem...)...)
}

// Checkout makes an empty git checkout at <town>/<rig>/mayor/rig and returns
// that directory.
func (s *Sandbox) Checkout(rig string) string {
	dir := s.Path(rig, "mayor", "rig")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.t.Fatalf("make checkout dir %s: %v", dir, err)
	}
	s.git(dir, "init", "-q", "-b", "main")
	return dir
}

// Commit writes files (its keys are paths under dir) and commits them on the
// checkout's current branch, returning the new commit's full sha.
func (s *Sandbox) Commit(dir, message string, files map[string]string) string {
	for name, body := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			s.t.Fatalf("make dir for %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			s.t.Fatalf("write %s: %v", full, err)
		}
	}
	s.git(dir, "add", ".")
	s.git(dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "-m", message)
	return s.git(dir, "rev-parse", "HEAD")
}

// Rig makes a checkout with two commits and returns "<first> <second>" as the
// shell mkrig does. A canonical checkout keeps origin/main at its pin, so
// origin/main is set to the first commit: the pin-drift guard reads it.
func (s *Sandbox) Rig(name string) (first, second string) {
	dir := s.Checkout(name)
	first = s.Commit(dir, "first", map[string]string{"first.txt": "one\n"})
	s.git(dir, "update-ref", "refs/remotes/origin/main", first)
	second = s.Commit(dir, "second", map[string]string{"second.txt": "two\n"})
	return first, second
}

// WriteFile writes body at the sandbox path rel, creating its parent.
func (s *Sandbox) WriteFile(rel, body string) {
	full := s.Path(rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		s.t.Fatalf("make dir for %s: %v", full, err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		s.t.Fatalf("write %s: %v", full, err)
	}
}

// SeedMake runs a seed target for real against the checkout under test, as the
// shell seedmk does. The seed variables are make command-line arguments because
// they must override the Makefile's own definitions: POLE_COMMIT is a `:=` made
// from `git rev-parse HEAD`, which an environment assignment does not replace.
// build is POLE_COMMIT. The recipe runs the stage command, so the caller reads
// the combined output and the exit status.
func (s *Sandbox) SeedMake(build string, args ...string) (string, error) {
	vars := []string{
		"POLE_TOWN=" + s.root,
		"POLE_SEED_PINS=" + s.Path("pins"),
		"POLE_SEED_DIR=" + s.Path("out"),
		"POLE_SEED_RIGS=" + seedSandboxRig,
		"POLE_SEED_BUILD_RIG=gastown",
		"POLE_COMMIT=" + build,
	}
	argv := append([]string{"-C", s.repo, "--no-print-directory"}, vars...)
	argv = append(argv, args...)
	ctx, cancel := context.WithTimeout(context.Background(), makeTestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "make", argv...)
	cmd.Env = sandboxEnv()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// sandboxEnv is the environment a sandbox command runs in: the caller's, with
// every GIT_* variable and the make flags dropped. An inherited GIT_DIR,
// GIT_WORK_TREE or GIT_INDEX_FILE would point the fixture's git calls at the
// caller's repository, and the caller's global config or GIT_CONFIG_PARAMETERS
// would decide what the fixture commits; an inherited make flag would decide
// whether the seed recipe's commands run. The config pins shut the global and
// system files the dropped variables would have named (gt-n8354).
func sandboxEnv() []string {
	env := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
	}
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "GIT_") {
			continue
		}
		switch key {
		case "MAKEFLAGS", "MFLAGS", "GNUMAKEFLAGS":
			continue
		}
		env = append(env, kv)
	}
	return env
}

// git runs git in dir and returns its trimmed output. A git failure fails the
// test: the fixture must be built exactly as the case describes.
func (s *Sandbox) git(dir string, args ...string) string {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), makeTestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = sandboxEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("git -C %s %s: %v\n%s", dir, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixedGit runs git in dir with an environment the case fixes itself, so the
// case reads the checkout the fixture built without the fixture's environment
// deciding what it sees. A git failure comes back as an error, not a failed
// test: a case about a refusal reads it.
func fixedGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), makeTestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// TestIntegrationSandboxGitIgnoresACallerGitDir checks that the fixture's git
// calls act on the sandbox checkout while the caller's environment names
// another repository. Mutation: run the fixture's git with the inherited
// environment, and the commits land in the caller's GIT_DIR, leaving the
// sandbox checkout without a repository of its own (gt-n8354).
func TestIntegrationSandboxGitIgnoresACallerGitDir(t *testing.T) {
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "elsewhere"))
	s := newSandbox(t)
	s.Rig("gastown")

	gitDir, err := fixedGit(s.Path("gastown", "mayor", "rig"), "rev-parse", "--git-dir")
	if err != nil || gitDir != ".git" {
		t.Fatalf("the sandbox checkout's git dir is %q (%v), want .git: the fixture's git calls followed the caller's GIT_DIR", gitDir, err)
	}
}

// TestIntegrationSandboxIgnoresACallerGlobalGitConfig checks that the caller's
// global git config does not reach the fixture's commits. Mutation: inherit the
// environment, and the global core.hooksPath runs a failing pre-commit hook,
// so the fixture cannot make its second commit (gt-n8354).
func TestIntegrationSandboxIgnoresACallerGlobalGitConfig(t *testing.T) {
	hooks := t.TempDir()
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write the failing hook: %v", err)
	}
	config := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(config, []byte("[core]\n\thooksPath = "+hooks+"\n"), 0o644); err != nil {
		t.Fatalf("write the global config: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)

	s := newSandbox(t)
	s.Rig("gastown")

	head, err := fixedGit(s.Path("gastown", "mayor", "rig"), "rev-parse", "HEAD")
	if err != nil || len(head) != 40 {
		t.Fatalf("the sandbox checkout has no commit (HEAD %q, %v): the fixture ran the caller's global pre-commit hook", head, err)
	}
}

// TestIntegrationSeedMakeIgnoresACallerGitDir checks that the seed recipe's git
// calls act on the sandbox checkouts while the caller's environment names
// another repository. Mutation: pass the inherited environment to make, and
// the stage's git calls fail against the caller's GIT_DIR before anything is
// staged (gt-n8354).
func TestIntegrationSeedMakeIgnoresACallerGitDir(t *testing.T) {
	s := newSandbox(t)
	build, _ := s.Rig("gastown")
	pinned, _ := s.Rig("fx")
	s.WriteFile("pins", "fx "+pinned+"\n")
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "elsewhere"))

	out, err := s.SeedMake(build, "polecat-seed-stage")
	if err != nil {
		t.Fatalf("polecat-seed-stage failed under the caller's GIT_DIR: %v\n%s", err, out)
	}
	mustExist(t, s.Path("out", "gastown", "first.txt"))
}
