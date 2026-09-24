> Status: design approved (2026-09-24). Implementation plan: `2026-09-24-test-dolt-init-contention-plan.md`. Source issue gt-elvf4; tracked in claude-z34.

# Test Dolt init contention under -p=8 gates — design

## Problem

The refinery gate runs `GOFLAGS=-p=8 make test`. Dolt-backed test packages start one shared Dolt container per test process (`internal/testutil/doltserver.go`). Each test that needs beads runs `bd init --server --database testdb_<hex>` against that server: CREATE DATABASE plus 66 migrations, each one DOLT_COMMITted.

Under gate load the gate log for gt-wisp-mlhp (`/tmp/mlhp-test-1.log`) shows 48 retries of `bd init`, all against the refinery package's own container:

- 43 × `refusing to auto-apply N pending schema migrations to a server-mode database` (N = 18–45);
- 5 × `could not resolve initial root for database testdb_<other>`;
- 1 × bad connection.

bd calls also hit the 60 s subprocess timeout, and one refinery test failed on it.

## Facts

- **The contention is inside one package, not across packages.** Each package process has its own server. In `internal/refinery`, 18 of about 20 container tests run in parallel, each with its own `bd init`. Every mlhp retry targeted port 55491, the refinery container.
- **"Refusing N pending migrations" is bd reading its own half-migrated database** (research confidence about 80%).
  - bd commits each migration step (`schema.go:1594`, `:1689`), so an interrupted init leaves the database at vK.
  - bd's retry loop re-runs `CheckRemoteMigrateGateForServer` on every attempt (`store.go:2660`). For a partly migrated server-mode database with no remote the gate returns a permanent error (`remote_migrate_gate.go:539`).
  - `BD_ALLOW_REMOTE_MIGRATE=1` is the gate's documented escape hatch.
- **"Could not resolve initial root" is a snapshot race** (about 85%). Dolt's `information_schema` walk covers every database on the server; a session that began before another test created its database has no root for it. bd does not treat this as retryable (`store.go:614-687`).
- **Both earlier patches failed on a broken semaphore, not on capacity.** `doltContainerSlots` was a buffered channel that nothing filled, and acquiring meant receiving from it, so every container start blocked: forever in voc6, 300 s in v7m. Scoping was also wrong: a per-process cap cannot limit containers across processes.
- **Gastown already retries** `bd init` against a test container (`internal/beads/bd_container_retry.go`, gt-o8i9f): 5 attempts in 60 s, a fresh database name and a clean `.beads` each time. It stays as the backstop.
- **Some tests bypass `Beads.Init`.** Four helpers exec `bd init --server-port` directly: `internal/polecat/manager_integration_test.go:26`, `internal/cmd/beads_db_init_test.go`, `internal/cmd/scheduler_integration_test.go` and `internal/cmd/dolt_test_helpers_test.go`.
- **A green gate** takes about 5.5–6.5 min. Container-backed packages account for 62–85% of package-seconds.

Full research: claude-z34 bead notes and the research file linked there.

## Decisions

| # | Decision |
|---|---|
| 1 | Fix in gastown now. File the root fixes as a beads bead; don't block on it. |
| 2 | Set `BD_ALLOW_REMOTE_MIGRATE=1` only on test-container bd calls. A test pins that real town calls never get it. |
| 3 | Cap concurrent test-container inits per process: default 4, override `GT_TEST_DOLT_INIT_CONCURRENCY`, a filled semaphore, context-bounded waits. |
| 4 | Test-container non-init bd calls: timeout 60 s → 3 min. Init keeps 5 min. |
| 5 | tmpfs data dir: a separate, measured follow-up. |
| 6 | Measure with 2 baseline and 2 branch gates under `gt slot run`. |
| 7 | Crew MR with gt-elvf4 as source issue; nothing slung. |
| 8 | One choke point in `internal/beads` (approach A), not per-test calls and not more retries. |

## Components

### 1. `internal/beads/test_container.go` (new)

The one place that knows what a test-container bd call needs.

- `testContainerInitSlots chan struct{}`, sized from `GT_TEST_DOLT_INIT_CONCURRENCY` (default 4) and **filled with tokens at package init**.
  - Acquire: receive a token.
  - Release: send it back.
- `AcquireTestContainerInitSlot(ctx context.Context) (release func(), err error)`.
  - Waits in a `select` on the channel and `ctx.Done()`.
  - On cancel it returns `fmt.Errorf("test Dolt init slot: %w", ctx.Err())`.
  - `release` is idempotent (`sync.Once`).
- `testContainerEnv() []string` returns `BD_ALLOW_REMOTE_MIGRATE=1`.
- `const bdContainerSubprocessTimeout = 3 * time.Minute`.
- `RunTestContainerInit(ctx context.Context, dir string, args []string, env []string) ([]byte, error)`, exported for test helpers that exec `bd init` directly.
  - It takes a slot, runs `bd <args>` in `dir` with `env` plus `testContainerEnv()`, and applies the 5 min init timeout.
  - It returns the combined output.

### 2. `Beads` wiring

This applies whenever `(*Beads).targetsTestDoltContainer()` is true, meaning isolated with a server port (`bd_container_retry.go:160`).

- `buildRunEnv` (`beads.go` ~1304) and `buildRoutingEnv` (~1333) append `testContainerEnv()` inside the existing `serverPort > 0` branch.
- The subprocess timeout choice: an explicit env override wins. Otherwise init gets `bdInitSubprocessTimeout` (5 min), other test-container calls get `bdContainerSubprocessTimeout` (3 min), and everything else gets `bdSubprocessTimeout` (60 s).
- `(*Beads).Init` (`beads.go:1022`) acquires a slot before the retry wrapper and releases it with `defer`. `Init` has no context, so the wait is bounded by a context with the 5 min init budget.

Real town databases never match the predicate, so nothing changes for them.

### 3. Direct-exec helpers

The four helpers listed under Facts replace their `exec.Command("bd", "init", ...)` launch with `beads.RunTestContainerInit(...)`. Their arguments, flags and assertions are unchanged.

### 4. Upstream and bookkeeping

- File a beads bead in the beads rig with two fixes:
  - (a) skip the remote-migrate gate for a database the same `bd init` created;
  - (b) treat 1105 "could not resolve initial root" as retryable, like "no root value found".
- File a follow-up gastown bead for a tmpfs data dir on test containers.
- Correct gt-elvf4's notes: the mechanism was an unfilled channel plus the burst of inits inside the refinery package, not "4 slots < 8 packages".

## Error handling

| Situation | Behaviour |
|---|---|
| Slot wait hits the context deadline | Returns `test Dolt init slot: context deadline exceeded`. The init and the test fail naming the slot, never a silent hang. |
| Caller has no deadline | `Init` bounds the wait at its 5 min budget. `RunTestContainerInit` requires a context (`t.Context()` or a bounded one). |
| Panic during init | The deferred release returns the slot. |
| Nested acquisition | Impossible by construction: only `Init` and `RunTestContainerInit` acquire, neither calls the other, and the retry loop runs inside one acquisition. |
| Bad `GT_TEST_DOLT_INIT_CONCURRENCY` | A value that doesn't parse, or is < 1, falls back to 4 with one stderr warning. No upper clamp. |
| Interrupted migration | bd resumes it under `BD_ALLOW_REMOTE_MIGRATE=1`. The gt-o8i9f retry (including its "refusing" class) stays as the backstop. |
| "Could not resolve initial root" | Unchanged: gastown retries with a fresh name. The cap makes it rarer; the beads bead fixes it. |
| Slow container bd call | Fails after 3 min, not 60 s. Accepted, because the cap and the env var remove the known causes. |
| Isolated containers (3 tests in `internal/doltserver`) | Their inits match the predicate and also take slots from the same per-process pool. That's harmless: no nesting, and few callers. |
| Real town calls | Unaffected; pinned by a test. |

## Testing

All unit tests are hermetic: no Docker.

- **Semaphore:**
  - capacity N admits N holders, and the (N+1)th blocks until a release;
  - 4 holders plus 12 queued waiters all finish under `-race` within a bounded deadline;
  - a cancelled context returns the wrapped error;
  - release is idempotent;
  - a panic still releases.
- **Knob:** default 4; invalid and < 1 values fall back to 4 with a warning; a valid value is honoured.
- **Scope:**
  - a test-container `Beads` gets `BD_ALLOW_REMOTE_MIGRATE=1` from both env builders;
  - a non-isolated or port-less `Beads` does not, and keeps 60 s;
  - init keeps 5 min;
  - these cases extend the table in `beads_subprocess_timeout_test.go`.
- **`RunTestContainerInit`:** a stub `bd` on PATH records the env and args. With capacity 1, a second call waits until the first stub exits.
- **Callers:** the four helpers compile against `RunTestContainerInit`, and their existing Docker-backed tests keep passing.

## Measurement

- **Commands:**
  - baseline, twice on `origin/main`: `gt slot run --role gastown/crew/sloan-z34 --nice 0 -- env GOFLAGS=-p=8 GT_TEST_DOCKER=1 make test` (nice 0 so the timing matches the refinery gate's), exit code captured;
  - then the same twice on the branch.
- **Metrics per package**, taken with `grep -a` from the logs:
  - wall time and FAIL count;
  - bd-init retry notices;
  - "refusing to auto-apply";
  - "could not resolve initial root";
  - bd timeouts;
  - "Dolt container setup failed".
- **Pass bar:**
  - zero FAILs in both branch runs;
  - "refusing" ≈ 0;
  - retry notices down ≥ 80% against baseline;
  - gate wall time ≤ baseline + 10%.
- **Record:** the result table goes in this spec's appendix and as a gt-elvf4 comment.

## Rollout

- **The MR:**
  1. Branch `crew/sloan/claude-z34-dolt-capacity`.
  2. Push it.
  3. Run `gt mq submit --issue gt-elvf4 --no-cleanup`.
  4. gt-elvf4 is assigned to `gastown/crew/sloan` and in progress; `needs-mayor-review` stays until the MR lands.
- **After landing:**
  - delete `polecat/agate/gt-elvf4+mufm8hwx` and `polecat/granite/gt-elvf4+mufr6noq`;
  - correct gt-elvf4's notes;
  - close gt-elvf4.

## Out of scope

- tmpfs data dir (follow-up bead).
- Pre-migrated template databases cloned per test.
- A host-wide or cross-package container cap.
- Lowering `-p` for Dolt packages.
- The beads-side fixes themselves (filed, not done here).
