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
	cmd.Env = seedMakeEnv()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// seedMakeEnv is the process environment without the make flags: an outer
// make's flags must not decide whether the seed recipe's commands run.
func seedMakeEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
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
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("git -C %s %s: %v\n%s", dir, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
