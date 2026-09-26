package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
)

var tapGuardContainerSuiteCmd = &cobra.Command{
	Use:   "container-suite",
	Short: "Block bare go test/make test on testcontainers-backed packages",
	Long: `Block unwrapped container-backed test suites via Claude Code PreToolUse hooks.

Some packages spin real Dolt/testcontainers containers in their tests (see
internal/slot's package doc). The host's Docker VM has a fixed CPU/memory
bound shared by the whole town, so two container-backed suites running
concurrently starve each other even when the host itself shows idle CPU.
'gt slot run' serializes access to that VM, but only for commands actually
run through it — an ad-hoc "go test ./internal/beads/..." run straight from
a polecat or refinery session is invisible to the slot and still collides
with anything else using the VM (gt-e2rs: two such collisions in one night,
one starving the refinery for 29 minutes).

This guard blocks, when running as a polecat or refinery:
  - go test <pkg...>   where <pkg...> intersects the testcontainers-backed
                        package list (containerSuitePackages) or is a
                        whole-repo wildcard (./..., ...)
  - go test .          (or a bare "go test") when the directory it runs in
                        is one of those packages — the cwd is judged like
                        any other target, not exempted
  - make test           which unconditionally runs "go test ./..."

...unless the command is already wrapped in 'gt slot run -- <command>', in
which case it is allowed through untouched. Bare go test runs that only
target non-Docker packages are also allowed through.

Exit codes:
  0 - Operation allowed
  2 - Operation BLOCKED`,
	RunE: runTapGuardContainerSuite,
}

func init() {
	tapGuardCmd.AddCommand(tapGuardContainerSuiteCmd)
}

// containerSuitePackages lists the repo-relative Go import paths whose tests
// spin real Dolt/testcontainers containers (or a shell script that launches
// them), derived (2026-09-11, gt-e2rs; re-verified 2026-09-22, gt-dlzc) by
// finding every package whose TEST BINARY links internal/testutil — the
// only path into container-spinning code (testutil.RequireDoltContainer,
// testutil.WithDolt, testutil.StartIsolatedDoltContainer, all in
// internal/testutil/doltserver.go) — with
// `go list -test -f '{{.ImportPath}} {{join .Deps "\n"}}' ./internal/...`
// and filtering for the module's testutil import path. This catches both
// call-site packages (internal/daemon, internal/mail, internal/refinery,
// internal/doltserver, internal/beads, internal/convoy, internal/polecat —
// test files calling the entry points) and import-only packages (doctor,
// crew, deacon, ... — the tests import testutil for the hermetic harness,
// and the harness re-exports the opt-in env GT_TEST_DOCKER, so the linked
// entry points CAN run when the switch is on even though the package's own
// test files call no container function). A package whose test binary links
// testutil is on this list even if no current test would actually start a
// container: the cost of a false entry is one unnecessary 'gt slot run'
// wrap (and doctor's tests are the ones that read the Docker daemon's
// capacity anyway, so even they are not wasted); a container-spinning
// package MISSING from this list is invisible to the guard entirely.
var containerSuitePackages = []string{
	"internal/beads",
	"internal/cmd",
	"internal/convoy",
	"internal/crew",
	"internal/daemon",
	"internal/deacon",
	"internal/deps",
	"internal/doctor",
	"internal/dog",
	"internal/doltserver",
	"internal/git",
	"internal/health",
	"internal/mail",
	"internal/plugin",
	"internal/polecat",
	"internal/protocol",
	"internal/proxy",
	"internal/refinery",
	"internal/refinery/editorial",
	"internal/rig",
	"internal/testutil",
	"internal/tui/convoy",
	"internal/tui/feed",
	"internal/web",
	"internal/witness",
}

func runTapGuardContainerSuite(cmd *cobra.Command, args []string) error {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil // fail open
	}
	command := extractCommand(input)
	if command == "" {
		return nil
	}
	if !isPolecatOrRefineryContext() {
		return nil
	}

	// The scope rule answers first for a polecat: its advice is "iterate with
	// -run", and it must win over the container-suite rule's "run it wrapped"
	// line, which for a whole-suite command is exactly the run the scope rule
	// forbids (gt-v6se: three polecats followed that line into full suites
	// beside the refinery's gate).
	if isPolecatContext() {
		if reason, matched := evaluatePolecatTestScope(command); reason != "" {
			printPolecatTestScopeBlock(reason, command, matched)
			return NewSilentExit(2)
		}
	}
	if reason, matched := evaluateContainerSuiteCommand(command); reason != "" {
		printContainerSuiteBlock(reason, command, matched)
		return NewSilentExit(2)
	}
	return nil
}

// isPolecatOrRefineryContext reports whether the current process is running
// as a polecat or refinery — the two roles whose ad-hoc scoped go test/make
// test invocations have repeatedly spun Dolt/testcontainers containers
// unwrapped next to the refinery's own gt-slot-run-wrapped gate (gt-e2rs:
// two collisions in one night, one starving the refinery for 29 minutes).
// Other agent contexts (crew, witness, deacon, mayor) are left unguarded —
// they don't run scoped test suites as part of their normal work.
func isPolecatOrRefineryContext() bool {
	if os.Getenv("GT_POLECAT") != "" {
		return true
	}
	if isRefineryRole() {
		return true
	}
	cwd, err := os.Getwd()
	if err == nil && strings.Contains(cwd, "/polecats/") {
		return true
	}
	return false
}

// containerSuiteSlotRunTokens is the token sequence that marks a shell
// segment as already running through the townwide container-gate slot (see
// internal/slot and 'gt slot run --role <rig>/<name> -- <command>').
var containerSuiteSlotRunTokens = []string{"gt", "slot", "run"}

// evaluateContainerSuiteCommand splits command into shell segments (on
// ;/&&/||/|, same as matchesPRWorkflowCommand) and evaluates each
// independently, so a "gt slot run -- go test ./..." wrapper and an
// unrelated later "go test ./internal/beads/..." on the same compound line
// are judged separately rather than one exemption covering the whole line.
// Returns the reason for the first blocked segment found, or ("", nil) if
// none is blocked.
func evaluateContainerSuiteCommand(command string) (reason string, matched []string) {
	tokens := shellTokenize(strings.TrimSpace(command))
	// Container-backed tests are opt-in (testutil.DockerTestsEnv). A bare
	// `go test` of a container-bearing package cannot start a container
	// unless the command turns the switch on somewhere — as an env prefix,
	// via env(1), or an earlier `export` on the same line — so only then is
	// the slot required. `make test` sets it in the Makefile and is judged
	// unconditionally. The switch is looked for across the WHOLE command,
	// not per segment, because `export X=1; go test ...` enables it for the
	// later segment. An opt-in already exported in the hook's environment
	// counts too: the test process inherits it without it being typed.
	dockerOn := commandEnablesDockerTests(tokens) || os.Getenv(dockerTestsEnv) == "1"

	var segment []string
	for _, tok := range tokens {
		if shellCommandSeparators[tok] {
			if r, m := evaluateContainerSuiteSegment(segment, dockerOn); r != "" {
				return r, m
			}
			segment = nil
			continue
		}
		segment = append(segment, tok)
	}
	return evaluateContainerSuiteSegment(segment, dockerOn)
}

// dockerTestsEnv mirrors testutil.DockerTestsEnv. It is spelled out here
// rather than imported because internal/testutil pulls testcontainers and
// the Docker client into whatever links it, and this is the gt binary;
// TestDockerTestsEnvMatchesTestutil keeps the two in step.
const dockerTestsEnv = "GT_TEST_DOCKER"

// commandEnablesDockerTests reports whether any token sets the container
// opt-in switch to 1 (GT_TEST_DOCKER=1, bare, quoted, or as an export/env
// value). The hook's own environment is checked by the caller: an exported
// GT_TEST_DOCKER=1 in the session reaches `go test` without appearing in
// the command text.
func commandEnablesDockerTests(tokens []string) bool {
	return commandSetsDockerTests(tokens, "1")
}

// commandSetsDockerTests reports whether any token assigns the container
// opt-in switch the given value (bare, quoted, or as an export/env value).
// It is commandEnablesDockerTests' parser, shared with gt done's
// --pre-verified slot decision, which also needs to see an explicit "0".
func commandSetsDockerTests(tokens []string, value string) bool {
	prefix := dockerTestsEnv + "="
	for _, t := range tokens {
		if i := strings.Index(t, prefix); i >= 0 && (i == 0 || t[i-1] == ' ') {
			v := strings.Trim(t[i+len(prefix):], `"'`)
			if v == value {
				return true
			}
		}
	}
	return false
}

// evaluateContainerSuiteSegment judges a single shell segment (tokens
// between shell operators). tokens is original-case; matching is done on a
// lowercased copy so "Go Test" and "go test" are treated the same.
func evaluateContainerSuiteSegment(tokens []string, dockerOn bool) (reason string, matched []string) {
	if len(tokens) == 0 {
		return "", nil
	}
	lower := make([]string, len(tokens))
	for i, t := range tokens {
		lower[i] = strings.ToLower(t)
	}

	if containsSubsequence(lower, containerSuiteSlotRunTokens) {
		return "", nil // already wrapped by gt slot run
	}

	if i := findTestInvocation(lower, "go"); i >= 0 && dockerOn {
		cwd, _ := os.Getwd()
		wholeRepo, pkgs := containerSuiteTarget(goTestPackageArgs(tokens[i+2:]), cwd)
		if wholeRepo {
			return "bare 'go test' with a whole-repo target touches every testcontainers-backed package", nil
		}
		if len(pkgs) > 0 {
			return "bare 'go test' targets testcontainers-backed package(s)", pkgs
		}
		return "", nil
	}

	if i := findTestInvocation(lower, "make"); i >= 0 {
		// The Makefile's "test" target unconditionally runs "go test ./..."
		// after its shell-script checks — there is no scoped form of "make
		// test", so it always touches every testcontainers-backed package.
		return "bare 'make test' runs 'go test ./...', which always touches testcontainers-backed packages", nil
	}

	return "", nil
}

// containsSubsequence reports whether want appears as a contiguous run
// within tokens (both already lowercased).
func containsSubsequence(tokens, want []string) bool {
	if len(want) == 0 || len(tokens) < len(want) {
		return false
	}
	for i := 0; i+len(want) <= len(tokens); i++ {
		match := true
		for j, w := range want {
			if tokens[i+j] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// makeValueFlags are the make(1) short options whose value is mandatory and
// may be the NEXT token ("-C dir", "-f file", "-o file", "-W file", "-I
// dir"); tokens reach here lowercased, so -W and -I appear as -w and -i,
// which is why the valueless -w/-i are absent from the map: a lowercased
// token cannot tell them apart, and the safe reading is "the next token may
// be a value". -j and -l take an OPTIONAL value, so the next token is a
// value only when it looks like one (all digits). No branch ever swallows
// the target "test" itself.
var makeValueFlags = map[string]bool{
	"-c": true, "-f": true, "-o": true, "-w": true, "-i": true,
	"--directory": true, "--file": true, "--makefile": true, "--old-file": true, "--assume-old": true,
	"--what-if": true, "--new-file": true, "--assume-new": true, "--include-dir": true,
}
var makeOptionalNumberFlags = map[string]bool{"-j": true, "-l": true}

// findTestInvocation returns the index of the first "<tool> test" invocation
// in tokens (already lowercased) — "go test" or "make test" — or -1.
func findTestInvocation(tokens []string, tool string) int {
	return findInvocation(tokens, tool, "test")
}

// findInvocation returns the index of the first "<tool> <target>" invocation
// in tokens (already lowercased) — e.g. "go test", "go build", "make test",
// "make build" — or -1. For make, options may sit between the program and
// the target ("make -j4 test", "make -e -w test", "make -C . test") and are
// stepped over: the target is what decides what runs, not the flags in
// front of it. For go, the target must immediately follow the tool with no
// flags between them (mirrors the interim host-hygiene hook's regex, which
// only recognized the exact "go test ./..."/"go build ./..." adjacency —
// see idleGateReason in tap_guard_dangerous.go).
func findInvocation(tokens []string, tool, target string) int {
	for i := 0; i+1 < len(tokens); i++ {
		if tokens[i] != tool {
			continue
		}
		j := i + 1
		if tool == "make" {
			for j < len(tokens) && strings.HasPrefix(tokens[j], "-") {
				flag := tokens[j]
				j++
				if j >= len(tokens) || tokens[j] == target {
					continue
				}
				if makeValueFlags[flag] || (makeOptionalNumberFlags[flag] && isAllDigits(tokens[j])) {
					j++
				}
			}
		}
		if j < len(tokens) && tokens[j] == target {
			return i
		}
	}
	return -1
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// goTestValueFlags are go test flags that consume the NEXT token as their
// value in space-separated form (e.g. "-run TestFoo"), as opposed to a
// self-contained flag (-v, -race) or the "-flag=value" form (a single
// token, needs no special handling). Without skipping these, a value like
// "TestFoo" would be mistaken for a package path.
var goTestValueFlags = map[string]bool{
	"-run": true, "-timeout": true, "-count": true, "-tags": true,
	"-cpu": true, "-parallel": true, "-p": true, "-bench": true,
	"-benchtime": true, "-coverprofile": true, "-covermode": true,
	"-coverpkg": true, "-cpuprofile": true, "-memprofile": true,
	"-blockprofile": true, "-mutexprofile": true, "-outputdir": true,
}

// goTestPackageArgs extracts the package-path arguments from the tokens
// following "go test", skipping flags and the values of flags known to
// consume one. Stops at "--", which marks the start of arguments passed
// through to the test binary rather than package paths.
func goTestPackageArgs(rest []string) []string {
	var pkgs []string
	for i := 0; i < len(rest); i++ {
		tok := rest[i]
		if tok == "--" {
			break
		}
		if strings.HasPrefix(tok, "-") {
			flag := strings.SplitN(tok, "=", 2)[0]
			if goTestValueFlags[flag] && !strings.Contains(tok, "=") && i+1 < len(rest) {
				i++
			}
			continue
		}
		pkgs = append(pkgs, tok)
	}
	return pkgs
}

// gastownModulePath is this module's path, as go.mod declares it. A go test
// argument may spell a package with the prefix
// ("github.com/steveyegge/gastown/internal/beads"); the guards compare
// package paths, so it comes off.
const gastownModulePath = "github.com/steveyegge/gastown"

// wholeRepoPackageArg is what a whole-repo wildcard normalizes to: "...",
// "./...", and their module-prefixed spellings all name every package in the
// module.
const wholeRepoPackageArg = "..."

// cwdPackageArg is what an argument naming the invocation's own directory
// normalizes to. "." and "./" both mean the caller's cwd, and so does a bare
// "go test" with no package argument; the guards resolve it against the
// actual cwd (cwdPackagePath) rather than reading it as a wildcard (gt-1lko).
const cwdPackageArg = "."

// normalizeGoPackageArg reduces a go test package argument to the form the
// guards match on: the module prefix, a leading "./", and a trailing "/..."
// or "/" are stripped, so "./internal/beads/...", "internal/beads/",
// "internal/beads" and "github.com/steveyegge/gastown/internal/beads" all
// normalize to "internal/beads". The two spellings that name no directory —
// the whole-repo wildcard and the cwd itself — normalize to
// wholeRepoPackageArg and cwdPackageArg, which the callers must recognize
// before matching.
func normalizeGoPackageArg(arg string) string {
	p := arg
	p = strings.TrimPrefix(p, gastownModulePath+"/")
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimSuffix(p, "/...")
	p = strings.TrimSuffix(p, "/")
	switch p {
	case "", ".":
		return cwdPackageArg
	case "...":
		return wholeRepoPackageArg
	}
	return p
}

// isWholeRepoPackageArg reports whether a normalized package argument (see
// normalizeGoPackageArg) names every package in the module.
func isWholeRepoPackageArg(norm string) bool {
	return norm == wholeRepoPackageArg
}

// packageArgCovers reports whether a normalized package argument reaches pkg:
// the argument names pkg ("internal/beads"), a directory containing it
// ("internal", from "internal/..." or "./internal/..."), or a directory
// inside it ("internal/beads/sub"). Every argument spelling goes through this
// one rule, so a cwd resolved to a package path (cwdContainerPackages,
// cwdHeavyPackages) is judged exactly as the same package named as an
// argument would be — no spelling can dodge a guard the others obey.
func packageArgCovers(arg, pkg string) bool {
	return arg == pkg || strings.HasPrefix(pkg, arg+"/") || strings.HasPrefix(arg, pkg+"/")
}

// normalizedPackageArgs normalizes a go test package-argument list
// (normalizeGoPackageArg), standing in the cwd argument when the list is
// empty: a bare "go test" tests the package of the directory it was started
// in, which is what an explicit "." names.
func normalizedPackageArgs(pkgArgs []string) []string {
	args := make([]string, 0, len(pkgArgs)+1)
	for _, arg := range pkgArgs {
		args = append(args, normalizeGoPackageArg(arg))
	}
	if len(args) == 0 {
		return []string{cwdPackageArg}
	}
	return args
}

// containerSuitePackagesCoveredBy returns the entries of
// containerSuitePackages that a normalized package argument reaches
// (packageArgCovers).
func containerSuitePackagesCoveredBy(arg string) []string {
	var matched []string
	for _, pkg := range containerSuitePackages {
		if packageArgCovers(arg, pkg) {
			matched = append(matched, pkg)
		}
	}
	return matched
}

// cwdContainerPackages returns the entries of containerSuitePackages the
// package at cwd reaches.
func cwdContainerPackages(cwd string) []string {
	pkg, ok := cwdPackagePath(cwd)
	if !ok || pkg == "" {
		return nil
	}
	return containerSuitePackagesCoveredBy(pkg)
}

// containerSuiteTarget reports the entries of containerSuitePackages a go
// test package-argument list reaches, or true for the whole-repo wildcard.
// Arguments naming the cwd are resolved here, through cwdContainerPackages,
// by the same rule as any other argument — one function judges every
// spelling, so no caller can resolve the argument form and miss the cwd form
// (gt-1lko).
func containerSuiteTarget(pkgArgs []string, cwd string) (wholeRepo bool, matched []string) {
	seen := map[string]bool{}
	for _, p := range normalizedPackageArgs(pkgArgs) {
		if isWholeRepoPackageArg(p) {
			return true, nil
		}
		covered := containerSuitePackagesCoveredBy(p)
		if p == cwdPackageArg {
			covered = cwdContainerPackages(cwd)
		}
		for _, pkg := range covered {
			if !seen[pkg] {
				seen[pkg] = true
				matched = append(matched, pkg)
			}
		}
	}
	return false, matched
}

// isGastownModule reports whether the file at path is a go.mod declaring
// this module — "module .../gastown" (import-path form) or "module
// gastown" (local form). It lives here rather than in internal/plugin
// because a guard that imports internal/plugin would link testcontainers
// (via the chain from internal/plugin) into the gt binary, which it
// deliberately avoids.
func isGastownModule(goModPath string) bool {
	f, err := os.Open(goModPath) //nolint:gosec // G304: path from traversal
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "module ") {
			return strings.HasSuffix(line, "/gastown") || line == "module gastown"
		}
	}
	return false
}

// moduleRootFromCwd walks up from dir to the nearest go.mod and returns its
// directory when that go.mod declares this module, "" otherwise. It reads
// the module declaration rather than the checkout's directory name: the
// refinery runs from /Users/sloan/gt/gastown/refinery/rig and a polecat
// worktree may be named "rig", so matching the name "gastown" makes the
// lookup miss in the very layouts the guards run in and lets "go test ."
// through (gt-1lko). A nearer go.mod that is a different module — e.g. the
// plugins/dolt-snapshots submodule — ends the walk: nothing under it is a
// gastown package path, so no listed package can be there to block.
func moduleRootFromCwd(dir string) string {
	for {
		goMod := filepath.Join(dir, "go.mod")
		if _, err := os.Stat(goMod); err == nil {
			if isGastownModule(goMod) {
				return dir
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// cwdPackagePath returns the module-relative package path that a "go test ."
// run from dir names — "internal/beads" — by walking up to the enclosing
// go.mod (moduleRootFromCwd). ok is false when dir is empty, unreadable, or
// outside this module, and the path is "" when dir is the module root, whose
// package is the repo root and appears on neither guard's list. An
// unresolvable dir leaves nothing to judge, which matches the guard's
// reading of any other target it cannot place: go cannot run a "." target
// from a directory it cannot name either.
func cwdPackagePath(dir string) (pkg string, ok bool) {
	if dir == "" {
		return "", false
	}
	root := moduleRootFromCwd(dir)
	if root == "" {
		return "", false
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return "", true
	}
	return rel, true
}

// printContainerSuiteBlock prints the standard block banner to stderr, naming
// the wrapped form the guard wants instead (gt-e2rs: "message names the wrapped
// form"). It names the two lighter paths as well — the non-container packages
// directly, or letting `gt done`'s gate run the containers — because a refusal
// that names only the hardest path is what sends polecats improvising around it
// (gt-7dxw).
func printContainerSuiteBlock(reason, originalCommand string, matched []string) {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(os.Stderr, "║  ❌ CONTAINER-SUITE COMMAND BLOCKED                              ║")
	fmt.Fprintln(os.Stderr, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintf(os.Stderr, "║  Command: %-53s ║\n", truncateStr(originalCommand, 53))
	fmt.Fprintf(os.Stderr, "║  Reason:  %-53s ║\n", truncateStr(reason, 53))
	if len(matched) > 0 {
		fmt.Fprintf(os.Stderr, "║  Packages: %-52s ║\n", truncateStr(strings.Join(matched, ", "), 52))
	}
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintln(os.Stderr, "║  This spins Dolt/testcontainers containers on the shared Docker ║")
	fmt.Fprintln(os.Stderr, "║  VM. Running it bare can collide with another rig's suite.      ║")
	fmt.Fprintln(os.Stderr, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintf(os.Stderr, "  Run it wrapped instead: %s\n", containerSuiteWrap(originalCommand))
	fmt.Fprintln(os.Stderr, "  Or run the non-container packages directly (they need no slot, and they are")
	fmt.Fprintln(os.Stderr, "  where your change usually lives) — or run neither and let `gt done` gate the")
	fmt.Fprintln(os.Stderr, "  container suites for you: its default test-verify gate runs them once you submit.")
	fmt.Fprintln(os.Stderr, "")
}

// containerSuiteWrap renders the remediation command for a blocked suite: the
// same command run through the container-gate slot. The line is printed for an
// operator to copy and paste, so it has to be exactly what a shell will
// execute — the printed form and the command are the same promise (gt-otvb).
// Three shapes used to break that:
//
//   - A leading VAR=value token is an assignment, not a program. 'gt slot run'
//     has understood one since gt-18nx, but the 'env' form is what the polecat
//     formula prints (mol-polecat-work.formula.toml) and what a gt older than
//     that fix accepts, so it is spelled out rather than relied on.
//   - A compound command cannot simply be prefixed with the wrapper: the wrap
//     would bind to the FIRST segment only, and the shell would run the rest
//     bare — the unwrapped suite this guard exists to prevent. "ls && GOFLAGS=
//     -p=6 make test" printed as one wrap ran 'ls' under the slot and 'make
//     test' outside it, and "export GT_TEST_DOCKER=1; go test ..." also asked
//     exec to run the shell builtin 'export'. Such a line goes to one shell
//     under the slot instead: sh -c '<command>' re-parses it whole, and
//     config.ShellQuote single-quotes it, so it reaches sh as one argument
//     with nothing inside expanded, split, or globbed that the original shell
//     would not have. Every shellCommandSeparator is a character ShellQuote
//     quotes for, so the quoting always fires on this path. sh is the same
//     interpreter the gate's own configured test command goes through
//     (mq_integration.go), not an extra dialect invented here.
//   - The '<rig>/<you>' placeholder is not runnable — '<' reads as a
//     redirection. The caller's own GT_ROLE is already the "rig/role" form the
//     --role flag documents, so it is printed when present and the placeholder
//     is kept only as the fallback for a hook that inherited no role.
func containerSuiteWrap(command string) string {
	command = strings.TrimSpace(command)
	role := containerSuiteWrapRole()
	if isCompoundShellCommand(command) {
		return fmt.Sprintf("gt slot run --role %s -- sh -c %s", role, config.ShellQuote(command))
	}
	// splitEnvPrefix is the same rule runSlotRun applies to its own argv
	// (gt-18nx), so the guard's advice and the runner's reading of it cannot
	// drift: whatever it peels is exactly what 'env' has to carry.
	if envAssigns, _ := splitEnvPrefix(shellTokenize(command)); len(envAssigns) > 0 {
		return fmt.Sprintf("gt slot run --role %s -- env %s", role, command)
	}
	return fmt.Sprintf("gt slot run --role %s -- %s", role, command)
}

// containerSuiteWrapRole is the holder name printed in the remediation's
// --role flag: GT_ROLE when it names a rig-scoped holder ("gastown/refinery",
// "gastown/polecats/zircon"), which is the form `gt slot run --role` documents
// and makes the printed line runnable verbatim. Without one the formula's
// placeholder is printed and the operator fills it in.
func containerSuiteWrapRole() string {
	role := strings.TrimSpace(os.Getenv("GT_ROLE"))
	if role == "" || !strings.Contains(role, "/") {
		return "<rig>/<you>"
	}
	// A role carrying whitespace or a quote would not survive the line as a
	// single word; refuse it rather than print a command that misreads.
	if strings.ContainsAny(role, " \t\n\"'`$") {
		return "<rig>/<you>"
	}
	return role
}

// isCompoundShellCommand reports whether command chains more than one command
// on a line, so the wrapper must be handed a shell rather than prefixed to it.
func isCompoundShellCommand(command string) bool {
	for _, tok := range shellTokenize(command) {
		if shellCommandSeparators[tok] {
			return true
		}
	}
	return false
}
