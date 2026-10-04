package land

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// ChangedPackages maps `git diff --name-status -M` output to the Go package
// directories whose tests the change can affect, sorted and relative to the
// repo root ("internal/land", never "./internal/land").
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
func ChangedPackages(nameStatus string, hasGo func(dir string) bool) []string {
	seen := map[string]bool{}
	eachChange(nameStatus, func(_ string, now, gone []string) {
		for _, files := range [][]string{now, gone} {
			for _, file := range files {
				if dir := owningPackage(file, hasGo); dir != "" {
					seen[dir] = true
				}
			}
		}
	})
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs
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
func GuardSelects(nameStatus string, hasGo func(dir string) bool) []GuardSelect {
	triggered := map[string]bool{}
	eachChange(nameStatus, func(_ string, now, gone []string) {
		for _, g := range treeWideGuards {
			if triggered[g.pkg] || !hasGo(g.pkg) {
				continue
			}
			if anyJudged(g, now) || (g.removal && anyJudged(g, gone)) {
				triggered[g.pkg] = true
			}
		}
	})
	tests := map[string][]string{}
	for _, g := range treeWideGuards {
		tests[g.pkg] = append(tests[g.pkg], g.tests...)
	}
	if len(triggered) == 0 {
		return nil
	}
	sel := make([]GuardSelect, 0, len(triggered))
	for pkg := range triggered {
		names := tests[pkg]
		sort.Strings(names)
		sel = append(sel, GuardSelect{Package: pkg, Tests: names})
	}
	sort.Slice(sel, func(i, j int) bool { return sel[i].Package < sel[j].Package })
	return sel
}

func anyJudged(g treeWideGuard, files []string) bool {
	for _, file := range files {
		if g.judges(file) {
			return true
		}
	}
	return false
}

// eachChange calls fn once per `git diff --name-status -M` line with the
// paths that exist after the change and the paths it removed: a guard that
// judges the tree as it will stand reads the first, a guard that ratchets a
// baseline down reads the second.
//
// A copy names only its destination, because its source is unchanged. A
// rename names both sides and a deletion only the removed path.
func eachChange(nameStatus string, fn func(status string, now, gone []string)) {
	for _, line := range strings.Split(nameStatus, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) < 2 {
			continue
		}
		// fields[0] is the status (M, A, D, R100, C75 ...); the rest are
		// paths: one, or old and new for a rename or copy.
		paths := make([]string, 0, len(fields)-1)
		for _, f := range fields[1:] {
			paths = append(paths, path.Clean(filepath.ToSlash(f)))
		}
		status, last := fields[0], paths[len(paths)-1:]
		switch {
		case strings.HasPrefix(status, "D"):
			fn(status, nil, paths)
		case strings.HasPrefix(status, "R"):
			fn(status, last, paths[:len(paths)-1])
		case strings.HasPrefix(status, "C"):
			fn(status, last, nil)
		default:
			fn(status, paths, nil)
		}
	}
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

// PackageDirHasGo reports whether repoRoot/dir holds a .go file directly.
func PackageDirHasGo(repoRoot, dir string) bool {
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
		},
		judges:  func(rel string) bool { return rel == ".om.json" },
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
	"internal/mayor/manager.go":           true,
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
