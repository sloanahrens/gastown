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
// A command substitution ($(...) or backticks) and an eval string are command
// lines of their own, judged like a -c payload: "out=$(go test ./internal/cmd)"
// and `eval "make gate"` run the suite though no segment word is a go binary
// (gt-t1pct, substitutionScripts, evalPayloads).
//
// Heredoc bodies are stripped before tokenizing, so a body that merely spells
// "make test" — a bead description, a doc, a formula — is data, not a live
// invocation. A body fed to a shell invoker is the exception and is judged as
// the nested script it is (gt-ohe8n), in the directory the shell had reached
// when it read the body (heredocBodyCwd, gt-1cvqj, gt-v02wh); a directory the
// walk cannot place is carried to the body as unplaced, as a -c payload's is
// (gt-jj3or). The shell still expands a $(...) or backtick written in an
// unquoted body, so those substitutions are judged too, in that same directory;
// a quoted tag keeps its body literal (gt-6fkng, heredocSubstitutionScripts).
func evaluatePolecatTestScope(proc guardProcess, command string) (reason string, matched []string) {
	cwd, _ := proc.getwd()
	return evaluatePolecatTestScopeDepth(proc, command, cwd, true, 0)
}

// evaluatePolecatTestScopeDepth judges command as a shell starting in cwd —
// the invocation's directory, or the directory a shell-fed heredoc's body
// runs in, one level down. cwdKnown is false for a -c payload whose segment
// directory the walk could not place: cwd then only stands in for that
// directory, and nothing in the payload is judged against it (gt-rws47).
func evaluatePolecatTestScopeDepth(proc guardProcess, command, cwd string, cwdKnown bool, depth int) (reason string, matched []string) {
	tokens := shellTokenize(strings.TrimSpace(stripHeredocBodies(command)))
	vars := shellVarAssignments(tokens)
	// judge covers one segment and the -c payloads of the shells it starts. A
	// payload runs in the segment's directory. When the walk cannot place it
	// the payload is judged as unplaced, so one that runs the suite is
	// refused, rather than read against the base, which is no evidence of
	// where the shell is (gt-y1zks, gt-ofj05, gt-rws47).
	judge := func(segment []string, start int) (string, []string) {
		segment = commandSegment(segment)
		dir, known := segmentWalkRoot(proc, tokens, start, cwd, vars)
		known = known && cwdKnown
		if r, m := evaluatePolecatTestScopeSegment(segment, dir, known); r != "" {
			return r, m
		}
		payloads := segmentPayloads(tokens, start, segment)
		if depth >= maxTestGuardNestDepth {
			return nestedTooDeepRefusal(len(payloads) > 0)
		}
		if !known {
			dir = cwd
		}
		for _, payload := range payloads {
			if r, m := evaluatePolecatTestScopeDepth(proc, payload, dir, known, depth+1); r != "" {
				return r, m
			}
		}
		return "", nil
	}

	var segment []string
	start := 0
	for i, tok := range tokens {
		if shellCommandSeparators[tok] {
			if r, m := judge(segment, start); r != "" {
				return r, m
			}
			segment = nil
			start = i + 1
			continue
		}
		segment = append(segment, tok)
	}
	if r, m := judge(segment, start); r != "" {
		return r, m
	}

	spans := shellFedHeredocSpans(command)
	subs := substitutionScripts(proc, command, cwd, cwdKnown)
	if depth >= maxTestGuardNestDepth {
		return nestedTooDeepRefusal(len(spans) > 0 || len(subs) > 0)
	}
	for _, sub := range subs {
		if r, m := evaluatePolecatTestScopeDepth(proc, sub.body, sub.dir, sub.known, depth+1); r != "" {
			return r, m
		}
	}
	for _, span := range spans {
		bodyCwd, placed := heredocBodyCwd(proc, command, span, cwd)
		known := placed && cwdKnown
		if !known {
			bodyCwd = cwd
		}
		if r, m := evaluatePolecatTestScopeDepth(proc, span.body, bodyCwd, known, depth+1); r != "" {
			return r, m
		}
	}
	return "", nil
}

// polecatWholeSuiteMakeGoals are the Makefile goals that start the whole-repo
// `go test` on the host: gate and its gate-test stage, the slow tiers,
// and test, which runs them all. test-integration-wall runs `go test -tags
// integration ./...`, tier-check is the budget runner over ./..., and
// test-timing defaults to ./... too (a PKGS assignment narrows it,
// makeTestTimingPKGSBounded). gate-lint, lint, build and presubmit are
// not here — they run no whole-suite test (presubmit tests the changed
// packages). gt done runs its own gate, so no polecat flow needs these
// (gt-gl3jb, gt-9udxk).
var polecatWholeSuiteMakeGoals = []string{"test", "gate", "gate-test", "test-slow", "test-integration", "test-integration-wall", "tier-check", "test-timing"}

// testTimingMakeGoal is the one whole-suite goal that PKGS narrows: its recipe
// is `go run ./internal/testpolicy/cmd/testtiming -- $(PKGS)`, and the program
// runs ./... only when PKGS is empty.
const testTimingMakeGoal = "test-timing"

// makeTestTimingPKGSBounded reports whether a `make test-timing` invocation
// carries a PKGS=<packages> assignment that bounds the run. args holds the
// invocation's tokens from the "make" word onward and dir is the directory the
// guard placed the segment in ("" when it could not).
//
// It is true only when the last PKGS=<packages> assignment names packages the
// guard can read and none is the whole repo, a heavy package, or a pattern that
// can reach one. Anything else, including PKGS:= and PKGS?=, stays unbounded
// (fail closed). Several packages need the quoted PKGS="a b" spelling; an
// unquoted second word is another make goal, not a package.
func makeTestTimingPKGSBounded(args []string, dir string) bool {
	runDir, known := makeRunDir(args, dir)
	if !known || strings.TrimSpace(runDir) == "" {
		return false
	}
	var words []string
	for _, tok := range args[1:] {
		if value, ok := strings.CutPrefix(tok, "PKGS="); ok {
			words = strings.Fields(value) // the last assignment wins, as in make
		}
	}
	if len(words) == 0 {
		return false
	}
	// The recipe's unquoted $(PKGS) is word-split, so each word is judged as a
	// go test package argument with no -run filter.
	for _, w := range words {
		if strings.HasPrefix(w, "-") { // a go flag the guard cannot read
			return false
		}
		if packageArgWildcard(w) && packageWildcardMayReach(w, runDir, heavyTestPackageList()) {
			return false
		}
	}
	wholeRepo, heavy := polecatHeavyTarget(words, runDir)
	return !wholeRepo && len(heavy) == 0
}

// findPolecatWholeSuiteMakeGoal returns the index of the first make invocation
// in tokens (already lowercased) that names a polecatWholeSuiteMakeGoals goal,
// and the goal, or -1. A goal PKGS cannot narrow wins over test-timing, which
// the caller may exempt, wherever either sits in tokens or in the goal list:
// "make test-timing gate PKGS=./x" reports gate and stays refused.
func findPolecatWholeSuiteMakeGoal(tokens []string) (int, string) {
	found, goal := -1, ""
	for _, g := range polecatWholeSuiteMakeGoals {
		if g == testTimingMakeGoal {
			continue
		}
		if i := findInvocation(tokens, "make", g); i >= 0 && (found < 0 || i < found) {
			found, goal = i, g
		}
	}
	if found >= 0 {
		return found, goal
	}
	if i := findInvocation(tokens, "make", testTimingMakeGoal); i >= 0 {
		return i, testTimingMakeGoal
	}
	return -1, ""
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
	// Every goal in polecatWholeSuiteMakeGoals starts the whole-repo `go test`
	// in a Go tree. A gt slot run wrapper or an env prefix does not change
	// what that costs the host. A rig outside every Go module has no such
	// goals, so its `make test` is left alone (gt-dieu9) — unless the
	// directory cannot be placed at all, which leaves that question open
	// (gt-ofj05).
	if i, goal := findPolecatWholeSuiteMakeGoal(lower); i >= 0 {
		treeDir := makeTreeDir(cwd, cwdKnown)
		if !makeActsOnWholeGoModule(tokens[i:], treeDir) {
			return "", nil
		}
		if goal == testTimingMakeGoal && makeTestTimingPKGSBounded(tokens[i:], treeDir) {
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

// runLeadingLetterProbes are the TestXxx names TestA..TestZ, one per leading
// letter. A pattern that matches more than half of them ("Test" "^Test"
// "^Test.*" "T" "^Test[^Z]") runs most test functions whatever the package
// holds, though it misses the Example and Benchmark probes above. A pattern
// naming a few tests or a prefix of some ("TestSpecific" "^TestApply"
// "TestFoo|TestQuux|TestZeta") matches only the few its letters pick out.
var runLeadingLetterProbes = leadingLetterProbes()

func leadingLetterProbes() []string {
	probes := make([]string, 0, 26)
	for c := 'A'; c <= 'Z'; c++ {
		probes = append(probes, "Test"+string(c))
	}
	return probes
}

// runLongNameTails are the endings runLongNameProbeSets put after "Test" and a
// leading letter. Every name above has one character after "Test", so a
// pattern that needs more ("Test.." "Test.{2,}") matches none of them and would
// be judged bounded while it selects every real test, whose names run longer
// (gt-uh7hy). The tails differ in length and in shape so a pattern that needs a
// long name ("Test.{30,}") or a mixed-case, underscored or numbered one
// ("Test[A-Za-z]+_") is read against a name that fits it (gt-ylkm2).
//
// The probes stay a heuristic. A pattern that needs more than the longest tail,
// or a shape none of them has, still passes as bounded, so the guard cannot
// rule out a pattern that counts or names more than this.
var runLongNameTails = []string{
	"bcdefghijklmnopqrstuvwxyz",
	"oo_Bar2Baz",
	strings.Repeat("longTestName", 6),
}

// runLongNameProbeSets holds one TestXxx set per tail, A to Z each. A pattern
// that matches most of any one set is judged to select most tests.
var runLongNameProbeSets = longNameProbeSets()

func longNameProbeSets() [][]string {
	sets := make([][]string, 0, len(runLongNameTails))
	for _, tail := range runLongNameTails {
		probes := make([]string, 0, 26)
		for c := 'A'; c <= 'Z'; c++ {
			probes = append(probes, "Test"+string(c)+tail)
		}
		sets = append(sets, probes)
	}
	return sets
}

// matchesMostLongNameProbes reports whether re matches most of any one set of
// long-name probes.
func matchesMostLongNameProbes(re *regexp.Regexp) bool {
	for _, probes := range runLongNameProbeSets {
		if matchesMostProbes(re, probes) {
			return true
		}
	}
	return false
}

// goTestRunFilterBounds reports whether the tokens after "go test" carry a
// -run (or, after "--", -test.run) filter that bounds the run; the last one
// wins, as in go. A pattern that selects every test bounds nothing. A -run with
// no value is go's own usage error and leaves nothing to run. A value the shell
// expands, or one the guard cannot compile, is a pattern the hook cannot read,
// so it bounds nothing (gt-ff2l8). The probes are a heuristic: a pattern that
// matches most of them but not every real test name is refused, which costs one
// more -run edit.
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
		bounded = !runValueShellExpanded(value) && runPatternBoundsRun(value)
	}
	return bounded
}

// runValueShellExpanded reports whether the shell rewrites a -run value before
// go sees it: a backtick, a "$" that opens a variable, a special parameter
// ("$@" and "$!" can expand to nothing and leave "-run=", which matches every
// test) or a substitution, or a brace expansion ("-run={x,.}" becomes "-run=x
// -run=.", and the last one wins). A "$" that closes a pattern ("^TestX$",
// "TestA$|TestB") is the regexp anchor.
//
// The value arrives with its quotes already removed, so a quoted "{1,}" that
// is a regexp repeat reads as a brace expansion and is refused with it. A
// "{n,m}" with both bounds numeric is let through quoted or not: expanded, its
// last word is the pattern with m written in, which still names a test.
func runValueShellExpanded(value string) bool {
	if strings.Contains(value, "`") || braceExpansion(value) {
		return true
	}
	for i := 0; i+1 < len(value); i++ {
		if value[i] != '$' {
			continue
		}
		c := value[i+1]
		if c == '{' || c == '(' || c == '_' || strings.IndexByte("@!*$#?-", c) >= 0 || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			return true
		}
	}
	return false
}

// braceExpansion reports whether value holds a "{" … "}" pair that carries a
// comma or a ".." between the braces, the two forms bash expands, other than a
// numeric regexp repeat "{n,m}".
func braceExpansion(value string) bool {
	for start := strings.IndexByte(value, '{'); start >= 0; {
		end := strings.IndexByte(value[start:], '}')
		if end < 0 {
			return false
		}
		body := value[start+1 : start+end]
		if (strings.Contains(body, ",") || strings.Contains(body, "..")) && !numericRepeatBody(body) {
			return true
		}
		next := strings.IndexByte(value[start+1:], '{')
		if next < 0 {
			return false
		}
		start += 1 + next
	}
	return false
}

// numericRepeatBody reports whether the text between braces is "n,m" with both
// bounds decimal digits: a regexp repeat, and a brace expansion whose words all
// carry a digit.
func numericRepeatBody(body string) bool {
	lo, hi, ok := strings.Cut(body, ",")
	return ok && isAllDigits(lo) && isAllDigits(hi)
}

// runPatternBoundsRun reports whether a -run pattern leaves some test names
// out (see goTestRunFilterBounds). go matches only the first "/" element of
// the pattern against the test name.
func runPatternBoundsRun(pattern string) bool {
	re, err := regexp.Compile(firstSubtestElement(pattern))
	if err != nil {
		// go rejects the pattern before any test starts, but the guard's
		// split may differ from go's, so an unreadable pattern bounds nothing.
		return false
	}
	return !matchesAllProbes(re, runFilterProbes) &&
		!matchesMostProbes(re, runLeadingLetterProbes) &&
		!matchesMostLongNameProbes(re)
}

// firstSubtestElement returns the part of a -run pattern that selects a test
// by its own name, as testing splits it (splitRegexp): the pattern is cut at
// each "|" outside a bracket class or parentheses into alternatives, a test
// runs when any alternative matches, and an alternative is cut at its first "/"
// outside a class or parentheses into the elements matched against the test
// name and then its subtests. The "/" in "[^/]*" or "(a/b)" separates nothing.
//
// A pattern with one alternative returns its first element. With several, the
// result is their first elements as one pattern, each in its own group so a
// flag in one ("(?i)") does not reach the next: "TestX/a|." selects every test
// through its second alternative whatever its first one names.
func firstSubtestElement(pattern string) string {
	var elements []string
	classes, groups := 0, 0
	altStart, elemEnd := 0, -1
	closeAlternative := func(end int) {
		if elemEnd < 0 {
			elemEnd = end
		}
		elements = append(elements, pattern[altStart:elemEnd])
		altStart, elemEnd = end+1, -1
	}
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '[':
			classes++
		case ']':
			if classes > 0 {
				classes--
			}
		case '(':
			if classes == 0 {
				groups++
			}
		case ')':
			if classes == 0 {
				groups--
			}
		case '\\':
			i++
		case '/':
			if classes == 0 && groups == 0 && elemEnd < 0 {
				elemEnd = i
			}
		case '|':
			if classes == 0 && groups == 0 {
				closeAlternative(i)
			}
		}
	}
	closeAlternative(len(pattern))
	if len(elements) == 1 {
		return elements[0]
	}
	for i, element := range elements {
		elements[i] = "(?:" + element + ")"
	}
	return strings.Join(elements, "|")
}

// matchesMostProbes reports whether re matches more than half of probes.
func matchesMostProbes(re *regexp.Regexp, probes []string) bool {
	n := 0
	for _, probe := range probes {
		if re.MatchString(probe) {
			n++
		}
	}
	return 2*n > len(probes)
}

// matchesAllProbes reports whether re matches every name in probes.
func matchesAllProbes(re *regexp.Regexp, probes []string) bool {
	for _, probe := range probes {
		if !re.MatchString(probe) {
			return false
		}
	}
	return true
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
