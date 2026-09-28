> Status: approved design, 2026-09-27. Tracked in claude-a3e.2 (phase 2 of the refinery redesign, claude-a3e).
> Plan: `2026-09-27-test-rewrite-plan.md`. Evidence: `~/.claude/docs/research/test-rewrite/` (`profile.md`, `seams.md`).
> Supersedes the capacity-tuning approach of `2026-09-24-test-suite-concurrency-design.md`, which made the existing
> suite fit the host. This design changes the tests so they no longer depend on the host.

# Test Suite Rewrite — Design

## Why

The refinery cannot land work when a test run takes 20–45 minutes and fails on tests the change never touched. Most
refinery "fixes" have been layers added around a slow, flaky suite. The suite is the problem.

What the suite looks like on 2026-09-27 (`origin/main` f277930):

- 9,278 test functions in 941 files: 370k lines of test code against 328k lines of production code.
- 467 per-test fake-tool shell scripts put on `PATH`, and tests that `go build` the `gt` binary. Every new
  executable costs a macOS Gatekeeper/XProtect scan (about 167 ms when the tmux server is not exempt, about
  5 ms when it is). The scans go through one daemon, so under `-p=8` they queue and timing tests time out.
- Shared process state blocks parallelism: 1,857 `t.Setenv`, about 470 `Chdir`, about 209 package-level variables
  swapped by tests, and 62 `os.Setenv` in production code.
- Real waiting: 159 `time.Sleep` in production code (tmux 56, cmd 39, doltserver 22, daemon 17) and 198 in tests.
  There is no clock abstraction.
- Measured, with the scan exempt: daemon 235 s, tmux 145 s, cmd 126 s, refinery 95 s, polecat 89 s, slot 57 s,
  editorial 35 s. Test time by cause: Dolt/bd setup 49%, git 29%, tmux 10%, real sleeps 7%. The sleeps are almost
  all in tests that run one at a time, so they cost the most wall time.
- Coverage disappears silently under load. Setup helpers `t.Skip` on store errors, and tmux tests skip after an
  earlier test killed the shared server.

## Goals and non-goals

Goals, all measured in a pane that is **not** exempt from the macOS scan, uncached (`-count=1`), with `-p=8`:

- The unit tier, which is what every MR runs, finishes in **≤ 90 s**.
- The full suite, unit plus integration, finishes in **≤ 3 min**.
- **Zero failures** when the unit tier runs 20 times while a full suite runs alongside on the same host.
- The unit tier also passes on Linux (GitHub CI green).
- Every rule below is checked by code that runs in `make test`. Nothing depends on a memory note.

Non-goals:

- Keeping the tests compatible with upstream `gastownhall/gastown`. The fork is its own project now.
- Windows support. It is removed.
- Changing product behaviour. Production changes in this work are limited to seams: dependency injection of
  runners, clock, notifier, runtime config and town root.

## Decisions

| # | Decision |
|---|---|
| D1 | Deleting tests is allowed and expected. A test stays only if it checks behaviour a caller relies on. Tests that pin implementation details or re-enact one incident's exact sequence are deleted or folded into a behaviour test. Each package MR lists the deleted tests, each with a one-line reason, and reports coverage before and after. A drop of more than 2 points needs a written justification. |
| D2 | Two tiers, split by build tag. **Unit** is untagged. **Integration** is `//go:build integration`. |
| D3 | Fakes model behaviour (approach B). Consumers depend on small interfaces that they declare themselves. Shared, stateful in-memory fakes implement them. Only the wrapper packages use argument-level recording runners, to test their own translation into commands. |
| D4 | Every shared fake has a contract suite. The suite runs against the fake in the unit tier and against the real tool in the integration tier. |
| D5 | Clock: `github.com/jonboulle/clockwork`, injected as a field or option. |
| D6 | Real git stays in the unit tier. Each package builds one template repo and tests clone it locally. |
| D7 | Environment is resolved once into `config.Runtime` at the command or daemon boundary. The town root is passed explicitly. Production `os.Setenv` is removed. |
| D8 | Enforcement is a Go test (`internal/testpolicy`), not a lint configuration. `.golangci.yml` has `run.tests: false`, so golangci never sees test files. |
| D9 | Every MR runs the whole unit tier. Once the unit tier meets its target, the changed-packages machinery (`make test-changed`, `changedGoPackages`, the `{packages}` substitution in `test_verify_command`) is deleted. |
| D10 | `internal/cmd` becomes a thin cobra layer. Command logic moves into leaf packages that daemon, witness and refinery can import. The rest of the code cannot import `cmd`, which is why `gt` shells out to itself about 86 times today. |
| D11 | Work order: tmux, slot, polecat and editorial, `beads.Client`, notify, daemon, the rest of `internal/*`, cmd, finish. The policy is enforced per package as each converts. |
| D12 | Execution: this crew session does the scaffolding and the tmux pilot by hand. After that, Claude subagents each take one package in their own worktree, and each branch is reviewed in this session before `gt mq submit`. No polecats. The town is frozen for gastown feature work during the rewrite. |

## Architecture

### 1. Interfaces and fakes

Consumers declare the narrow interface they need (Go's structural typing; the codebase already has 15+ such
interfaces for tmux). No wrapper publishes a large interface. One shared fake per collaborator satisfies all of
the consumer interfaces.

| Package | Models | Scope |
|---|---|---|
| `internal/tmux/tmuxfake` | sessions: environment, panes, pane command, captured output, sent keys; scripted agent state (idle, ready, exited) | about 25 methods consumers call. By call-site count: `HasSession` 148, `KillSessionWithProcesses` 70, `ListSessions` 44, `Get/SetEnvironment` 52, `NudgeSession` 27, `CapturePane` 20, `SendKeys*` 19, `New*Session` 12, `WaitFor*`/`IsIdle`/`IsAgentRunning` 30 |
| `internal/beads/beadsfake` | issues, labels, deps, comments in memory, built on `beadsdk.Storage` (about 62 methods, from the embedded `steveyegge/beads` library) | the ~40 domain methods consumers call on a gastown-owned `beads.Client` |
| `internal/notify` + `notifyfake` | `Notifier{MailSend, Nudge, Escalate}`; production is backed by `mail.Router.Send`, `nudge.Enqueue`, and escalate extracted from `cmd` | replaces the gt-calls-gt sites |
| clock | `clockwork.Clock`; production uses `NewRealClock()`, tests use `NewFakeClock()` | daemon, tmux and slot first |
| `slot.ContainerRuntime` | `{List, Remove, Info}` | replaces slot's func vars and `Set*ForTest` |

Wrapper packages (`tmux.Tmux`, `git.Git`, and the `bd` side of `beads`) get an unexported `runner` field, which
defaults to running the real program. Their own unit tests use a recording runner that checks the command sent and
returns canned output. This is the only argument-level fake in the design.

Contract suites live in the fake package as exported functions, for example
`func RunSessionsContract(t *testing.T, newImpl func(t *testing.T) Sessions)`. The fake's unit test calls it.
One integration-tagged test calls it against the real implementation: real tmux on a `gt-test-*` socket, or a real
`bd` against Dolt.

Test-only code leaves production packages. Package-level stub variables and `Set*ForTest` setters are deleted as
each package converts.

### 2. Time, environment and working directory

- **Time.** Any type that sleeps, polls, ticks or reads the clock takes a `clockwork.Clock`: `Daemon` (15 tickers),
  `tmux.Tmux` (2 s kill grace, 500 ms send-keys delay, 8 s dialog poll), `slot` (2 s poll). The eight ad-hoc
  `sleepFn`/`nowFn`/`timeNow` variables are replaced by it. Tests move time with `Advance` and synchronize with
  `BlockUntilContext`. Poll intervals stay configurable as plain fields.
- **Environment.** A typed `config.Runtime` (town root, Dolt host and port, role, session, agent identity) is resolved
  once at the cobra root or at daemon start and passed down. The 345 `os.Getenv` calls move to that boundary, package
  by package. Child-process environments are built explicitly (`config.AgentEnv`, `exec.Cmd.Env`). The 62
  production `os.Setenv` calls are removed.
- **Working directory.** `workspace.FindFromCwd*` (385 calls) and `os.Getwd` (82) are called only at the command
  boundary. Everything below takes `townRoot` or `rigDir` as a parameter. `testutil.ScratchTown` stays.
- **Kept real:** the filesystem (`t.TempDir`) and git (template-repo clones).

Once a package is converted, every test in it runs under `t.Parallel()` with no shared state.

### 3. Tiers, commands and CI

| Target | Runs | Used by |
|---|---|---|
| `make test` | unit tier: `go test -p=8 -json ./...` piped through `internal/testpolicy/cmd/budget` (fails if a converted package takes over 10 s) | every MR: `gt done`, the refinery gate, CI |
| `make test-integration` | `go test -tags integration ./...`; Docker/Dolt suites run under `gt slot run` | `main_branch_test` after merge, on the Mac, target ≤ 90 s (so unit + integration ≤ 3 min) |
| `make lint` | existing golangci | every MR, CI |
| `make test-timing` | the unit tier in a launchd-started (non-exempt) tmux server, compared against 90 s | phase checkpoints and acceptance |

Removed along the way:
- `make test-changed`, `changedGoPackages` and the `{packages}` substitution in `test_verify_command` (D9).
- The `-timeout 20m` in `Makefile:220`.
- The container slot for MR gates, since the unit tier needs no Docker.
- The Windows CI, e2e, nightly and triage/label workflows, the 32 `*_windows.go` files, and the 563 Windows branches
  in tests.

One GitHub workflow is kept: `make lint` plus `make test` on Linux. Making it green is the last step of this work;
it has been red since 2026-09-08. `main_branch_test` switches its command to `make test-integration`. A red run
alerts the mayor, as it does today.

### 4. Enforcement: `internal/testpolicy`

A unit test loads every package with `golang.org/x/tools/go/packages` (syntax and types) and walks each untagged
`_test.go` file. A violation fails the test and reports `file:line` and the rule.

| Rule | Forbids |
|---|---|
| no-exec-files | writing a file with an executable mode, or writing content that starts with `#!` |
| no-build | `exec.Command("go", …)` |
| no-subprocess | `exec.Command*` of anything other than `git` |
| no-sleep | `time.Sleep`, `time.After`, `time.NewTimer`, `time.NewTicker`, `time.Tick` |
| no-env | `os.Setenv`, `os.Unsetenv`, `t.Setenv` |
| no-chdir | `os.Chdir`, `t.Chdir` |
| no-global-swap | assigning to a package-level variable declared in a non-test file |
| no-skip | `t.Skip`, `t.Skipf`, `t.SkipNow` |
| parallel | a top-level `Test*` that does not call `t.Parallel()` |

Production-code rules, in the same test:
- `os.Setenv` is forbidden outside `package main`.
- `time.Sleep` is forbidden in packages that hold a `clockwork.Clock`.

Mechanics:
- **Integration files** (`//go:build integration`) are exempt.
- **Line exemption:** `//testpolicy:allow <rule> — <reason>`. The reason is required. The test logs every exemption.
- **Unconverted packages** are listed in `internal/testpolicy/unconverted.txt`. The list starts with every package
  and only shrinks. A listed package that already passes every rule fails the test, so entries cannot outlive
  their reason.
- **Budget:** `internal/testpolicy/cmd/budget` reads `go test -json` from `make test` and fails when a converted
  package's elapsed time exceeds 10 s, printing its three slowest tests.
- **Contracts:** every `*fake` package exports a `Run*Contract` function, and exactly one integration-tagged test
  calls it against the real implementation. A missing contract fails the test.

### 5. Pilot: tmux

Baseline: 40 files and 16k lines, including 22 test files with 258 tests. 145 s, all of it serial. There are 57
sleeps and timers in production code. The slowest tests take 6–14 s, all of it spent waiting on sleeps or a real
tmux server.

- **MR 1 (scaffolding, no behaviour change).** Adds `internal/testpolicy` (the policy test and the budget tool),
  with every package listed in `unconverted.txt`. Wires `make test` through the budget tool. Adds
  `make test-integration` and `make test-timing`, and the `clockwork` dependency.
- **MR 2 (tmux seams).** Adds unexported `runner` and `clock` fields to `Tmux`, with defaults in `NewTmux*`. All 57
  sleeps and timers, and the ~20 `kill`/`ps` calls, go through them. Behaviour does not change: the existing tests
  pass unchanged.
- **MR 3 (tmux rewrite).** Adds `tmuxfake` and `RunSessionsContract`, and removes `tmux` from `unconverted.txt`.
  Each existing test goes into one bucket:
  - command translation: unit test with a recording runner
  - logic over tmux output (parsing, idle detection, dialog handling, respawn decisions): unit test with canned
    output and the fake clock
  - real tmux behaviour we depend on: integration tier, contributing to the contract where it concerns the fake
  - re-enactments already covered by a behaviour test: deleted and listed in the commit message
- **Done when:**
  - tmux unit tier ≤ 5 s and integration tier ≤ 60 s, measured non-exempt
  - no `//testpolicy:allow` lines in `tmux`
  - coverage drops by no more than 2 points, or the drop is justified
  - `docs/testing.md` is written from the pilot: rules plus worked examples. Every later package follows it.

### 6. Rollout

| # | Step | Depends on |
|---|---|---|
| 0 | scaffolding (MR 1) | |
| 1 | tmux pilot, plus `docs/testing.md` | 0 |
| 2 | slot: clock, `ContainerRuntime` | 0 |
| 3 | polecat, editorial: `t.Parallel`, template repos, `tmuxfake` | 1 |
| 4 | `beads.Client` + `beadsfake`: move the 159 raw bd-argv sites onto typed methods; contract against real bd/Dolt | 0 |
| 5 | notify: `Notifier`, escalate extracted from `cmd` | 0 |
| 6 | daemon: clock, fakes; replace the 45 s `SELECT SLEEP` test with a check of the configured timeout | 1, 4, 5 |
| 7 | refinery, witness, doctor, the rest of `internal/*` | 4, 5 |
| 8 | cmd extraction; cmd smoke tests move to the integration tier | 4, 5, 7 |
| 9 | finish: `unconverted.txt` empty, deletions from section 3, Linux CI green, `main_branch_test` on `make test-integration` | all |

Steps 0 and 1 are done by hand in the crew session. Steps 2–5 then run in parallel, one subagent per package, each
in its own worktree. Changes to shared interfaces (`beads.Client`, notify, clock) are made by one agent at a time,
and their consumers start only after the interface has landed. Each branch is reviewed in the crew session before
`gt mq submit`: diff, policy test, `make test`, coverage change and the deleted-test list. Commits carry no
attribution trailers.

Checkpoints, measured with `make test-timing` in a non-exempt pane:

| After | Target |
|---|---|
| pilot | tmux unit tier ≤ 5 s. If this misses, stop and rethink the pattern before fanning out. |
| step 6 | whole unit tier, excluding `cmd`, ≤ 60 s |
| final | unit tier ≤ 90 s; 20 repeats with zero failures under a concurrent full suite; full suite ≤ 3 min; Linux CI green |

Time box: 2 weeks, ending 2026-10-11. Every step stands on its own, so stopping early keeps what has landed. If the
work runs over, the decision goes back to Sloan; it is not extended automatically.

## Risks

- **`beads.Client` (step 4).** `*beads.Beads` has 217 methods, is constructed in 254 places, and 159 sites build raw
  `bd` argv. Mitigation: fake only the ~40 methods consumers use and leave the rest on the concrete type. Move the
  argv sites package by package.
- **cmd extraction (step 8) may not fit the time box.** If it does not, cmd stays in `unconverted.txt`, and the 90 s
  target depends on how far the fakes alone brought cmd's 126 s down. This is decided at checkpoint 2.
- **Gastown feature work is frozen for the duration.** This is accepted to avoid merge conflicts, and it is the reason
  for the time box.
- **Fakes drift from reality.** Mitigated by D4: contract suites run against the real tools after every merge.
- **The macOS scan returns after a reboot until the rewrite lands.** The manual bounce recipe covers the gap. The
  final `make test-timing` check proves the scan no longer matters.
