package cmd

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/style"
)

// heavyTestPackages are the packages whose full test run costs minutes on
// this host (measured 2026-09-18, alone: internal/cmd 183s, internal/polecat
// 175s, internal/daemon 385s; under contention 500-700s). A polecat has no
// reason to run one whole: the refinery runs the full suite once per
// submission (and `gt done` tests the light changed packages). Five polecats doing it anyway
// put ~81 of 185 agent-minutes into the test loop in one afternoon
// (gt-pxlg), and the formula text asking them not to was not enough.
var heavyTestPackages = map[string]bool{
	"internal/cmd":      true,
	"internal/daemon":   true,
	"internal/polecat":  true,
	"internal/refinery": true,
}

// isPolecatContext reports whether the current process runs as a polecat —
// the only role this rule applies to. The refinery MUST run whole packages
// (that is its job) and crew are operators.
func isPolecatContext() bool {
	if os.Getenv("GT_POLECAT") != "" {
		return true
	}
	// GT_ROLE is the signal every spawn carries ("gastown/polecats/topaz");
	// GT_POLECAT is set by AgentEnv alongside it but a hook that inherited
	// only the role must still recognize the polecat.
	if role := os.Getenv("GT_ROLE"); strings.Contains(role, "/polecats/") || role == "polecat" {
		return true
	}
	cwd, err := os.Getwd()
	return err == nil && strings.Contains(cwd, "/polecats/")
}

// evaluatePolecatTestScope blocks, per shell segment, a `go test` that (a)
// targets the whole repo, or (b) names a heavy package with no -run filter.
// Wrapping in gt slot run does not exempt it: the slot protects Docker, not
// the host's CPU. A -run filter on any number of packages is allowed — the
// cost of a filtered run is the compile, seconds not minutes.
func evaluatePolecatTestScope(command string) (reason string, matched []string) {
	tokens := shellTokenize(strings.TrimSpace(command))
	var segment []string
	for _, tok := range tokens {
		if shellCommandSeparators[tok] {
			if r, m := evaluatePolecatTestScopeSegment(segment); r != "" {
				return r, m
			}
			segment = nil
			continue
		}
		segment = append(segment, tok)
	}
	return evaluatePolecatTestScopeSegment(segment)
}

func evaluatePolecatTestScopeSegment(tokens []string) (reason string, matched []string) {
	lower := make([]string, len(tokens))
	for i, t := range tokens {
		lower[i] = strings.ToLower(t)
	}
	// `make test` is `go test ./...` under another name (see the Makefile
	// target); a gt slot run wrapper or an env prefix does not change what
	// it costs the host.
	if findTestInvocation(lower, "make") >= 0 {
		return "polecat 'make test' runs the whole suite", nil
	}
	i := findTestInvocation(lower, "go")
	if i < 0 {
		return "", nil
	}
	rest := tokens[i+2:]
	// A filter may come as -run before the packages, or as -test.run after
	// "--" (passed straight to the test binary); either bounds the run.
	hasRun := false
	for _, t := range rest {
		if t == "-run" || strings.HasPrefix(t, "-run=") || t == "-test.run" || strings.HasPrefix(t, "-test.run=") {
			hasRun = true
		}
	}
	pkgArgs := goTestPackageArgs(rest)
	cwd, _ := os.Getwd()
	wholeRepo, heavy := polecatHeavyTarget(pkgArgs, cwd)
	if wholeRepo {
		return "polecat 'go test' of the whole repo", nil
	}
	if len(heavy) > 0 && !hasRun {
		return "polecat 'go test' of a whole heavy package with no -run filter", heavy
	}
	return "", nil
}

// polecatHeavyTarget reports the heavy packages a go test package-argument
// list reaches, or true for the whole-repo wildcard. Arguments naming the
// cwd are resolved here, through cwdHeavyPackages, by the same rule as any
// other argument — one function judges every spelling, so no caller can
// resolve the argument form and miss the cwd form, and a polecat cannot dodge
// the rule by cd-ing into the package and running "go test ." (gt-1lko).
func polecatHeavyTarget(pkgArgs []string, cwd string) (wholeRepo bool, matched []string) {
	var heavy []string
	for _, p := range normalizedPackageArgs(pkgArgs) {
		if isWholeRepoPackageArg(p) {
			return true, nil
		}
		if p == cwdPackageArg {
			heavy = append(heavy, cwdHeavyPackages(cwd)...)
			continue
		}
		heavy = append(heavy, heavyPackagesCoveredBy(p)...)
	}
	return false, dedupeSorted(heavy)
}

// heavyPackagesCoveredBy returns the heavy packages a normalized package
// argument reaches, by the same rule the container guard uses
// (packageArgCovers): the package itself ("internal/cmd"), a subpackage of
// one ("internal/cmd/sub"), or a directory containing one ("internal", which
// is what "./internal/..." normalizes to). The list is fixed, so this is
// every match rather than a sample of them.
func heavyPackagesCoveredBy(arg string) []string {
	var heavy []string
	for hp := range heavyTestPackages {
		if packageArgCovers(arg, hp) {
			heavy = append(heavy, hp)
		}
	}
	sort.Strings(heavy)
	return heavy
}

// cwdHeavyPackages returns the heavy packages the invocation's cwd reaches —
// the cwd form of heavyPackagesCoveredBy, so a polecat cannot dodge the rule
// by cd-ing into the package and running "go test ." (gt-1lko). It walks up
// to the enclosing go.mod rather than matching the checkout's directory name,
// so it resolves from the refinery worktree (…/gastown/refinery/rig) as well
// as from a polecat's.
func cwdHeavyPackages(cwd string) []string {
	pkg, ok := cwdPackagePath(cwd)
	if !ok || pkg == "" {
		return nil
	}
	return heavyPackagesCoveredBy(pkg)
}

// dedupeSorted returns the distinct packages in sorted order, so the block
// message names each package once whatever order the arguments came in.
func dedupeSorted(pkgs []string) []string {
	sort.Strings(pkgs)
	out := make([]string, 0, len(pkgs))
	for i, p := range pkgs {
		if i == 0 || p != pkgs[i-1] {
			out = append(out, p)
		}
	}
	return out
}

func printPolecatTestScopeBlock(reason, command string, matched []string) {
	fmt.Fprintln(os.Stderr, style.Bold.Render("❌ TEST SCOPE (polecat)"))
	fmt.Fprintf(os.Stderr, "  %s\n", reason)
	if len(matched) > 0 {
		fmt.Fprintf(os.Stderr, "  packages: %s\n", strings.Join(matched, " "))
	}
	fmt.Fprintf(os.Stderr, "  command:  %s\n", command)
	fmt.Fprintln(os.Stderr, "  Whole-package runs of internal/cmd, daemon, polecat or refinery take 3-10 minutes")
	fmt.Fprintln(os.Stderr, "  each and there is no need for one: the refinery runs the full suite once per")
	fmt.Fprintln(os.Stderr, "  submission (gt done covers the light changed packages). Iterate on what you touched:")
	fmt.Fprintln(os.Stderr, "    go test ./internal/<pkg>/ -run 'TestOne|TestTwo'")
	fmt.Fprintln(os.Stderr, "  (gt-pxlg)")
}
