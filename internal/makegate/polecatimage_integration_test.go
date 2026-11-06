//go:build integration

package makegate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The polecat-image cases: one test per case in lines 441-600 of
// scripts/makefile-gate_test.sh, from the BD_SRC guard through the empty-bd
// commit refusal. Each case runs the real recipe against a scratch checkout in
// the test's temp directory, with a fake go and docker first on PATH, so no
// case reads the live town, the caller's repository or the host's toolchain.
//
// The scratch checkout is a Sandbox (Checkout and Commit), whose git calls run
// under sandboxEnv: an inherited GIT_DIR, GIT_CONFIG_PARAMETERS or global config
// cannot decide what the fixture commits or what its tree guard sees (gt-n8354).
// Every make call runs under the same environment.
//
// Each case runs make, a process, so this file is integration tier
// (docs/standards/test-engineering.md, rule 2); it starts no container and
// reaches no network. Every name carries the TestIntegrationPolecatImage prefix
// the bead's `-run` covers, and every comment names the mutation that turns the
// test red.

// scratchGoMod and scratchGTMain are the compilable minimum the fixture's tree
// guard needs: a module and a main package for the gt build line. The fake go
// answers those lines, so neither is compiled.
const (
	scratchGoMod  = "module example.invalid/scratch\n\ngo 1.21\n"
	scratchGTMain = "package main\n\nfunc main() {}\n"
)

// polecatStubGo answers go for the scratch checkout. It logs every invocation to
// GO_ARGS_LOG so a case can see which recipe lines ran, writes (and makes
// executable) any -o path a recipe names, and answers the context command
// (internal/polecat/imagectx/cmd/build) with the context and label lines it
// prints, the bd label taken from STUB_BD_COMMIT, or exits STUB_BD_EXIT when that
// is set.
const polecatStubGo = `#!/bin/sh
[ -n "$GO_ARGS_LOG" ] && printf '%s\n' "$*" >>"$GO_ARGS_LOG"
out=""; prev=""
for a in "$@"; do
  [ "$prev" = "-o" ] && out="$a"
  prev="$a"
done
if [ -n "$out" ]; then
  mkdir -p "$(dirname "$out")" && printf 'stub\n' >"$out" && chmod +x "$out" || exit 1
fi
if [ "$1" = "run" ] && [ "$2" = "./internal/polecat/imagectx/cmd/build" ]; then
  [ -n "$STUB_BD_EXIT" ] && exit "$STUB_BD_EXIT"
  rev=""; ctx=""; prev=""
  for a in "$@"; do
    [ "$prev" = "-rev" ] && rev="$a"
    [ "$prev" = "-out" ] && ctx="$a"
    prev="$a"
  done
  mkdir -p "$ctx/bin" || exit 1
  printf 'context: %s\n' "$ctx"
  printf 'label: org.gastown.gt-commit=%s\n' "$rev"
  printf 'label: org.gastown.bd-commit=%s\n' "$STUB_BD_COMMIT"
  printf 'label: go-floor=1.27.1\n'
fi
exit 0
`

// polecatStubDocker records the arguments of each docker build in
// DOCKER_ARGS_LOG, where a case reads the image labels.
const polecatStubDocker = `#!/bin/sh
printf '%s\n' "$*" >>"$DOCKER_ARGS_LOG"
`

// stagePolecatStubs writes the fake go and docker into one temp directory and
// returns that directory as the PATH entry the recipe must reach before the
// host's tools, with the two argument logs beside them.
func stagePolecatStubs(t *testing.T) (bin, goLog, dockerLog string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"go": polecatStubGo, "docker": polecatStubDocker} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	return dir, filepath.Join(dir, "go.args"), filepath.Join(dir, "docker.args")
}

// polecatScratch is the scratch checkout a case runs the recipe in: a Sandbox
// checkout holding the checkout under test's Makefile plus the files the case
// needs, with the stub tools first on PATH and every town path the recipe reads
// pointing inside the test's temp directory.
type polecatScratch struct {
	s         *Sandbox
	dir       string
	bin       string
	goLog     string
	dockerLog string
	env       []string
}

// newPolecatScratch makes the scratch checkout: files are written beside the
// Makefile and committed, so the tree guard starts clean. The Docker package
// list is included because the Makefile reads it while parsing; without it every
// run prints a sed error from a variable none of these targets use.
func newPolecatScratch(t *testing.T, files map[string]string) *polecatScratch {
	t.Helper()
	s := newSandbox(t)
	dir := s.Checkout("scratch")
	all := map[string]string{
		"Makefile":                       makefileText(t, repoRoot(t)),
		"internal/testpolicy/docker.txt": "internal/cmd # scratch fixture\n",
	}
	for name, body := range files {
		all[name] = body
	}
	s.Commit(dir, "init", all)
	bin, goLog, dockerLog := stagePolecatStubs(t)
	return &polecatScratch{s: s, dir: dir, bin: bin, goLog: goLog, dockerLog: dockerLog}
}

// newLabelScratch is the checkout the label family runs: the Makefile, a
// compilable cmd/gt, the seed pins and the floor script the context carries. The
// context command is stubbed, so the labels it prints come from the stub, and no
// case reads a live rig or the host's toolchain.
func newLabelScratch(t *testing.T) *polecatScratch {
	return newPolecatScratch(t, map[string]string{
		"go.mod":                     scratchGoMod,
		"cmd/gt/main.go":             scratchGTMain,
		"deploy/polecat/seed-pins":   "fx 92b9e510ac2f9ff8c80b0291f312ac482bf96c38\n",
		"deploy/polecat/go-floor.sh": "#!/bin/sh\necho 1.21\n",
	})
}

// run runs the recipe for real in the scratch checkout with args, under
// sandboxEnv plus the fixture's environment plus extra (later values win). The
// variables a case decides are removed from the caller's environment first, so
// only the case names them: an inherited POLE_NODE_PRUNE would turn a default
// case into its own opt-in (gt-pkl4q), and a removed flag leaves the Makefile's
// `?=` default in force.
func (p *polecatScratch) run(t *testing.T, extra []string, args ...string) (string, error) {
	t.Helper()
	env := []string{
		"PATH=" + p.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"POLE_TOWN=" + p.s.Path("town"),
		"POLE_SEED_DIR=" + p.s.Path("seed"),
		"GO_ARGS_LOG=" + p.goLog,
		"DOCKER_ARGS_LOG=" + p.dockerLog,
	}
	env = append(env, p.env...)
	env = append(env, extra...)
	ctx, cancel := context.WithTimeout(context.Background(), makeTestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "make", append([]string{"-C", p.dir, "--no-print-directory"}, args...)...)
	cmd.Env = polecatEnv(env)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// polecatEnv is sandboxEnv with the variables a case decides removed, then add.
// A variable is removed rather than set empty where the Makefile's own default
// must apply: `?=` does not override a variable that is set, even to nothing.
func polecatEnv(add []string) []string {
	decided := map[string]bool{
		"PATH":            true,
		"POLE_TOWN":       true,
		"POLE_SEED_DIR":   true,
		"POLE_NODE_PRUNE": true,
		"GO_ARGS_LOG":     true,
		"DOCKER_ARGS_LOG": true,
		"STUB_BD_COMMIT":  true,
		"STUB_BD_EXIT":    true,
	}
	var env []string
	for _, kv := range sandboxEnv() {
		key, _, _ := strings.Cut(kv, "=")
		if !decided[key] {
			env = append(env, kv)
		}
	}
	return append(env, add...)
}

// exitCode is make's exit status, failing the test when make succeeded: a case
// that asserts a refusal reads the status, not a missing one.
func exitCode(t *testing.T, err error) int {
	t.Helper()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("make succeeded, want a refusal: %v", err)
	}
	return exit.ExitCode()
}

// mustNotBuild fails when a refusal's output reached a build line. make echoes a
// recipe line before running it, so the echo is the evidence that the guard
// stopped the run rather than a later line failing for another reason.
func mustNotBuild(t *testing.T, out string) {
	t.Helper()
	for _, build := range []string{"go build", "docker build"} {
		if line := lineOf(out, build); line != 0 {
			t.Fatalf("the refusal ran past its guard to a build (line %d holds %q)\n%s", line, build, out)
		}
	}
}

// goInvocations is the stub go's argument log. It fails the test when the stub
// never ran or stopped before the bd build helper: a case that asserts a
// command is absent needs the log that would hold it, or a run that never
// reached that point passes by default.
func (p *polecatScratch) goInvocations(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(p.goLog)
	if err != nil {
		t.Fatalf("the stub go recorded no invocation, so nothing proves the run reached the build: %v", err)
	}
	log := string(data)
	if !strings.Contains(log, "run ./internal/polecat/imagectx/cmd/build") {
		t.Fatalf("the stub go log holds no context command invocation, so the run stopped before the point these assertions cover\n%s", log)
	}
	return log
}

// dockerArgs is the stub docker's argument log, failing the test when docker
// never ran.
func (p *polecatScratch) dockerArgs(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(p.dockerLog)
	if err != nil {
		t.Fatalf("the stub docker recorded no build, so the recipe stopped before the image build: %v", err)
	}
	return string(data)
}

// TestIntegrationPolecatImageNeedsNoBDSrc checks "polecat-image builds bd from
// the pins, so a run with no BD_SRC reaches the context command and the image".
// Mutation: put back a BD_SRC guard ahead of the context command, and the run
// with BD_SRC unset refuses before the command is logged.
func TestIntegrationPolecatImageNeedsNoBDSrc(t *testing.T) {
	p := newLabelScratch(t)
	out, err := p.run(t, []string{"STUB_BD_COMMIT=" + helperCommit}, "polecat-image", "BD_SRC=")
	if err != nil {
		t.Fatalf("polecat-image with BD_SRC unset failed: %v\n%s", err, out)
	}
	if log := p.goInvocations(t); !strings.Contains(log, "run ./internal/polecat/imagectx/cmd/build") {
		t.Fatalf("polecat-image with BD_SRC unset did not reach the context command\n%s", log)
	}
	p.dockerArgs(t)
}

// TestIntegrationPolecatImageRefusesATreeWithAnUntrackedFile checks "polecat-image
// refuses a tree with an untracked file, before building anything".
// Mutation: drop the `git status --porcelain` guard, and the untracked file is
// not named: the run falls through to the next guard with nothing refusing the
// tree.
func TestIntegrationPolecatImageRefusesATreeWithAnUntrackedFile(t *testing.T) {
	p := newPolecatScratch(t, map[string]string{
		"go.mod":         scratchGoMod,
		"cmd/gt/main.go": scratchGTMain,
	})
	if err := os.WriteFile(filepath.Join(p.dir, "scratch.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write the untracked file: %v", err)
	}
	out, err := p.run(t, nil, "polecat-image", "BD_SRC=/nonexistent")
	if code := exitCode(t, err); code != 2 {
		t.Fatalf("polecat-image with an untracked file exited %d, want 2\n%s", code, out)
	}
	if !strings.Contains(out, "uncommitted or untracked changes") {
		t.Fatalf("polecat-image with an untracked file does not name the tree guard\n%s", out)
	}
	mustNotBuild(t, out)
}

// TestIntegrationPolecatImageLetsACleanTreePastItsGuardToTheBuild checks
// "polecat-image lets a clean tree past its guard to the context command".
// Mutation: replace the tree guard with a constant false, and the clean tree is
// refused; drop the staging line, and the command never runs. The stub go
// answers the command, so the case reads the line the recipe ran, with the
// context directory it named, not the host's toolchain or a checkout.
func TestIntegrationPolecatImageLetsACleanTreePastItsGuardToTheBuild(t *testing.T) {
	p := newPolecatScratch(t, map[string]string{
		"go.mod":         scratchGoMod,
		"cmd/gt/main.go": scratchGTMain,
	})

	// No bd commit is printed here, so the recipe still exits non-zero after the
	// command; this case reads the command, not the run's verdict.
	out, err := p.run(t, nil, "polecat-image", "BD_SRC=/nonexistent")
	if code := exitCode(t, err); code != 2 {
		t.Fatalf("polecat-image on a clean tree exited %d, want 2 as the empty bd commit stops the recipe\n%s", code, out)
	}
	if strings.Contains(out, "uncommitted or untracked") {
		t.Fatalf("polecat-image refused a clean tree at its tree guard\n%s", out)
	}
	ctx := filepath.Join(p.dir, "deploy", "polecat", "context")
	log := p.goInvocations(t)
	if !strings.Contains(log, "run ./internal/polecat/imagectx/cmd/build -rev ") || !strings.Contains(log, "-out "+ctx) {
		t.Fatalf("the recipe never ran the context command into %s\n%s", ctx, log)
	}
}

// helperCommit is what the fake bd build helper prints: the commit the image's
// bd was built at, which the label and the refusal cases read.
const helperCommit = "3b90ab0c0ffee0000000000000000000000000aa"

// TestIntegrationPolecatImageLabelsTheImageWithTheHelpersCommit checks
// "polecat-image labels the image with the commit the bd build helper printed".
// Mutation: drop the --label org.gastown.bd-commit argument, or label from
// another source than the helper's stdout, and docker's recorded arguments
// carry no such label or another commit.
func TestIntegrationPolecatImageLabelsTheImageWithTheHelpersCommit(t *testing.T) {
	p := newLabelScratch(t)
	out, err := p.run(t, []string{"STUB_BD_COMMIT=" + helperCommit}, "polecat-image", "BD_SRC=/somewhere/else")
	if err != nil {
		t.Fatalf("polecat-image failed with a labelled build: %v\n%s", err, out)
	}
	if args := p.dockerArgs(t); !strings.Contains(args, "--label org.gastown.bd-commit="+helperCommit) {
		t.Fatalf("docker's arguments do not label the image with the helper's commit %s\n%s", helperCommit, args)
	}
}

// TestIntegrationPolecatImageLeavesTheSandboxNodesAloneByDefault checks "a
// default polecat-image leaves the sandbox nodes alone (gt-06ni7)".
// Mutation: default POLE_NODE_PRUNE to 1, and the stub go log records the
// nodeprune command in a build nobody opted into.
func TestIntegrationPolecatImageLeavesTheSandboxNodesAloneByDefault(t *testing.T) {
	p := newLabelScratch(t)
	out, err := p.run(t, []string{"STUB_BD_COMMIT=" + helperCommit}, "polecat-image", "BD_SRC=/somewhere/else")
	if err != nil {
		t.Fatalf("a default polecat-image failed: %v\n%s", err, out)
	}
	p.dockerArgs(t) // the build ran, so the prune line was reachable
	if log := p.goInvocations(t); strings.Contains(log, "run ./cmd/nodeprune") {
		t.Fatalf("a default polecat-image pruned the sandbox nodes\n%s", log)
	}
}

// TestIntegrationPolecatImagePrunesTheSandboxNodesOnOptIn checks "polecat-image
// prunes the sandbox nodes after the build with POLE_NODE_PRUNE=1 (gt-06ni7)".
// Mutation: drop the recursive $(MAKE) polecat-node-prune line, or invert the
// ifeq so the flag no longer selects it, and the stub go log records no prune.
func TestIntegrationPolecatImagePrunesTheSandboxNodesOnOptIn(t *testing.T) {
	p := newLabelScratch(t)
	out, err := p.run(t, []string{"STUB_BD_COMMIT=" + helperCommit, "POLE_NODE_PRUNE=1"}, "polecat-image", "BD_SRC=/somewhere/else")
	if err != nil {
		t.Fatalf("polecat-image with POLE_NODE_PRUNE=1 failed: %v\n%s", err, out)
	}
	if log := p.goInvocations(t); !strings.Contains(log, "run ./cmd/nodeprune") {
		t.Fatalf("polecat-image with POLE_NODE_PRUNE=1 did not prune the sandbox nodes\n%s", log)
	}
}

// TestIntegrationPolecatImageStopsBeforeDockerWhenTheBDBuildFails checks
// "polecat-image stops before docker when the context command fails, bd's build
// among its steps".
// Mutation: drop the staging line's `|| { echo ...; exit 2; }` guard, and the
// failed run is read as an empty context instead, so the output no longer names
// the failure.
func TestIntegrationPolecatImageStopsBeforeDockerWhenTheBDBuildFails(t *testing.T) {
	p := newLabelScratch(t)
	out, err := p.run(t, []string{"STUB_BD_EXIT=1", "POLE_NODE_PRUNE=1"}, "polecat-image", "BD_SRC=/somewhere/else")
	exitCode(t, err)
	if !strings.Contains(out, "did not stage from the pins") {
		t.Fatalf("polecat-image with a failing bd build does not name the failure\n%s", out)
	}
	mustNotExist(t, p.dockerLog)
	if log := p.goInvocations(t); strings.Contains(log, "run ./cmd/nodeprune") {
		t.Fatalf("a failed bd build ran the node prune the run asked for\n%s", log)
	}
}

// TestIntegrationPolecatImageRefusesAnEmptyBDCommit checks "polecat-image
// refuses a context with an empty bd commit before docker builds the image".
// Mutation: drop the `test -n "$bd_commit"` part of the label guard, and docker
// runs with an empty org.gastown.bd-commit label, so the output no longer names
// the refusal.
func TestIntegrationPolecatImageRefusesAnEmptyBDCommit(t *testing.T) {
	p := newLabelScratch(t)
	out, err := p.run(t, []string{"STUB_BD_COMMIT=", "POLE_NODE_PRUNE=1"}, "polecat-image", "BD_SRC=/somewhere/else")
	exitCode(t, err)
	if !strings.Contains(out, "names no context, gt commit, bd commit or Go floor") {
		t.Fatalf("polecat-image with an empty bd commit does not name the refusal\n%s", out)
	}
	mustNotExist(t, p.dockerLog)
	if log := p.goInvocations(t); strings.Contains(log, "run ./cmd/nodeprune") {
		t.Fatalf("an empty bd commit ran the node prune the run asked for\n%s", log)
	}
}
