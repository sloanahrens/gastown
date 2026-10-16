package cmd

import (
	"fmt"
	"io"
	"regexp"
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
// A make goal that runs the whole suite (make test, gate, test-slow,
// test-integration) is refused on the same ground (gt-gl3jb).
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

// polecatWholeSuiteMakeGoals are the Makefile goals that start the whole-repo
// `go test` on the host: gate and its gate-test stage, the two slow tiers,
// and test, which runs them all. gate-lint, lint, build and presubmit are not
// here — they run no whole-suite test (presubmit tests the changed packages).
// gt done runs its own gate, so no polecat flow needs these (gt-gl3jb).
var polecatWholeSuiteMakeGoals = []string{"test", "gate", "gate-test", "test-slow", "test-integration"}

// findPolecatWholeSuiteMakeGoal returns the index of the first make invocation
// in tokens (already lowercased) that names a polecatWholeSuiteMakeGoals goal,
// and the goal, or -1.
func findPolecatWholeSuiteMakeGoal(tokens []string) (int, string) {
	found, goal := -1, ""
	for _, g := range polecatWholeSuiteMakeGoals {
		if i := findInvocation(tokens, "make", g); i >= 0 && (found < 0 || i < found) {
			found, goal = i, g
		}
	}
	return found, goal
}

// evaluatePolecatTestScopeSegment judges one shell segment in cwd, the
// directory the walk placed it in (cwdKnown says whether it could). The go
// case is located through findGoSubcommand, so a global -C flag between `go`
// and `test` does not hide the run (gt-zn7ma). A '.', './' or bare target is
// judged in the -C directory, where Go runs it (gt-tzdpe); an unfiltered
// package argument relative to a shell-expanded -C is refused because that
// directory cannot be placed (gt-tgwt7).
func evaluatePolecatTestScopeSegment(tokens []string, cwd string, cwdKnown bool) (reason string, matched []string) {
	lower := make([]string, len(tokens))
	for i, t := range tokens {
		lower[i] = strings.ToLower(t)
	}
	// The make goals in polecatWholeSuiteMakeGoals each run the whole suite
	// in a Go tree: test chains the other three, test-slow and
	// test-integration are its two stages, and gate (with gate-test, its
	// build and unit stage) is the 20-minute `go test ./...`. A gt slot run
	// wrapper or an env prefix does not change what any of them costs the
	// host. A rig outside every Go module has no such goals, so its `make
	// test` is left alone (gt-dieu9) — unless the directory cannot be placed
	// at all, which leaves that question open (gt-ofj05).
	if i, goal := findPolecatWholeSuiteMakeGoal(lower); i >= 0 {
		if !makeActsOnWholeGoModule(tokens[i:], makeTreeDir(cwd, cwdKnown)) {
			return "", nil
		}
		return fmt.Sprintf("polecat 'make %s' runs the whole suite", goal), nil
	}
	i := findGoSubcommand(lower, "test")
	if i < 0 {
		return "", nil
	}
	rest := tokens[i+1:]
	runCwd, runCwdKnown := goTestRunDir(tokens, lower, i, cwd, cwdKnown)
	hasRun := goTestRunFilterBounds(rest)
	pkgArgs := goTestPackageArgs(rest)
	if cwdTargetsUnplacedDir(pkgArgs, runCwdKnown) {
		return "polecat 'go test' names a directory the guard cannot place", nil
	}
	// A package argument spelled relative to a directory the walk could not
	// place ("cd \"$(git rev-parse --show-toplevel)/internal\" && go test
	// ./cmd") reads as a module-root package that is not heavy, though go runs
	// it in that directory. Like the expanded -C below, a -run filter is the
	// way through (gt-ff2l8).
	if !hasRun && unplacedDirRelativeArg(pkgArgs, runCwdKnown, heavyTestPackageList()) {
		return "polecat 'go test' runs in a directory the guard cannot place", nil
	}
	wholeRepo, heavy := polecatHeavyTarget(pkgArgs, runCwd)
	if wholeRepo {
		return "polecat 'go test' of the whole repo", nil
	}
	// An argument with a wildcard inside a path element ("./internal/c?d") or
	// one the shell rewrites ("$PWD/internal/cmd") names a set of packages the
	// hook cannot see into, so polecatHeavyTarget reads it as the literal text
	// and finds no heavy package. Without a -run filter the set is refused when
	// it can include one, the way the container rule refuses it for its own
	// list (gt-ff2l8).
	if !hasRun {
		for _, arg := range pkgArgs {
			if packageArgWildcard(arg) && packageWildcardMayReach(arg, runCwd, heavyTestPackageList()) {
				return fmt.Sprintf("polecat 'go test' package argument %q has a wildcard that can reach a whole heavy package with no -run filter", arg), nil
			}
		}
	}
	// A -C directory the shell expands is one goTestRunDir cannot place, so a
	// package argument spelled relative to it ("./cmd") is read as the
	// module-root package "cmd", which is not heavy. Without a -run filter
	// the run could be any package in any tree, so it is refused as the
	// container rule refuses it (gt-tgwt7). The refusal is blanket: a light
	// package ("./style") is refused as well as a heavy one, because the
	// directory it is relative to is the thing the guard cannot place. A
	// -run filter, a literal -C or a cd into the package is the way through
	// (gt-k4lsy).
	if !hasRun && goDirFlagShellExpanded(tokens, lower, i) && runDirTargetsUnknownDir(pkgArgs) {
		return "polecat 'go test' runs in a -C directory the shell expands, which the guard cannot place", nil
	}
	if len(heavy) > 0 && !hasRun {
		return "polecat 'go test' of a whole heavy package with no -run filter", heavy
	}
	return "", nil
}

// heavyTestPackageList is heavyTestPackages as a sorted list.
func heavyTestPackageList() []string {
	list := make([]string, 0, len(heavyTestPackages))
	for pkg := range heavyTestPackages {
		list = append(list, pkg)
	}
	sort.Strings(list)
	return list
}

// runFilterProbes are test names a -run pattern is tried against to tell a
// filter from one that selects everything: a pattern that matches each of them
// ("." ".*" "" "^" "(?i)") runs every test whatever the package holds.
var runFilterProbes = []string{"TestA", "ExampleB", "BenchmarkC", "FuzzD", "x"}

// goTestRunFilterBounds reports whether the tokens after "go test" carry a
// -run filter that bounds the run. A filter may come as -run before the
// packages, or as -test.run after "--" (passed straight to the test binary),
// in the "-run X" or "-run=X" form with one or two dashes. The last one wins,
// as it does in go. A pattern that selects every test ("-run .", "-run ''",
// "-run '.*'") bounds nothing, so it does not count: matching only the
// part before a "/" is go's rule for the first level of subtests (gt-ff2l8). A
// pattern go cannot compile fails the run before any test starts, and a
// -run with no value is go's own usage error, so neither leaves anything to
// run. The probes are a heuristic for "matches everything"; a pattern that
// happens to match all of them and not every real test name is refused, which
// costs one more -run edit.
func goTestRunFilterBounds(rest []string) bool {
	bounded := false
	for i, t := range rest {
		flag, value, hasValue := strings.Cut(strings.TrimLeft(t, "-"), "=")
		if !strings.HasPrefix(t, "-") || (flag != "run" && flag != "test.run") {
			continue
		}
		if !hasValue {
			if i+1 >= len(rest) {
				bounded = true
				continue
			}
			value = rest[i+1]
		}
		bounded = runPatternBoundsRun(value)
	}
	return bounded
}

// runPatternBoundsRun reports whether a -run pattern leaves some test names
// out (see goTestRunFilterBounds).
func runPatternBoundsRun(pattern string) bool {
	first, _, _ := strings.Cut(pattern, "/")
	re, err := regexp.Compile(first)
	if err != nil {
		return true
	}
	for _, probe := range runFilterProbes {
		if !re.MatchString(probe) {
			return true
		}
	}
	return false
}

// polecatHeavyTarget reports the heavy packages a go test package-argument
// list reaches, or true for the whole-repo wildcard. Arguments naming the
// cwd are resolved here, through cwdHeavyPackages, by the same rule as any
// other argument — one function judges every spelling, so no caller can
// resolve the argument form and miss the cwd form, and a polecat cannot dodge
// the rule by cd-ing into the package and running "go test ." (gt-1lko).
// cwd is the directory the invocation runs in, so an argument spelled as a
// path under it is judged there too (packageArgsUnderRunDir) and an absolute
// directory by the module-relative path it names (moduleRelativePackageArgs),
// the reading containerSuiteTarget gives the same arguments (gt-q82x1).
func polecatHeavyTarget(pkgArgs []string, cwd string) (wholeRepo bool, matched []string) {
	var heavy []string
	for _, p := range packageArgsUnderRunDir(moduleRelativePackageArgs(pkgArgs), cwd) {
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
	fmt.Fprintln(w, "  Whole-package runs of internal/cmd, daemon or polecat take 3-10 minutes")
	fmt.Fprintln(w, "  each and there is no need for one: the landing worker runs the full suite once per")
	fmt.Fprintln(w, "  submission (gt done covers the light changed packages). Iterate on what you touched:")
	fmt.Fprintln(w, "    go test ./internal/<pkg>/ -run 'TestOne|TestTwo'")
	fmt.Fprintln(w, "  (gt-pxlg)")
}
