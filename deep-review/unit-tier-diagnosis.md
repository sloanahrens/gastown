> Status: diagnosis for gt-z56xs (2026-09-30, main 32b3d1e0). Read-only; nothing fixed.

# Why the gastown unit tier takes ~700 s

## Verdict

The "unit tier" is not a unit tier. 28 of 88 packages run real git, bd, dolt,
tmux or `go` processes, and those 28 hold **2049 of 2139 package-seconds (96%)**
in the 0908 gate log. The 60 packages that are genuinely unit tests take
**88 package-seconds in total**, none over 3.8 s. The wall clock is set by three
things, in this order:

1. **internal/cmd's serial phase.** 911 of its 2371 tests cannot run in
   parallel (t.Setenv, t.Chdir, package-global swaps, PATH-stubbed bd). Their
   times sum to 402 s in a 472 s run; 59 of them (>=1 s each) are 274 s, and
   the `TestRunDone*` family alone is ~175 s. That is the gate's critical path.
2. **fork/exec under contention.** Every git/bd call is a fork of a large Go
   test binary. At load 40-130 an exec costs 60-160 ms of *system* CPU, which
   is why sys is 10-17x user in git, land, refinery, editorial and cmd. More
   cores do not help: six `make gate` runs were live on the host while I
   measured, each fanning out up to 24 test binaries.
3. **Everything outside the Go suite.** In all three gate logs from this
   morning the Go suite took 6.5 min of a ~12 min gate (log file birth to last
   write). The other ~5 min is lint + build before the unit tier starts; the
   gate serialises lint on golangci-lint's host-wide lock
   (`$TMPDIR/golangci-lint.lock`, `--allow-serial-runners`, Makefile:253).
   Inferred from timestamps, not measured separately.

Deleting the packages on the delete list removes a third of the package-seconds
but **does not touch the critical path** (cmd). Both options below have to fix
cmd's serial tail first.

## Measurement conditions

Every number carries its load average (24-core host, 128 GiB). I never had a
quiet host: load was 40-138 during this session, with 5-6 concurrent
`make gate` runs (pgrep at 09:45 and 10:53). "Standalone" below means one
package binary at a time from me, not a quiet machine.

| source | load |
|---|---|
| 0908 gate log (bead's numbers) | 40-60 |
| my standalone top-5 runs, 09:41-10:25 | 104-127 |
| my exec-counted runs of 49 packages, 09:41-10:50 | 68-134 |
| slow-cmd.log (another session, -v run of internal/cmd, 472 s) | ~100, not recorded |
| cmd single-test probes, git/land re-runs, 10:50 | 40-46 |

Exec counts come from a PATH shim logging every `git`, `bd`, `dolt`, `tmux`,
`gt`, `go`, `docker` exec (scratchpad `diag-shim/`). They **undercount**:
stub scripts written into temp dirs and prepended to PATH bypass the shim, so
bd stub calls are not in the counts. The stub-script column counts those sites
statically.

## 1. Gate structure (why 700 s)

From `tier-gate-0908.log` and `internal/testpolicy/cmd/budget/main.go:14-23`:

| stage | 0908 | notes |
|---|---|---|
| lint + build | ~5 min (inferred) | host-wide golangci-lint lock, serial across gates |
| budget phase 1: 53 converted pkgs | 57 s | slowest: git 43 s, land 41 s |
| budget phase 2: 37 unconverted pkgs | 5 m 32 s | cmd 301 s + its compile (75 s at load ~100, measured) |
| shell tests | beside the Go suite | not on the critical path |

The two budget phases run one after the other by design. gt-22hdp.60 measured
running them side by side as no win at low load, so it stays off.
Sum of package walls is 2139 s against ~390 s of Go-suite wall: effective
package parallelism about 5.5 on 24 cores, because the host is shared.

## 2. Per-package classification (all 88 + guardlint's lint run)

Rules. **TRUE UNIT**: no exec beyond the harness's own `go env` x2 and
`tmux kill-server`, no stub scripts, no sleeps that matter.
**SUBPROCESS-IN-DISGUISE**: spawns git/bd/tmux/dolt/go, writes stub
executables, or sleeps. **INTEGRATION MISLABELLED**: needs a real bd, a real
Dolt, or spawns `go test` to test the harness.

Columns: 0908 gate wall; test count (static, non-integration files);
serial = top-level tests that never called t.Parallel (from a -v run);
execs = shim count for one run; stub scripts and Sleep sites = static counts.

| package | gate wall s (0908) | tests | serial tests | execs (shim run) | stub scripts | Sleep sites | class | note |
|---|---|---|---|---|---|---|---|---|
| cmd | 301.4 | 2384 | 911 | 4991 | 192 | 13 | SUBPROCESS-IN-DISGUISE |  |
| refinery | 212.1 | 395 | 63 | 6020 | 16 | 3 | SUBPROCESS-IN-DISGUISE | DELETE LIST |
| refinery/editorial | 166.6 | 142 | 67 | 3336 | 20 | 0 | SUBPROCESS-IN-DISGUISE | DELETE LIST |
| witness | 95.0 | 364 | 132 | 541 | 7 | 0 | SUBPROCESS-IN-DISGUISE |  |
| checkpoint | 88.4 | 48 | 48 | 405 | 0 | 0 | SUBPROCESS-IN-DISGUISE |  |
| beads | 87.9 | 562 | 501 | 92 | 60 | 6 | INTEGRATION MISLABELLED | real bd init (TestBdFirstRunMetrics...); DELETE LIST |
| rig | 87.6 | 109 | 99 | 211 | 12 | 0 | SUBPROCESS-IN-DISGUISE |  |
| version | 86.8 | 26 | 26 | 551 | 0 | 0 | SUBPROCESS-IN-DISGUISE |  |
| web | 82.5 | 247 | 240 | 140 | 30 | 3 | INTEGRATION MISLABELLED | real bd+dolt schema (TestRawTrackedDeps_RealBdSchema 63s); DELETE LIST |
| dog | 82.2 | 86 | 85 | 163 | 0 | 3 | SUBPROCESS-IN-DISGUISE | DELETE LIST |
| daemon | 81.2 | 809 | 156 | 599 | 49 | 24 | SUBPROCESS-IN-DISGUISE |  |
| polecat | 77.5 | 265 | 12 | 2332 | 1 | 5 | SUBPROCESS-IN-DISGUISE |  |
| doltserver | 74.3 | 227 | 224 | 8 | 2 | 4 | INTEGRATION MISLABELLED | real dolt binary + process kill/port tests |
| testutil | 71.2 | 97 | 79 | 36 | 0 | 0 | INTEGRATION MISLABELLED | spawns go/go test to test the harness (22 go execs) |
| formula | 67.5 | 124 | 125 | 56 | 6 | 0 | SUBPROCESS-IN-DISGUISE |  |
| crew | 66.4 | 20 | 16 | 132 | 0 | 0 | SUBPROCESS-IN-DISGUISE |  |
| doctor | 63.7 | 729 | 110 | 843 | 17 | 0 | SUBPROCESS-IN-DISGUISE |  |
| util | 44.6 | 67 | 60 | 98 | 0 | 4 | SUBPROCESS-IN-DISGUISE |  |
| git | 43.3 | 146 | 0 | 2034 | 0 | 0 | SUBPROCESS-IN-DISGUISE |  |
| land | 41.0 | 48 | 0 | 875 | 0 | 0 | SUBPROCESS-IN-DISGUISE |  |
| mail | 32.5 | 146 | 145 | 69 | 9 | 0 | SUBPROCESS-IN-DISGUISE |  |
| deacon | 32.3 | 147 | 144 | 40 | 2 | 0 | SUBPROCESS-IN-DISGUISE | DELETE LIST |
| plugin | 19.2 | 61 | 59 | 41 | 7 | 0 | SUBPROCESS-IN-DISGUISE |  |
| tui/feed | 15.6 | 65 | 64 | 3 | 5 | 3 | SUBPROCESS-IN-DISGUISE | DELETE LIST |
| testpolicy | 15.4 | 22 | 0 | 0 | 0 | 0 | TRUE UNIT |  |
| config | 12.1 | 408 | 94 | 15 | 6 | 2 | SUBPROCESS-IN-DISGUISE |  |
| nudge | 11.3 | 41 | 40 | 0 | 0 | 7 | SUBPROCESS-IN-DISGUISE | DELETE LIST |
| quota | 6.6 | 74 | 74 | 0 | 0 | 0 | TRUE UNIT | DELETE LIST |
| slot | 3.8 | 99 | 0 | 0 | 0 | 0 | TRUE UNIT |  |
| hooks | 3.4 | 119 | 119 | 0 | 0 | 0 | TRUE UNIT |  |
| intent | 3.3 | 16 | 0 | 3 | 0 | 0 | TRUE UNIT | execs are harness only (go env, tmux kill-server) |
| feed | 3.1 | 22 | 22 | 0 | 0 | 9 | SUBPROCESS-IN-DISGUISE | DELETE LIST |
| cmdtree | 3.1 | 15 | 0 | 3 | 0 | 0 | TRUE UNIT | execs are harness only (go env, tmux kill-server) |
| guardlint | 2.9 | 8 | 0 | 0 | 0 | 0 | TRUE UNIT |  |
| testdb | 2.8 | 4 | 0 | 0 | 0 | 0 | TRUE UNIT |  |
| runtime | 2.8 | 66 | 66 | 0 | 0 | 0 | TRUE UNIT |  |
| supervisor | 2.7 | 20 | 0 | 3 | 0 | 0 | TRUE UNIT | execs are harness only (go env, tmux kill-server) |
| townconfig | 2.7 | 11 | 0 | 0 | 0 | 0 | TRUE UNIT |  |
| agentpause | 2.5 | 17 | 0 | 3 | 0 | 0 | TRUE UNIT | execs are harness only (go env, tmux kill-server) |
| deps | 2.3 | 15 | 0 | 0 | 0 | 0 | TRUE UNIT |  |
| beads/beadsfake | 2.3 | 6 | 0 | 3 | 0 | 0 | TRUE UNIT | execs are harness only (go env, tmux kill-server) |
| scheduler/capacity | 2.3 | 35 | 35 | 0 | 0 | 0 | TRUE UNIT |  |
| tui/convoy | 2.1 | 8 | 0 | 3 | 0 | 0 | TRUE UNIT | execs are harness only (go env, tmux kill-server) |
| tmux | 2.0 | 289 | 0 | 0 | 0 | 0 | TRUE UNIT |  |
| liveness | 1.9 | 12 | 0 | 3 | 0 | 0 | TRUE UNIT | execs are harness only (go env, tmux kill-server) |
| health | 1.9 | 2 | 0 | 3 | 0 | 0 | TRUE UNIT | execs are harness only (go env, tmux kill-server) |
| protocol | 1.7 | 33 | 32 | 19 | 0 | 0 | SUBPROCESS-IN-DISGUISE |  |
| session | 1.6 | 78 | 78 | 0 | 0 | 0 | TRUE UNIT |  |
| templates | 1.3 | 52 | 46 | 0 | 0 | 0 | TRUE UNIT |  |
| tmux/tmuxfake | 1.3 | 6 | 0 | 0 | 0 | 0 | TRUE UNIT |  |
| notify | 1.2 | 28 | 0 | 0 | 0 | 0 | TRUE UNIT |  |
| krc | 0.9 | 29 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate); DELETE LIST |
| wisp | 0.9 | 22 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| lock | 0.7 | 23 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| worktree | 0.7 | 9 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| workspace | 0.7 | 21 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| events | 0.7 | 44 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| boot | 0.7 | 3 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| lintlock | 0.6 | 12 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| wrappers | 0.6 | 10 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| reaper | 0.6 | 49 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| notify/notifyfake | 0.6 | 4 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| hookutil | 0.6 | 1 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| landings | 0.5 | 5 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| townlog | 0.5 | 8 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| convoy | 0.5 | 87 |  |  | 3 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| guard | 0.5 | 11 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| dispatch | 0.5 | 25 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| shell | 0.5 | 4 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| patrolstate | 0.5 | 16 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| style | 0.4 | 9 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| mayor | 0.4 | 18 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate); DELETE LIST |
| estop | 0.4 | 12 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| channelevents | 0.3 | 13 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| state | 0.3 | 11 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| atomicfile | 0.3 | 20 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| templates/commands | 0.3 | 9 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| testpolicy/cmd/budget | 0.2 | 2 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| cli | 0.2 | 4 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| cmdtree/bdsnapshot | 0.2 | 3 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| bitbucket | 0.2 | 17 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| bdgate | 0.2 | 3 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| suggest | 0.1 | 3 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| ui | 0.1 | 22 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| constants | 0.1 | 18 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| agentlog | 0.1 | 14 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |
| telemetry | 0.0 | 54 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate); DELETE LIST |
| activity | 0.0 | 9 |  |  | 0 | 0 | TRUE UNIT | not exec-counted (<1s in gate) |

Totals: 60 TRUE UNIT packages = 88 package-seconds. 24 SUBPROCESS-IN-DISGUISE
+ 4 INTEGRATION MISLABELLED = 2049 package-seconds.

Evidence for the four mislabelled packages:

- **beads**: `internal/beads/beads_test.go:5589` runs the installed `bd init`
  (14.3 s at load 105). 501 of 561 tests are serial.
- **web**: `TestRawTrackedDeps_RealBdSchema` runs real bd against real Dolt,
  63 s at load 95, 72% of the package.
- **testutil**: `TestHermeticHarnessEnforced` (16.8 s) and
  `TestPreserveGoEnv_PinsGoEnvPastHomeRedirect` (11.7 s) spawn `go` 22 times
  to test the harness itself.
- **doltserver**: runs the dolt binary and real process-kill/port-squatter
  tests. 224 of 226 tests serial.

## 3. The 30 slowest tests

Times are from my exec-counted runs at load 68-134 and slow-cmd.log for cmd.
Parallel tests' durations are inflated by contention with their siblings, which
share the process's
fork lock and the host's CPUs. land, for example, is 67 s at load 126 and
14 s at load 46.

| # | seconds | package | test | serial/parallel | what it spawns or waits on |
|---|---|---|---|---|---|
| 1 | 64.5 | land | TestLandGateInfraErrorIsNotARejection | parallel | real git: temp bare origin, worktree add, fetch, merge, push (875 git execs / 48 tests) |
| 2 | 63.2 | web | TestRawTrackedDeps_RealBdSchema | serial | real bd init + bd subcommands against a real Dolt (19 bd, 7 dolt execs) |
| 3 | 51.2 | polecat | TestAllocateAndAdd_NoDuplicateNames | parallel | real git worktrees + branches on temp repos (2324 git execs / 264 tests) |
| 4 | 48.7 | version | TestResolveBuildBranchRef | serial | real git repos + fetch from temp remote (551 git execs / 26 tests) |
| 5 | 47.5 | land | TestLandRepairsAnIncompleteRecord | parallel | real git: temp bare origin, worktree add, fetch, merge, push (875 git execs / 48 tests) |
| 6 | 46.9 | land | TestLandMergesGatesPushesAndRecords | parallel | real git: temp bare origin, worktree add, fetch, merge, push (875 git execs / 48 tests) |
| 7 | 40.5 | land | TestLandLostLeaseIsRaceErrorAndLeavesBead | parallel | real git: temp bare origin, worktree add, fetch, merge, push (875 git execs / 48 tests) |
| 8 | 37.4 | land | TestLandConflictIsARejectionWithFiles | parallel | real git: temp bare origin, worktree add, fetch, merge, push (875 git execs / 48 tests) |
| 9 | 35.7 | land | TestLandRunsGateAndReviewConcurrently | parallel | real git: temp bare origin, worktree add, fetch, merge, push (875 git execs / 48 tests) |
| 10 | 35.7 | land | TestLandReadBackFailureWritesNoRecord | parallel | real git: temp bare origin, worktree add, fetch, merge, push (875 git execs / 48 tests) |
| 11 | 35.1 | land | TestLandUnknownVerdictIsInfra | parallel | real git: temp bare origin, worktree add, fetch, merge, push (875 git execs / 48 tests) |
| 12 | 32.9 | land | TestLandRequestChangesRejectsWithFindings | parallel | real git: temp bare origin, worktree add, fetch, merge, push (875 git execs / 48 tests) |
| 13 | 32.9 | land | TestLandRefusesEmptyMerge | parallel | real git: temp bare origin, worktree add, fetch, merge, push (875 git execs / 48 tests) |
| 14 | 32.6 | git | TestSubmoduleChanges_SkipsClaudeWorktrees | parallel | real git: the adapter under test (2034 git execs / 146 tests) |
| 15 | 31.9 | land | TestLandRedGateNeverPushes | parallel | real git: temp bare origin, worktree add, fetch, merge, push (875 git execs / 48 tests) |
| 16 | 31.6 | land | TestLandRejectionLeavesABeadThatChangedHands | parallel | real git: temp bare origin, worktree add, fetch, merge, push (875 git execs / 48 tests) |
| 17 | 30.9 | cmd | TestRunDoneFailureClearsTheDoneIntentLabel | serial | runDone/prune/recover end to end: real git repos + temp origin + bd recorder stub script, t.Setenv/t.Chdir (84-128 execs per test) |
| 18 | 28.6 | witness | TestHasPendingMRFromSnapshotAssessesMRStatus | serial | real git repos + bd stub (541 execs / 363 tests) |
| 19 | 28.6 | refinery | TestDoMerge_RechecksSourceFlagsBeforeDirectPush | parallel | real git merges/pushes + gate stub (6020 git execs / 395 tests) |
| 20 | 28.3 | refinery | TestProcessBatch_GateFailure_BisectsToFindCulprit | parallel | real git merges/pushes + gate stub (6020 git execs / 395 tests) |
| 21 | 26.6 | cmd | TestRunDoneClassifiesOnTheLastPushAttempt | serial | runDone/prune/recover end to end: real git repos + temp origin + bd recorder stub script, t.Setenv/t.Chdir (84-128 execs per test) |
| 22 | 26.3 | git | TestUnpushedCommitsDetachedHeadOffRemoteStillBlocks | parallel | real git: the adapter under test (2034 git execs / 146 tests) |
| 23 | 25.8 | land | TestLandHeadAlreadyOnTargetNeedsAHuman | parallel | real git: temp bare origin, worktree add, fetch, merge, push (875 git execs / 48 tests) |
| 24 | 25.5 | polecat | TestWorkstateDispositionForPolecat_StashScopedToOwningSeat | parallel | real git worktrees + branches on temp repos (2324 git execs / 264 tests) |
| 25 | 25.0 | polecat | TestReuseIdlePolecat_ResumesBranchHeldByIdleUnreapedPolecat | parallel | real git worktrees + branches on temp repos (2324 git execs / 264 tests) |
| 26 | 24.8 | land | TestLandHeadMustBeOnOrigin | parallel | real git: temp bare origin, worktree add, fetch, merge, push (875 git execs / 48 tests) |
| 27 | 24.6 | git | TestVerifyPushedCommitReachableFromPushTarget | parallel | real git: the adapter under test (2034 git execs / 146 tests) |
| 28 | 24.3 | polecat | TestAddWithOptions_ResumesBranchHeldByStalledPolecatOnSameBead | parallel | real git worktrees + branches on temp repos (2324 git execs / 264 tests) |
| 29 | 24.2 | cmd | TestPolecatInventoryDanglingMRGate | parallel | runDone/prune/recover end to end: real git repos + temp origin + bd recorder stub script, t.Setenv/t.Chdir (84-128 execs per test) |
| 30 | 24.2 | git | TestInitSubmodules_WithSubmodules | parallel | real git: the adapter under test (2034 git execs / 146 tests) |

Single-test probes of cmd's worst tests, run alone at load 40-45 (the
test's own time vs the same test inside the full package at ~100 load):

| test | alone | in package | execs | sys CPU alone |
|---|---|---|---|---|
| TestRunDoneFailureClearsTheDoneIntentLabel | 15.2 s | 30.9 s | 128 (121 git) | 9.4 s |
| TestRunDoneClassifiesOnTheLastPushAttempt | 3.5 s | 26.6 s | 87 | 2.7 s |
| TestPolecatInventoryDanglingMRGate | 3.2 s | 24.2 s | 119 | 1.9 s |
| TestRecoverDivergedPush_RealRepo | 6.4 s | 19.9 s | 43 | 4.5 s |
| TestPruneRemotePolecatBranchesUsesUpstreamBaseForOriginFork | 1.2 s | 19.8 s | 49 | 0.8 s |
| TestGetGitStateDistinguishesSharedStashes | 0.8 s | 17.5 s | 46 | 0.5 s |

The same code costs 2-20x more inside the package run. The cost is not in
the test logic. It is in how many git processes each test forks and in what
else is forking at the same time.

The worst harness is `runDoneSubmitWithFlags`
(`internal/cmd/done_submit_test.go:57`). It builds a routed town and a git
repo with a temp origin, installs a bd recorder stub script, sets 7 env vars
with t.Setenv, calls t.Chdir, swaps package globals, and then runs the whole
`gt done` path (`runDone`) against real git. That is an end-to-end test in the
unit tier, and every test built on it is serial by construction.

## 4. Shared helpers that create repos, servers, towns, stubs

There is **no shared git fixture**. 167 distinct test functions in 98 files
run `git init` themselves:

| package | helper functions that git init |
|---|---|
| cmd | 41 |
| doctor | 19 |
| polecat | 18 |
| git | 18 |
| rig | 16 |
| refinery | 12 |
| crew | 11 |
| daemon | 7 |
| witness | 6 |
| version | 4 |
| others | 15 |

The one cache that exists, `cachedGitFixture` in
`internal/cmd/git_fixture_cache_test.go`, has 18 call sites (cachedGitFixture plus cachedGitFixtureStrings).

Stub executables, written per test and prepended to PATH (static counts):

| package | stub script sites | Setenv("PATH") sites |
|---|---|---|
| cmd | 192 | 170 |
| beads | 60 | 54 |
| daemon | 49 | 14 |
| web | 30 | 2 |
| editorial | 20 | 2 |
| doctor | 17 | 18 |
| refinery | 16 | 14 |
| rig | 12 | 13 |

Every `Setenv("PATH")` forces the test serial. A PATH-stubbed test cannot call
t.Parallel. The 170 in cmd are a large part of its 911 serial tests.

`internal/testutil` itself is cheap per package. `StartHermetic`
(`internal/testutil/hermetic.go:205`) makes one sandbox, snapshots the live
town, and costs three execs per package: `go env` twice and a tmux
kill-server. It is not the problem. Its Dolt pool only runs with
GT_TEST_DOCKER=1. The helpers that cost time are the per-package git/bd
fixtures above, and they are spread across every package.

sys CPU per package, standalone (`/usr/bin/time -l`):

| package | load | wall | user | sys | sys/user |
|---|---|---|---|---|---|
| cmd run 1 | 111 | 519 s | 43.6 s | 624 s | 14x |
| cmd run 2 | 104 | 346 s | 41.9 s | 558 s | 13x |
| refinery run 1 | 126 | 302 s | 40.7 s | 299 s | 7x |
| refinery run 2 | 121 | 258 s | 41.4 s | 405 s | 10x |
| editorial run 1 | 124 | 372 s | 23.1 s | 285 s | 12x |
| editorial run 2 | 127 | 280 s | 22.8 s | 219 s | 10x |
| witness run 1 | 105 | 85 s | 4.8 s | 30.5 s | 6x |
| checkpoint run 1 | 117 | 49 s | 2.6 s | 25.2 s | 10x |
| git | 42 | 19.6 s | 14.9 s | 247 s | 17x |
| land | 46 | 14.0 s | 7.1 s | 95.9 s | 13x |

## 5. Why sys is 10-20x user in git and land

Exec counts in one run: internal/git 2034 git execs (474 rev-parse, 239
remote, 117 commit, 113 add), internal/land 875. So:

| package | load | sys per git exec |
|---|---|---|
| git, 0908 gate | 40-60 | 304 s / 2034 = 150 ms |
| git, my run | 42 | 247 s / 2034 = 121 ms |
| land, 0908 gate | 40-60 | 136 s / 875 = 156 ms |
| land, my run | 46 | 96 s / 875 = 110 ms |

A standalone fork/exec benchmark (scratchpad `diag-forkbench/`, load 132)
separates the parts:

| case | per-exec wall | sys per exec |
|---|---|---|
| /usr/bin/true, 1 at a time, 10 MB heap | 19 ms | 4.2 ms |
| git --version, 1 at a time, 10 MB heap | 32 ms | 10 ms |
| git --version, 16 at a time, 10 MB heap | 11 ms | 18 ms |
| git --version, 1 at a time, 500 MB heap | 56 ms | 20 ms |
| git --version, 16 at a time, 500 MB heap | 25 ms | 25 ms |

What this shows:

- **Forking a big process costs more.** Go on darwin forks with libc
  `fork()`, which copies the parent's page tables. A bigger test binary means
  a more expensive fork: 500 MB of heap doubles sys per exec.
- **Parallel forks in one binary contend.** Forks inside one Go process
  serialise on `syscall.ForkLock`, and concurrent ones raise sys per exec.
  internal/git runs 160 parallel tests in one binary. The git run saw
  252,271 involuntary context switches in 19.6 s.
- **Real git operations cost 5-10x a trivial one.** A real operation
  (commit, worktree add, fetch against a temp bare repo) does filesystem work
  on fresh temp dirs. That takes the 10-25 ms of a `git --version` up to the
  110-160 ms measured per exec.
- **It is not fsync.** fsync is already off in the sandbox gitconfig
  (gt-22hdp.33). Exec probes from this session measured 33 ms for
  `/usr/bin/true`, 64 ms for `git --version`, and 460 ms per `git init` plus
  one commit (load 111).

The same internal/git binary run with `-test.parallel=1` (load 48) took
200 s wall, 12.8 s user and 89.9 s sys: 44 ms sys and 98 ms wall per git exec.
At the default parallelism (load 42) it took 19.6 s wall and 247 s sys:
121 ms sys per exec. Parallelism inside one binary buys 10x wall but nearly
triples the kernel cost of each exec, which is the contention signature.

**Conclusion.** A test that runs 50-130 git execs cannot be fast on this host
whatever its logic. Its floor is exec count x 60-160 ms, and concurrent gates
raise that floor.

## 6. Standalone vs loaded, top 5 packages

| package | 0908 gate (load 40-60, 6 gates) | standalone run 1 | standalone run 2 |
|---|---|---|---|
| cmd | 301 s | 519 s (load 111) | 346 s (load 104) |
| refinery | 212 s | 302 s (load 126) | 258 s (load 121) |
| refinery/editorial | 167 s | 372 s (load 124) | 280 s (load 127) |
| witness | 95 s | 85 s (load 105) | 144 s (load 125) |
| checkpoint | 88 s | 49 s (load 117) | 68 s (load 110) |

Running a package alone did not make it faster, because the host was never
idle. Other gates set the load, and run-to-run spread at similar load is
1.5-1.7x. The gate's wall is a function of host contention as much as of the
code. For comparison, gt-22hdp.60 recorded the same suite at 217 s uncached
at load 5-14. A unit tier whose wall moves 3x with neighbours' load is
measuring the kernel, not the code.

The serial fraction is what makes a package slow:

| package | serial / total tests | serial sum | package wall |
|---|---|---|---|
| cmd | 911 / 2371 | 402 s | 472 s |
| witness | 132 / 363 | 72 s | 85 s |
| checkpoint | 48 / 48 | all | 49-68 s |
| version | 26 / 26 | all | 126 s |
| beads | 501 / 561 | 47 s | 47 s |
| web | 240 / 240 | all | 88 s |
| dog | 85 / 85 | all | 73 s |

## 7. Recommendation

What "a test that actually works" means here:

- It finishes in **under 1 s** at any host load, and under 100 ms typically.
- It **starts no process**: no git, bd, dolt, tmux, gt, go, and no stub
  script. The seam is an interface, and the fake runs in-process.
- It is **deterministic**: no sleeps, no wall-clock polling, no real timers.
  Time is injected.
- It is **parallel-safe**: no t.Setenv, t.Chdir, or package-global swaps.
  Inputs are arguments or struct fields, so every test can call t.Parallel.
- It has **no live-town coupling**: nothing reads HOME, cwd, or the town,
  and the harness tripwire is not needed to keep it honest.

Tests that must exercise real git or bd are adapter tests. They belong in
`make test-integration`, run once per adapter, not once per caller. The 60
TRUE UNIT packages already meet this bar and total 88 package-seconds. That
is the proof it is achievable in this codebase.

### (a) REPAIR: keep the tests, change the plumbing

Steps, in order:

1. Delete the delete-list packages as D2/D4/D7/D1 land. This removes 702
   package-seconds, including the two biggest exec producers: refinery 6020
   and editorial 3336 git execs per run. Critical path unchanged.
2. Move every real-binary test to the integration tier. That is
   `*RealRepo*`, `*RealGit*`, `*RealBd*`, the runDone family, the dog
   `*_Integration_*` tests, and version, checkpoint, crew, git and land. It
   takes cmd's serial tail from about 400 s to about 100 s loaded.
3. Put a `git.Runner` interface under internal/git and replace per-test
   `git init` fixtures with an in-memory fake. Do this for the git consumers
   that stay: polecat, witness, daemon, doctor, rig.
4. Replace PATH bd stubs with the existing beads.Client fake
   (internal/beads/beadsfake). The 170 PATH-Setenv sites in cmd then go
   parallel. This is gt-22hdp.57's plan.

Projected result at 0908-like load:

| stage | projection |
|---|---|
| phase 1 | about 20 s (git and land leave) |
| phase 2 | cmd 100-150 s, next witness/daemon/polecat under 30 s each |
| Go suite | about 2.5-3 min, down from 6.5 |
| gate | about 7-8 min with lint/build unchanged, 4-5 min if lint stops queuing on the host lock |

Cost: 1-2 days for steps 1-2, multi-day for step 3, and gt-22hdp.57's
multi-round work for step 4.

Risk: repaired tests keep their old shape. They are still large, scenario-style
tests over package globals, so the serial count only falls as fast as the
globals are removed.

### (b) REWRITE: new unit tests against interfaces

Throw away the unit tests of the subprocess packages. Move the few real-git
and real-bd scenarios that encode incident knowledge into the integration
tier. Write new tests against interfaces: git.Runner, beads.Client (exists),
tmux (tmuxfake exists), notify (notifyfake exists), and a clock.

The target is the shape of the 60 existing unit packages: every test
t.Parallel, no exec.

Projected result:

| stage | projection |
|---|---|
| Go suite | test binary time well under 30 s per package; wall dominated by compiling cmd's test binary (75 s measured at load ~100) |
| Go suite total | 1.5-2 min loaded, under 1 min quiet |
| gate | 3-4 min with lint as today |

Cost: larger up front. cmd alone has 2371 tests. Production code also needs
seams where it shells out directly, which D7's thin-cobra extraction already
plans.

Work order, by seconds off the critical path per hour of work:

| # | package or work | saves | effort | seconds per hour |
|---|---|---|---|---|
| 1 | cmd runDone family (13 serial tests via runDoneSubmitWithFlags) | ~175 s serial | ~2-3 h | ~60-90 |
| 2 | cmd prune/recover/git-state serial tests (46 tests >=1 s) | ~100 s | ~4-6 h | ~20 |
| 3 | version | 87-126 s | ~1-2 h | ~60 |
| 4 | checkpoint | 49-88 s | ~1-2 h | ~40 |
| 5 | web RealBdSchema | 63 s | ~0.5 h | ~120 |
| 6 | beads bd init | 14 s | ~0.5 h | ~25 |
| 7 | witness (serial 72 s), dog (73 s, all serial, *_Integration_ names) | ~140 s off phase 2 | ~1 day | ~15 |
| 8 | git and land via git.Runner | phase 1 from 57 s to ~15 s | ~1 day | ~5 |
| 9 | refinery, editorial | 379 s of package time | none: deleted by D2 | n/a |

Notes on the rows:

- **Row 1:** move the tests to integration, then write in-process tests of
  runDone's decisions against fake git and beads.
- **Row 2:** each needs a small in-memory repo model.
- **Rows 3-4:** 26 and 48 serial git-repo tests. Swap in the runner
  interface; the production code is small.
- **Row 5:** move to integration outright. It is the #2 slowest test in the
  suite.
- **Row 8:** the adapter's own tests stay in integration. Callers use the
  fake.

**Recommendation: (b) for cmd, version and checkpoint. (a), meaning move to
integration and delete, for everything else.**

cmd is the critical path. Its slow tests are end-to-end scenarios over
package globals, and D7 is already moving its logic into leaf packages.
Rewriting cmd's tests against the extracted interfaces is the only path that
gets cmd's serial tail under 60 s.

Elsewhere, moving the real-binary tests to integration and letting the delete
list land gets most of the benefit for hours of work, not days.

Two things are outside test code and matter as much:

- **Lint lock.** Measure lint and build separately under gate load. The ~5
  min before the unit tier starts is inferred, and the host-wide golangci-lint
  lock serialises every concurrent gate.
- **Load-sensitive target.** The 180 s target cannot be met while 5-6 gates
  share 24 cores with exec-bound tests. Either the tests stop forking or the
  gate count is capped.

## Artifacts (scratchpad, not committed)

All under `/private/tmp/claude-501/-Users-sloan-gt/435e51c1-2aee-45d7-9bc7-23f64635ca30/scratchpad/`:

- `diag-standalone/`: top-5 x2 runs, -v logs and `time -l`.
- `diag-shimrun/`: 49 packages, -v logs, per-package exec logs, summary.
- `diag-cmdprobe/`: single-test probes of cmd's slowest tests.
- `diag-gitland/`: git/land re-runs and the test.parallel=1 run.
- `diag-forkbench/`: the fork/exec benchmark.
- `diag-pkgtable.md`, `diag-top30.tsv`, `diag-class.tsv`: the generated tables.
