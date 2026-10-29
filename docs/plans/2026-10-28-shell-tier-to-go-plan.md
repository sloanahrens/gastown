> Status: historical (2026-10). Merged in: gt-h0b6y (design and plan; the implementation beads are filed from the plan, epic gt-u560q). Not maintained.

# The shell tier in Go: implementation plan

> **For agentic workers:** the executor here is the Gas Town dispatcher. Each task below becomes one shaped bead (Goal, Constraints, Out of scope, Gate, Size, Acceptance) filed human-only and assigned to the overseer, who releases them. A task's "Acceptance" lines are the bead's checkbox items; the polecat writes the code test-first.

**Goal:** replace the post-land shell tier's three orchestrating scripts with one Go runner that runs each shell test only when its inputs changed, and port `scripts/makefile-gate_test.sh` to Go.

**Architecture:** package `internal/shelltier` (manifest, runner, summary line, trigger via `internal/tiertrigger`), run as `go run ./internal/shelltier/cmd/shelltier` from the worktree under test. Package `internal/makegate` holds the Go port of `makefile-gate_test.sh`, one recipe family at a time.

**Tech stack:** Go; `internal/tiertrigger` (`Due`, `State`, `Store`, `Git`, `Files`); `internal/land` (`ShellTierInputs`, `shellTierSummaryRE`, exported by task A3 as `ShellTierSummaryRE`); `make -n` for recipe-text checks.

**Spec:** `docs/plans/2026-10-28-shell-tier-to-go-design.md`.

## Scope (a scope split, not a placeholder)

In this plan: (A) the runner program, design sections 3.1 to 3.5; (B) the port of `scripts/makefile-gate_test.sh` (101 cases, 44 s serial), which blocks the `go vet -tags integration` gate wiring (operator follow-up on `gt-tpg0p`, 2026-10-26).

Not in this plan, each with its reason:

- The ports and deletions of the other scripts. They follow from the classification of all 73 allowlisted scripts, a bird report the operator must see first (grilling answer Q6); they get their own plan.
- The integration and race branches of `scripts/tier-sweep.sh`: their own port.
- `deploy/k8s/kind/*`, `scripts/deep-review*`, `scripts/install-gt*`: deleted by their own programs (delete when the k8s driver replaces the cluster; the deep-review plan; `gt-tpg0p.14`). `install-om` leaves with `gt-tpg0p.16.6`.
- The `go vet -tags integration` Makefile wiring itself: a Makefile change, the overseer's, unblocked by B13 below.

## Global constraints (every task)

- Read `~/.claude/docs/standards/test-engineering.md` before writing a test. Never raise a timeout, budget or ratchet; never skip or weaken a test; no retry-to-pass.
- Unit tests use fakes for exec, clock, filesystem and the history reader; no unit test starts a process or waits in real time.
- No new scripts. A thin `exec` wrapper is allowed only where a task says so, with its reason.
- A task's acceptance boxes use a narrow `go test -run` command; never a whole container-suite package (`internal/daemon`, `internal/cmd`, `internal/polecat`, `internal/beads`); never `make gate`; no live-town measurement (live before/after goes in Gate as owed by the overseer).
- Each task is one package and about twenty minutes. Tasks that share a package are chained so the dispatcher runs one at a time.
- My crew clones are shallow. The `makefile-gate_test.sh` tracker case (its "first commit" base) needs a full-history clone or CI.

## Verified facts the tasks rely on

| Fact | Source |
|---|---|
| The summary line is `tier-sweep: shell <GREEN\|RED> passed=N failed=N skipped=N[ failed: <names>] (logs <dir>) in <dur>`; verdict is RED when failed > 0 or when passed+failed+skipped is 0; `<dur>` is `Ns`, `NmNs` or `NhNmNs` | `scripts/tier-sweep.sh` `summary()` (line 79) and `fmt_duration()` (line 60) |
| The daemon and the post-land revert parse the RED line with `shellTierSummaryRE` (unexported today; task A3 exports it as `ShellTierSummaryRE`) | `internal/land/gate.go` |
| `tiertrigger.Due(st State, pkgs []Package, g Git, now time.Time, loc *time.Location) (due []string, unreadable map[string]error)`; `Package{Name string; Triggers []string}`; `State map[string]Record{Commit string; At time.Time}`; `State.Ran(pkg, commit string, when time.Time)`; `Store{Path string; Files Files}` with `Load() (State, error)` and `Save(State) error`; `Git` has `DiffNameOnly(base, head string) ([]string, error)` and `Worktree() ([]string, error)`; `Files` has `ReadFile`, `WriteFile`, `RemoveFile`; `OSFiles{}`; `DailyZoneLocation()` | `internal/tiertrigger/state.go`, `daily.go` |
| `land.ShellTierInputs` is an ERE constant, pinned to the INPUTS line of `scripts/post-land-shell.sh` by `TestShellTierInputsMatchPostLandScript` | `internal/land/gate.go`, `gate_test.go:526` |
| The sweep's shell stage budget is `tierSweepShellBudget` = 20 minutes; only the integration stage sets `slot: true` | `internal/daemon/tier_sweep.go` (line 932) |
| Today the tier runs 28 scripts: `scripts/test-makefile.sh` runs 24 in a fixed order ending with `makefile-gate_test.sh`; the sweep then runs four more (`.githooks/pre-push_test.sh`, `scripts/guards/context-budget-guard_test.sh`, `scripts/post-land-shell_test.sh`, `scripts/town_metrics_test.sh`) | `scripts/test-makefile.sh`, `scripts/tier-sweep.sh` lines 158 to 170 |
| Today `passed=5` counts `test-makefile.sh` as one script; the runner counts each script, so `passed=28` | the post-land logs in `~/.runtime/landing-logs/gastown/post-*/test.log` |
| The sweep bead's acceptance text names `GT_TEST_DOCKER=0 bash scripts/test-makefile.sh` | `internal/daemon/tier_sweep_bead.go`, `tier_sweep_bead_test.go:385-396` |

---

# Part A: the runner (`internal/shelltier`)

Tasks A1 to A11 share one package and run in order. A12 and the cutover tasks touch other packages and are chained after the tasks they depend on.

### Task A1: Manifest and guard

**Files:** create `internal/shelltier/manifest.go`, `manifest_test.go`, `main_test.go`.

**Interfaces:**
- Produces: `type Mode int` with `Serial` and `Parallel`; `type Entry struct { Path string; Triggers []string; Mode Mode; BuildsGo bool }`; `var DefaultTriggers = []string{"scripts/", "Makefile"}`; `var GoTriggers = []string{"go.mod", "go.sum", "cmd/", "internal/", "plugins/"}`; `func Manifest() []Entry` (today's 28 scripts in today's order, every one `Serial`, every one with `DefaultTriggers`); `func (e Entry) TriggersOrDefault() []string`.
- Guard seam: `type Lister func() ([]string, error)` returning the tracked `*_test.sh` paths (production: `git ls-files '*_test.sh'`, minus any path containing `/testdata/`).

**Acceptance (bead boxes):**
- [ ] `TestManifestCoversEveryTrackedShellTest`: with a fake `Lister` returning the 28 paths the test passes; a tracked `*_test.sh` missing from the manifest fails with its path named. Mutation: delete one manifest entry; the test fails.
- [ ] `TestManifestEntriesExist`: a manifest path the filesystem fake does not hold fails and names it. Mutation: add a bogus path; the test fails.
- [ ] `TestManifestOrderIsToday`: the first 24 entries are the `scripts/test-makefile.sh` order, ending with `scripts/makefile-gate_test.sh`, then the four extras. Mutation: swap two entries.
- [ ] `TestTriggersOrDefault`: an entry with no triggers returns `DefaultTriggers`; an entry with `BuildsGo` also returns every `GoTriggers` path. Mutation: ignore `BuildsGo`.
- [ ] `TestOnlyInstallGtBuildsGo`: `scripts/install-gt_test.sh` is the one entry with `BuildsGo` (it compiles `./cmd/install-lock`, `./cmd/image-supervisor` and `./plugins/polecat-image` for real, lines 294 to 303, so a Go-only landing can change its result; every other script stubs `go`). Mutation: clear its flag.
- [ ] `go test ./internal/shelltier -run 'TestManifest|TestTriggersOrDefault' -count=1` passes and `go vet ./internal/shelltier` is clean.

**Depends on:** nothing.

### Task A2: The serial runner

**Files:** create `internal/shelltier/run.go`, `run_test.go`.

**Interfaces:**
- Consumes: `Entry`, `Manifest()` from A1.
- Produces: `type Command struct { Dir string; Argv []string; Env []string }`; `type Exec func(ctx context.Context, c Command) (exitCode int, err error)`; `type Config struct { Entries []Entry; Root, LogDir string; Budget time.Duration; Exec Exec; Now func() time.Time; WithTimeout func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) }`; `type Result struct { Passed, Failed, Skipped int; FailedNames []string; Elapsed time.Duration }`; `func Run(ctx context.Context, cfg Config) Result`.
- Behaviour: each entry runs `bash <path>` in `Root` with `GT_TEST_DOCKER=0` and `TMPDIR=<LogDir>/tmp/<entry log name>`, its combined output going to `<LogDir>/shell-<path with / as _>.log`; exit 0 is passed, non-zero or a timeout (the `Budget` context ends) is failed and its path is recorded in order.

**Acceptance:**
- [ ] `TestRunCountsPassedAndFailed`: a fake `Exec` failing the second of three scripts gives Passed 2, Failed 1, `FailedNames` the second path. Mutation: count a failure as passed.
- [ ] `TestRunBoundsAScriptThatNeverReturns`: a fake `Exec` that blocks until its context ends, with a `WithTimeout` that cancels at once, gives one failed script and the others still run. Mutation: ignore the context.
- [ ] `TestRunGivesEachScriptItsOwnEnvAndLog`: the recorded commands carry `GT_TEST_DOCKER=0`, a distinct `TMPDIR`, and the log name rule above.
- [ ] `go test ./internal/shelltier -run 'TestRun' -count=1` passes and `go vet ./internal/shelltier` is clean.

**Depends on:** A1.

### Task A3: The summary line and exit codes

**Files:** create `internal/shelltier/summary.go`, `summary_test.go`, `summary_land_test.go` (package `shelltier_test`).

**Interfaces:**
- Consumes: `Result` from A2.
- Produces: `func FormatDuration(d time.Duration) string` (`Ns`, `NmNs`, `NhNmNs`, whole seconds); `func Summary(r Result, logDir string) string`; `type Exit int` with `ExitGreen = 0` and `ExitRed = 1`; `func (r Result) Exit() Exit`.
- Verdict: RED when `Failed > 0` or `Passed+Failed+Skipped == 0`, otherwise GREEN; the RED form appends ` failed: <names joined by a space>`.

**Acceptance:**
- [ ] `TestSummaryGolden`: a table of GREEN (`tier-sweep: shell GREEN passed=28 failed=0 skipped=0 (logs /d) in 4m47s`), RED with names, and the empty tier (RED, no scripts) matches `scripts/tier-sweep.sh`'s output byte for byte. Mutation: change the field order.
- [ ] `TestFormatDuration`: 0s, 59s, 60s, 3599s, 3600s, 3661s give `0s`, `59s`, `1m0s`, `59m59s`, `1h0m0s`, `1h1m1s`.
- [ ] `TestRedSummaryParsesWithLandsRegexp` (in `shelltier_test`, importing `internal/land`): a RED summary with two failed scripts is matched by `land.ShellTierSummaryRE` (an exported alias is added in `internal/land` if the regexp is unexported; that one-line export is part of this task) and captures both names without the `(logs …)` suffix. Mutation: drop the `(logs …)` suffix from `Summary`.
- [ ] `TestExitCodes`: GREEN exits 0, RED exits 1.
- [ ] `go test ./internal/shelltier ./internal/land -run 'TestSummary|TestFormatDuration|TestRedSummary|TestExitCodes|TestShellTierSummary' -count=1` passes.

**Depends on:** A2. (Touches `internal/land` only to export the regexp; the bead says so.)

### Task A4: The command and the end-to-end test

**Files:** create `internal/shelltier/cmd/shelltier/main.go`, `main_test.go`, `internal/shelltier/osexec.go`, `internal/shelltier/e2e_integration_test.go` (`//go:build integration`).

**Interfaces:**
- Consumes: `Run`, `Summary`, `Result.Exit`, `Manifest`.
- Produces: `func Main(args []string, out io.Writer, env Env) int` with flags `--all` (every entry), `--root <dir>` (default `.`), `--logdir <dir>` (default a new temp dir under `$TMPDIR`), `--budget <duration>` (default 10m); it prints `tier-sweep: shell <name> ...` to the error stream before each script as `scripts/tier-sweep.sh` does, then the summary line to the error stream, and returns the exit code. `osexec.go` is the real `Exec` using `os/exec`.
- Without `--all` this task runs the same as `--all` (the trigger arrives in A6 and A7).

**Acceptance:**
- [ ] `TestMainRunsEveryEntryWithAll`: with a fake `Exec`, `--all` runs every manifest entry once in order. Mutation: skip the last entry.
- [ ] `TestMainPrintsTheScriptLinesAndTheSummary`: the error stream carries one `tier-sweep: shell <path> ...` line per script and then the summary. Mutation: omit the per-script lines.
- [ ] `TestMainRejectsAnUnknownFlag`: exit 2 with the usage line.
- [ ] `TestIntegrationOSExecReportsTheExitCode` (a real `true` and `false` through `os/exec`, in the integration-tagged file): exit 0 and exit 1.
- [ ] `TestIntegrationRunnerEndToEndOnATwoScriptFixture` (integration tag): a temp directory with one passing and one failing `*_test.sh`, a fixture manifest, and the real runner prints a RED summary naming the failing one and exits 1.
- [ ] `go test ./internal/shelltier/... -run 'TestMain' -count=1` and `go test -tags integration ./internal/shelltier -run 'TestIntegration' -count=1` pass.

**Depends on:** A3.

### Task A5a: Cutover of the sweep's shell stage (for the overseer)

**Files:** `internal/daemon/tier_sweep.go` (the shell stage's `command()`), `internal/daemon/tier_sweep_bead.go` (the acceptance text that names `bash scripts/test-makefile.sh`), and their tests.

**Behaviour:** the shell stage runs `go run ./internal/shelltier/cmd/shelltier --all` and the sweep bead's acceptance text names that command. The integration and race stages are unchanged.

**Acceptance:**
- [ ] a test of the stage table shows the shell stage's command is the runner with `--all`, the integration stage's is unchanged, and only the integration stage has `slot: true`. Mutation: restore the old command.
- [ ] a test of the sweep bead's acceptance text shows it names the runner command, not `scripts/test-makefile.sh`. Mutation: restore the old text.
- [ ] `go test ./internal/daemon -run 'TierSweep' -count=1` (narrow filter only) passes.

**Depends on:** A4. Filed human-only, assigned to the overseer. Gate owed by the overseer: one live sweep whose shell result line parses (`passed=28`).

### Task A5b: Cutover of the make targets (for the overseer)

**Files:** `Makefile` (the `test-slow` and `test-makefile` recipes and the `SHELL_TESTS` variable), `scripts/makefile-gate_test.sh` (its cases for these two recipes), `scripts/tier-sweep.sh` (the shell branch becomes one `exec` line of the runner: a thin wrapper, allowed because the sweep script still serves the integration and race tiers until they are ported).

**Acceptance:**
- [ ] the `makefile-gate_test.sh` cases `make -n test-slow exits 0`, `test-slow runs the gate, then the shell tests, containers off, no slot` and `test-makefile runs the script alone, as test-slow does` are updated to the runner command and pass (`bash scripts/makefile-gate_test.sh` in a full-history clone).
- [ ] `scripts/tier-sweep_test.sh` passes with the shell branch delegating.
- [ ] `grep -n 'test-makefile.sh' Makefile` prints nothing.

**Depends on:** A4. Human-only, overseer.

### Task A6: Per-script trigger selection

**Files:** create `internal/shelltier/select.go`, `select_test.go`.

**Interfaces:**
- Consumes: `tiertrigger.Due`, `tiertrigger.Package`, `tiertrigger.State`, `tiertrigger.Git`; `Entry`.
- Produces: `func Select(entries []Entry, st tiertrigger.State, g tiertrigger.Git, now time.Time, loc *time.Location) (run, skipped []Entry, unreadable map[string]error)`; `func Record(st tiertrigger.State, e Entry, commit string, now time.Time)` (calls `st.Ran(e.Path, commit, now)`); `func StatePath(townRoot string) string` returning `<townRoot>/.runtime/tiertrigger/shell-state.json`.
- Behaviour: an entry runs when `Due` lists it; every other entry is skipped; an entry whose record cannot be diffed runs and appears in `unreadable`.

**Acceptance:**
- [ ] `TestSelectRunsAnEntryWithNoRecord` (empty state runs everything). Mutation: skip entries with no record.
- [ ] `TestSelectRunsWhenATriggerPathChanged` and `TestSelectSkipsWhenOnlyUnrelatedPathsChanged` (a fake `Git`; the entry's triggers are `scripts/install-gt.sh`). Mutation: invert the match.
- [ ] `TestSelectRunsWhenTheDailyWindowHasPassed`.
- [ ] `TestSelectRunsAndReportsAnUnreadableRecord` (a `Git` that errors): the entry runs and is in `unreadable`.
- [ ] `TestStatePathIsSeparateFromTheContainerState`: the path ends in `shell-state.json`, not `state.json`.
- [ ] `go test ./internal/shelltier -run 'TestSelect|TestStatePath' -count=1` passes.

**Depends on:** A4 (and A1).

### Task A7: `--since-green` in the runner

**Files:** modify `internal/shelltier/cmd/shelltier/main.go`, add `internal/shelltier/since.go`, `since_test.go`.

**Behaviour:** `--since-green --town <dir>` loads the shell state (`Store{Path: StatePath(town), Files}`), calls `Select`, runs only the selected entries, counts the rest as `skipped`, and after the run records `Record(st, e, <HEAD commit>, now)` for each entry that passed, then saves the state. A state that cannot be loaded runs everything and says so on the error stream. `--all` ignores the state and does not write it. HEAD comes from an injected `Head func() (string, error)`.

**Acceptance:**
- [ ] `TestSinceGreenRunsOnlyTheSelectedEntries` (summary shows `skipped=` equal to the unselected count). Mutation: run all.
- [ ] `TestSinceGreenRecordsOnlyPassedEntries`: a failed entry keeps its old record so it reruns next time. Mutation: record failures too.
- [ ] `TestSinceGreenFailsOpenOnAnUnreadableState`: a corrupt state file runs every entry. Mutation: treat it as empty-and-skip.
- [ ] `TestAllIgnoresAndLeavesTheState`: the state file is not written by `--all`.
- [ ] `go test ./internal/shelltier/... -run 'TestSinceGreen|TestAll' -count=1` passes.

**Depends on:** A6.

### Task A7b: The path-scan guard and the go-command guard

**Files:** create `internal/shelltier/triggers_guard_test.go`.

**Behaviour:** for each manifest entry, scan the script's text for repo-relative paths it names (`scripts/…`, `deploy/…`, `plugins/…`, `.githooks/…`, `Makefile`, `internal/…`), and fail when a named path is not at or under one of the entry's triggers (`tiertrigger.Runs`). Entries with the default triggers (`scripts/`, `Makefile`) are checked the same way, so `deploy/…` and `plugins/…` references force their triggers to be declared. A second rule covers what a path scan cannot see: a Go change can change a test that compiles the repository's Go for real. Every script whose text names `go build`, `go run`, `go test`, `go vet`, `go install` or `make install` must either have `BuildsGo` set or appear in a stub-only list in the guard test with a one-line reason each (install-om_test.sh: a `go.log` stub; tier-sweep_test.sh: a fake `tiertrigger`; makefile-gate_test.sh: a stub `go`; and whatever else the scan finds).

**Acceptance:**
- [ ] `TestEveryReferencedRepoPathIsATrigger` passes on the manifest as filed, which means A1's default triggers are widened in this task wherever the scan finds an uncovered path. Mutation: remove one trigger; the test fails and names the script and path.
- [ ] `TestScanFindsPathsInQuotesAndVariables` (a fixture script text).
- [ ] `TestEveryGoCommandScriptIsBuildsGoOrStubOnly`: a fixture script naming `go build` with neither `BuildsGo` nor a stub-only entry fails and names it; the real manifest passes. Mutation: clear `BuildsGo` on `scripts/install-gt_test.sh`.
- [ ] `go test ./internal/shelltier -run 'TestEveryReferenced|TestScanFinds' -count=1` passes.

**Depends on:** A7 (chained so the dispatcher runs one task at a time in `internal/shelltier`; it comes before A8 because A8 builds the union of the triggers this task widens).

### Task A8: `land.ShellTierInputs` from the manifest

**Files:** `internal/land/gate.go`, `internal/land/gate_test.go` (replace `TestShellTierInputsMatchPostLandScript`).

**Behaviour:** `ShellTierInputs` is built from the union of the manifest's triggers (`shelltier.Manifest()`), as an ERE of the same shape, so it covers at least every path of today's constant (`scripts/`, `plugins/`, `.githooks/`, `Makefile`, `internal/testpolicy/docker.txt`, `internal/cmd/scheduler_integration_test.go`). `internal/shelltier` must not import `internal/land` outside its external test packages.

**Acceptance:**
- [ ] `TestShellTierInputsCoverTheManifestTriggers`: every trigger of every entry matches the ERE. Mutation: drop a trigger from the union.
- [ ] `TestShellTierInputsCoverTodaysRule`: each of the six path families of today's constant still matches. Mutation: remove `plugins/`.
- [ ] the old `TestShellTierInputsMatchPostLandScript` is gone, and `grep -n 'INPUTS' internal/land/gate_test.go` prints nothing.
- [ ] `go test ./internal/land -run 'TestShellTierInputs' -count=1` passes and `go build ./...` succeeds.

**Depends on:** A7b. The union includes `GoTriggers` while any entry has `BuildsGo`, so the post-land revert's blame rule is honest about `install-gt_test.sh` until `gt-tpg0p.14` deletes it.

### Task A9: The timing log

**Files:** create `internal/shelltier/timing.go`, `timing_test.go`; modify `internal/shelltier/run.go` to call it.

**Interfaces:** `type TimingFiles interface { Append(path string, line []byte) error }`; `func TimingPath(townRoot string) string` returning `<townRoot>/.runtime/tiertiming/shell.jsonl`; one JSON line per script: `{"script":…,"seconds":…,"exit":…,"at":…}` (RFC 3339 UTC).

**Acceptance:**
- [ ] `TestTimingLineIsOneJSONObjectPerScript` (golden). Mutation: omit `exit`.
- [ ] `TestTimingAppendFailureIsLoggedNotFatal`: the run's result and exit are unchanged. Mutation: return the error.
- [ ] `TestTimingPathIsUnderTheTownRuntime`.
- [ ] `go test ./internal/shelltier -run 'TestTiming' -count=1` passes.

**Depends on:** A7.

### Task A10: The parallel pool

**Files:** modify `internal/shelltier/run.go`; add `pool_test.go`; add `Jobs int` to `Config` and a `--jobs` flag (default 1) in the command.

**Behaviour:** entries with `Mode == Parallel` run through at most `Jobs` workers, then the `Serial` entries run one at a time after the pool has drained; results, log files and the summary keep manifest order whatever the finish order; one failure never cancels the others.

**Acceptance:**
- [ ] `TestPoolNeverExceedsTheCap` (a fake `Exec` that records concurrent runs; cap 3, eight entries). Mutation: use `Jobs+1`.
- [ ] `TestSerialEntriesRunAlone`: no other entry overlaps a `Serial` one. Mutation: start serial entries inside the pool.
- [ ] `TestResultOrderIsTheManifestOrder`: `FailedNames` is in manifest order though the failing entries finish out of order.
- [ ] `TestOneFailureDoesNotCancelTheOthers`.
- [ ] `go test ./internal/shelltier -run 'TestPool|TestSerialEntries|TestResultOrder|TestOneFailure' -race -count=1` passes.

**Depends on:** A9.

### Task A11: Flip the forgejo tests to parallel, with the proof

**Files:** `internal/shelltier/manifest.go` (four entries to `Parallel`), `internal/shelltier/parallel_integration_test.go` (`//go:build integration`).

**Behaviour:** `scripts/forgejo-cutover_test.sh` (13.4 s), `forgejo-probe_test.sh` (11.9 s), `forgejo-provision_test.sh` (7.8 s) and `forgejo-resync_test.sh` (6.2 s) become `Parallel`. The proof is an integration-tagged test that runs those four through the real runner with `--jobs 3`, three times in a row, and requires three green summaries.

**Acceptance:**
- [ ] `TestIntegrationForgejoTestsRunConcurrentlyThreeTimesGreen` (integration tag) passes. Mutation: make one of the four write a fixed temp path; the test fails.
- [ ] `TestManifestMarksExactlyTheProvenScriptsParallel`: only these four are `Parallel`. Mutation: flip a fifth.
- [ ] `go test -tags integration ./internal/shelltier -run 'TestIntegrationForgejoTests|TestManifestMarks' -count=1` passes.

**Depends on:** A10. Gate owed by the overseer: the same proof on the live town (three concurrent runs on the real host).

### Task A12: Delete the three orchestration scripts

**Files:** delete `scripts/test-makefile.sh`, `scripts/post-land-shell.sh`, `scripts/post-land-shell_test.sh`; remove their lines from `internal/repoguard/shell-scripts.txt`; remove `scripts/post-land-shell_test.sh` from the manifest; update every reference found by `git grep -n -E 'test-makefile\.sh|post-land-shell'`.

**Acceptance:**
- [ ] `git grep -n -E 'test-makefile\.sh|post-land-shell' -- . ':!docs'` prints nothing.
- [ ] `go test ./internal/repoguard ./internal/shelltier ./internal/land -run 'Shell|Manifest|Guard' -count=1` passes.
- [ ] the three allowlist lines are gone from `internal/repoguard/shell-scripts.txt`.

**Depends on:** A5a, A5b, A11; and the overseer's change of gastown's `post_land_command` to `GT_TEST_DOCKER=0 go run ./internal/shelltier/cmd/shelltier --since-green` (settings, not a bead; filed as an owed action on this task's Gate).

---

# Part B: the port of `scripts/makefile-gate_test.sh` (`internal/makegate`)

101 cases in nine families. Every case below is accounted for by line number in `scripts/makefile-gate_test.sh` at `eec0ced3`. Tasks B1 to B13 share one package and run in order.

**Tier and naming (decided here, not by a measurement):** every test in `internal/makegate` runs real `make` (a process), so it is integration tier by the test standard (rule 2: unit tier is in-process; the standard's own answer for process-based checks is an integration tag, plus a trigger and a sweep). Every test file is `*_integration_test.go`, starts with `//go:build integration`, and every test is named `TestIntegration<Case>` (the repo's pattern, as in `internal/testutil`). Every `go test` command here carries `-tags integration`; an untagged `go test ./internal/makegate` runs nothing and would pass vacuously. The seam is `type Make func(args ...string) (stdout string, err error)` whose real value runs `make -C <root> --no-print-directory -n <args>`; stub-driven families add a `Sandbox` (in a tagged test file) that stages the Makefile and stub tools in a temp directory. The hourly tier sweep runs every package that has an integration-tagged test file (`scripts/tier-sweep.sh` `tagged_dirs`), so these cases run there. Consequence for the operator: the landing gate and the post-land run stop running these 101 cases; a Makefile-recipe regression is caught by the hourly sweep instead of within minutes after a landing (decision 4 of the design).

### Task B1: The seam and the first nine gate-shape cases

**Files:** create `internal/makegate/doc.go` (package clause and doc comment only, so `go build ./...` has a non-test file), `make_integration_test.go` (the `Make` seam, `lineOf` and the shared helpers; tagged, so no production code has only a test caller), `gate_shape_integration_test.go`.

**Cases ported (lines 35 to 89):** `make -n gate exits 0`; gate, test-slow, test-integration and test recipes do not recurse through `$(MAKE)`; gate runs lint, then `go build ./...`, then the cached unit tier; gate leaves the shell tests to test-slow; gate runs the unit tier over every package through the test cache (no budget `-exec`, no `-count=1`); gate only warns on wall time; gate prints its wall time; gate's lint waits on the lint lock; gate never takes the container-gate slot.

**Interfaces:** produces, in `make_integration_test.go`: `type Make func(args ...string) (string, error)`, `func RealMake(root string) Make`, `func lineOf(out, needle string) int` (the shell `line_of`).

**Acceptance:** [ ] one integration test per case above, named with the `TestIntegration` prefix (for example `TestIntegrationGateExitsZero`, `TestIntegrationGateNeverTakesTheSlot`), each failing when its recipe fact is reversed (the mutation is named in each test's comment). [ ] every file of the package except `doc.go` starts with `//go:build integration`, and `go vet -tags integration ./internal/makegate` is clean. [ ] `go test -tags integration ./internal/makegate -run 'TestIntegrationGate' -count=1` passes; the bead notes that an untagged run reports no test files and is not a pass. **Depends on:** nothing in Part A.

### Task B2: Gate shape, second nine (lines 93 to 145)

Cases: gate writes the container opt-in off where the unit tier starts; gate never turns the container opt-in on or defaults it; an inherited `GT_TEST_DOCKER=1` does not change the gate; gate carries one `-timeout`, on the unit tier's `go test`; `make $t is gone` (the retired targets); make test only chains test-slow and test-integration; the slow tier is gone (no `slow.txt`); no `PKGS` default for a changed-package gate; no recipe defaults the container opt-in from the environment. Test names `TestGateOptInOffWhereTheUnitTierStarts` and so on, one per case, each with its mutation. **Depends on:** B1. File `internal/makegate/gate_shape2_integration_test.go`.

### Task B3: test-slow, test-makefile, polecat-image and the dry-run targets (lines 150 to 242, 12 cases)

Cases: `make -n test-slow exits 0`; test-slow runs the gate then the shell tests, containers off, no slot; `make -n test-makefile exits 0`; test-makefile runs the script alone; the shell-test list runs the scripts the old hand list skipped; `make -n polecat-image exits 0`; polecat-image cross-compiles gt and the cache-trim helper; builds the Dockerfile tagged with the short sha and `:dev`; takes `BD_SRC`; labels the image with the bd commit; `make -n polecat-caches`; `make -n openshell-providers`. Note: after A5b the first five assert the runner command; this task ports whatever the file holds at the time (the bead names the lines). File `internal/makegate/targets_integration_test.go`. **Depends on:** B2.

### Task B4: The seed stage and pins (lines 277 to 363, 9 cases)

Cases: polecat-seed-stage stages the build rig at the build commit and every other rig at its pin; stops on a pin or build commit the checkout does not hold, naming rig and sha; refuses a build commit it cannot read; refuses an empty pins file; polecat-seed-pins pins each seeded rig to its HEAD; polecat-image runs the seed stage with the pin-drift guard before docker build; a stale committed pin stops the seed stage before anything is staged. Needs the `Sandbox` helper (temp git repos, `mkrig`/`seedmk` in the shell); the bead ports those two helpers first. File `internal/makegate/seed_integration_test.go`, `sandbox.go`. **Depends on:** B3.

### Task B5a: Node prune, first five (lines 378 to 431)

Cases: polecat-image does not call polecat-node-prune by default; calls it after docker build with `POLE_NODE_PRUNE=1`; polecat-node-prune removes the old polecat image from a node with keep 0; leaves a node it cannot list alone and names the failure; the polecat sandbox targets are declared and phony. File `internal/makegate/nodeprune_integration_test.go`. **Depends on:** B4.

### Task B5b: polecat-image guards (lines 441 to 600, 8 cases)

Cases: refuses to run without `BD_SRC`, before building; refuses a tree with an untracked file; lets a clean tree past its guard; labels the image with the commit the bd build helper printed; a default polecat-image leaves the sandbox nodes alone; prunes after the build with `POLE_NODE_PRUNE=1`; stops before docker when the bd build fails; refuses an empty bd commit. File `internal/makegate/polecatimage_integration_test.go`. **Depends on:** B5a.

### Task B6: test-integration (lines 605 to 651, 7 cases)

Cases: `make -n test-integration exits 0`; runs the integration tag with containers on; passes no `-run` name filter; the no-filter check rejects the pre-`gt-ik4a1.2` recipe; runs every package in `internal/testpolicy/docker.txt`; also vets and tests the tracker module with the integration tag; sets the test-Dolt init pool to the town slot count on both commands. File `internal/makegate/integration_integration_test.go`. **Depends on:** B5b.

### Task B7: test-integration-wall (lines 658 to 719, 8 cases)

Cases: `make -n test-integration-wall exits 0`; runs test-integration's two `go test` commands each `-json -count=1`; is an uncached run; the `-count=1` check rejects the pre-`gt-gq4gl` recipe; `INTEGRATION_GO_TEST` still swaps the runner with `-count=1` after it; the runner default carries no `-count=1`; pipes both commands through one `tierwall` run under pipefail; sets the test-Dolt init pool to the town slot count. File `internal/makegate/wall_integration_test.go`. **Depends on:** B6.

### Task B8: Gate failure paths with stubs, part 1 (lines 798 to 828, 5 cases)

Cases: green stubs (exit 0, wall printed, lint waited on the lock, only the Go suite ran with the opt-in off); a taxed host (the gate warns on the exec tax and still passes); lint fails (non-zero, nothing built or tested); build fails (names the build stage, unit tier never starts); Go suite fails (names the Go half). Ports the shell `run_gate` helper (stub `go`, `golangci-lint` and preflight) into `Sandbox`. File `internal/makegate/gate_paths_integration_test.go`. **Depends on:** B7.

### Task B9: The tracker module (lines 840 to 871, 5 cases)

Cases: green stubs vet and test the tracker module after the root suite; tracker tests fail (names the Go half, no PASSED line); root suite fails (the tracker module does not run); lint runs golangci-lint in the tracker module with the root config; a lint finding in the tracker module fails the gate and names the module. File `internal/makegate/tracker_integration_test.go`. **Depends on:** B8.

### Task B10: Presubmit, interrupt and test-slow paths (lines 882 to 925, 7 cases)

Cases: presubmit with a changed tracker module whose tests fail fails presubmit (needs a full-history clone for its "first commit" base: the test builds its own two-commit repo in the sandbox and does not depend on the clone's depth, which also removes the shallow-clone trap); an unchanged tracker module is not tested; a failing tracker integration test fails test-integration and names the module; an interrupted gate exits non-zero and the trap stops the suite; test-slow green stubs (Go suite and shell tests ran with the opt-in off); test-slow Go suite fails; test-slow shell tests fail (names the shell tests). File `internal/makegate/presubmit_integration_test.go`. **Depends on:** B9.

### Task B11: lint-lock-wait and the empty docker list (lines 944 to 975, 4 cases)

Cases: a lint contended twice is retried and passes; still contended at the wait bound exits with the lint's code and keeps the lock line; a lint with findings is never retried; test-integration refuses an empty `docker.txt` instead of testing nothing. The `lint-lock-wait` cases run `scripts/lint-lock-wait.sh` itself, so the bead does not port that script; it ports the four assertions with the script as the thing under test. File `internal/makegate/lintlock_integration_test.go`. **Depends on:** B10.

### Task B12: The `openshell-providers` cases (lines 1059 to 1196, 13 cases)

Cases: the openshell-providers cases run the stub gt, not the host's; the stub gt refuses a flag with no value and `--from-export` without `--render`; the Docker volume script `scripts/polecat-caches.sh` is gone; creates both providers the profiles name; puts no key in argv or on stdout and masks `provider create`'s output; names the missing profile directory and imports nothing; skips an existing provider without reading its key file; fails naming the file when a key file is missing; lints each profile before it imports it; keeps both keys out of an xtrace; masks a short credential by its value; reads a key file as data and never runs it; matches a provider by its whole name. First step: compare these with `scripts/openshell-providers_test.sh` (5.6 s) and write a table in the bead comment of which cases it already covers; port every case that is not covered, and for a duplicate say which test of the other file covers it and keep both until the other file is itself ported. No test is dropped without that table. If the table shows more than ten cases to port, the bead is split into two before it is worked. File `internal/makegate/providers_integration_test.go`. **Depends on:** B11.

### Task B13: Delete `scripts/makefile-gate_test.sh`

**Files:** delete `scripts/makefile-gate_test.sh`; remove its manifest entry and its `internal/repoguard/shell-scripts.txt` line; remove it from `scripts/test-makefile.sh` if that script still exists (it does not after A12, in which case the manifest line is the only reference).

**Acceptance:**
- [ ] a table in the bead comment accounts for all 101 cases by original line number and the Go test that replaced each (taken from B1 to B12).
- [ ] `git grep -n 'makefile-gate_test' -- . ':!docs'` prints nothing.
- [ ] `go test ./internal/makegate ./internal/shelltier ./internal/repoguard -count=1` passes.

**Depends on:** B12 and A12. Gate owed by the overseer: the hourly sweep's first shell run after this lands is green and the shell tier's wall is lower by about 44 s (the file's measured serial time).

---

## Dependency summary

A1 → A2 → A3 → A4 → (A5a, A5b: overseer) ; A4 → A6 → A7 → A7b → A8 → A9 → A10 → A11 ; A12 needs A5a, A5b, A11 and the overseer's `post_land_command` change. B1 → B2 → B3 → B4 → B5a → B5b → B6 → B7 → B8 → B9 → B10 → B11 → B12 → B13 (B13 also needs A12). Parts A and B touch different packages (`internal/shelltier` and `internal/makegate`), so they can run side by side; within each part the dispatcher runs one task at a time.

## Out of this plan, and what it waits for

The per-script ports (the other ~22 scripts in the manifest) wait for the classification of the 73 allowlisted scripts and the operator's choice of port, delete or keep for each. Its plan is written after that report.

## Self-review

- Spec coverage: design 3.1 (A1), 3.2 (A6, A7, A8), 3.3 (A2, A9, A10, A11), 3.4 (A3, A5a, A5b, A12), 3.5 (A1 to A12 and B1 to B13). Section 3.6's tests are in each task's acceptance. The path-scan guard is task A7b, placed before A12.
- Placeholder scan: none; every task names its files and tests. The names in Part B tasks B2 to B12 give the case titles and the file, and the polecat names the Go tests after the cases, as B1 shows.
- Type consistency: `Entry`, `Mode`, `Config`, `Result`, `Exec`, `Exit`, `Select`, `Record`, `StatePath` are defined in A1 to A6 and used unchanged in A7 to A12.
