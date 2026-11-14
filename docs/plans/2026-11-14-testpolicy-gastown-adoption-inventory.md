> Status: historical (2026-11). Merged in: gt-zq17ds. Not maintained.

# Gastown's internal/testpolicy against the shared module: adoption inventory

**Goal:** the evidence slice 7 of devops's shared testpolicy module needs: every
file of gastown's `internal/testpolicy`, what it holds, whether the module's
extraction slices move it, its exported names and their callers, gastown's extra
rules, and the beads for gastown's own adoption.

The devops plan `docs/plans/2026-11-02-testpolicy-shared-module.md` section 9
could not inventory gastown's copy because the copy was unreadable on the devops
host (dv-oqr3). In this repository the copy is the repository, so this document
inventories gastown's side.

## What was read, and the read that was not made

Every claim below is from `origin/main` of this repository (commit `48d233810`)
and from the beads that describe the module. Commands run:

```
git ls-tree -r --name-only origin/main internal/testpolicy
git grep -n 'gastown/internal/testpolicy"' origin/main -- '*.go'
git grep -n 'testpolicy\.[A-Za-z0-9_]*' origin/main -- internal/testpolicy/cmd
go doc ./internal/testpolicy
git show origin/main:<file> | wc -l
bd show dv-oqr3 dv-ebt2 dv-ebt2.1 dv-ebt2.2
```

**The module itself was not read.** Town rule gt-690mog (operator rule
2026-11-12) says a polecat reads only its own rig's repository and does not look
for, clone or guess another rig's code. The devops repository is another rig and
its local directory here is empty. The exact question this document could not
answer is in the bead notes for gt-zq17ds and in escalation `hq-wisp-zdbxc`:
the file list and exported names of `testpolicymod` at `origin/main`, the exact
`Rule`, `ScanContext` and `ScanDirWithRules` signatures, and its fixture
directories.

The classes below therefore use the module's declared contents: the devops
bead that shapes each extraction slice says which symbols and fixtures it
moves. They are not a comparison against the module's current text. The finer
question — whether gastown's copy is identical, trimmed or drifted from the
module — is marked **not determined** wherever it needs the module.

## The module's declared shape

From dv-ebt2.1 (slice 1a), dv-ebt2.2 (slice 1b), and the parent dv-ebt2's
children:

- Module path `gt.local/testpolicy`, nested at `testpolicymod/` in devops until
  the operator publishes it. The parent's 2026-11-05 comment records the
  operator's acceptance of "gt.local/testpolicy, operator-pushed v* tags,
  per-rig rules through the Rule interface passed as arguments".
- Slice 1a: `Violation`, `Exemption`, `ScanDir`, `ScanDirWithExemptions`, the
  package walk, the AST parse, `collectAllows`, `applyAllows`; and the new
  `ScanContext` (Fset, Tests, Prod, Allow, Imports), the `Rule` interface
  (`Name`, `Check`), and `ScanDirWithRules(dir string, extra ...Rule)`. Rules are
  arguments, never a package-level registry; an empty `Name` or two Rules with
  one `Name` is refused.
- Slice 1b: six rules — `no-sleep`, `no-subprocess`, `no-network`, `no-build`,
  `no-exec-files`, `allow-reason` — with `bannedTestCalls`, `checkTestFile`,
  `checkTestCall`, `markReadOnlyLits`, `readOnlyStringFuncs`, `stringLit`,
  `execMode`, `netDialerType`, `isTestName` (dv-ebt2.2's list; `isTestName` has
  no gastown counterpart today, which is itself a drift signal), and the
  `clean`, `allow_ok`, `allow_noreason`, `sleep`, `exec_*`, `shebang*` and
  `network*` fixtures.
- Slices 2 to 6a: lists, split and tier-wall parsing (dv-ebt2.3); the CPU check
  and its exemption list (dv-ebt2.4); the budget check and `cmd/budget`
  (dv-ebt2.5); `changed.go` and `triggers.go` (dv-ebt2.6); the presubmit
  selector commands (dv-ebt2.7); the slow list, scripts check and script tree
  (dv-ebt2.8); `cmd/tierwall` and `cmd/slowcheck` (dv-ebt2.9); `cmd/teststats`
  and `cmd/scripts` (dv-ebt2.10); the per-Test ratchet code and its fixtures
  (dv-ebt2.11).

## Every file of gastown's internal/testpolicy

Class is one of **gastown-only** (no slice moves it), **shared** (a slice moves
its code), or **split** (part moves, part stays). A shared file's finer class
among identical, trimmed and drifted is not determined; see "What could not be
determined".

### Source files and tests

| File | Lines | Holds | Class |
|---|---:|---|---|
| `allow.go` | 64 | `allowPrefix`, `allowKey`, `Exemption`, `applyAllows` | shared (1a) |
| `scan.go` | 161 | package doc; `Violation` and `String`; `ScanDir`; `ScanDirWithExemptions`; `packageVars`; `packageConsts`; `importsPath`; `imports` | shared (1a) |
| `srccache.go` | 191 | `repoSources`, `sourceCache`, `cachedFile`, `cachedMatch`, `sourceSet`, `cacheable`, `cacheableUnder`, `within`, `moduleRootAbove`, `tempRoots`, `checkoutRoot`; imports `internal/callercwd` | gastown-only |
| `rules.go` | 434 | 15 rule constants; `bannedTestCalls`; `bannedTestMethods`; `checkTestFile`; `checkTestCall`; `checkEnvValue`; `checkProdFile`; `isTestingReceiver`; `testingParam`; `isTestingType`; `callsMethodOn`; `envName`; `constValue`; `stringLit`; `execMode`; `netDialerType`; `markReadOnlyLits`; `readOnlyStringFuncs` | split (1b moves six rules; nine stay) |
| `contracts.go` | 156 | `RuleContract`, `RuleIntegrationName`, `contractName`, `CheckContracts`, `hasIntegrationTag`, `requiresIntegration`, `collectTags` | gastown-only |
| `docker.go` | 188 | `containerEntryPoints`, `StartsContainers`, `CheckDockerTier`, `exportedEntryWrappers`, `callsEntryPoint` | gastown-only |
| `gitfree.go` | 253 | `RuleNoGit`, `gitPkgPath`, `testutilPkgPath`, `realGitFuncs`, `GitFreeFindings`, `unitTierTestFiles`, `helperKey`, `directGitCalls`, `helperCalls`, `firstNoGitWithin`, `CheckGitFree` | gastown-only |
| `realgit.go` | 119 | `RealGitFiles`, `CheckRealGit`, `ConvertedShare`, `relPath`, `sortedKeys` | gastown-only |
| `sweephost.go` | 137 | `SweepHostEntry`, `ReadSweepHost`, `ParseSweepHost`, `CheckSweepHostPackages`, `CheckSweepHostReasons`, `CheckSweepHostReasonFiles`, `SweepRunnerPackages`, `CheckSweepHostCount` | gastown-only |
| `tiermain.go` | 123 | `unittierPkgPath`, `tierRunners`, `UnitTierMain`, `callsTierRunner`, `CheckUnitTierMains`, `AllowedTools`, `RuleAllowTools`, `isAllowTools` | gastown-only |
| `overbudget.go` | 103 | `OverBudgetEntry`, `TrackedRun`, `beadIDPattern`, `ParseOverBudget`, `ReadOverBudget`, `CheckOverBudget` | shared (3b/6a); `overbudget.txt` stays |
| `list.go` | 61 | `ReadList`, `PackageDirs` | shared (2) |
| `split.go` | 127 | `goTestBoolFlags`, `goTestValueFlags`, `flagName`, `SplitTestArgs`, `TagsArgs`, `PartitionPackages` | shared (2) |
| `tier.go` | 94 | `FastTierMaxWall`, `PackageWall`, `WallSummary`, `ParsePackageWalls`, `WallOverruns` | shared (2) |
| `cpu.go` | 69 | `CPUDirEnv`, `CPUTime`, `CPUSource`, `cpuRecord`, `cpuFile`, `WriteCPU`, `ReadCPU` | shared (3a) |
| `budget.go` | 301 | `TestTime`, `Overrun`, `testEvent`, `pkgOutput`, `WatchBudget`, `SlowRun`, `PkgTime`, `BudgetResult`, `IdleTop`, `IdleRatioFactor`, `IdleWall`, `IdleReport`, `IdleJudged`, `medianDuration`, `WatchBudgetTracked`, `StrictBudgetEnv`, `EnforceBudget`, `BudgetFails` | shared (3b) |
| `integration.go` | 96 | `IntegrationPackages`, `addsIntegrationFiles`, `skipForGoTool` | gastown-only |
| `policy_test.go` | 134 | `seed`, `maxUnconverted`, `maxOverBudget`, `TestPolicy` | gastown-only |
| `main_test.go` | 12 | `TestMain` through `unittier.Main` | gastown-only |
| `unionmerge_test.go` | 73 | `growOnlyHeader`, `TestUnionMergeOnlyOnGrowOnlyLists` | gastown-only |
| `scan_test.go` | 141 | `TestScanDirFixtures`, `TestScanDirWithExemptions`, `TestCheckContracts`, `TestRequiresIntegration` | split (1a tests move; contracts tests stay) |
| `budget_test.go` | 323 | `TestWatchBudget`, `TestWatchBudgetSysTimeNotJudged`, `TestWatchBudgetUnmeasured`, `TestWatchBudgetBuildOutput`, `TestWatchBudgetPassSummaryOnly`, `TestWatchBudgetFailWholePackageInterleaved`, `TestIdleJudged`, `TestIdleJudgedTopIsBounded`, `TestEnforceBudget`, `TestEnforceBudgetReasonNamesTheIdleReading`, `TestBudgetFails` | shared (3b) |
| `budget_integration_test.go` | 186 | the budget integration tests and `runBudget`/`fixturePkg` | shared (3b) |
| `overbudget_test.go` | 140 | `TestParseOverBudget`, `TestCheckOverBudget`, `TestWatchBudgetTracked`, `TestWatchBudgetSlowWall` | shared (3b/6a) |
| `cpu_test.go` | 31 | `TestCPURecordRoundTrip` | shared (3a) |
| `list_test.go` | 48 | `TestPackageDirsSkipsNestedModules` | shared (2) |
| `split_test.go` | 74 | `TestSplitTestArgs`, `TestPartitionPackages`, `TestTagsArgs` | shared (2) |
| `tier_test.go` | 65 | `TestParsePackageWalls`, `TestWallOverruns` | shared (2) |
| `integration_test.go` | 51 | `TestIntegrationPackages` | gastown-only |
| `docker_test.go` | 89 | `TestStartsContainersFixtures`, `TestCheckDockerTierFixtures`, `TestDockerTier` | gastown-only |
| `gitfree_test.go` | 124 | `minGitFree`, `TestGitFreeFindingsFixtures`, `TestCheckGitFreeFixtures`, `TestGitFree` | gastown-only |
| `realgit_test.go` | 128 | `TestRealGit`, `TestRealGitFixtures` | gastown-only |
| `sweephost_test.go` | 246 | the nine sweep tests, `TestSweepHostFixtures` among them | gastown-only |
| `tiermain_test.go` | 127 | `TestUnitTierMainFixtures`, `TestCheckUnitTierMainsFixtures`, `TestUnitTierMain`, `TestAllowedToolsFixture`, `TestAllowedTools` | gastown-only |
| `srccache_test.go` | 154 | the five srccache tests, from `TestCheckoutFilesAreParsedOnce` to `TestModuleRootIsTheNearestGoModAbove` | gastown-only |
| `cmd/budget/main.go` | 619 | the budget runner: flags, `listPackages`, `runJudged`, `reportIdle`, `reportOver`, `usage`, and the `-exec` wrapper mode | shared (3b) |
| `cmd/budget/die_unix.go` | 36 | the wrapper's signal exit | shared (3b) |
| `cmd/budget/main_test.go` | 320 | the runner's tests | shared (3b) |
| `cmd/budget/testmain_test.go` | 12 | `TestMain` through `unittier.Main` | shared (3b) |
| `cmd/tierwall/main.go` | 766 | the wall reporter: flags, JSONL reader, series writer | shared (5b) |
| `cmd/tierwall/main_test.go` | 1234 | the reporter's tests | shared (5b) |
| `cmd/tierwall/testmain_test.go` | 12 | `TestMain` through `unittier.Main` | shared (5b) |
| `cmd/integrationvet/main.go` | 305 | the integration vet: `main`, flags and the `testpolicy.IntegrationPackages` call | gastown-only |
| `cmd/integrationvet/main_integration_test.go` | 77 | the vet's integration test | gastown-only |
| `cmd/integrationvet/main_test.go` | 396 | the vet's tests | gastown-only |
| `cmd/gateverdict/main.go` | 35 | the `gate: FAILED at ...` verdict writer; no recipe calls it yet | gastown-only |
| `cmd/gateverdict/main_test.go` | 38 | its tests | gastown-only |

### The six lists

All six are gastown data and stay in gastown, whatever the module does with the
parsers.

| File | Lines | What it lists | Class |
|---|---:|---|---|
| `unconverted.txt` | 3 | packages not yet converted to the unit rules; ratcheted by `maxUnconverted` | gastown-only |
| `overbudget.txt` | 5 | converted packages over the time budget, each with its bead; `maxOverBudget` | gastown-only |
| `realgit.txt` | 5 | unit-tier files that still run real git; `maxRealGit` | gastown-only |
| `gitfree.txt` | 30 | packages whose unit tier may run no git; `minGitFree` | gastown-only |
| `docker.txt` | 9 | packages whose unit-tier tests start a container; `TestDockerTier` | gastown-only |
| `sweep-host.txt` | 28 | integration packages left to the host sweep; `maxSweepHost` | gastown-only |

### testdata fixtures

83 files in 42 directories. The 15 files of the 14 shared fixtures move in
slice 1b; the other 68 files pin gastown's own rules and lists.

| Fixtures | Files | What they pin | Class |
|---|---:|---|---|
| `clean`, `allow_ok`, `allow_noreason`, `sleep`, `exec_file`, `exec_git`, `exec_go`, `exec_other`, `shebang`, `shebang_passthrough`, `shebang_readonly`, `shebang_written`, `network`, `network_more` | 15 | the six shared rules and the allow machinery | shared (1b) |
| `allowtools`, `tiermain` | 9 | `AllowedTools` and the tier-runner check | gastown-only |
| `budget`, `dockertier`, `contracts` | 11 | the budget, docker-tier and contract fixtures | gastown-only |
| `gitfree`, `realgit`, `docker` | 25 | the no-git, real-git and docker checks | gastown-only |
| `chdir`, `env`, `skip`, `fake_clock_epoch`, `global_blank`, `global_shadow`, `global_swap`, `host_env_declared`, `host_env_other`, `host_env_reads`, `host_env_seam`, `host_env_unjudged`, `host_env_value`, `nontesting_receiver`, `parallel_missing`, `parallel_subtest_only`, `prod_setenv`, `prod_sleep_clock`, `setenv_in_subtest` | 22 | gastown's rule rows that are not the six shared ones | gastown-only |
| `integration_skipped` | 1 | `IntegrationPackages` | gastown-only |

## Exported names and their callers outside the package

`git grep` at `origin/main` finds only two packages outside `internal/testpolicy`
that import this one: `internal/testpolicy/cmd/budget` and
`internal/testpolicy/cmd/integrationvet`. Every other exported name has no
caller outside the package; the package's own code and tests are its only
users. No file in the repo-root `cmd/`, `internal/land` or `internal/presubmit`
imports it; the policy tests are the entry point for the checkers.

| Exported name | Declared in | Caller outside the package |
|---|---|---|
| `ScanDir`, `ScanDirWithExemptions` | `scan.go` | none (tests only) |
| `Violation`, `Violation.String` | `scan.go` | none |
| `Exemption` | `allow.go` | none |
| `CheckContracts`, `RuleContract`, `RuleIntegrationName` | `contracts.go` | none |
| `StartsContainers`, `CheckDockerTier` | `docker.go` | none |
| `RuleNoGit`, `GitFreeFindings`, `CheckGitFree` | `gitfree.go` | none |
| `RealGitFiles`, `CheckRealGit`, `ConvertedShare` | `realgit.go` | none |
| `UnitTierMain`, `CheckUnitTierMains`, `AllowedTools`, `RuleAllowTools` | `tiermain.go` | none |
| `SweepHostEntry`, `ReadSweepHost`, `ParseSweepHost`, `CheckSweepHostPackages`, `CheckSweepHostReasons`, `CheckSweepHostReasonFiles`, `SweepRunnerPackages`, `CheckSweepHostCount` | `sweephost.go` | none |
| `ParseOverBudget`, `ReadOverBudget`, `CheckOverBudget`, `OverBudgetEntry`, `TrackedRun` | `overbudget.go` | `ReadOverBudget` in `cmd/budget/main.go:75`; the rest none |
| `ReadList`, `PackageDirs` | `list.go` | `ReadList` in `cmd/budget/main.go:71`; `PackageDirs` none |
| `SplitTestArgs`, `TagsArgs`, `PartitionPackages` | `split.go` | all three in `cmd/budget/main.go:100,101,105` |
| `FastTierMaxWall`, `PackageWall`, `WallSummary`, `ParsePackageWalls`, `WallOverruns` | `tier.go` | `FastTierMaxWall` at `cmd/budget/main.go:67,68,224,251,257` and `main_test.go:112`; `ParsePackageWalls` at `main.go:237`; `WallOverruns` at `main.go:251`; `PackageWall`/`WallSummary` none |
| `CPUDirEnv`, `CPUTime`, `CPUSource`, `WriteCPU`, `ReadCPU` | `cpu.go` | `CPUDirEnv` at `cmd/budget/main.go:483,554,563,579`; `CPUTime` at `main.go:493,518,582` and `main_test.go:137`; `WriteCPU` at `main.go:582`; `ReadCPU` at `main.go:494`; `CPUSource` none |
| `WatchBudget`, `WatchBudgetTracked`, `BudgetResult`, `Overrun`, `TestTime`, `SlowRun`, `PkgTime`, `IdleReport`, `IdleJudged`, `EnforceBudget`, `IdleTop`, `IdleRatioFactor`, `IdleWall`, `StrictBudgetEnv`, `BudgetFails` | `budget.go` | `WatchBudgetTracked` at `cmd/budget/main.go:500`; `BudgetResult` at `main.go:127,461,462`; `Overrun` at `main.go:204` and `main_test.go:137,138,141,146,147,148`; `PkgTime` at `main.go:180`; `IdleJudged` at `main.go:181`; `EnforceBudget` at `main.go:93`; `StrictBudgetEnv` at `main.go:93,97,207`; `BudgetFails` at `main.go:220`; `WatchBudget`, `TestTime`, `SlowRun`, `IdleReport`, `IdleTop`, `IdleRatioFactor`, `IdleWall` none |
| `IntegrationPackages` | `integration.go` | `cmd/integrationvet/main.go:86` |
| `RuleNoSleep`, `RuleNoEnv`, `RuleNoHostEnv`, `RuleNoChdir`, `RuleNoSkip`, `RuleNoSubprocess`, `RuleNoNetwork`, `RuleFakeClockEpoch`, `RuleNoBuild`, `RuleNoExecFiles`, `RuleNoGlobalSwap`, `RuleParallel`, `RuleAllowReason`, `RuleProdSetenv`, `RuleProdSleep` | `rules.go` | none |

## Gastown's extra rules and the module's Rule seam

The module's seam is one `Rule` with `Name` and `Check`, passed as
`ScanDirWithRules(dir, extra ...Rule)`; a rule is an argument, never a
package-level registration, and duplicate or empty names are refused
(dv-ebt2.1). Adoption means gastown keeps its own rule types in
`internal/testpolicy` and passes them as the `extra` arguments. The exact
`Check` signature is not determined: dv-ebt2.1 says `Check` takes the
`ScanContext`, but the field names it exposes are the read in `hq-wisp-zdbxc`.

### The six rules that move

| Rule | Enforced today | After adoption |
|---|---|---|
| `no-sleep` | `bannedTestCalls` in `rules.go` (`time.Sleep`, `time.After`, `time.NewTimer`, and the rest) | the module's Rule; gastown registers nothing |
| `no-subprocess` | `checkTestCall` in `rules.go` | the module's Rule |
| `no-network` | `checkTestCall`, the `netDialerType` branches of `checkTestFile` | the module's Rule |
| `no-build` | `checkTestCall` (`os/exec` with `go`) | the module's Rule |
| `no-exec-files` | `checkTestCall` and the `#!` literal branch of `checkTestFile` | the module's Rule |
| `allow-reason` | `applyAllows` in `allow.go` | the module's allow machinery |

### The nine rules that stay gastown's

Each becomes a `Rule` type in `internal/testpolicy` whose `Check` reads the
`ScanContext`'s test files and production files and returns `Violation`s.

| Rule | Enforced today | What its Rule needs from `ScanContext` |
|---|---|---|
| `no-env` | `bannedTestCalls` (`os.Setenv`/`os.Unsetenv`), `bannedTestMethods` (`Setenv`) | the test files |
| `no-host-env` | `checkTestCall` (`os.Getenv`/`os.LookupEnv`, `os.UserHomeDir`/`ConfigDir`/`CacheDir`) and `checkEnvValue` | the test files and `packageConsts`, to read a constant's name |
| `no-chdir` | `bannedTestCalls` (`os.Chdir`), `bannedTestMethods` (`Chdir`) | the test files |
| `no-skip` | `bannedTestMethods` (`Skip`, `Skipf`, `SkipNow`) | the test files |
| `fake-clock-epoch` | `bannedTestCalls` (`clockwork.NewFakeClock`) | the test files |
| `no-global-swap` | the `AssignStmt` branch of `checkTestFile` | the test files and `packageVars` |
| `parallel` | the `Parallel` check at the end of `checkTestFile` | the test files |
| `prod-no-setenv` | `checkProdFile` | the production files and their file name (`main`) |
| `prod-no-sleep` | `checkProdFile` with the `clocked` flag | the production files and whether the package imports `clockwork` |

This is the design's most likely production failure: `no-host-env`,
`no-global-swap` and the two production rules depend on `packageVars`,
`packageConsts` and the `clockwork` import check that live inside `scan.go`
today. If the module's `ScanContext` exposes only `Fset`, `Tests`, `Prod`,
`Allow` and `Imports` (dv-ebt2.1's list), gastown cannot implement those four
rules outside the module without adding a module field first.

### The checks that are not per-violation Rules

These stay gastown's package functions and policy tests; they are not Rules and
do not register through the seam:

- `CheckDockerTier` and `TestDockerTier` (`docker.go`, `docker_test.go`) keep
  `docker.txt` exact.
- `GitFreeFindings`, `CheckGitFree` (`gitfree.go`) and their ratchet
  `minGitFree` keep `gitfree.txt`.
- `RealGitFiles`, `CheckRealGit`, `ConvertedShare` (`realgit.go`) and
  `maxRealGit` keep `realgit.txt`.
- `CheckSweepHost*`, `SweepRunnerPackages` (`sweephost.go`) and `maxSweepHost`
  keep `sweep-host.txt`.
- `CheckContracts` (`contracts.go`) enforces `fake-contract` and
  `integration-name` over the whole tree.
- `CheckUnitTierMains` and `AllowedTools` (`tiermain.go`) keep the tier-runner
  and allow-tools checks.

## What stays in gastown's lists and policy test

- All six `.txt` lists, unchanged in place: they are gastown's data.
- `policy_test.go`: `TestPolicy`, the `-seed` mode, and the ratchets
  `maxUnconverted` (1) and `maxOverBudget` (0). `TestPolicy` and
  `scan_test.go`'s `TestScanDirWithExemptions` are the two callers of
  `ScanDirWithExemptions`; after adoption both call the module's
  `ScanDirWithRules` with gastown's nine extra Rules and the same lists.
- The per-list ratchets in their own tests: `minGitFree` (24) in
  `gitfree_test.go`, `maxRealGit` in `realgit_test.go`, `maxSweepHost` in
  `sweephost_test.go`.
- `unionmerge_test.go`, which reads the repo-root `.gitattributes` and holds
  every `merge=union` path to the "only grows" header.
- `main_test.go` and every `testmain_test.go`, which run the unit tests through
  `unittier.Main`.

## What could not be determined

- The module's file list, exported names and fixture directories at
  `origin/main`, and the exact `Rule`, `ScanContext` and `ScanDirWithRules`
  signatures. The read is in `hq-wisp-zdbxc`.
- The finer class of each shared file: identical, trimmed at the copy, or
  drifted. dv-oqr3 records the known drift direction — gastown gained rules and
  tier files the devops copy lacks, and devops removed rules gastown keeps — but
  which file carries which difference needs the module.
- Whether the module already carries code this inventory calls gastown-only
  (`overbudget.go`, `tiermain.go`, `contracts.go`, `integration.go`, the docker
  and sweep checks). A devops copy of any of them changes the adoption plan.
- Whether `ScanContext` exposes the package variables, string constants and the
  `clockwork` import that `no-host-env`, `no-global-swap`, `prod-no-setenv` and
  `prod-no-sleep` need. This decides whether gastown's nine extra rules can be
  external Rules at all.

## Implementation beads for gastown's adoption (slice 7)

Each is under 8 points. Every bead is blocked by the operator's publication of
the module version that carries its slice: the first needs slice 1a, S7.1 needs
1b, and each later bead needs the slices its files name. Sizes are story points
in this rig's scale.

1. **S7.0 — add `gt.local/testpolicy` and adopt the scanner, allow machinery and
   Rule seam.** 5 points. Depends on: operator publish (1a+1b), and this
   inventory. Files: `go.mod`, `go.sum`, `internal/testpolicy/scan.go`,
   `allow.go`, `srccache.go`, `main_test.go`. Make `ScanDir` and
   `ScanDirWithExemptions` thin wrappers over the module's `ScanDirWithRules`,
   keep the exported names as aliases, keep the `srcache` read path, and pass
   gastown's rules as arguments. Exit: `TestScanDirFixtures`,
   `TestScanDirWithExemptions` and `TestPolicy` pass unchanged; `go build ./...`
   exits 0.
2. **S7.1 — adopt the six shared rules and keep the nine extra rules as module
   Rules.** 5 points. Depends on: S7.0. Files: `rules.go`, `scan_test.go`, and
   the 15 shared fixture files. Delete the moved checks and constants; write the
   nine extra rules as `Rule` types; prove each rule row still fires. Exit:
   every row of `TestScanDirFixtures` and `TestPolicy` unchanged.
3. **S7.2 — adopt the module's lists, split and tier-wall parsing.** 3 points.
   Depends on: S7.0. Files: `list.go`, `split.go`, `tier.go`, `list_test.go`,
   `split_test.go`, `tier_test.go`. Delete the gastown copies; the six `.txt`
   lists stay. Exit: `TestSplitTestArgs`, `TestPartitionPackages`,
   `TestTagsArgs`, `TestParsePackageWalls`, `TestWallOverruns` move with the
   code or call the module, and `cmd/budget` still builds.
4. **S7.3 — adopt the module's CPU and budget code and `cmd/budget`.** 5 points.
   Depends on: S7.2. Files: `cpu.go`, `budget.go`, `overbudget.go`,
   `cpu_test.go`, `budget_test.go`, `budget_integration_test.go`,
   `overbudget_test.go`, `cmd/budget/*`. Keep `overbudget.txt` and the
   Makefile's invocations. Exit: `make gate`'s budget stage and `make
   tier-check` behave as before.
5. **S7.4 — adopt the module's changed/triggers code and presubmit selectors.**
   5 points. Depends on: S7.2. Files: `internal/land/changed.go`,
   `internal/land/cmd/changedpkgs`, `internal/presubmit/cmd/errbranch`,
   `internal/presubmit/cmd/gomodrationale`. These live outside
   `internal/testpolicy` in gastown, a layout difference from the devops copy;
   the bead moves them onto the module's copies. Exit: `make presubmit`'s
   package selection and its two judges are unchanged.
6. **S7.5 — adopt the module's slow/scripts checks and the tierwall command.**
   5 points. Depends on: S7.2. Files: `internal/testpolicy/cmd/tierwall/*`,
   `internal/shelltier/*`, the `scripts/` tree, and the Makefile's two
   `cmd/tierwall` invocations. Keep gastown's shell manifest. Exit: `make gate`'s
   tierwall stage and `make test-slow` behave as before.
7. **S7.6 — adopt the module's per-Test ratchet code.** 3 points. Depends on:
   S7.3. Files: `overbudget.go`, `policy_test.go`, and the ratchet constants.
   Keep gastown's lists and their pins. Exit: every ratchet fails the same way
   on the same input.
8. **S7.7 — delete the last duplicate and verify the tree.** 2 points. Depends
   on: S7.1, S7.4, S7.5, S7.6. Exit: `git grep` finds one copy of each moved
   function; `make gate` exits 0; no `replace` for `gt.local/testpolicy`
   remains.

## What the operator must do

Publish the module before any adoption bead starts: extract `testpolicymod/`
from devops to its own repository under the Forgejo namespace, push the `v*`
tag, and publish it through the Forgejo Go registry the way `gt.local/devops`
is published (dv-ebt2's 2026-11-05 comment; dv-ebt2.1's note that publishing is
the plan's 7.2). Until that tag resolves, gastown cannot require the module and
its adoption beads stay blocked.

## Gate

This is a docs-only change. The docs checks were run from the repository root:

```
$ go run ./cmd/docs-lint
exit 0, no findings
```

The same result is recorded in the bead notes for gt-zq17ds. The landing gate
is `make lint`, which `gt done` runs as part of `make presubmit`; it also runs
`golangci-lint` over the unchanged Go tree.
