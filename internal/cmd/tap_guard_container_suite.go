package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
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
// spin real Dolt/testcontainers containers, derived (2026-09-11, gt-e2rs) by
// grepping the tree for call sites of testutil.RequireDoltContainer,
// testutil.WithDolt, and testutil.StartIsolatedDoltContainer — the three
// entry points into internal/testutil's container-spinning code
// (internal/testutil/doltserver.go). Keep in sync with actual usage; a
// package added here that doesn't touch containers only costs an
// unnecessary 'gt slot run' wrap, but a real container-spinning package
// missing from this list is invisible to the guard entirely.
var containerSuitePackages = []string{
	"internal/beads",
	"internal/cmd",
	"internal/convoy",
	"internal/daemon",
	"internal/doltserver",
	"internal/mail",
	"internal/polecat",
	"internal/refinery",
	"internal/testutil",
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

	var segment []string
	for _, tok := range tokens {
		if shellCommandSeparators[tok] {
			if r, m := evaluateContainerSuiteSegment(segment); r != "" {
				return r, m
			}
			segment = nil
			continue
		}
		segment = append(segment, tok)
	}
	return evaluateContainerSuiteSegment(segment)
}

// evaluateContainerSuiteSegment judges a single shell segment (tokens
// between shell operators). tokens is original-case; matching is done on a
// lowercased copy so "Go Test" and "go test" are treated the same.
func evaluateContainerSuiteSegment(tokens []string) (reason string, matched []string) {
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

	if i := findAdjacentPair(lower, "go", "test"); i >= 0 {
		pkgArgs := goTestPackageArgs(tokens[i+2:])
		wholeRepo, pkgs := containerSuitePackagesIntersect(pkgArgs)
		if wholeRepo {
			return "bare 'go test' with a whole-repo target touches every testcontainers-backed package", nil
		}
		if len(pkgs) > 0 {
			return "bare 'go test' targets testcontainers-backed package(s)", pkgs
		}
		return "", nil
	}

	if i := findAdjacentPair(lower, "make", "test"); i >= 0 {
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

// findAdjacentPair returns the index of the first occurrence of a
// immediately followed by b in tokens (already lowercased), or -1.
func findAdjacentPair(tokens []string, a, b string) int {
	for i := 0; i+1 < len(tokens); i++ {
		if tokens[i] == a && tokens[i+1] == b {
			return i
		}
	}
	return -1
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

// normalizeGoPackageArg strips the module-path prefix, leading "./", and
// trailing "/..." / "/" from a go test package argument, so
// "./internal/beads/...", "internal/beads/...", and
// "github.com/steveyegge/gastown/internal/beads/..." all normalize to
// "internal/beads". A bare "./...", "...", or "." normalizes to "".
func normalizeGoPackageArg(arg string) string {
	p := arg
	p = strings.TrimPrefix(p, "github.com/steveyegge/gastown/")
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimSuffix(p, "/...")
	p = strings.TrimSuffix(p, "/")
	if p == "..." || p == "." {
		p = ""
	}
	return p
}

// containerSuitePackagesIntersect reports whether pkgArgs (go test package
// arguments) cover the whole repo, or which entries of containerSuitePackages
// they overlap with. An arg overlaps a listed package if it names that
// package exactly, names an ancestor directory of it (e.g. "internal/..."
// covers "internal/beads"), or names a path inside it. A bare "go test"
// with no package arguments is NOT treated as whole-repo — go resolves that
// to only the current directory's package, which this guard cannot verify
// without also knowing the invocation's cwd; failing to block here is safer
// than guessing (see tap_guard_dangerous.go's false-positive history).
func containerSuitePackagesIntersect(pkgArgs []string) (wholeRepo bool, matched []string) {
	if len(pkgArgs) == 0 {
		return false, nil
	}
	seen := map[string]bool{}
	for _, arg := range pkgArgs {
		p := normalizeGoPackageArg(arg)
		if p == "" {
			return true, nil
		}
		for _, pkg := range containerSuitePackages {
			if p == pkg || strings.HasPrefix(pkg, p+"/") || strings.HasPrefix(p, pkg+"/") {
				if !seen[pkg] {
					seen[pkg] = true
					matched = append(matched, pkg)
				}
			}
		}
	}
	return false, matched
}

// printContainerSuiteBlock prints the standard block banner to stderr,
// naming the wrapped form the guard wants instead (gt-e2rs: "message names
// the wrapped form").
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
	fmt.Fprintf(os.Stderr, "  Run it wrapped instead: gt slot run --role <rig>/<you> -- %s\n", originalCommand)
	fmt.Fprintln(os.Stderr, "")
}
