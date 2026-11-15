> Status: historical (2026-11). Merged in: gt-zq17ds. Not maintained.

# Gastown's internal/testpolicy against the shared module: adoption inventory

**Goal:** the evidence slice 7 of the shared testpolicy module needs: every file
of gastown's `internal/testpolicy` classed against the module
`gt.local/testpolicy` (the `testpolicymod/` directory of `sloan/devops`), each
class cited by file and function name, and every exported name with the callers
it has outside the package.

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
check.

## Implementation beads for gastown's adoption (slice 7)

Sizes are story points in this rig's scale. Each is blocked by the operator's
publication of the module version that carries its slice; the class column
above says which of these the module already carries.

1. **S7.0 — add `gt.local/testpolicy` and adopt the scanner, allow machinery and
   Rule seam.** 5 points. Files: `go.mod`, `go.sum`, `scan.go`, `allow.go`,
   `srccache.go`, `main_test.go`. Make `ScanDir` and `ScanDirWithExemptions`
   thin wrappers over the module's `ScanDirWithRules` and pass gastown's rules
   as arguments; decide whether `srccache.go` survives or is deleted with
   `TestCheckoutFilesAreParsedOnce`. Exit: `TestScanDirFixtures`,
   `TestScanDirWithExemptions` and `TestPolicy` pass unchanged; `go build ./...`
   exits 0.
2. **S7.1 — adopt the six shared rules and keep the nine extra rules as module
   Rules.** 5 points. Depends on S7.0. Files: `rules.go`, `scan_test.go`, the
   15 shared fixture files. Delete the moved checks and constants; write the
   nine extra rules as `Rule` types that recompute what they need from
   `ScanContext`; prove each rule row still fires. Exit: every row of
   `TestScanDirFixtures` and `TestPolicy` unchanged.
3. **S7.2 — adopt the module's lists, split and tier-wall parsing.** 3 points.
   Depends on S7.0. Files: `list.go`, `split.go`, `tier.go`, `list_test.go`,
   `split_test.go`, `tier_test.go`. Delete the gastown copies; the six lists
   stay. Gastown's `PackageDirs` nested-module skip and the module's `RelPath`
   have to be reconciled before both can live in one function. Exit:
   `TestSplitTestArgs`, `TestPartitionPackages`, `TestTagsArgs`,
   `TestParsePackageWalls`, `TestWallOverruns` pass, and `cmd/budget` builds.
4. **S7.3 — adopt the module's CPU and budget code and `cmd/budget`.** 5 points.
   Depends on S7.2 and on slices 3b (dv-ebt2.5) and 6a (dv-ebt2.11) reaching the
   module. Files: `cpu.go`,
   `budget.go`, `overbudget.go`, their tests, `cmd/budget/*`. Keep
   `overbudget.txt` and the Makefile's invocations. Exit: `make gate`'s budget
   stage and `make tier-check` behave as before.
5. **S7.4 — adopt the module's changed/triggers code and presubmit selectors.**
   5 points. Depends on S7.2 and on slices 4a (dv-ebt2.6) and 4b (dv-ebt2.7).
   Files:
   `internal/land/changed.go`, `internal/land/cmd/changedpkgs`,
   `internal/presubmit/cmd/errbranch`, `internal/presubmit/cmd/gomodrationale` —
   they live outside `internal/testpolicy` in gastown, a layout difference from
   devops. Exit: `make presubmit`'s package selection and its two judges are
   unchanged.
6. **S7.5 — adopt the module's slow/scripts checks and the tierwall command.**
   5 points. Depends on S7.2 and on slices 5a (dv-ebt2.8), 5b (dv-ebt2.9) and 5c
   (dv-ebt2.10). Files:
   `internal/testpolicy/cmd/tierwall/*`, `internal/shelltier/*`, the `scripts/`
   tree, the Makefile's two `cmd/tierwall` invocations. Keep gastown's shell
   manifest. Exit: `make gate`'s tierwall stage and `make test-slow` behave as
   before.
7. **S7.6 — adopt the module's per-Test ratchet code.** 3 points. Depends on
   S7.3 and on slice 6a (dv-ebt2.11). Files: `overbudget.go`, `policy_test.go`, the ratchet
   constants. Keep gastown's lists and their pins. Exit: every ratchet fails
   the same way on the same input.
8. **S7.7 — delete the last duplicate and verify the tree.** 2 points. Depends
   on S7.1, S7.4, S7.5, S7.6. Exit: `git grep` finds one copy of each moved
   function; `make gate` exits 0; no `replace` for `gt.local/testpolicy`
   remains.

## Gate

This is a docs-only change: the only file it touches is this one.

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
