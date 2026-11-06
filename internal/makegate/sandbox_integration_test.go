//go:build integration

package makegate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		"POLE_BUILD_SRC=" + s.Path("gastown", "mayor", "rig"),
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

// gateRunTimeout bounds one gate run against the stub tools. Every stage of
// the recipe is stubbed, so a run that takes this long is hung rather than
// slow.
const gateRunTimeout = 2 * time.Minute

// The gate failure-path stubs. Each logs its call to $STUB_LOG so a case sees
// which stages ran, and refuses only when the case's variable says so, as the
// shell fixture's stubs do. STUB_GO_FAIL_DIR limits the go failure to a run
// inside that directory name, or (with a leading !) to a run anywhere else, so
// a case can fail the tracker module alone or the root suite alone; the
// `in tracker` line records the module a run happened in.
const (
	// gateStubHead is the part of the go stub every variant shares: the call log,
	// the GOFLAGS line and the module marker. The module is the stub's working
	// directory relative to the checkout (STUB_REPO), not a directory name, so a
	// checkout that happens to be called tracker is not a tracker module.
	gateStubHead = `dir=$PWD
if [ "$1" = -C ]; then dir=$2; shift 2; fi
rel=$(cd "$dir" && pwd -P); rel=${rel#"$STUB_REPO"}; rel=${rel#/}
echo "go $1 GT_TEST_DOCKER=${GT_TEST_DOCKER:-unset}" >>"$STUB_LOG"
[ -n "${GOFLAGS:-}" ] && echo "go $1 GOFLAGS=$GOFLAGS" >>"$STUB_LOG"
[ "$rel" = tracker ] && echo "go $1 in tracker" >>"$STUB_LOG"
`
	// gateStubTail is the failure switch: STUB_GO_FAIL_DIR limits the go failure
	// to a run inside that module, or (with a leading !) to a run anywhere else.
	gateStubTail = `dir_ok=1
if [ -n "${STUB_GO_FAIL_DIR:-}" ]; then
	if [ "${STUB_GO_FAIL_DIR#!}" != "$STUB_GO_FAIL_DIR" ]; then
		[ "$rel" != "${STUB_GO_FAIL_DIR#!}" ] || dir_ok=0
	else
		[ "$rel" = "$STUB_GO_FAIL_DIR" ] || dir_ok=0
	fi
fi
[ "$1" = "${STUB_GO_FAIL:-}" ] && [ "$dir_ok" = 1 ] && exit 1
exit 0
`
	// gateStubGoPlusLintpin forwards the recipe's `go run ./cmd/lintpin` to a
	// binary a case built from the checkout under test, so the recipe's linter
	// check runs for real against the case's stub linter. A case that sets no
	// STUB_LINTPIN gets a no-op, which keeps the other gate cases about their own
	// stage; the check's branches are unit-tested in internal/lintpin.
	gateStubGoPlusLintpin = `if [ "$1" = run ] && [ "$2" = ./cmd/lintpin ]; then
	if [ -n "${STUB_LINTPIN:-}" ]; then
		shift 2
		exec "$STUB_LINTPIN" "$@"
	fi
	exit 0
fi
`
	gateStubGo = "#!/bin/sh\n" + gateStubHead + gateStubGoPlusLintpin + gateStubTail

	// gateStubLint stands in for golangci-lint, including the one behaviour the
	// gate relies on it for: with STUB_LINT_CONTEND=n another run holds the module
	// lock for the first n runs, so each of those exits 3 with golangci-lint's
	// lock line, as the real one does, and the gate's wait script has to retry.
	// `version --short` answers STUB_LINT_VERSION, or the pin the fixture passes
	// in STUB_LINT_SHORT, so a case never reads the Makefile itself, and
	// STUB_LINT_MISSING models a PATH with no linter at all.
	gateStubLint = `#!/bin/sh
echo "golangci-lint $*" >>"$STUB_LOG"
[ -n "${STUB_LINT_MISSING:-}" ] && exit 127
if [ "$1" = version ]; then
	echo "${STUB_LINT_VERSION:-$STUB_LINT_SHORT}"
	exit 0
fi
rel=$(pwd -P); rel=${rel#"$STUB_REPO"}; rel=${rel#/}
[ "$rel" = tracker ] && echo "golangci-lint in tracker" >>"$STUB_LOG"
if [ "$1" = run ] && [ -n "${STUB_LINT_CONTEND:-}" ]; then
	n=$(grep -c '^golangci-lint run' "$STUB_LOG")
	if [ "$n" -le "$STUB_LINT_CONTEND" ]; then
		echo "Error: parallel golangci-lint is running"
		exit 3
	fi
fi
[ "$1" = run ] && [ -n "${STUB_LINT_FAIL:-}" ] && exit 1
[ "$1" = run ] && [ -n "${STUB_LINT_FAIL_DIR:-}" ] && [ "$rel" = "$STUB_LINT_FAIL_DIR" ] && exit 1
exit 0
`
	// gateStubPreflight stands in for the exec-tax preflight: it logs that the
	// gate ran it, and under EXEC_TAX_WARN it warns and still exits 0, as the real
	// one does on a taxed host.
	gateStubPreflight = `#!/bin/sh
echo "preflight" >>"$STUB_LOG"
if [ -n "${EXEC_TAX_WARN:-}" ]; then
	echo "gate: WARNING exec tax 180 ms/exec in this process tree: a fresh executable here waits on a macOS scan or a throttled process, and the unit tier builds one per package (see gt-2ycne.1)" >&2
fi
exit 0
`
	gateStubShellTests = `#!/bin/sh
echo "shell-tests" >>"$STUB_LOG"
exit 0
`
	// gateStubDocker logs the docker command line the image recipes run. It
	// starts no build, so a case reads which build args a recipe passes.
	gateStubDocker = `#!/bin/sh
echo "docker $*" >>"$STUB_LOG"
exit 0
`
)

// GateSandbox runs the real gate recipe against stub tools, so the recipe's
// stages decide the exit code without building, linting or testing the tree.
type GateSandbox struct {
	*Sandbox
	bin string
}

// GateRun is one gate run against a GateSandbox's stubs: the exit code, the
// gate's stdout and stderr, and the stub call log. stdout carries the recipe's
// own messages (make runs the lint loop's `lint failed in $m` there), stderr
// the gate's stage and wall lines.
type GateRun struct {
	Code   int
	Stdout string
	Stderr string
	Calls  []string
}

// hasCall reports whether the run's stub call log holds line exactly.
func (r GateRun) hasCall(line string) bool {
	for _, call := range r.Calls {
		if call == line {
			return true
		}
	}
	return false
}

// hasCallWithPrefix reports whether any call line begins with prefix.
func (r GateRun) hasCallWithPrefix(prefix string) bool {
	for _, call := range r.Calls {
		if strings.HasPrefix(call, prefix) {
			return true
		}
	}
	return false
}

// callLog is the run's stub call log, one call per line, for a failure message.
func (r GateRun) callLog() string { return strings.Join(r.Calls, "\n") }

// newGateSandbox returns a GateSandbox whose PATH leads with stub go,
// golangci-lint, the exec-tax preflight and the shell-test script, and whose
// checkout is a scratch directory holding the checkout under test's Makefile,
// a tracker module and the docker package list: the gate runs the real recipe
// there, never in the live tree, so a stage that slipped past its stub could
// not touch the checkout under test.
func newGateSandbox(t *testing.T) *GateSandbox {
	t.Helper()
	s := newSandbox(t)
	g := &GateSandbox{Sandbox: s, bin: s.Path("bin")}
	for _, stub := range []struct{ name, body string }{
		{"go", gateStubGo},
		{"golangci-lint", gateStubLint},
		{"docker", gateStubDocker},
		{"preflight", gateStubPreflight},
		{"shell-tests", gateStubShellTests},
	} {
		g.writeStub(stub.name, stub.body)
	}
	g.repo = s.Path("scratch")
	for name, body := range gateScratchFiles(t) {
		s.WriteFile(filepath.Join("scratch", name), body)
	}
	return g
}

// gateScratchFiles is what the gate recipes read from the tree: the checkout
// under test's Makefile and lint-lock-wait script, a tracker module and the
// docker package list.
func gateScratchFiles(t *testing.T) map[string]string {
	t.Helper()
	wait, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "lint-lock-wait.sh"))
	if err != nil {
		t.Fatalf("read the lint-lock-wait script the gate runs lint through: %v", err)
	}
	return map[string]string{
		"Makefile":                       makefileText(t, repoRoot(t)),
		"scripts/lint-lock-wait.sh":      string(wait),
		"tracker/go.mod":                 "module example.invalid/tracker\n\ngo 1.21\n",
		"internal/testpolicy/docker.txt": "internal/cmd # scratch fixture\n",
	}
}

// writeStub writes an executable stub named name into the sandbox's bin
// directory. A stub that cannot be written or made executable fails the test:
// make would otherwise find the host's tool, not the case's stub.
func (g *GateSandbox) writeStub(name, body string) {
	g.t.Helper()
	path := filepath.Join(g.bin, name)
	if err := os.MkdirAll(g.bin, 0o755); err != nil {
		g.t.Fatalf("make the stub directory %s: %v", g.bin, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		g.t.Fatalf("write the %s stub %s: %v", name, path, err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		g.t.Fatalf("make the %s stub %s executable: %v", name, path, err)
	}
}

// Run runs the real gate recipe in the sandbox's checkout against the stubs.
// makeFlags go to make before the target, as the shell's run_gate takes its
// flags; vars are environment assignments for the run (STUB_GO_FAIL and
// friends).
func (g *GateSandbox) Run(makeFlags []string, vars ...string) GateRun {
	g.t.Helper()
	return g.runTarget("gate", makeFlags, vars...)
}

// runTarget runs the named make goal in the sandbox's checkout against the
// stubs, as the shell fixture's run_gate runs the goal its TARGET names: the
// gate, presubmit, test-slow or test-integration. The inherited
// GT_TEST_DOCKER=1 is set too, so a recipe that forgets to turn containers off
// is seen to pass it on. The call log is truncated first, so a case reads only
// this run's calls; the recipe's stdout and stderr come back on the GateRun.
func (g *GateSandbox) runTarget(target string, makeFlags []string, vars ...string) GateRun {
	g.t.Helper()
	calls := g.Path("calls")
	if err := os.WriteFile(calls, nil, 0o644); err != nil {
		g.t.Fatalf("truncate the stub call log %s: %v", calls, err)
	}
	argv := append([]string{"-C", g.repo, "--no-print-directory"}, makeFlags...)
	argv = append(argv, target,
		"SHELL_TESTS="+filepath.Join(g.bin, "shell-tests"),
		"EXEC_TAX_PREFLIGHT="+filepath.Join(g.bin, "preflight"))
	ctx, cancel := context.WithTimeout(context.Background(), gateRunTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "make", argv...)
	cmd.Env = g.gateEnv(vars)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			g.t.Fatalf("run make %s: %v\n%s", target, err, stderr.String())
		}
		code = exit.ExitCode()
	}
	if ctx.Err() != nil {
		g.t.Fatalf("make %s did not finish within %s: %v\n%s", target, gateRunTimeout, ctx.Err(), stderr.String())
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		g.t.Fatalf("read the stub call log %s: %v", calls, err)
	}
	var log []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line != "" {
			log = append(log, line)
		}
	}
	return GateRun{Code: code, Stdout: stdout.String(), Stderr: stderr.String(), Calls: log}
}

// gateEnv is the environment the gate runs in: the sandbox environment with
// the stub directory leading PATH and the case's variables appended last, so
// a case's variable wins over anything inherited. The caller's PATH stays
// below the stubs because the recipe itself runs bash, date, mktemp and find,
// as the shell fixture's `PATH="$TMP/bin:$PATH"` leaves them.
func (g *GateSandbox) gateEnv(vars []string) []string {
	drop := map[string]bool{"PATH": true, "STUB_LOG": true, "STUB_REPO": true, "GT_TEST_DOCKER": true, "CPU_CLASS": true, "GOFLAGS": true}
	for _, kv := range vars {
		key, _, _ := strings.Cut(kv, "=")
		drop[key] = true
	}
	env := []string{"PATH=" + g.bin + string(os.PathListSeparator) + os.Getenv("PATH")}
	for _, kv := range sandboxEnv() {
		key, _, _ := strings.Cut(kv, "=")
		if !drop[key] {
			env = append(env, kv)
		}
	}
	// The stubs name a module by its path under the checkout, read physically
	// (pwd -P), so the checkout is given the same way.
	repo, err := filepath.EvalSymlinks(g.repo)
	if err != nil {
		g.t.Fatalf("resolve the checkout %s: %v", g.repo, err)
	}
	// The stub linter answers the Makefile's own pin, so a case that sets no
	// STUB_LINT_VERSION carries the version the recipe requires.
	env = append(env, "GT_TEST_DOCKER=1", "STUB_LOG="+g.Path("calls"), "STUB_REPO="+repo,
		"STUB_LINT_SHORT="+strings.TrimPrefix(golangciPin(g.t), "v"))
	return append(env, vars...)
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
