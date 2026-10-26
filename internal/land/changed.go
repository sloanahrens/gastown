package land

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// NameStatusArgs is the `git diff` argument list every name-status reader
// runs: -z, so git writes each path as it is and never octal-quotes a
// non-ASCII name (gt-fz8xk). The revs go after it.
func NameStatusArgs(revs ...string) []string {
	return append([]string{"diff", "--name-status", "-z", "-M"}, revs...)
}

// ChangedPackages maps `git diff --name-status -z -M` output to the Go package
// directories whose tests the change can affect, sorted and relative to the
// repo root ("internal/land", never "./internal/land"). It returns an error
// for output that is not NUL-separated, rather than selecting nothing.
//
// A .go file names its own directory, unless it sits in one the go tool skips
// (testdata, or a name beginning with "." or "_"): a .go file there is a
// fixture, not a package member, and names the owning package like any other
// fixture (gt-f1ynu). A file of any other kind (an embedded template, a
// fixture) names the nearest ancestor directory that holds Go files, because
// that package is the one that reads it; a file with no such ancestor, such as
// docs/x.md, names nothing. Both sides of a rename and a deleted file count,
// so a package that lost a file is still tested. A directory with no Go files
// left is dropped: `go test` of a deleted package is an error, not a pass.
//
// The guards that judge paths outside their own package are GuardSelects,
// not this: testing the package a guard file lives in would run its whole
// suite, minutes of it in internal/cmd and internal/polecat (gt-ydzwb).
//
// hasGo reports whether a repo-relative directory holds a non-test or test
// .go file; tests replace it, PackageDirHasGo is the real one.
func ChangedPackages(nameStatus string, hasGo func(dir string) bool) ([]string, error) {
	seen := map[string]bool{}
	err := eachChange(nameStatus, func(_, now, gone []string) {
		for _, files := range [][]string{now, gone} {
			for _, file := range files {
				if dir := owningPackage(file, hasGo); dir != "" {
					seen[dir] = true
				}
			}
		}
	})
	if err != nil {
		return nil, err
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs, nil
}

// GuardSelect names the tree-wide guard tests one package contributes to
// presubmit: the package directory and the test functions to run there.
type GuardSelect struct {
	Package string
	Tests   []string
}

// GuardSelects returns the tree-wide guard tests whose inputs nameStatus
// changed, sorted by package. A branch that changes no guard's input returns
// nil.
func GuardSelects(nameStatus string, hasGo func(dir string) bool) ([]GuardSelect, error) {
	triggered := map[string]bool{}
	err := eachChange(nameStatus, func(_, now, gone []string) {
		for _, g := range treeWideGuards {
			if triggered[g.pkg] || !hasGo(g.pkg) {
				continue
			}
			if anyJudged(g, now) || (g.removal && anyJudged(g, gone)) {
				triggered[g.pkg] = true
			}
		}
	})
	if err != nil {
		return nil, err
	}
	tests := map[string][]string{}
	for _, g := range treeWideGuards {
		tests[g.pkg] = append(tests[g.pkg], g.tests...)
	}
	if len(triggered) == 0 {
		return nil, nil
	}
	sel := make([]GuardSelect, 0, len(triggered))
	for pkg := range triggered {
		names := tests[pkg]
		sort.Strings(names)
		sel = append(sel, GuardSelect{Package: pkg, Tests: names})
	}
	sort.Slice(sel, func(i, j int) bool { return sel[i].Package < sel[j].Package })
	return sel, nil
}

func anyJudged(g treeWideGuard, files []string) bool {
	for _, file := range files {
		if g.judges(file) {
			return true
		}
	}
	return false
}

// ChangedPaths returns every path `git diff --name-status -z` names, both
// sides of a rename or copy, cleaned and slash-separated. A tree-wide guard
// reads the same records the package and guard selection read.
func ChangedPaths(nameStatus string) ([]string, error) {
	var paths []string
	err := eachChange(nameStatus, func(names, _, _ []string) {
		paths = append(paths, names...)
	})
	if err != nil {
		return nil, err
	}
	return paths, nil
}

// errNotNUL is returned for name-status output that has tab or newline
// separators and no NUL: `git diff --name-status` without -z. Read as NUL
// records it names no path, so package selection would fail open and select
// nothing; the caller fails instead (gt-fz8xk).
var errNotNUL = errors.New("name-status output is not NUL-separated: run git diff with -z")

// eachChange calls fn once per `git diff --name-status -z` record with every
// path the record names, the paths that exist after the change and the paths
// it removed: a guard that judges the tree as it will stand reads the second,
// a guard that ratchets a baseline down reads the third.
//
// A copy's source is unchanged, so only its destination is now or gone, but
// the source is still in names. A rename names both sides and a deletion only
// the removed path.
//
// With -z, git writes each field NUL-terminated and never quotes a name, so a
// path holding a non-ASCII byte, a tab or a quote arrives as written. Without
// -z git quotes such a path ("caf\303\251.go") and the .go suffix is lost.
func eachChange(nameStatus string, fn func(names, now, gone []string)) error {
	if !strings.Contains(nameStatus, "\x00") && strings.ContainsAny(nameStatus, "\t\n") {
		return errNotNUL
	}
	// Fields alternate: a status (M, A, D, R100, C75 ...), then one path, or
	// old and new for a rename or copy. An empty status is skipped: a trailing
	// NUL leaves one. A record cut short, with no path or an empty one, ends
	// the scan: no later record can be read past it.
	fields := strings.Split(nameStatus, "\x00")
	for i := 0; i < len(fields); {
		status := fields[i]
		if status == "" {
			i++
			continue
		}
		n := 1
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			n = 2
		}
		if i+n >= len(fields) || !completePaths(fields[i+1:i+1+n]) {
			break
		}
		names := make([]string, 0, n)
		for _, f := range fields[i+1 : i+1+n] {
			names = append(names, path.Clean(filepath.ToSlash(f)))
		}
		i += 1 + n
		switch {
		case strings.HasPrefix(status, "D"):
			fn(names, nil, names)
		case strings.HasPrefix(status, "R"):
			fn(names, names[1:], names[:1])
		case strings.HasPrefix(status, "C"):
			fn(names, names[1:], nil)
		default:
			fn(names, names, nil)
		}
	}
	return nil
}

// completePaths reports whether every path field of a record is present and
// non-empty.
func completePaths(paths []string) bool {
	for _, p := range paths {
		if p == "" {
			return false
		}
	}
	return true
}

// owningPackage is the package directory file belongs to, or "".
func owningPackage(file string, hasGo func(dir string) bool) string {
	file = path.Clean(filepath.ToSlash(file))
	dir := path.Dir(file)
	if strings.HasSuffix(file, ".go") && !isFixtureDir(dir) {
		if hasGo(dir) {
			return dir
		}
		return ""
	}
	for ; dir != "." && dir != "/"; dir = path.Dir(dir) {
		if !isFixtureDir(dir) && hasGo(dir) {
			return dir
		}
	}
	return ""
}

// isFixtureDir reports whether dir is not a package directory, mirroring the
// go tool: `./...` skips a directory named testdata and one whose name begins
// with "." or "_". A .go file there is a fixture, not a package member, so it
// maps to the package that reads it — and a directory the go tool skips must
// never be handed to `go test`, which cannot build it (gt-f1ynu).
func isFixtureDir(dir string) bool {
	if dir == "." || dir == "/" {
		return false
	}
	for _, elem := range strings.Split(dir, "/") {
		if elem == "testdata" || strings.HasPrefix(elem, ".") || strings.HasPrefix(elem, "_") {
			return true
		}
	}
	return false
}

// PackageDirHasGo reports whether repoRoot/dir holds a .go file directly and
// belongs to the root module. A directory inside a nested module (a go.mod in
// it or above it, below repoRoot) is not one: `go test` from the root cannot
// build it, and the Makefile tests those modules in their own directory.
func PackageDirHasGo(repoRoot, dir string) bool {
	if inNestedModule(repoRoot, dir) {
		return false
	}
	entries, err := os.ReadDir(filepath.Join(repoRoot, filepath.FromSlash(dir)))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}

// inNestedModule reports whether dir, or an ancestor of it below repoRoot,
// holds a go.mod: the directory then belongs to that module, not the root's.
func inNestedModule(repoRoot, dir string) bool {
	for d := path.Clean(filepath.ToSlash(dir)); d != "." && d != "/"; d = path.Dir(d) {
		if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(d), "go.mod")); err == nil {
			return true
		}
	}
	return false
}

// treeWideGuards are the repo's guard tests that read paths outside their own
// package, each with the inputs that make it fail. presubmit runs the guard
// tests a branch's changed paths trigger, so a branch that breaks one learns
// it in seconds rather than at the landing gate minutes later (gt-ydzwb).
//
// Add an entry when a test reads a file its own package does not own, with
// the paths it reads. Err wide: a predicate wider than the test only costs a
// guard run, while a narrower one is the miss this table exists to close.
//
// removal marks a guard a deletion can fail: one that ratchets a baseline
// down (a vanished entry is stale) or opens a path that must exist. A guard
// that judges only the new content of a file cannot fail on a removal.
var treeWideGuards = []treeWideGuard{
	{
		// The sandbox policy is data the openshell backend sends with every
		// sandbox; this guard pins which hosts it may reach. The tests read
		// the file, so deleting it fails them too.
		pkg:     "internal/session",
		tests:   []string{"TestPolecatPolicyAllowsOnlyTheReviewedEgress", "TestPolecatPolicyLetsOnlyNodeReachTheNpmRegistryAndFonts"},
		judges:  func(rel string) bool { return rel == "deploy/polecat/policy.yaml" },
		removal: true,
	},
	{
		// TestPolicy, TestUnitTierMain, TestDockerTier and friends walk every
		// package directory in the module, so they judge test files the
		// branch never touches (gt-gzmfs). A deletion counts: the lists they
		// ratchet (unconverted.txt and the rest) go stale when the last test
		// file of a listed package is removed with it.
		//
		// TestUnionMergeOnlyOnGrowOnlyLists reads the repo-root .gitattributes
		// and the lists it marks merge=union, so those paths are inputs too.
		pkg: "internal/testpolicy",
		tests: []string{"TestPolicy", "TestUnitTierMain", "TestAllowedTools", "TestRealGit",
			"TestGitFree", "TestDockerTier", "TestUnionMergeOnlyOnGrowOnlyLists"},
		judges: func(rel string) bool {
			return strings.HasSuffix(rel, "_test.go") || rel == ".gitattributes" ||
				rel == "internal/testpolicy/gitfree.txt"
		},
		removal: true,
	},
	{
		// The agent prose allowlist scans templates, formula prose and the
		// hints gt's commands print, and its baseline only shrinks.
		pkg:     "internal/cmdtree",
		tests:   []string{"TestAgentProseBdAllowlist"},
		judges:  agentProse,
		removal: true,
	},
	{
		// TestCommandTokensResolve scans the same tree through ScanRepo and
		// TestMakefileHandsTheContainerOptInToTheSuite reads the Makefile.
		pkg:    "internal/cmd",
		tests:  []string{"TestCommandTokensResolve", "TestMakefileHandsTheContainerOptInToTheSuite"},
		judges: func(rel string) bool { return rel == "Makefile" || commandTreeInputs(rel) },
	},
	{
		pkg:     "internal/bdgate",
		tests:   []string{"TestEverySessionStartCallsTheGate"},
		judges:  func(rel string) bool { return sessionStartFiles[rel] },
		removal: true,
	},
	{
		pkg: "internal/polecat",
		tests: []string{
			"TestNoWorkstateInputLiteralsOutsideConstructor",
			"TestNoCanIgnoreStaleCleanupStatusCallsOutsidePolecat",
		},
		judges: underInternal,
	},
	{
		pkg: "internal/beads",
		tests: []string{
			"TestNoShellBdWritesToAgentBeads",
			"TestBdSubprocessPolicyInHardenedPackages",
			"TestNoAdHocBdSubprocessesOutsideBeads",
			"TestMachineModeOptOutsAreEnumerated",
			"TestMigratedFilesImportNoBeadsLibrary",
		},
		judges:  underInternal,
		removal: true,
	},
	{
		pkg:     "internal/beads/beadsfake",
		tests:   []string{"TestNoNewRawBdSites"},
		judges:  goFile,
		removal: true,
	},
	{
		pkg: "internal/beadsql",
		tests: []string{
			"TestNoDirectSQLReadsOfBdTablesOutsideBeadsql",
			"TestBdSQLStatementsComeFromBeadsql",
		},
		judges: underInternal,
	},
	{
		pkg: "internal/config",
		tests: []string{
			"TestNoNewGTEnvReads",
			"TestNoAgentNamePresetLookups",
			"TestRetiredConfigFilesAreReadThroughTheLoaders",
		},
		judges:  underInternalOrCmd,
		removal: true,
	},
	{
		pkg:     "internal/townconfig",
		tests:   []string{"TestDoltDatabaseIsReadThroughTheKernel"},
		judges:  underInternalOrCmd,
		removal: true,
	},
	{
		pkg:     "internal/guardlint",
		tests:   []string{"TestNoNewFailOpenGuards"},
		judges:  underInternal,
		removal: true,
	},
	{
		pkg:    "internal/rig",
		tests:  []string{"TestNoMergeSettingsCommandCallsOutsideResolver"},
		judges: underInternal,
	},
	{
		pkg:    "internal/tmux",
		tests:  []string{"TestNoAutoRespawnHookInTheTree"},
		judges: underInternal,
	},
	{
		pkg:    "internal/doltserver",
		tests:  []string{"TestNoGoSourceWritesBdTables"},
		judges: underInternal,
	},
	{
		pkg:    "internal/deps",
		tests:  []string{"TestNoBDGoInstallAnywhere"},
		judges: underInternalOrCmd,
	},
	{
		pkg:    "internal/testdb",
		tests:  []string{"TestOneDefinition"},
		judges: nonTestGo,
	},
	{
		pkg: "internal/testutil",
		tests: []string{
			"TestNoParallelTestsReachProcessGlobalSwaps",
			"TestHermeticHarnessEnforced",
		},
		judges: goFile,
	},
	{
		// The shipped-rubric tests read the repo-root .om.json, which no
		// package owns, so a change to the rubric alone names no package in
		// ChangedPackages and the guards would run only at the landing gate
		// (gt-1zff). A deletion counts: the tests open the file, and a
		// landing that removed the rubric would take the gate with it.
		pkg: "internal/land",
		tests: []string{
			"TestShippedOMRubricIsGradeable",
			"TestShippedOMRubricCarriesInstructionProliferationCriterion",
			"TestShippedOMRubricCarriesScriptsAreDebtCriterion",
			"TestShippedOMThresholdIsGastownBar",
		},
		judges:  func(rel string) bool { return rel == ".om.json" },
		removal: true,
	},
	{
		// Any .sh file is the guard's input: it holds the tracked scripts to
		// the shrinking allowlist in internal/repoguard/shell-scripts.txt
		// (gt-lpm8b). The real listing is git ls-files, so the tree check is
		// integration-tier — the unit tier runs no git (realgit.txt), and its
		// guard takes an injected lister. A deletion or a rename counts too:
		// the script's allowlist line must go in the same change.
		//
		// The plugin shim guard reads the same .sh paths through Glob and
		// ReadFile, so its checkout test is integration-tier too; the shim
		// allowlist is an input the branch can change on its own.
		pkg:   "internal/repoguard",
		tests: []string{"TestIntegrationNoNewShellScripts", "TestIntegrationPluginRunShTree"},
		judges: func(rel string) bool {
			return strings.HasSuffix(rel, ".sh") || rel == "internal/repoguard/plugin-run-sh.txt"
		},
		removal: true,
	},
}

// treeWideGuard is one guard package's inputs: the test functions to run and
// a predicate over a repo-relative changed path.
type treeWideGuard struct {
	pkg     string
	tests   []string
	judges  func(rel string) bool
	removal bool
}

// sessionStartFiles are the session-start functions TestEverySessionStartCallsTheGate
// opens directly.
var sessionStartFiles = map[string]bool{
	"internal/session/lifecycle.go":       true,
	"internal/polecat/session_manager.go": true,
	"internal/crew/manager.go":            true,
}

// agentProse reports whether rel is a file TestAgentProseBdAllowlist reads:
// the formula prose, role and message templates, plugins, agent commands and
// skills, AGENTS.md, and the internal/cmd Go whose printed hints it scans.
func agentProse(rel string) bool {
	switch {
	case rel == "AGENTS.md":
		return true
	case strings.HasPrefix(rel, "internal/formula/formulas/"):
		return strings.HasSuffix(rel, ".toml")
	case strings.HasPrefix(rel, "internal/templates/"), strings.HasPrefix(rel, "templates/"):
		return strings.HasSuffix(rel, ".md") || strings.HasSuffix(rel, ".tmpl")
	case strings.HasPrefix(rel, "plugins/"):
		return strings.HasSuffix(rel, ".sh") || strings.HasSuffix(rel, "/plugin.md")
	case strings.HasPrefix(rel, ".claude/commands/"), strings.HasPrefix(rel, ".claude/skills/"):
		return strings.HasSuffix(rel, ".md")
	}
	return strings.HasPrefix(rel, "internal/cmd/") && nonTestGo(rel)
}

// commandTreeInputs reports whether rel is a file TestCommandTokensResolve
// scans. It is wider than agentProse: gt's own scripts, git hooks and role
// configs carry command lines too, and a human or gt runs those, not an
// agent.
func commandTreeInputs(rel string) bool {
	switch {
	case agentProse(rel):
		return true
	case strings.HasPrefix(rel, "scripts/"):
		return strings.HasSuffix(rel, ".sh")
	case strings.HasPrefix(rel, ".githooks/"):
		return true
	case strings.HasPrefix(rel, "internal/config/roles/"):
		return strings.HasSuffix(rel, ".toml")
	case strings.HasPrefix(rel, "internal/"), strings.HasPrefix(rel, "cmd/"):
		return nonTestGo(rel)
	}
	return false
}

func goFile(rel string) bool    { return strings.HasSuffix(rel, ".go") }
func nonTestGo(rel string) bool { return goFile(rel) && !strings.HasSuffix(rel, "_test.go") }
func underInternal(rel string) bool {
	return nonTestGo(rel) && strings.HasPrefix(rel, "internal/")
}

func underInternalOrCmd(rel string) bool {
	return nonTestGo(rel) && (strings.HasPrefix(rel, "internal/") || strings.HasPrefix(rel, "cmd/"))
}
