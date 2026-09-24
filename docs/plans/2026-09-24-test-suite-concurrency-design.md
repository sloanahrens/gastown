> Status: design approved (2026-09-24). Implementation plan: `2026-09-24-test-suite-concurrency-plan.md`. Tracked in claude-yfj.

# Two concurrent full test suites on one host — design

Date: 2026-09-24. Tracking: claude-yfj (handoff bead); gastown rig beads filed from the plan.
Related: gt-elvf4 / claude-z34 (in-package `bd init` race; lands first).

## Problem

Two `GOFLAGS=-p=8 make test` suites at once (the refinery gate plus one crew or
polecat run) push an M2 Ultra (24 cores, 128 GB) to load 55-59, and Dolt-backed
packages fail on timeouts. On 2026-09-24 two MR gates failed on load, not code:
gt-wisp-e4d, and gt-wisp-rvm (`internal/daemon` `TestScheduledMaintenance*`,
`create database: context deadline exceeded` after 15 s). The same tree passed
at load 9.7. A host this size should run two suites; that it cannot points at a
bottleneck, not at capacity.

**Goal.** Two concurrent full `-p=8` suites both finish with 0 FAIL and each in
≤ 8 min, across 3 paired runs. A single suite must not regress past today's
5.5-6.5 min (slot_hold events for green gates).

**Scope.** Host and Docker-VM capacity, the container-gate slot policy, and the
daemon package's in-process `CREATE DATABASE` path. Out of scope: the
same-server `bd init` race inside one package, which claude-z34 owns.

## Facts this design rests on

Cited from `origin/main` at 41b976c unless noted.

- **The Docker VM is small.** Docker Desktop `settings-store.json`:
  `MemoryMiB 8092`, `SwapMiB 1024`, `Cpus 24`, `DiskFlush "os"`. `docker info`
  reports 8,211,824,640 B. The host has 128 GB.
- **One gate starts about 11 Dolt servers.** One shared container per
  container-backed package process (cmd, daemon, convoy, doltserver, mail,
  polecat, refinery, testutil) plus up to 3 isolated ones in
  `internal/doltserver`. An idle test container holds 141-404 MiB
  (`docker stats`, 2026-09-24). So one gate needs about 4.4 GiB and two gates
  exceed the VM. This is plausible, not yet measured under load.
- **Every Dolt commit fsyncs through to the host SSD.** Test containers keep
  data on the image's declared volume `/var/lib/dolt` (image config
  `Volumes`), on the VM disk, with `DiskFlush "os"`. Each `bd init` runs 66
  migrations, each `DOLT_COMMIT`ed. The beads analogue measured 4.0 s per
  init on disk against 1.3 s on a RAM disk.
- **A test container's data is small.** A live shared container after a run
  held 18 MB in `/var/lib/dolt` (`du`), including its testdbs.
- **Both container start paths share one function.** `RequireDoltContainer`
  (via `startSharedDoltContainer`) and `StartIsolatedDoltContainer`
  (`internal/testutil/doltserver.go:339`) both call
  `runDoltContainerWithRetry` → `runDoltContainer` (`:149-160`), which calls
  `dolt.Run(ctx, DoltDockerImage, dolt.WithDatabase("gt_test"), WithEnv{DOLT_ROOT_HOST:%})`.
  testcontainers-go v0.42.0 has `testcontainers.WithTmpfs` (options.go:513).
- **The slot pool admits several suites.** `~/gt/settings/config.json:279`
  sets `container_gate: {slots: 4, reserved_for_gate: 2}`. Acquire hands out
  slots 0-3 only (`internal/slot/pool.go` `candidates`). `gt slot status`
  shows 5 rows because `StatusPoolLocksOnly` also lists lock files left on
  disk from an earlier 6-slot config (`discoverSlots`). The fifth row is
  cosmetic.
- **A doctor check already reads the VM size.** `ContainerCapacityCheck`
  (`internal/doctor/container_capacity_check.go`) reports vCPU and MiB and
  always returns `StatusOK`. The fake `dockerInfoCPUMem` makes it testable.
- **Aborted suites leak containers and block every gate.** Killing a
  `GT_TEST_DOCKER=1 go test` by pid skips `TerminateDoltContainer`, and no
  reaper removes the containers. `gt slot status` then reports an unwrapped
  suite and the refinery waits (10m28s on 2026-09-24).
- **The daemon package creates databases in process.** 41 `setupTestStore`
  calls (`internal/daemon/convoy_manager_test.go:25`) use the beads SDK, not
  `bd init`, so z34's init cap does not cover them. `dolt_remotes_test.go`
  uses 15 s contexts for CREATE/DROP.

## Approach

Stage the work and gate each stage on measurement. Change one variable per
stage so every effect is attributable; stop as soon as the goal is met.

| stage | change | vehicle |
|---|---|---|
| 1 | none: baseline on the 8 GiB VM, after z34 lands | measurement only |
| 2 | Docker VM 32 GiB memory, 4 GiB swap | host setting |
| 3 | tmpfs data dir for test containers; doctor warning; docs | MR 1 |
| 4a | pre-migrated template database cloned per test | spike, then MR 2 |
| 4b | at most one full suite in the slot pool | MR 3 |

Rejected: one bundled MR measured once (it confounds the effects), and the
template database first (most invasive, and cheaper fixes may be enough).

## Measurement harness and protocol

**Tools.** Copy the z34 tooling into `scripts/test-capacity/` so any session
can reproduce a measurement:

- `run-gate.sh <worktree> <label>` runs `go test -json -count=1 ./...` with the
  gate's environment (`GOFLAGS=-p=8`, `GT_TEST_DOCKER=1`) under
  `gt slot run --role <caller> --nice 0`, and writes the JSON log next to its
  output directory.
- `dolt-gate-metrics.sh` turns a log into per-package rows: elapsed, FAIL,
  retry notices, "refusing to auto-apply", "could not resolve initial root",
  bd timeouts, container-setup failures. Use per-package rows; the ALL row's
  bd-timeout column also counts `internal/web`'s simulated-timeout test.
- `sample-capacity.sh <outdir>` (new) samples every 5 s until killed: host
  load (`sysctl -n vm.loadavg`), `docker stats --no-stream` (per-container
  memory and CPU), VM memory and swap in use, and host disk throughput
  (`iostat -d 5 1`). VM memory comes from
  `docker run --rm alpine:3.20 grep -E 'MemTotal|MemAvailable|SwapTotal|SwapFree' /proc/meminfo`:
  a container sees the whole Docker Desktop VM's meminfo. Probed 2026-09-24:
  MemTotal 8,019,360 kB, SwapFree 976,508 of 1,048,572 kB, so swap was
  already in use at idle. The slot gate ignores this short-lived container:
  it only counts containers matching `dolt`, `testcontainers` or `ryuk`
  (`gateContainerPatterns`, `internal/slot/slot.go:177`).
- `paired-run.sh <label> <worktree-a> <worktree-b>` (new) checks
  preconditions, starts the sampler, starts both suites within 5 s, waits,
  and writes one summary row per suite.

**Preconditions.** The script refuses to start unless all hold: no
`gastown/refinery` slot holder, load < 20, the gastown merge queue is empty,
and `gt slot status` shows no unwrapped containers. The operator session tells
the mayor before each batch.

**Abort safety.** A `trap` on INT, TERM and EXIT kills each `go test` process
group, then removes only the containers whose `org.testcontainers.sessionId`
label was recorded for this run at start. It never matches by image or name
pattern. On exit it re-checks `gt slot status` for unwrapped containers and
prints them if any remain.

**Runs per stage.** One single-suite run and three paired runs, all on the
same commit in two worktrees.

**Recording.** Append a results table to this document after each stage:
wall time and FAIL count per suite, retry and timeout counts, peak VM memory,
peak swap, peak host load.

**Pass.** Every paired run has 0 FAIL and both suites ≤ 8 min, and the single
run is ≤ 6.5 min.

## Stage 2: Docker VM memory

Run after stage 1 results are recorded, in a quiet window (the preconditions
above), after telling the mayor.

- Back up `~/Library/Group Containers/group.com.docker/settings-store.json` to
  `settings-store.json.bak-<date>-yfj`.
- Set `MemoryMiB` to 32768 and `SwapMiB` to 4096. Restart Docker Desktop. If
  the file edit does not take effect, use the Docker Desktop UI.
- Verify: `docker info --format '{{.MemTotal}}'` reports about 32 GiB.
- Rollback: restore the backup and restart.

## Stage 3: tmpfs data directory (MR 1)

**Container options.** In `internal/testutil/doltserver.go`, move the options
passed to `dolt.Run` into one helper, `doltContainerOpts()`, and add
`testcontainers.WithTmpfs(map[string]string{"/var/lib/dolt": "rw,size=2g"})`.
`runDoltContainer` is the only caller, so shared and isolated containers
cannot drift apart. The 2 GB cap bounds a runaway test; measured use is about
18 MB per container.

**Opt-out.** `GT_TEST_DOLT_TMPFS=0` drops the tmpfs option, for a Docker
runtime that cannot mount tmpfs over the image's declared volume.

**Tests.**
- Unit, no Docker: `doltContainerOpts()` includes the tmpfs mount by default
  and omits it when `GT_TEST_DOLT_TMPFS=0`.
- Integration, opt-in (`doltserver_optin_test.go`): run
  `stat -f -c %T /var/lib/dolt` through `ctr.Exec` and expect `tmpfs`.

**Doctor warning.** `ContainerCapacityCheck` returns `StatusWarning` when VM
memory is below `minContainerVMMemBytes` (16 GiB). The message names the fix
(raise Docker Desktop memory) and this document. It keeps `StatusSkipped` when
Docker cannot be queried. Tests use the existing `dockerInfoCPUMem` fake to
cover below-threshold, at-threshold and error cases.

**Docs.** One paragraph in `docs/reference.md`, next to the container opt-in
text: the VM memory the suites need, the doctor warning, and the tmpfs
opt-out.

## Stage 4: only if stage 3 misses the goal

Decide from the stage 3 table:

- **Goal met.** Close claude-yfj when MR 1 lands. File a bead describing the
  template database as an optional speedup, with no commitment.
- **Paired runs fail, single runs pass, and memory or swap peaked.** Go back
  to stage 2 with 48 GiB before writing code.
- **Paired runs fail and the VM had memory headroom** (per-package times
  inflate; CPU- or Dolt-bound). Go to 4a.

**4a: template database.** First a spike answering one question: can a
migrated `testdb_template` become a new database on the same server in under
1 s? Compare `CALL DOLT_CLONE` from a `file://` remote pushed from the
template against copying the template's files into a new database directory.
The result must pass bd's identity guard (gt-uq28): the copied
`.beads/metadata.json` carries the template's `project_id`. If the spike
succeeds, test-container `Beads.Init` becomes clone-then-write-`.beads`, and
the full `bd init` runs once per test process. That change gets its own short
design.

**4b: suite-aware slot cap.** Only if 4a fails or still misses the goal. Add
`container_gate.max_full_suites` (default unset = no cap). `gt slot run` marks
a hold running the full `make test` as `full`; `Acquire` refuses a second
`full` holder and still admits package-scoped holders.

**Daemon 15 s contexts.** If `TestScheduledMaintenance*` or
`dolt_remotes_test.go` still fail after stage 3, raise their `CREATE DATABASE`
contexts to the shared test-container budget, citing the stage 3 numbers in
the commit message. Otherwise leave them unchanged: raising them first would
hide the signal the measurements need.

## Tracking and delivery

- One gastown epic with a child bead per stage: stage 1 measurement, stage 2
  VM, stage 3 MR 1, and one conditional stage 4 placeholder. claude-yfj is the
  handoff bead that tracks them.
- Crew does the work in `crew/sloan-yfj` and merges through the gastown
  refinery. Nothing is slung.
- Stage 1 waits for claude-z34 to merge so the two effects do not mix.

## Risks

- **tmpfs over a declared volume.** Docker normally lets a tmpfs mount shadow
  an image `VOLUME`; the opt-in integration test proves it on this runtime,
  and the opt-out covers others.
- **tmpfs counts against VM memory.** At about 18 MB per container this is
  small, and the 2 GB cap bounds the worst case. Stage 2 comes first.
- **Measurement disturbs the town.** Each paired run holds two non-reserved
  slots for 7-9 min; the whole programme is about 1.5-2 hours of slot time.
  Mitigated by the preconditions and by telling the mayor.
- **Docker restart kills running containers.** Only done when the
  preconditions hold.
