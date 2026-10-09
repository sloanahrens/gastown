package cmd

import (
	"fmt"
	"io"
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
	"internal/cmd":     true,
	"internal/daemon":  true,
	"internal/polecat": true,
}

// isPolecatContext reports whether the current process runs as a polecat —
// the only role this rule applies to. The refinery MUST run whole packages
// (that is its job) and crew are operators.
func isPolecatContext(proc guardProcess) bool {
	// GT_ROLE is the signal every spawn carries ("gastown/polecats/topaz") and
	// it decides first, as it does for the rest of the guard family: a
	// refinery or crew session whose environment still carries a GT_POLECAT
	// from the polecat that started it is not a polecat (gt-pb77k).
	if role := strings.TrimSpace(proc.getenv("GT_ROLE")); role != "" {
		return strings.Contains(role, "/polecats/") || role == "polecat" || isPolecatRole(role)
	}
	if proc.getenv("GT_POLECAT") != "" {
		return true
	}
	cwd, err := proc.getwd()
	return err == nil && strings.Contains(cwd, "/polecats/")
}

// evaluatePolecatTestScope blocks, per shell segment, a `go test` that (a)
// targets the whole repo, or (b) names a heavy package with no -run filter.
// Wrapping in gt slot run does not exempt it: the slot protects Docker, not
// the host's CPU. A -run filter on any number of packages is allowed — the
// cost of a filtered run is the compile, seconds not minutes.
//
// Segment directories come from segmentWalkRoot, so cd-ing into a heavy
// package and running it there is judged in that package (gt-5mc21, gt-n7ksl).
// A directory the walk cannot place is refused rather than read as a light one,
// the reading an unplaceable make -C already gets (gt-ofj05).
//
// Heredoc bodies are stripped before tokenizing, so a body that merely spells
// "make test" — a bead description, a doc, a formula — is data, not a live
// invocation. A body fed to a shell invoker is the exception and is judged as
// the nested script it is (gt-ohe8n), in the directory the shell had reached
// when it read the body (heredocBodyDir, gt-1cvqj, gt-v02wh).
func evaluatePolecatTestScope(proc guardProcess, command string) (reason string, matched []string) {
	cwd, _ := proc.getwd()
	return evaluatePolecatTestScopeDepth(proc, command, cwd, 0)
}

// evaluatePolecatTestScopeDepth judges command as a shell starting in cwd —
// the invocation's directory, or the directory a shell-fed heredoc's body
// runs in, one level down.
func evaluatePolecatTestScopeDepth(proc guardProcess, command, cwd string, depth int) (reason string, matched []string) {
	tokens := shellTokenize(strings.TrimSpace(stripHeredocBodies(command)))
	vars := shellVarAssignments(tokens)
	var segment []string
	start := 0
	for i, tok := range tokens {
		if shellCommandSeparators[tok] {
			dir, known := segmentWalkRoot(proc, tokens, start, cwd, vars)
			if r, m := evaluatePolecatTestScopeSegment(segment, dir, known); r != "" {
				return r, m
			}
			segment = nil
			start = i + 1
			continue
		}
		segment = append(segment, tok)
	}
	dir, known := segmentWalkRoot(proc, tokens, start, cwd, vars)
	if r, m := evaluatePolecatTestScopeSegment(segment, dir, known); r != "" {
		return r, m
	}

	if depth >= maxTestGuardNestDepth {
		return "", nil
	}
	for _, span := range shellFedHeredocSpans(command) {
		bodyCwd := heredocBodyDir(proc, command, span, cwd)
		if r, m := evaluatePolecatTestScopeDepth(proc, span.body, bodyCwd, depth+1); r != "" {
			return r, m
		}
	}
	return "", nil
}

func evaluatePolecatTestScopeSegment(tokens []string, cwd string, cwdKnown bool) (reason string, matched []string) {
	lower := make([]string, len(tokens))
	for i, t := range tokens {
		lower[i] = strings.ToLower(t)
	}
	// `make test` is `go test ./...` under another name (see the Makefile
	// target) in a Go tree, and a gt slot run wrapper or an env prefix does
	// not change what it costs the host. A rig outside every Go module has no
	// such target, so its `make test` is left alone (gt-dieu9) — unless the
	// directory cannot be placed at all, which leaves that question open
	// (gt-ofj05).
	if i := findTestInvocation(lower, "make"); i >= 0 {
		if !makeActsOnWholeGoModule(tokens[i:], makeTreeDir(cwd, cwdKnown)) {
			return "", nil
		}
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
	if cwdTargetsUnplacedDir(pkgArgs, cwdKnown) {
		return "polecat 'go test' names a directory the guard cannot place", nil
	}
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

func printPolecatTestScopeBlock(w io.Writer, reason, command string, matched []string) {
	fmt.Fprintln(w, style.Bold.Render("❌ TEST SCOPE (polecat)"))
	fmt.Fprintf(w, "  %s\n", reason)
	if len(matched) > 0 {
		fmt.Fprintf(w, "  packages: %s\n", strings.Join(matched, " "))
	}
	fmt.Fprintf(w, "  command:  %s\n", command)
	fmt.Fprintln(w, "  Whole-package runs of internal/cmd, daemon, polecat or refinery take 3-10 minutes")
	fmt.Fprintln(w, "  each and there is no need for one: the refinery runs the full suite once per")
	fmt.Fprintln(w, "  submission (gt done covers the light changed packages). Iterate on what you touched:")
	fmt.Fprintln(w, "    go test ./internal/<pkg>/ -run 'TestOne|TestTwo'")
	fmt.Fprintln(w, "  (gt-pxlg)")
}
