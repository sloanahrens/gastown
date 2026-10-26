> Status: historical (2026-10). Merged in: gt-p85pw.62 (R8). Not maintained.

# R8: every test that starts a Dolt container or server, and the condition that removes or ports it

**Bead:** gt-p85pw.62 (epic gt-p85pw, track R, research). **Completed by:** polecat ace, 2026-10-26.
**Read at:** gastown `origin/main` 3b0d4d42.
**Method:** read-only in the tree. One integration package was run to measure its replacement
(`internal/beads/trackerclient`, section 8); no container test was run and no file outside this
report changed.
**Plan drift:** `docs/plans/2026-10-25-town-tracker-plan.md` section 5 lists R1-R5 only; it has no
R7 or R8 entry, so this report answers the bead's own acceptance boxes, not the plan's.

## Answer

Twenty-eight test files in **seven packages** call a Dolt-container entry point
(`beads`, `cmd`, `daemon`, `mail`, `notify`, `polecat`, `testutil`). Three of them start a shared
container in `TestMain` (`cmd`, `daemon`, `notify`), one starts it from inside a test (`beads`), and
two start it lazily on the first `RequireDoltContainer` (`mail`, `polecat`); `internal/cmd` and
`internal/testutil` also lease **scratch** containers (up to three more per process). A further five test files start, or impersonate, a real
`dolt` **binary** instead. Nothing in `internal/slot`, `internal/doctor` or `internal/hooks`
starts a Dolt container or server: their "dolt" hits are `docker ps` fixtures, the guard that blocks
unwrapped container runs, and the image-name patterns of the container gate.

Every row falls into one of four classes: **a** the container is incidental (a fake or the unit tier
can carry it), **b** port to the tracker on Postgres when a new town's default backend flips,
**c** test Dolt itself and die with the Dolt surface in S4, **d** test `bd` behaviour the shim keeps
and need no change until the shim retires. Sections 2-5 are the tables, section 6 states each class's
removal as a checkable fact, section 7 lists the gaps found, and section 8 is the deletion order.

The two entries in `internal/testpolicy/docker.txt` (`internal/beads`, `internal/testutil`) start no
Dolt **test container** in their unit tier at all: `make test-integration`'s second command re-runs
both packages with Docker opted in for nothing (section 4). That is the cheapest thing on this list.

## 1. The entry points, and what each starts

| Entry point | Where | What it does |
|---|---|---|
| `testutil.RequireDoltContainer` | `internal/testutil/doltserver.go:485` | Starts the package's shared `dolthub/dolt-sql-server:2.0.7` container once (`:414`), if `GT_TEST_DOCKER=1`; the data dir is tmpfs by default (`:217-239`). Skips the test without the opt-in. |
| `testutil.EnsureDoltContainerForTestMain` | `internal/testutil/doltserver.go:470` | Same start, from `TestMain`; an opted-in run fails instead of skipping. |
| `testutil.WithDolt()` | `internal/testutil/hermetic.go:188` | `TestMain` option that calls the above and routes `GT_DOLT_PORT`/`BEADS_DOLT_PORT` to it. |
| the pool created at container start | `internal/testutil/doltpool.go:233`, sizes `:55` `:59`; `doltpool_template.go:33` | Before any test runs: one bd-init template (dropped before tests reach the container), 24 clones of it for init leases, 32 spares and 8 plain-SQL databases — 64 at test time, about 13 MiB each. No CREATE/DROP while tests run; a lease is reset to its head instead (`doltreset.go`). |
| `testutil.TakePooledSQLDatabase` | `internal/testutil/doltpool.go:471` | Leases one of the 8 SQL databases. Starts nothing itself. |
| `testutil.LeaseScratchDoltContainer[Env]` | `internal/testutil/doltscratch.go:87`, `:103` | Leases one of up to 3 **extra** containers (`:38`) for tests whose code creates databases; drops what the test created on release. |
| `testutil.DockerTestsEnabled` | `internal/testutil/dockeroptin.go:21` | Reads `GT_TEST_DOCKER`; starts nothing. |
| `beads.RunTestContainerInit` | `internal/beads/test_container.go:158` | Runs one `bd init` against the shared container under one of the package's init slots (`:125`), so a package's parallel inits do not all migrate at once. |
| `beads.InitForTest` | `internal/beads/beads.go:1458` | Same, for an isolated `*beads.Beads`; takes an init slot when the client points at the container. |

Two gate facts frame all of this. `make gate` writes `GT_TEST_DOCKER=0` itself, so no container test
runs on the landing path (`Makefile:416`). `make test-integration` runs the `integration` tag over
every package, then every package in `internal/testpolicy/docker.txt` **whole and untagged** with
`GT_TEST_DOCKER=1` (`Makefile:508-513`), and refuses to run if that list is empty (`:509`). The static rule
that keeps the list exact matches a **function name** in a unit-tier test file
(`internal/testpolicy/docker.go:19-28`), which is why section 4 exists.

## 2. Tests that start a container

`starts` says which entry point the row reaches. `recorded` carries only numbers already written in
the tree or on a bead; **no `make test-integration-wall` output is in the repo**, so most walls are
"not recorded" and the bead that removes a row should take the measurement first (section 7).

| # | Package / test file:line | Starts | Class | Recorded wall | Removal or port |
|---|---|---|---|---|---|
| 1 | `internal/beads/client_contract_integration_test.go:26` (`newDB` `:49`) | shared container, lazily, inside the test | **d** | 27 bd inits = **2 min** on the container (`:34-38`) | `bd` side of the seam contract; the tracker side already runs on `pgtest` (section 8). Stays until bd/the shim retires. |
| 2 | `internal/cmd/integration_testmain_test.go:48` | shared container in `TestMain` for the whole package | **b** | not recorded (package total 320-607 s before conversion, `docs/plans/2026-09-30-d9-gate-plan.md:18`) | A town built by `gt install` routes to the tracker; the tier's store becomes `trackerd` + `pgtest`. |
| 3 | `internal/cmd/dolt_test_helpers_test.go:23`, `:40` | require / scratch lease helpers | **a** / **b** / **d** by caller | not recorded | Follows its callers (rows 4-11). |
| 4 | `internal/cmd/rig_integration_test.go:196`, `:400`, `:474`, `:528`, `:564`, `:626`, `:803` (7 leases) | scratch container | **a** | the pool exists because eight tests queued ~100 s of a 170 s package wall before it (`doltscratch.go:18-22`) | These assert directories, `rigs.json`, `config.json`, routes and worktree cleanliness; none reads the database. Needs one seam for `rig.Manager.InitBeads` (`internal/rig/manager.go:671`), then the unit tier carries them. |
| 5 | `internal/cmd/rig_integration_test.go:323` `TestIntegrationRigAddInitializesBeads`, `:752` `TestIntegrationAgentBeadIDs` | scratch container | **b** | not recorded | Asserts the rig's store was initialised; port to the tracker's create path. |
| 6 | `internal/cmd/beads_db_init_test.go:394` (via `newAdoptTown`), `:90`, `:460` | scratch container + `RunTestContainerInit` | **b** | "whole 60s" before the scratch pool (`:386`) | `gt rig add --adopt` creating a store; port to the tracker. |
| 7 | `internal/cmd/hook_slot_integration_test.go:113-116` | shared container + `RunTestContainerInit` | **d** | not recorded | Calls `beads.Client` create/update/list; the shim answers the same verbs. |
| 8 | `internal/cmd/beads_routing_integration_test.go:145` | `RunTestContainerInit` | **d** | not recorded | gt's route resolution over a real store; the shim routes the same prefixes (`docs/plans/2026-10-25-town-tracker-design.md:113-119`), so the cases pass unchanged. |
| 9 | `internal/cmd/scheduler_integration_test.go:68` | `RunTestContainerInit` | **d** | not recorded | Pins the `bd init` argv and port wiring the dispatcher uses; shim keeps `init` until S4. |
| 10 | `internal/cmd/real_bd_helpers_integration_test.go:28` | shared container + `InitForTest` | **d** | not recorded | Patrol reads over a real store; unchanged through the shim. |
| 11 | `internal/cmd/dolt_pool_integration_test.go:36` | shared container | **c** | not recorded | Tests the pool's own invariant (no catalog change); deleted with the pool. |
| 12 | `internal/daemon/testmain_integration_test.go:33` | shared container + pool for the package | **c** | not recorded | Starts it for row 13; no other daemon test file calls a container entry point. Deleted with the pool. |
| 13 | `internal/daemon/maintenance_gc_dolt_integration_test.go:17` (`:22` pooled DB) | shared container + pooled SQL DB | **c** | not recorded | `CALL dolt_gc('--full')` is Dolt, in S4's deletion list (`docs/plans/2026-10-25-town-tracker-plan.md:255`). |
| 14 | `internal/daemon/dolt_helpers_integration_test.go:22-36`, `:77` | pooled SQL database | **c** | not recorded | Helper for row 13; deleted with it. |
| 15 | `internal/mail/router_integration_test.go:30` | shared container, lazily | **a** | not recorded | The reads go through `Router.bd` (`internal/mail/router.go:53`) and `Router.town` (`:56`); a scripted `bd list` answer carries every case to the unit tier. |
| 16 | `internal/notify/testmain_integration_test.go:19` | shared container in `TestMain` | **b** | not recorded | Row 17's scratch town becomes a `trackerd` town on `pgtest`. |
| 17 | `internal/notify/contract_integration_test.go:84-115` (`RunTestContainerInit` `:107`) | `RunTestContainerInit` | **b** | not recorded | Runs a real `gt` against a scratch town; the town's store is what flips, not the contract. |
| 18 | `internal/polecat/manager_integration_test.go:25`, `:28` (`initBeadsDBWithPrefix`) | shared container + `RunTestContainerInit` | **d** | not recorded | Feeds the `realAddBeads` contract cases (row 19); they pin the fake to real `bd`. |
| 19 | `internal/polecat/manager_integration_test.go:251` (`realAddBeads`) | shared container + `InitForTest` | **d** | not recorded | Same contract as `fakeAddBeads` in the unit tier (`internal/polecat/manager_test.go:79`); the real side changes only when `bd` does. |
| 20 | `internal/polecat/manager_integration_test.go:63`, `:159` | shared container | **b** | not recorded | `Manager.Get` state derivation with hooked/stale agent beads; the store should be the tracker. |
| 21 | `internal/testutil/doltpool_reuse_integration_test.go:26` | scratch container | **c** | not recorded | Tests the pool's reset; deleted with the pool. |
| 22 | `internal/testutil/doltpool_guard_integration_test.go:21` | scratch container | **c** | not recorded | Tests the catalog guard; deleted with the pool. |
| 23 | `internal/testutil/doltserver_tmpfs_integration_test.go:21` | scratch container | **c** | not recorded | Tests the container's tmpfs option; deleted with the container. |

Container cost already on record, for sizing: two `dolt-sql-server` containers ran at 208 MiB and
340-607 MiB with peak container memory 822 MiB of the VM's 7850 MiB (bead gt-xtk4, 2026-09-17); the
same bead measured whole integration packages at **daemon 407.6 s, refinery 660.4 s** on the host of
that day. A database in the pool costs about 13 MiB and a `bd init` about 2.5 s, of which ~1.5 s is
the migration the pool's template removes (`internal/testutil/doltpool_template.go:18-19`).

## 3. Tests that start (or impersonate) a real `dolt` server binary, not a container

| # | File:line | What it starts | Class | Removal or port |
|---|---|---|---|---|
| 24 | `internal/doltserver/process_integration_test.go:129`, `:166`, `:211`, `:258` | real `dolt sql-server` on a free loopback port and a temp data dir (`:32-38`, `:259`) | **c** | `internal/doltserver` is the adapter S4 deletes; the whole file goes with it. |
| 25 | `internal/daemon/maintenance_backup_integration_test.go:28`, `:41`, `:91`, `:116` | two private `dolt sql-server` processes, `dolt backup restore` into a second copy | **c** | Backup/restore is Dolt surface; deleted in S4 with the nightly backup path. |
| 26 | `internal/daemon/pid_identity_integration_test.go:179`, `:182` | re-execs itself as a process whose argv is `dolt sql-server` | **c** | Tests the daemon's Dolt stop path; the daemon cases in the same file (`:144`, `:162`) stay. |
| 27 | `internal/cmd/fresh_setup_integration_test.go:28-43` | real `dolt` via `gt install --dolt-port`, stopped with `gt dolt stop` | **b** | The install → rig add → hook flow survives on the tracker; the `dolt` binary and `--dolt-port` do not. |
| 28 | `internal/cmd/install_integration_test.go:24` (`//go:build e2e`, `:32-37`) | real `dolt` inside the e2e image (`Dockerfile.e2e`, `make test-e2e-container`) | **b** | Same as row 27; the e2e image drops the dolt binary with the surface. |

## 4. In `docker.txt` or the guard's list, but starting nothing

| File | Why it is listed | What it actually does |
|---|---|---|
| `internal/beads/test_container_test.go:545`, `:601`, `:613`; `internal/beads/test_database_source_test.go:118` | call `RunTestContainerInit` by name, so `internal/testpolicy/docker.go:19-28` marks the package Docker-backed | run a **stub** `bd` installed on `PATH` (`installBdStub`, `internal/beads/bd_container_retry_test.go:789`; `installSucceedingBdStub`, `internal/beads/test_container_test.go:371`) and assert the init-slot semaphore and database lease. No container, even with `GT_TEST_DOCKER=1`. |
| `internal/testutil/hermetic_test.go:322`, `:345` | call `startHermetic(WithDolt())` by name | use the package's `fakeHarness`, whose `ensureDolt` is a func field. No container. |
| `internal/slot/*_test.go` (all) | bead names `internal/slot` | fake the `docker ps` listing (`internal/slot/containers_test.go:17`, `owner_labels_test.go`, `reap_test.go`); the package's dolt strings are image-name patterns (`internal/slot/slot.go:253`) and owner labels (`internal/slot/owner_labels.go:17-21`). |
| `internal/doctor/slot_debris_check_test.go`, `container_capacity_check` tests | bead names `internal/doctor` | `fakeContainers` answers the listing (`internal/doctor/slot_debris_check_test.go:22-43`). The dolt doctor checks (section 5) start nothing. |
| `internal/hooks/config.go:925-947` | bead names `internal/hooks` | registers `gt tap guard container-suite` in the PreToolUse hooks; it starts nothing and is the machinery that blocks unwrapped container runs. |
| `internal/cmd/tap_guard_container_suite.go:100-115` | — | lists **14** packages whose test binary links `internal/testutil`, a superset of `docker.txt`'s 2; the guard is deliberately wider than the true set (its comment says so), and it missed `internal/notify` (section 7). |

The checkable consequence: `make test-integration`'s second command re-runs both `docker.txt`
packages whole with `GT_TEST_DOCKER=1`, and neither starts a container in an untagged file. Their
integration-tagged files already run in the first command. Dropping both entries and the Makefile's
empty-list guard (`Makefile:509`) removes two whole-package runs with no coverage lost.

## 5. Dolt-surface tests that start nothing, deleted with S4

These start no container or server, so they are outside the table the bead asks for, but they are the
rest of the sweep's Dolt cost and they share S4's condition: `internal/doltserver`'s unit tests on
`fakeHost` (20 test files, two of them integration-tagged; `hermetic_main_test.go`), `internal/doctor`'s Dolt checks and their tests
(`dolt_binary_check`, `dolt_commit_rate_check`, `dolt_orphan_servers_check`, `dolt_pause_check`,
`dolt_remote_leftovers_check`, `dolt_server_patrol_check`, `dolt_stray_servers_check`,
`stale_dolt_port_check`), `internal/beadsql` (5 files), `internal/daemon`'s Dolt manager tests
(`dolt_start_verify*`, `dolt_backoff`, `dolt_pause`, `dolt_endpoint_config`, `shutdown_dolt`,
`compactor_dog`, `maintenance_gc_*`), `internal/cmd`'s `dolt*_test.go` (7 files),
`internal/cmd/up_dolt_env_integration_test.go` (readiness and port plumbing, no server),
`internal/cmd/init_shared_dolt_integration_test.go` (the port guard, no server), and
`internal/util/dolt_{orphan,process,workspace_servers}_test.go` if they outlive the adapter.

## 6. Each class's removal condition, as a checkable fact

| Class | Meaning | Removal condition | Check |
|---|---|---|---|
| **a** | The container is incidental; a fake or the unit tier carries the test | The case passes in the unit tier with no Docker on the host and no container call in its file | `GT_TEST_DOCKER=0 go test -run '<case>' ./<pkg>` passes; `grep -c 'RequireDoltContainer\|LeaseScratchDoltContainer\|RunTestContainerInit' <file>` is 0 |
| **b** | Port to the tracker when a new town's default backend flips | A town built by `gt install` routes every prefix to the tracker, and the test's scratch town is built on `trackerd` + `pgtest` | `tmp=$(mktemp -d); gt install "$tmp" --name t; jq -r '.backend // "dolt"' "$tmp/.beads/routes.jsonl" \| sort -u` prints `tracker` only (a route's backend is `dolt` when empty, `internal/beads/routes.go:22-33`; `gt install` writes none today, `internal/cmd/install.go:762`) |
| **c** | Tests Dolt itself; deleted with the surface | The Dolt surface is gone: no `doltserver` package, no `gt dolt` verbs, no test container pool | `test ! -e internal/doltserver && test ! -e internal/beadsql && ! grep -rq 'DoltServerManager\|GT_TEST_DOCKER\|testcontainers' --include='*.go' internal/ Makefile` and `pgrep -fl 'dolt sql-server'` is empty |
| **d** | Tests `bd` behaviour the shim keeps | The shim is retired: `cmd/bdshim` and the per-prefix routing are gone, so the behaviour is the tracker's or nothing | `test ! -d cmd/bdshim && ! grep -rq 'bdshim' --include='*.go' .` after the last rig flips |

Class **c** has a second, earlier exit for the parts that are only the test harness: the pool, the
scratch pool and the container entry points can go as soon as no test in any row above calls them,
which is the same fact as `internal/testpolicy/docker.txt` being empty and
`containerSuitePackages` (`internal/cmd/tap_guard_container_suite.go:100`) shrinking to nothing.
`make test-integration`'s guard, `scripts/tier-sweep.sh:182-184` and the `integration-docker` marker
(`internal/daemon/tier_sweep_bead.go:122-140`) then have nothing to run, and the container gate
(`internal/slot`, `gateContainerPatterns` at `internal/slot/slot.go:253`) has no Dolt container left
to serialize.

## 7. Gaps found while reading

- **The plan has no R8** (it lists R1-R5), so the acceptance boxes, not the plan, define this work.
- **No wall log is in the repo.** The sweep records one at
  `<town>/.runtime/tier-sweep/logs/<rig>/integration.log` (named in every tier-sweep bead, e.g. gt-0z23n
  names the operator's own path there), which is on the operator's host and not readable from a
  polecat worktree. Every "not recorded" above should be measured with
  `gt slot run --role <rig>/<who>/full-suite -- make test-integration-wall` before its row is removed,
  so the saving is a number rather than an estimate.
- **`docker.txt` is a name match, not a run.** Both of its entries start no container in an untagged
  file (section 4). `TestDockerTier` keeps the list exact against the static rule, so the list is
  correct by its own definition and wrong about the run; the second `make test-integration` command
  is the cost.
- **The guard's list is 14 packages, `docker.txt`'s is 2.** Six of its entries start a container in a
  test file (`beads`, `cmd`, `daemon`, `mail`, `polecat`, `testutil`); eight start none
  (`crew`, `deps`, `doctor`, `doltserver`, `git`, `plugin`, `protocol`, `rig`), so those eight
  shrink only when the guard's linking rule does.
- **`internal/notify` is missing from the guard.** It starts a shared container in its `TestMain`
  (`internal/notify/testmain_integration_test.go:19`) but is not in `containerSuitePackages`
  (`internal/cmd/tap_guard_container_suite.go:100-115`), which was derived from packages whose test
  binary imports `internal/testutil`; `notify`'s unit-tier `TestMain` imports
  `internal/testutil/unittier` (`internal/notify/main_test.go:8`), a different path, so the
  derivation missed it. With `GT_TEST_DOCKER=1` in a polecat's environment, `go test ./internal/notify`
  is not blocked and starts a container outside the slot. The fix is one entry in the list; the row
  it concerns is 16.
- **`pgtest` is not wired into the gt side yet.** `internal/beads/trackerclient`'s contract test
  imports `tracker/store/pgtest` directly and starts a loopback `httptest` server; a gt package that
  wants a tracker town has no constructor for one (there is no `testutil` equivalent of
  `pgtest.New` for a whole town). Rows 5, 6, 16, 17 and 27 each need that helper before they can port.
- **`internal/beads/backend.Open` still has no production caller** (`internal/beads/backend/backend.go:43`;
  R7 reported the same). A route can name `tracker` today without any test or command taking that
  path, so class **b**'s condition is about the default a new town writes, not about a code path that
  already exists.

## 8. Order: delete or port, largest saving first

1. **`internal/cmd` (rows 2-11).** One shared container plus the package's scratch pool (up to three
   more) per integration run, for a package whose total was 320-607 s before its conversion. First move the seven
   artifact cases (row 4) to the unit tier behind one seam; then port the gt-behaviour cases
   (rows 5-6) to a tracker town; then the shim-kept cases (rows 7-10) move with the shim; delete
   row 11 with the pool. Removing the scratch pool alone drops up to three more container starts
   from a parallel run.
2. **`internal/daemon` (rows 12-14).** A whole shared container **and** its 64-database pool for one
   test. Deleting the gc test after the Dolt surface goes removes both from every integration run.
3. **`internal/beads` (row 1).** 2 minutes of bd inits in one test, measured; the tracker side of
   the same contract already runs in ~3 s (see "What the replacement already costs" below). It stays
   only because it pins `bd`, so its saving lands with the shim.
4. **`internal/notify` (rows 16-17).** A container plus a built `gt` per run; port the scratch town
   to `pgtest`.
5. **`internal/mail` (row 15) and `internal/polecat` (rows 18-20).** One lazily started container
   each; `mail` leaves the seven packages entirely with row 15's move, `polecat` at the flip.
6. **`internal/testutil` and `internal/doltserver` (rows 21-25).** The harness's own tests; they go
   with the harness and the adapter, last.
7. **Real-server rows 26-28** with the Dolt stop path, the installer's `--dolt-port`, and the e2e
   image.

**The first three changes that need no rig flip** are:

1. Move the seven `internal/cmd/rig_integration_test.go` artifact cases (row 4) to the unit tier.
   They read no database; one seam for `InitBeads` is all they need. This removes the scratch pool's
   startup (up to three containers) from `internal/cmd`, the largest single saving available today.
2. Move `internal/mail/router_integration_test.go` (row 15) to the unit tier on the `Router.bd` and
   `Router.town` seams. `internal/mail` then starts no container at all and leaves the seven.
3. Delete both entries from `internal/testpolicy/docker.txt` and the empty-list guard in
   `Makefile:509`. Their untagged files start nothing (section 4), and their tagged files already run
   in the tagged command. This drops two whole-package runs from the container tier with zero
   coverage lost, and it needs no code, no flip, and no Postgres.

### What the replacement already costs

`internal/beads/trackerclient` runs the same seam contract — `RunClientContract`, `RunActorContract`,
`RunAdminContract` — against a real `trackerd` on a `pgtest` cluster
(`internal/beads/trackerclient/contract_integration_test.go:56-67`). Run on this host on 2026-10-26:

```
$ go test -tags integration -count=1 ./internal/beads/trackerclient
--- FAIL: TestIntegrationTrackerClientSatisfiesTheAdminContract (0.14s)
    --- FAIL: .../kv_store: KVList: tracker: the key-value store is not built yet (plan task B4)
FAIL github.com/steveyegge/gastown/internal/beads/trackerclient  2.983s
```

16 top-level tests pass; the one failing subtest is the unbuilt KV store (plan task B4), not the
harness. A whole package run, cluster start included, takes under 5 s of wall.

Against row 1's 27 bd inits and ~2 minutes. `pgtest` needs PostgreSQL 17 on `PATH`, `TRACKER_PG_BIN`,
or `/usr/lib/postgresql/17/bin` / the Homebrew keg (`tracker/store/pgtest/bin.go:12-21`); it is
present at `/usr/lib/postgresql/17/bin` on this host, and the polecat image still needs it (plan task
E1).

