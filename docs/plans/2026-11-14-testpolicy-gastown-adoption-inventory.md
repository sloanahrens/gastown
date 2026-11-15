> Status: historical (2026-11). Merged in: gt-zq17ds, gt-b119av. Not maintained.

# Gastown's internal/testpolicy against the shared module: adoption inventory

**Goal:** the evidence slice 7 of the shared testpolicy module needs: every file
of gastown's `internal/testpolicy` classed against the module
`gt.local/testpolicy` (the `testpolicymod/` directory of `sloan/devops`), each
class cited by file and function name, and every exported name with the callers
it has outside the package.

The first half (gt-zq17ds) is the comparison above. The second half (gt-b119av,
below the "What the comparison settles" section) names gastown's extra rules and
how each registers through the module's `Rule` seam, says what in gastown's six
lists and `policy_test.go` stays, and lists the implementation beads of slice 7
in dependency order with the operator's publishing step.

## What was read, and how to reproduce it

Two trees, both at their `origin/main`:

| Tree | Commit | How it was read |
|---|---|---|
| gastown `internal/testpolicy` | `627c878ee0f37fc9a933385d19aec52b2398e154` | the checkout's own `origin` remote |
| `sloan/devops` `testpolicymod/` | `9bfd72568d951d0b352de93d96f99e15cbb101cb` | read-only clone, sparse checkout of `testpolicymod/` only |

Commands run, from the gastown checkout:

```sh
git rev-parse origin/main                     # 627c878ee0f37...
git log -1 origin/main                         # the land commit, so the id is checkable
git ls-tree -r --name-only origin/main internal/testpolicy      # 139 files

# module side: no checkout of the rest of devops, nothing built or run
git clone --depth 1 --filter=blob:none --no-checkout <forgejo>/sloan/devops.git scratch/devops-read
git -C scratch/devops-read sparse-checkout set testpolicymod
git -C scratch/devops-read rev-parse origin/main                # 9bfd72568d95...

# the per-file comparison every row below comes from
diff -u scratch/devops-read/testpolicymod/<file> <gastown internal/testpolicy>/<file>
```

Only files under `testpolicymod/` were read from that clone, and nothing from it
was built or executed. The bead authorises the read
(`sloan/devops`, read only); the earlier draft of this document declined it
under the rule that a polecat reads only its own rig (gt-690mog), and that
decline is what this version replaces.

## How each class is decided

- **identical** — `diff -q` reports no difference at all.
- **trimmed at the copy** — one side is the other's text with whole
  declarations added or removed and every shared line matching; the direction
  is named in the row.
- **drifted** — at least one shared line differs, in code or only in comments
  (the row says which).
- **gastown-only** — `testpolicymod/` at `9bfd7256` has no file of that name.

A gastown-only class means the module does not carry the file *today*; it does
not mean gastown keeps it. The slice named in the row is the one the module
plan moves it with.

## The module's shape at 9bfd7256

`testpolicymod/` is its own module — `go.mod` declares `module
gt.local/testpolicy` on `go 1.27` — and holds 16 Go files plus 20 fixture files
in 18 directories. It carries no `cmd/` subtree and none of gastown's list
files.

| Module file | Lines | Exported names |
|---|---:|---|
| `rule.go` | 49 | `ScanContext`, `Rule` |
| `scan.go` | 117 | `Violation`, `ScanDir`, `ScanDirWithRules`, `ScanDirWithExemptions`, `FileImports` |
| `allow.go` | 80 | `RuleAllowReason`, `AllowKey`, `Exemption`, `CollectAllows` |
| `rules.go` | 225 | `RuleNoSleep`, `RuleNoSubprocess`, `RuleNoNetwork`, `RuleNoBuild`, `RuleNoExecFiles`, `CheckTestCall`, `IsTestName` |
| `list.go` | 63 | `ReadList`, `RelPath`, `PackageDirs` |
| `split.go` | 127 | `SplitTestArgs`, `TagsArgs`, `PartitionPackages` |
| `tier.go` | 92 | `FastTierMaxWall`, `PackageWall`, `WallSummary`, `ParsePackageWalls`, `WallOverruns` |
| `cpu.go` | 75 | `CPUDirEnv`, `CPUTime`, `CPUSource`, `WriteCPU`, `ReadCPU` |
| `cpuexempt.go` | 94 | `CPUExemptSize`, `CPUExemptRow`, `CPUExemptSeed`, `ParseCPUExempt`, `ReadCPUExempt` |
| `scan_test.go` | 219 | 5 tests: `TestScanDirWithRulesSortsViolationsByFile`, `TestScanDirWithRulesRunsEachRulePerPackage`, `TestScanDirWithRulesRefusesBadRuleNames`, `TestAllowSuppressesOnlyTheNamedRule`, `TestScanDirWithExemptionsReportsEachSuppression` |
| `rules_test.go` | 151 | 5 tests: `TestSharedRulesFlagEachBannedCall`, `TestSharedRulesCleanAndAllowedFixtures`, `TestSharedRulesTreatAShadowedPackageNameAsAValue`, `TestSharedRuleNamesAreReserved`, `TestIsTestName` |
| `list_test.go` | 93 | `TestReadListAndPackageDirs` |
| `split_test.go` | 74 | `TestSplitTestArgs`, `TestPartitionPackages`, `TestTagsArgs` |
| `tier_test.go` | 65 | `TestParsePackageWalls`, `TestWallOverruns` |
| `cpu_test.go` | 55 | `TestCPURecordRoundTrip`, `TestCPUTimeTotal` |
| `cpuexempt_test.go` | 181 | 5 tests: `TestParseCPUExempt`, `TestParseCPUExemptLongLine`, `TestReadCPUExemptMissingFile`, `TestReadCPUExemptBadRowNamesTheFile`, `TestCPUExemptSeedMatchesItsCount` |

The `Rule` seam, in the module's own text:

```go
type ScanContext struct {
	Fset    *token.FileSet
	Tests   []*ast.File
	Prod    []*ast.File
	Allow   map[AllowKey]string
	Imports func(f *ast.File) map[string]string
}

type Rule interface {
	Name() string
	Check(ctx *ScanContext) []Violation
}

func ScanDirWithRules(dir string, extra ...Rule) ([]Violation, error)
func ScanDirWithExemptions(dir string, extra ...Rule) ([]Violation, []Exemption, error)
```

`sharedRules()` returns one rule, `testFileRule`, whose `Name()` is
`"test-file"`; `checkRuleNames` refuses a nil rule, an empty name and a
duplicate name before any directory is read.

The module's fixtures are `allow_noreason`, `allow_ok`, `clean` (2 files),
`exec_file`, `exec_git`, `exec_go`, `exec_other`, `fakerule` (2),
`network`, `network_more`, `shadow_net`, `shadow_strings`, `shadow_time`,
`shebang`, `shebang_passthrough`, `shebang_readonly`, `shebang_written`,
`sleep`.

**What the module does not carry at this commit.** Slice 1a and 1b (scanner,
allow machinery, Rule seam, six shared rules), slice 2 (lists, split, tier
walls) and the CPU check of slice 3a (`cpu.go`, `cpuexempt.go`) are in the
module. `sloan/devops`'s own `internal/testpolicy/` additionally holds
`budget.go`, `slow.go`, `scripts.go`, `scripttree.go`, `presubmit.go`,
`changed.go`, `triggers.go`, `nameguard.go`, `identityguard.go`,
`violating.go`, `unconvertedtests.go` and a `cmd/` subtree — slices 3b to 6 —
which the module has not taken yet.

## Every file of gastown's internal/testpolicy

139 files: 35 Go files at the package root, 6 lists, 15 files under `cmd/` and
83 fixture files in 42 directories.

### The 35 Go files at the package root

Lines are gastown's; the module's count is in brackets where a counterpart
exists.

| File | Lines | Holds | Class | Evidence |
|---|---:|---|---|---|
| `allow.go` | 64 (80) | `allowPrefix`, `allowKey`, `Exemption`, `applyAllows` | **drifted** | The module exports `AllowKey` and splits one pass into `CollectAllows(fset, files)` plus `applyAllows(allowed, vs)`; gastown keeps `allowKey` unexported, declares `RuleAllowReason` in `rules.go`, and holds a single `applyAllows(fset, files, vs)`. `Exemption` is the same struct in both. |
| `budget.go` | 301 | `TestTime`, `Overrun`, `testEvent`, `pkgOutput`, `WatchBudget`, `SlowRun`, `PkgTime`, `BudgetResult`, `IdleTop`, `IdleRatioFactor`, `IdleWall`, `IdleReport`, `IdleJudged`, `medianDuration`, `WatchBudgetTracked`, `StrictBudgetEnv`, `EnforceBudget`, `BudgetFails` | gastown-only (slice 3b) | no `budget.go` in the module |
| `budget_integration_test.go` | 186 | six `TestIntegrationBudget*` tests, `runBudget`, `fixturePkg` | gastown-only (slice 3b) | — |
| `budget_test.go` | 323 | eleven `TestWatchBudget*`/`TestIdle*`/`TestEnforce*`/`TestBudgetFails` tests | gastown-only (slice 3b) | — |
| `contracts.go` | 156 | `RuleContract`, `RuleIntegrationName`, `contractName`, `CheckContracts`, `hasIntegrationTag`, `requiresIntegration`, `collectTags` | gastown-only | — |
| `cpu.go` | 69 (75) | `CPUDirEnv`, `CPUTime`, `CPUSource`, `cpuRecord`, `cpuFile`, `WriteCPU`, `ReadCPU` | **trimmed at the copy** (the module adds `CPUTime.Total()`) | every shared line matches; the module's only addition is `Total()`, user plus system |
| `cpu_test.go` | 31 (55) | `TestCPURecordRoundTrip` | **trimmed at the copy** (the module adds `TestCPUTimeTotal`) | — |
| `docker.go` | 188 | `containerEntryPoints`, `StartsContainers`, `CheckDockerTier`, `exportedEntryWrappers`, `callsEntryPoint` | gastown-only | — |
| `docker_test.go` | 89 | `TestStartsContainersFixtures`, `TestCheckDockerTierFixtures`, `TestDockerTier` | gastown-only | — |
| `gitfree.go` | 253 | `RuleNoGit`, `GitFreeFindings`, `unitTierTestFiles`, `CheckGitFree` and their helpers | gastown-only | — |
| `gitfree_test.go` | 124 | `minGitFree`, `TestGitFreeFindingsFixtures`, `TestCheckGitFreeFixtures`, `TestGitFree` | gastown-only | — |
| `integration.go` | 96 | `IntegrationPackages`, `addsIntegrationFiles`, `skipForGoTool` | gastown-only | — |
| `integration_test.go` | 51 | `TestIntegrationPackages` | gastown-only | — |
| `list.go` | 61 (63) | `ReadList`, `PackageDirs` | **drifted** | The module adds `RelPath`; gastown's `PackageDirs` skips any directory holding its own `go.mod`. `ReadList`'s body matches. |
| `list_test.go` | 48 (93) | `TestPackageDirsSkipsNestedModules` | **drifted** | the module's `TestReadListAndPackageDirs` tests `ReadList`'s comment and blank-line handling plus `PackageDirs`; gastown tests only the nested-module skip |
| `main_test.go` | 12 | `TestMain` through `unittier.Main` | gastown-only | — |
| `overbudget.go` | 103 | `OverBudgetEntry`, `TrackedRun`, `beadIDPattern`, `ParseOverBudget`, `ReadOverBudget`, `CheckOverBudget` | gastown-only (slice 3b/6a) | — |
| `overbudget_test.go` | 140 | `TestParseOverBudget`, `TestCheckOverBudget`, `TestWatchBudgetTracked`, `TestWatchBudgetSlowWall` | gastown-only (slice 3b/6a) | — |
| `policy_test.go` | 134 | `seed`, `maxUnconverted`, `maxOverBudget`, `TestPolicy` | gastown-only | — |
| `realgit.go` | 119 | `RealGitFiles`, `CheckRealGit`, `ConvertedShare`, `relPath`, `sortedKeys` | gastown-only | — |
| `realgit_test.go` | 128 | `TestRealGit`, `TestRealGitFixtures` | gastown-only | — |
| `rules.go` | 434 (225) | 15 rule constants; `bannedTestCalls`, `bannedTestMethods`, `checkTestFile`, `checkTestCall`, `checkEnvValue`, `checkProdFile`, `isTestingReceiver`, `testingParam`, `isTestingType`, `callsMethodOn`, `envName`, `constValue`, `stringLit`, `execMode`, `netDialerType`, `markReadOnlyLits`, `readOnlyStringFuncs` | **drifted** | The module keeps five constants (`RuleNoSleep`, `RuleNoSubprocess`, `RuleNoNetwork`, `RuleNoBuild`, `RuleNoExecFiles`), `sharedRules()`, `testFileRule` (name `"test-file"`), exports `CheckTestCall` and adds `IsTestName`; gastown declares nine more constants, keeps `checkTestCall` unexported with an extra `consts` parameter, adds `bannedTestMethods`, `checkEnvValue` and `checkProdFile`, and its `checkTestFile` takes `pkgVars` and `consts`. gastown has no `IsTestName`. |
| `scan.go` | 161 (117) | package doc; `Violation` and `String`; `ScanDir`; `ScanDirWithExemptions`; `packageVars`; `packageConsts`; `importsPath`; `imports` | **drifted** | The module's file is the seam: `ScanDirWithRules`, `ScanContext`, `CollectAllows`, `FileImports`, `before`. gastown's takes no rules parameter, reads through `srccache` (`sourcesFor`), computes `packageVars`, `packageConsts` and `importsPath` inline, calls `checkTestFile`/`checkProdFile` directly and sorts by file then line. |
| `scan_test.go` | 141 (219) | `TestScanDirFixtures`, `TestScanDirWithExemptions`, `TestCheckContracts`, `TestRequiresIntegration` | **drifted** | the module has the five `TestScanDirWithRules*`/allow tests and no contract or integration-tag tests; gastown's two contract tests cover code the module does not carry |
| `split.go` | 127 (127) | `goTestBoolFlags`, `goTestValueFlags`, `flagName`, `SplitTestArgs`, `TagsArgs`, `PartitionPackages` | **drifted in comments only** | all six declarations are the same text; three doc comments differ (the module says "a caller", gastown names the budget runner) |
| `split_test.go` | 74 (74) | `TestSplitTestArgs`, `TestPartitionPackages`, `TestTagsArgs` | **drifted** | same test names; `TestPartitionPackages`'s rows use `internal/one`/`internal/two` in the module and `internal/cmd`/`internal/beads` in gastown, and one comment differs |
| `srccache.go` | 191 | `repoSources`, `sourceCache`, `cachedFile`, `cachedMatch`, `sourceSet`, `cacheable`, `cacheableUnder`, `within`, `moduleRootAbove`, `tempRoots`, `checkoutRoot` | gastown-only | the module parses each package with `go/parser` and `go/build` directly |
| `srccache_test.go` | 154 | the five srccache tests | gastown-only | — |
| `sweephost.go` | 137 | `SweepHostEntry`, `ReadSweepHost`, `ParseSweepHost`, `CheckSweepHostPackages`, `CheckSweepHostReasons`, `CheckSweepHostReasonFiles`, `SweepRunnerPackages`, `CheckSweepHostCount` | gastown-only | — |
| `sweephost_test.go` | 246 | the nine sweep tests, `TestSweepHostFixtures` among them | gastown-only | — |
| `tier.go` | 94 (92) | `FastTierMaxWall`, `PackageWall`, `WallSummary`, `ParsePackageWalls`, `WallOverruns` | **drifted in comments only** | `FastTierMaxWall` is `30 * time.Second` in both; two doc comments differ |
| `tier_test.go` | 65 (65) | `TestParsePackageWalls`, `TestWallOverruns` | **drifted** | `tierModule` is `example.test/mod` in the module and `github.com/steveyegge/gastown` in gastown, and every fixture output line carries that path |
| `tiermain.go` | 123 | `unittierPkgPath`, `tierRunners`, `UnitTierMain`, `callsTierRunner`, `CheckUnitTierMains`, `AllowedTools`, `RuleAllowTools`, `isAllowTools` | gastown-only | — |
| `tiermain_test.go` | 127 | `TestUnitTierMainFixtures`, `TestCheckUnitTierMainsFixtures`, `TestUnitTierMain`, `TestAllowedToolsFixture`, `TestAllowedTools` | gastown-only | — |
| `unionmerge_test.go` | 73 | `growOnlyHeader`, `TestUnionMergeOnlyOnGrowOnlyLists` | gastown-only | — |

### The six lists

gastown data: the module carries none of these lists.

| File | Lines | What it lists | Class |
|---|---:|---|---|
| `unconverted.txt` | 3 | packages not yet converted to the unit rules; ratcheted by `maxUnconverted` | gastown-only |
| `overbudget.txt` | 5 | converted packages over the time budget, each with its bead; `maxOverBudget` | gastown-only |
| `realgit.txt` | 5 | unit-tier files that still run real git; `maxRealGit` | gastown-only |
| `gitfree.txt` | 30 | packages whose unit tier may run no git; `minGitFree` | gastown-only |
| `docker.txt` | 9 | packages whose unit-tier tests start a container; `TestDockerTier` | gastown-only |
| `sweep-host.txt` | 28 | integration packages left to the host sweep; `maxSweepHost` | gastown-only |

### The cmd/ subtree

The module has no `cmd/` directory, so every file here is gastown-only.

| File | Lines | Holds | Class |
|---|---:|---|---|
| `cmd/budget/main.go` | 619 | the budget runner: flags, `listPackages`, `runJudged`, `reportIdle`, `reportOver`, `usage`, the `-exec` wrapper mode | gastown-only (slice 3b) |
| `cmd/budget/die_unix.go` | 36 | the wrapper's signal exit | gastown-only (slice 3b) |
| `cmd/budget/main_test.go` | 320 | the runner's tests | gastown-only (slice 3b) |
| `cmd/budget/testmain_test.go` | 12 | `TestMain` through `unittier.Main` | gastown-only (slice 3b) |
| `cmd/integrationvet/main.go` | 305 | the integration vet: flags and the `testpolicy.IntegrationPackages` call | gastown-only |
| `cmd/integrationvet/main_integration_test.go` | 77 | the vet's integration test | gastown-only |
| `cmd/integrationvet/main_test.go` | 396 | the vet's tests | gastown-only |
| `cmd/tierwall/main.go` | 766 | the wall reporter: flags, JSONL reader, series writer | gastown-only (slice 5b) |
| `cmd/tierwall/main_test.go` | 1234 | the reporter's tests | gastown-only (slice 5b) |
| `cmd/tierwall/testmain_test.go` | 12 | `TestMain` through `unittier.Main` | gastown-only (slice 5b) |
| `cmd/tierwall/testdata/fail.jsonl` | 28 | the reporter's failing-input fixture | gastown-only (slice 5b) |
| `cmd/tierwall/testdata/pass.jsonl` | 35 | its passing-input fixture | gastown-only (slice 5b) |
| `cmd/tierwall/testdata/slow.jsonl` | 15 | its slow-run fixture | gastown-only (slice 5b) |
| `cmd/gateverdict/main.go` | 35 | the `gate: FAILED at ...` verdict writer; no recipe calls it yet | gastown-only |
| `cmd/gateverdict/main_test.go` | 38 | its tests | gastown-only |

### testdata: 83 files in 42 directories

15 of these files are byte-identical to the module's; the four module
directories `fakerule`, `shadow_net`, `shadow_strings` and `shadow_time` have no
gastown counterpart.

| Directory | Files | Pins | Class |
|---|---|---|---|
| `allow_noreason` | `a_test.go` | an exemption with no reason | **identical** |
| `allow_ok` | `a_test.go` | an exemption with a reason | **identical** |
| `clean` | `a_test.go`, `prod.go` | a package that breaks nothing | **identical** (both files) |
| `exec_file` | `a_test.go` | writing an executable file | **identical** |
| `exec_git` | `a_test.go` | running git, which is allowed | **identical** |
| `exec_go` | `a_test.go` | running the go tool | **identical** |
| `exec_other` | `a_test.go` | running any other subprocess | **identical** |
| `network` | `a_test.go` | dialling and listening | **identical** |
| `network_more` | `a_test.go` | the remaining net dial and listen calls | **identical** |
| `shebang` | `a_test.go` | writing a `#!` literal | **identical** |
| `shebang_passthrough` | `a_test.go` | a `#!` literal passed through a non-read-only call | **identical** |
| `shebang_readonly` | `a_test.go` | a `#!` literal compared, not written | **identical** |
| `shebang_written` | `a_test.go` | a `#!` literal reaching a write | **identical** |
| `sleep` | `a_test.go` | `time.Sleep` and the other timers | **identical** |
| `allowtools` | `x_test.go`, `x_integration_test.go` | `AllowedTools` | gastown-only |
| `budget` | `cached/cached_test.go`, `childcpu/childcpu_test.go`, `idle/idle_test.go`, `quick/quick_test.go`, `sigkill/sigkill_test.go` | the budget runner | gastown-only |
| `chdir` | `a_test.go` | `os.Chdir` | gastown-only |
| `contracts` | `badfake/fake.go`, `goodfake/fake.go`, `real/int_test.go` | `CheckContracts` | gastown-only |
| `docker` | `integration_only/x_integration_test.go`, `none/x_test.go`, `prod_only/x.go`, `qualified/x_test.go`, `unqualified/x_test.go` | `StartsContainers` | gastown-only |
| `dockertier` | `calls/x_test.go`, `listedstale/x_test.go`, `wrapper/w.go` | `CheckDockerTier` | gastown-only |
| `env` | `a_test.go` | `os.Setenv`/`Unsetenv` and the `Setenv` method | gastown-only |
| `fake_clock_epoch` | `a_test.go` | `clockwork.NewFakeClock` | gastown-only |
| `gitfree` | `allowed/main_test.go`, `allowed/x_test.go`, `clean/main_test.go`, `clean/x_test.go`, `execgit/main_test.go`, `execgit/x_test.go`, `inpkg/main_test.go`, `inpkg/x_test.go`, `integration_only/main_test.go`, `integration_only/x_integration_test.go`, `newgit/main_test.go`, `newgit/x_test.go`, `nomain/x_test.go`, `testutil/main_test.go` | `CheckGitFree` | gastown-only |
| `global_blank` | `a_test.go`, `prod.go` | a package variable that is `_` | gastown-only |
| `global_shadow` | `a_test.go`, `prod.go` | a local shadowing a package variable | gastown-only |
| `global_swap` | `a_test.go`, `prod.go` | assigning a package variable | gastown-only |
| `host_env_declared` | `a_test.go` | a declared host-environment read | gastown-only |
| `host_env_other` | `a_test.go` | a read of a variable that is not host state | gastown-only |
| `host_env_reads` | `a_test.go` | `os.Getenv`/`LookupEnv`/`UserHomeDir` and friends | gastown-only |
| `host_env_seam` | `a_test.go` | a read through an injected seam | gastown-only |
| `host_env_unjudged` | `a_test.go` | a read the check does not judge | gastown-only |
| `host_env_value` | `a_test.go` | an `os.Getenv` used as a value | gastown-only |
| `integration_skipped` | `a_test.go` | `IntegrationPackages` | gastown-only |
| `nontesting_receiver` | `a_test.go` | a same-named method on a type that is not a testing handle | gastown-only |
| `parallel_missing` | `a_test.go` | a test with no `t.Parallel` | gastown-only |
| `parallel_subtest_only` | `a_test.go` | a subtest's `t.Parallel` only | gastown-only |
| `prod_setenv` | `prod.go` | `os.Setenv` in a production file | gastown-only |
| `prod_sleep_clock` | `prod.go` | `time.Sleep` in a package holding a clock | gastown-only |
| `realgit` | `clean/x_test.go`, `helpers/a_test.go`, `helpers/b_test.go`, `helpers/c_test.go`, `helpers/d_test.go`, `listed/x_test.go` | `CheckRealGit` | gastown-only |
| `setenv_in_subtest` | `setenv_in_subtest_test.go` | `t.Setenv` in a subtest | gastown-only |
| `skip` | `a_test.go` | `t.Skip`, `Skipf`, `SkipNow` | gastown-only |
| `tiermain` | `harness/x_test.go`, `inpkg/x_test.go`, `integration_only/main_integration_test.go`, `integration_only/x_test.go`, `none/x_test.go`, `notests/x.go`, `unittier/x_test.go` | `CheckUnitTierMains` | gastown-only |

## Exported names and their callers outside the package

78 exported names across the 17 non-test Go files. The callers are from
`git grep -nE 'testpolicy\.<Name>\b' origin/main`, excluding the package's own
files; a name counts as called only where the qualifier `testpolicy.` is
written, because no file dot-imports the package. Two packages outside it
import it — `internal/testpolicy/cmd/budget` and
`internal/testpolicy/cmd/integrationvet` — and `docs/testing.md` names two of
the exported names in prose.

Command:

```sh
git grep -nE 'testpolicy\.FastTierMaxWall\b' origin/main
```

| File | Exported names | Callers outside the package |
|---|---|---|
| `allow.go` | `Exemption` | none |
| `budget.go` | `TestTime`, `SlowRun`, `PkgTime`, `BudgetResult`, `IdleTop`, `IdleRatioFactor`, `IdleReport`, `IdleWall`, `IdleJudged`, `Overrun`, `WatchBudget`, `WatchBudgetTracked`, `StrictBudgetEnv`, `EnforceBudget`, `BudgetFails` | `BudgetResult` `cmd/budget/main.go:127,461,462`; `Overrun` `main.go:204`, `main_test.go:137,138,141,146,147,148`; `PkgTime` `main.go:180`; `IdleJudged` `main.go:181`; `StrictBudgetEnv` `main.go:93,97,207`; `EnforceBudget` `main.go:93`; `BudgetFails` `main.go:202` (comment) and `main.go:220`; `WatchBudgetTracked` `main.go:500`; `WatchBudget` named in comments `main.go:502,531` only; `IdleWall` `docs/testing.md:33`; `TestTime`, `SlowRun`, `IdleTop`, `IdleRatioFactor`, `IdleReport` none |
| `contracts.go` | `RuleContract`, `RuleIntegrationName`, `CheckContracts` | none |
| `cpu.go` | `CPUDirEnv`, `CPUTime`, `CPUSource`, `WriteCPU`, `ReadCPU` | `CPUDirEnv` `cmd/budget/main.go:483,554,563,579`; `CPUTime` `main.go:493,518,582`, `main_test.go:137`; `WriteCPU` `main.go:582`; `ReadCPU` `main.go:494`; `CPUSource` none |
| `docker.go` | `StartsContainers`, `CheckDockerTier` | none |
| `gitfree.go` | `RuleNoGit`, `GitFreeFindings`, `CheckGitFree` | none |
| `integration.go` | `IntegrationPackages` | `cmd/integrationvet/main.go:86` |
| `list.go` | `ReadList`, `PackageDirs` | `ReadList` `cmd/budget/main.go:71`; `PackageDirs` none |
| `overbudget.go` | `OverBudgetEntry`, `TrackedRun`, `ParseOverBudget`, `ReadOverBudget`, `CheckOverBudget` | `ReadOverBudget` `cmd/budget/main.go:75`; the rest none |
| `realgit.go` | `RealGitFiles`, `CheckRealGit`, `ConvertedShare` | none |
| `rules.go` | `RuleNoSleep`, `RuleNoEnv`, `RuleNoHostEnv`, `RuleNoChdir`, `RuleNoSkip`, `RuleNoSubprocess`, `RuleNoNetwork`, `RuleFakeClockEpoch`, `RuleNoBuild`, `RuleNoExecFiles`, `RuleNoGlobalSwap`, `RuleParallel`, `RuleAllowReason`, `RuleProdSetenv`, `RuleProdSleep` | none |
| `scan.go` | `Violation` (and `Violation.String`), `ScanDir`, `ScanDirWithExemptions` | none |
| `split.go` | `SplitTestArgs`, `TagsArgs`, `PartitionPackages` | `SplitTestArgs` `cmd/budget/main.go:100`; `TagsArgs` `main.go:101`; `PartitionPackages` `main.go:105` |
| `sweephost.go` | `SweepHostEntry`, `ReadSweepHost`, `ParseSweepHost`, `CheckSweepHostPackages`, `CheckSweepHostReasons`, `CheckSweepHostReasonFiles`, `SweepRunnerPackages`, `CheckSweepHostCount` | none; `SweepRunnerPackages` is named in the `sweep-host.txt` comment, not called |
| `tier.go` | `FastTierMaxWall`, `PackageWall`, `WallSummary`, `ParsePackageWalls`, `WallOverruns` | `FastTierMaxWall` `cmd/budget/main.go:67,68,224,251,257`, `main_test.go:112`, `docs/testing.md:29`; `ParsePackageWalls` `main.go:237`; `WallOverruns` `main.go:251`; `PackageWall` and `WallSummary` none (reached through `ParsePackageWalls`' return) |
| `tiermain.go` | `UnitTierMain`, `CheckUnitTierMains`, `AllowedTools`, `RuleAllowTools` | none |

`ScanDir`, `ScanDirWithExemptions`, `CheckContracts`, `CheckGitFree`,
`CheckDockerTier`, `CheckRealGit`, `CheckOverBudget`, `CheckSweepHost*`,
`CheckUnitTierMains` and `AllowedTools` are called only by the package's own
tests — `policy_test.go`'s `TestPolicy` and the per-check `*_test.go` files —
which is why the table has no outside caller for them.

## What the comparison settles, and what it does not

**Settled: the nine extra rules can be external `Rule`s.** `ScanContext` carries
`Fset`, `Tests`, `Prod`, `Allow` and `Imports`. `no-host-env` needs package
constants and `no-global-swap` needs package variables; neither is a
`ScanContext` field, but `packageVars` reads `Prod` and `packageConsts` reads
`Prod` and `Tests`, all of which the context carries, so a gastown Rule can
recompute both with gastown's own helpers. `checkProdFile`'s `clocked` flag
comes from the package's imports, which `ctx.Imports` gives. No module change is
needed for the nine.

**Settled: the API breaks for the first adoption bead.**

- `ScanDirWithExemptions` gains `extra ...Rule`, so every caller changes with
  it: `ScanDir` in `scan.go`, `TestPolicy` in `policy_test.go` and
  `TestScanDirWithExemptions` in `scan_test.go`.
- The module's scanner parses with `go/parser` and `go/build` itself, and offers
  no parse hook, so gastown's `srccache.go` cache has no module counterpart:
  taking the module's scanner drops the one-parse-per-file path and its
  `TestCheckoutFilesAreParsedOnce`, unless the module gains a hook first.
- `FileImports` and `CheckTestCall` are exported in the module; gastown's
  equivalents are the unexported `imports` and `checkTestCall`, and the latter
  also takes an extra `consts` argument. Both change at the seam.

**Settled: two names must be handled at adoption.** `"test-file"` is registered
by the module's `sharedRules()`, so a gastown Rule may not take it —
`checkRuleNames` refuses the whole scan. `"allow-reason"` is not a registered
name but the violation rule the module's `CollectAllows` already emits, so
gastown's `RuleAllowReason` and the emission inside its `applyAllows` must be
deleted rather than wrapped as a Rule, or every unreasoned exemption is
reported twice.

**Not determined: how the devops `internal/testpolicy/` copies of the
slices 3b-6 files differ from gastown's.** The module does not carry
`budget.go`, `overbudget.go`, the slow and script checks or `cmd/`, and this
inventory compared only the module, as the bead scopes it. A drift question
about those files needs the devops copy read, which is slice 3b-6's own bead.

**Not determined: when the module is published.** Every adoption bead below
needs a `gt.local/testpolicy` version to require. No repository named
`sloan/testpolicy`, `sloan/testpolicymod` or `sloan/gt-testpolicy` exists on the
Forgejo host at the time of writing (three `git ls-remote` calls, each
`Repository not found`), and the module still lives inside `sloan/devops` at
`testpolicymod/`. A publish under another name would not have shown up in that
check. Re-checked for this half, with the same answer for all three, and
`sloan/devops` itself carries only the tags `v0.1.0` to `v0.1.7` — its own
module's releases, none of them for `gt.local/testpolicy`.

## Gastown's nine extra rules, and how each registers through the Rule seam

`rules.go` declares fifteen rule constants. Five of them are the module's
(`RuleNoSleep`, `RuleNoSubprocess`, `RuleNoNetwork`, `RuleNoBuild`,
`RuleNoExecFiles`), one — `RuleAllowReason` — belongs to the module's allow
machinery, and the other nine are gastown's own. The nine are what adoption
registers as `Rule` values. None is emitted by the module's `sharedRules()`,
whose single rule `testFileRule` (`Name()` `"test-file"`) runs the module's
`checkTestFile` over `ctx.Tests` and reports only the five shared names.

All nine are emitted today from three unexported functions: `checkTestFile`
(through `checkTestCall` and `checkEnvValue`) for the seven test-file rules, and
`checkProdFile` for the two production-file rules. Each extra rule registers as
one struct whose `Name()` is its constant and whose `Check` reads
`*shared.ScanContext`:

```go
shared "gt.local/testpolicy"   // the module's package is also named testpolicy

type noEnvRule struct{}

func (noEnvRule) Name() string { return RuleNoEnv }
func (noEnvRule) Check(ctx *shared.ScanContext) []shared.Violation { ... }

// gastownRules is the list every gastown scan passes as extra ...Rule.
func gastownRules() []shared.Rule {
	return []shared.Rule{noEnvRule{}, noHostEnvRule{}, noChdirRule{}, noSkipRule{},
		fakeClockEpochRule{}, noGlobalSwapRule{}, parallelRule{},
		prodSetenvRule{}, prodSleepRule{}}
}
```

`Name()` returning the constant, rather than one name for the whole set, is
what keeps the registration and the exemption in one name space: an
`//testpolicy:allow` comment covers a violation by its `Violation.Rule`, and
`checkRuleNames` refuses a nil rule, an empty `Name()` and a repeated `Name()`
across the shared and extra rules before any directory is read.

One rule per constant, and each reads only what it needs:

| Rule | Today's emitter | `ScanContext` fields | What `Check` walks | Helper that moves with it | Fixture rows that pin it |
|---|---|---|---|---|---|
| `RuleNoEnv` (`no-env`) | `bannedTestCalls` for `os.Setenv`/`os.Unsetenv` and `bannedTestMethods` for `Setenv`, both read by `checkTestCall` | `Tests`, `Fset`, `Imports` | test-file call expressions: `Setenv` on a `*testing.T/B/F/TB` receiver, else the `os` call | `isTestingReceiver`, `isTestingType` | `env` (2), `setenv_in_subtest` (1); `nontesting_receiver` stays clean |
| `RuleNoHostEnv` (`no-host-env`) | `checkTestCall`'s `os.Getenv`/`LookupEnv` and `os.UserHomeDir`/`UserConfigDir`/`UserCacheDir` switch, plus `checkEnvValue` for a reader used as a value | `Tests`, `Prod`, `Fset`, `Imports` | test-file calls, then selectors the calls did not mark, in one walk (the `called` set is what stops a call being reported twice) | `envName`, `constValue`, `packageConsts` | `host_env_reads` (6), `host_env_unjudged` (2), `host_env_value` (10); `host_env_declared`, `host_env_other`, `host_env_seam` stay clean |
| `RuleNoChdir` (`no-chdir`) | `bannedTestCalls` for `os.Chdir` and `bannedTestMethods` for `Chdir` | `Tests`, `Fset`, `Imports` | test-file call expressions, by the same receiver test as `no-env` | `isTestingReceiver`, `isTestingType` | `chdir` (2) |
| `RuleNoSkip` (`no-skip`) | `bannedTestMethods` for `Skip`, `Skipf`, `SkipNow` | `Tests`, `Fset`, `Imports` | test-file call expressions on a testing receiver | `isTestingReceiver`, `isTestingType` | `skip` (3) |
| `RuleFakeClockEpoch` (`fake-clock-epoch`) | `bannedTestCalls` for `clockwork.NewFakeClock` | `Tests`, `Fset`, `Imports` | test-file call expressions with an import path | — | `fake_clock_epoch` (1) |
| `RuleNoGlobalSwap` (`no-global-swap`) | `checkTestFile`'s `*ast.AssignStmt` branch | `Tests`, `Prod`, `Fset`, `Imports` | assignment statements with `Tok == token.ASSIGN`: an unresolved identifier that names a package variable, or `pkg.Name` on an import | `packageVars` | `global_swap` (1); `global_shadow`, `global_blank` stay clean |
| `RuleParallel` (`parallel`) | `checkTestFile`'s top-level `*ast.FuncDecl` loop | `Tests`, `Fset`, `Imports` | test functions whose one parameter is `*testing.T` and whose body calls no `t.Parallel()` directly, not descending into a `t.Run` closure | `testingParam`, `isTestingType`, `callsMethodOn` | `parallel_missing`, `parallel_subtest_only` (1 each) |
| `RuleProdSetenv` (`prod-no-setenv`) | `checkProdFile` | `Prod`, `Fset`, `Imports` | production-file calls to `os.Setenv`/`os.Unsetenv` in a package not named `main` | — | `prod_setenv` (1) |
| `RuleProdSleep` (`prod-no-sleep`) | `checkProdFile`, guarded by the package's imports | `Prod`, `Fset`, `Imports` | production-file `time.Sleep` calls in a package where some production file imports `github.com/jonboulle/clockwork` | `importsPath` | `prod_sleep_clock` (1) |

None of the nine reads `ctx.Allow`: the module applies every exemption to every
rule's output after the rules run, so a rule carries no allow code and no
exemption logic of its own. Three helpers the rules need live in `scan.go`
today — `packageVars`, `packageConsts` and `importsPath` — and move into
gastown's rule file with them.

The two banned-call maps hold shared and extra entries mixed today. Adoption
leaves gastown's rule file the extra entries only — `os.Setenv`/`os.Unsetenv`,
`os.Chdir` and `clockwork.NewFakeClock` in `bannedTestCalls`, plus all of
`bannedTestMethods`, which the module has no copy of — while the `os/exec`,
`os.WriteFile`/`os.OpenFile`, `os.Chmod` and `#!` branches go with the shared
rules, because the module's `CheckTestCall` and `checkTestFile` already carry
them. An extra rule built on a reused `CheckTestCall` therefore cannot report a
shared name; it must walk with the trimmed maps.

**What the registration must not do.**

- **No extra rule may be named `test-file`.** It is the module's own
  registration, and `checkRuleNames` fails the whole scan on the repeat.
- **No extra rule may emit a shared name or `allow-reason`.** Exemptions are
  applied by the violation's rule string, so a gastown rule emitting `no-sleep`
  would have its finding suppressed by an exemption written for the shared
  rule, and the same call would also be reported by `testFileRule`.
- **`RuleAllowReason` must be deleted, not registered.** The module's
  `CollectAllows` emits it; gastown's `applyAllows` emits it too today, so
  keeping both reports every unreasoned exemption twice.
- **The nine strings must not change.** Of the tree's 367 live
  `//testpolicy:allow` comments, 251 name one of the nine — 121 `parallel`, 57
  `no-env`, 27 `no-global-swap`, 20 `prod-no-setenv`, 19 `no-chdir`, 4
  `no-host-env`, 3 `no-skip`, none `fake-clock-epoch` or `prod-no-sleep` — and
  the rest name a shared rule or `no-git`. A renamed rule stops matching its
  exemptions and fails `TestPolicy` on a site nobody edited:

  ```sh
  git grep -h -oE '//testpolicy:allow [a-z-]+' origin/main -- '*.go' |
    sed 's#.*allow ##' | sort | uniq -c | sort -rn
  ```

**What adoption changes in the scanner's callers.** Two functions call
gastown's allow machinery today: `ScanDirWithExemptions` in `scan.go` and
`GitFreeFindings` in `gitfree.go`. The module splits the pass into
`CollectAllows(fset, files)` and `applyAllows(allowed, vs)`, so both callers
rewire: `ScanDirWithExemptions` disappears into the module's scanner, and
`GitFreeFindings` collects the allowed keys once and applies them inside its
helper fixpoint loop.

**The way this fails open.** `TestPolicy` is green on today's tree, so every
extra-rule finding in a converted package is already exempted and the
unconverted list covers the rest: an adoption that takes the module's scanner
but forgets to pass `gastownRules()` leaves `TestPolicy` green with nine rules
switched off. The fixture rows in `TestScanDirFixtures` and
`TestScanDirWithExemptions` are the only guard, which is why S7.1's exit
criterion is those rows unchanged.

## What stays in gastown: the six lists, the policy test, the checks outside the seam

The module carries no list file, no ratchet constant, and no tree-level check.
Per the plan's §7.4 the rig keeps its data files, their counts and
`policy_test.go`; what moves is the mechanism that reads them.

| List | Entries at origin/main | Pin | Reader and check that stay | Module counterpart |
|---|---:|---|---|---|
| `unconverted.txt` | 1 (`internal/beads`) | `maxUnconverted = 1` | `TestPolicy`; membership read with the module's `ReadList` | none |
| `overbudget.txt` | 0 | `maxOverBudget = 0` | `ParseOverBudget`, `ReadOverBudget`, `CheckOverBudget` | none |
| `realgit.txt` | 0 | `maxRealGit = 0` | `RealGitFiles`, `CheckRealGit`, `ConvertedShare` | none |
| `gitfree.txt` | 24 | `minGitFree = 24` | `GitFreeFindings`, `CheckGitFree` | none |
| `docker.txt` | 2 | kept exact by `TestDockerTier` | `StartsContainers`, `CheckDockerTier` | none |
| `sweep-host.txt` | 11 | `maxSweepHost = 11` | `ReadSweepHost`, `ParseSweepHost`, `CheckSweepHost*` | none |

`unconverted.txt`'s membership and `realgit.txt`'s are the ones the scanner
adoption touches, and only through `ReadList`: S7.2 takes the module's `ReadList`
in place of gastown's, and its test (`TestReadListAndPackageDirs`) replaces
`list_test.go`. The module's `RelPath` also takes the place of gastown's
unexported `relPath` in `realgit.go`, which S7.2's file list above misses: its
callers are in `realgit.go` and `tiermain_test.go`. The list files, their header
prose and their line counts do not change.

`policy_test.go` stays whole. These parts of it survive adoption unchanged:

- the `-seed` flag and its print-instead-of-fail path;
- `maxUnconverted` and `maxOverBudget`, and the shrink-only ratchet that fails
  a listed package which already passes (`originallyListed`, the `realgit`
  membership exemption);
- the per-package loop, its exemption log line, and the rule that only
  converted packages log exemptions;
- `CheckOverBudget` over the whole tree, and `CheckContracts` with its
  integration-name findings filtered by `originallyListed` while
  fake-contract findings are never filtered.

Three things in it change, and only at the seam: the scan call becomes
`shared.ScanDirWithExemptions(dir, gastownRules()...)`; `RuleAllowReason`
becomes the module's constant; and the five shared rule constants come from the
module, so `rules.go` keeps only the nine and their helpers.

The checks below are not package-scanner rules — each judges a tree, a list or a
run, so none registers through the `Rule` seam and all stay in gastown:

| Check | Rule names it emits | Driven by |
|---|---|---|
| `CheckContracts`, `RuleContract`, `RuleIntegrationName` | `fake-contract`, `integration-name` | `TestPolicy`, over every directory |
| `CheckGitFree`, `GitFreeFindings`, `RuleNoGit` | `no-git` | `TestGitFree`; its allow pass becomes the module's `CollectAllows` plus `applyAllows` in S7.0 |
| `CheckDockerTier`, `StartsContainers` | none | `TestDockerTier` |
| `CheckRealGit`, `RealGitFiles`, `ConvertedShare` | none | `TestRealGit` |
| `CheckOverBudget`, `ParseOverBudget`, `ReadOverBudget` | none | `TestPolicy` |
| the `CheckSweepHost*` family | none | `TestSweepHost*` |
| `CheckUnitTierMains`, `AllowedTools`, `RuleAllowTools` | `allow-tools` | `TestUnitTierMains`, `TestAllowedTools`; a constant, not a scanner rule |
| `growOnlyHeader` (`unionmerge_test.go`) | none | the grow-only list headers |

`RuleTierSplit` and `checkTierSplit` are devops's own rule and never existed in
gastown (`git grep TierSplit` at origin/main finds nothing). The devops plan's
§6 counts "eight gastown rules the copy dropped" where this tree names nine; the
plan gives the count but not the list, so which one its eight omits is not
determined here. The table above is the list to build from: each of the nine
has a constant in `rules.go` and a fixture row that pins it, and a slice-7 bead
written for eight would leave one rule unregistered and its exemptions dead.

## Implementation beads for gastown's adoption (slice 7)

Sizes are story points in this rig's scale, and every bead is under 8. The order
below is the dependency order, which is not the numeric order of the ids: S7.4
and S7.5 are blocked only by S7.2 and start before S7.3, whose own precondition
is a module version that carries devops's slice 6a. The version column is the
module release each bead's first file needs, per dv-oqr3's build order (§8 of
the devops plan: slice N ships as `v0.N.0`); the class column above says which
of those files the module already carries.

| Order | Bead | Points | Needs module | Blocked by |
|---:|---|---:|---|---|
| 1 | S7.0 — scanner, allow machinery, Rule seam | 5 | `v0.1.0` or later | operator publishing |
| 2 | S7.1 — the nine extra rules as module Rules | 5 | `v0.1.0` or later | S7.0 |
| 3 | S7.2 — lists, split and tier parsing | 3 | `v0.2.0` or later | S7.0 |
| 4 | S7.4 — changed/triggers and presubmit selectors | 5 | `v0.4.0` or later | S7.2 |
| 5 | S7.5 — slow/scripts checks and tierwall | 5 | `v0.5.0` or later | S7.2 |
| 6 | S7.3 — CPU, budget and `cmd/budget` | 5 | the first version carrying 3b and 6a | S7.2 |
| 7 | S7.6 — the per-Test ratchet | 3 | the same version | S7.3 |
| 8 | S7.7 — delete the last duplicate, verify the tree | 2 | any | S7.1, S7.4, S7.5, S7.6 |

1. **S7.0 — add `gt.local/testpolicy` and adopt the scanner, allow machinery and
   Rule seam.** 5 points. Needs `v0.1.0`. Files: `go.mod`, `go.sum`, `scan.go`,
   `allow.go`, `srccache.go`, `main_test.go`. Make `ScanDir` and
   `ScanDirWithExemptions` thin wrappers over the module's `ScanDirWithRules` and
   pass gastown's rules as arguments; decide whether `srccache.go` survives or
   is deleted with `TestCheckoutFilesAreParsedOnce`. Exit: `TestScanDirFixtures`,
   `TestScanDirWithExemptions` and `TestPolicy` pass unchanged; `go build ./...`
   exits 0.
2. **S7.1 — adopt the module's shared rules and keep the nine extra rules as
   module Rules.** 5 points. Needs `v0.1.0`. Blocked by S7.0. Files: `rules.go`,
   `scan_test.go`, the 15 shared fixture files. Delete the moved checks and
   constants; write the nine extra rules as `Rule` types that recompute what
   they need from `ScanContext`; prove each rule row still fires. Exit: every
   row of `TestScanDirFixtures` and `TestPolicy` unchanged.
3. **S7.2 — adopt the module's lists, split and tier-wall parsing.** 3 points.
   Needs `v0.2.0`. Blocked by S7.0. Files: `list.go`, `split.go`, `tier.go`,
   `list_test.go`, `split_test.go`, `tier_test.go`. Delete the gastown copies;
   the six lists stay. Gastown's `PackageDirs` nested-module skip and the
   module's `RelPath` have to be reconciled before both can live in one
   function. Exit: `TestSplitTestArgs`, `TestPartitionPackages`, `TestTagsArgs`,
   `TestParsePackageWalls`, `TestWallOverruns` pass, and `cmd/budget` builds.
4. **S7.4 — adopt the module's changed/triggers code and presubmit selectors.**
   5 points. Needs `v0.4.0`. Blocked by S7.2 and on slices 4a (dv-ebt2.6) and 4b
   (dv-ebt2.7) reaching the module. Files: `internal/land/changed.go`,
   `internal/land/cmd/changedpkgs`, `internal/presubmit/cmd/errbranch`,
   `internal/presubmit/cmd/gomodrationale` — they live outside
   `internal/testpolicy` in gastown, a layout difference from devops. Exit:
   `make presubmit`'s package selection and its two judges are unchanged.
5. **S7.5 — adopt the module's slow/scripts checks and the tierwall command.**
   5 points. Needs `v0.5.0`. Blocked by S7.2 and on slices 5a (dv-ebt2.8), 5b
   (dv-ebt2.9) and 5c (dv-ebt2.10). Files: `internal/testpolicy/cmd/tierwall/*`,
   `internal/shelltier/*`, the `scripts/` tree, the Makefile's two `cmd/tierwall`
   invocations. Keep gastown's shell manifest. Exit: `make gate`'s tierwall stage
   and `make test-slow` behave as before.
6. **S7.3 — adopt the module's CPU and budget code and `cmd/budget`.** 5 points.
   Needs the first published version carrying slice 3b (dv-ebt2.5) and 6a
   (dv-ebt2.11). Blocked by S7.2. Files: `cpu.go`, `budget.go`, `overbudget.go`,
   their tests, `cmd/budget/*`. Keep `overbudget.txt` and the Makefile's
   invocations. Exit: `make gate`'s budget stage and `make tier-check` behave as
   before.
7. **S7.6 — adopt the module's per-Test ratchet code.** 3 points. Needs the same
   version as S7.3. Blocked by S7.3 and on slice 6a (dv-ebt2.11). Files:
   `overbudget.go`, `policy_test.go`, the ratchet constants. Keep gastown's
   lists and their pins. Exit: every ratchet fails the same way on the same
   input.
8. **S7.7 — delete the last duplicate and verify the tree.** 2 points. Blocked
   by S7.1, S7.4, S7.5 and S7.6. Exit: `git grep` finds one copy of each moved
   function; `make gate` exits 0; no `replace` for `gt.local/testpolicy`
   remains.

### What the operator must do

Publishing is the operator's act, not any polecat's: the devops plan gives
version tags to the operator's own account so the registry's trust stays with
the human who owns it. A `replace` to a local path is no substitute for a
published version — S7.7's exit criterion requires none to remain. Four steps:

1. **Create the module's repository** under the account the Forgejo Go registry
   serves, with `testpolicymod/`'s contents at its root (`go.mod` included) and
   the registry's mapping from `gt.local/testpolicy` to it — the mapping
   `gt.local/devops` already has. At this bead's read no such repository existed:
   `sloan/testpolicy`, `sloan/testpolicymod` and `sloan/gt-testpolicy` each
   answered `Repository not found`, and `sloan/devops` carries only its own
   `v0.1.0`–`v0.1.7` tags. Whether the module's source moves there or is copied
   from `devops` each release is the operator's choice; slices 1 to 6 keep
   devops green either way.
2. **Tag each slice as its version**, per the devops plan's build order: slice 1
   `v0.1.0`, slice 2 `v0.2.0`, slice 3 `v0.3.0`, slice 4 `v0.4.0`, slice 5
   `v0.5.0`, slice 6 `v0.6.0`. Tag the commit where both the module's own
   `go test ./...` and devops's adoption of that slice are green: a slice is one
   version plus a rig's adoption, and a tag taken before devops's half proves
   nothing gastown's bead can rely on.
3. **Confirm gastown can resolve each tag before the bead is dispatched.** From
   a gastown checkout, `go get gt.local/testpolicy@v0.N.0` must record the
   requirement in `go.mod` and `go.sum` through the registry. A version that
   does not resolve leaves the bead blocked, not green.
4. **Tell the bead owner the repository and the version, and re-dispatch the
   bead the table above names.** Until `v0.1.0` exists no S7 bead can start;
   until `v0.2.0` none past S7.1; S7.3 and S7.6 wait for the first version that
   carries devops's 3b and 6a.

Publishing is the same gesture for all seven slices; only step 4 is new at
gastown's end. No other operator action is needed: the six lists, their pins and
`policy_test.go` stay in gastown, so adoption publishes no gastown data.

## Gate

This is a docs-only change: the only file it touches is this one. Both halves
checked it the same way:

```
$ go run ./cmd/docs-lint
exit 0, no findings
```

`make lint` is the landing gate. `gt done` runs it as the first stage of `make
presubmit`, and the landing worker runs the full gate again on the merge
candidate; a polecat does not run it separately, so its output is in the
landing record rather than here. `make lint` is `docs-lint` plus `lintpin`,
`golangci-lint`, `repo-guards` and `guardlint` — the last four over a Go tree
this change does not touch.
