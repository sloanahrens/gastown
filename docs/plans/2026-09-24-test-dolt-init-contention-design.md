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

## Appendix: measurement (2026-09-24)

Runs: `gt slot run --role gastown/crew/sloan-z34 --nice 0 -- env GOFLAGS=-p=8 GT_TEST_DOCKER=1 go test -json -count=1 -timeout 20m ./...` — the Makefile `test` recipe's go test line plus `-json` (plain package-list output drops a passing package's log, so the retry counts would be invisible) and `-count=1` (a repeat run would be cached). Wall time is measured inside the slot.

- baseline: origin/main at `eb79dec71073828fdfc7f6c9ca59de72319c03ec`; branch: `fb72a49a5026e4a994339b26a8f30f528f06cac3` (`crew/sloan/claude-z34-dolt-capacity`, Tasks 2-4: pre-filled per-process init-slot pool, `BD_ALLOW_REMOTE_MIGRATE=1` scoped to test-container calls, 3m container timeout, `RunTestContainerInit`).
- Load (1/5/15-min) at start/end of each run:
  - base-1: 9.69/8.14/11.91 -> 27.38/24.84/18.81
  - base-2: 18.27/22.85/18.30 -> 31.47/27.27/21.34
  - branch-1: 4.68/4.49/8.17 -> 14.84/13.16/11.17 (no refinery slot overlap; a first branch-1 attempt was aborted mid-run because it started the same minute the live refinery began gating an unrelated MR, to avoid double `-p=8` load and to not starve the refinery's retry; this is the retry, gated on the refinery MR closing, no refinery holder, and load < 20)
  - branch-2: 9.48/11.85/10.87 -> 24.07/21.86/15.97 (no refinery slot overlap)

| run | package | result | elapsed s | failed tests | retry notices | refusing | init root | bd timeouts | setup failed | resumed migrations |
|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|
| base-1 | internal/beads | pass | 77.933 | 0 | 22 | 2 | 8 | 0 | 0 | 0 |
| base-1 | internal/cmd | pass | 305.03 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| base-1 | internal/convoy | pass | 172.584 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| base-1 | internal/daemon | pass | 306.75 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| base-1 | internal/doltserver | pass | 10.228 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| base-1 | internal/mail | pass | 24.892 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| base-1 | internal/polecat | pass | 141.954 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| base-1 | internal/refinery | pass | 273.202 | 0 | 66 | 65 | 7 | 0 | 0 | 0 |
| base-1 | internal/testutil | pass | 6.05 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| base-1 | ALL (exit 1) | wall | 366 | 1 | 88 | 67 | 15 | 1 | 0 | 0 |
| base-2 | internal/beads | pass | 73.643 | 0 | 22 | 2 | 8 | 0 | 0 | 0 |
| base-2 | internal/cmd | pass | 295.959 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| base-2 | internal/convoy | pass | 158.397 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| base-2 | internal/daemon | pass | 294.597 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| base-2 | internal/doltserver | pass | 9.48 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| base-2 | internal/mail | pass | 21.559 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| base-2 | internal/polecat | pass | 133.059 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| base-2 | internal/refinery | pass | 233.99 | 0 | 46 | 40 | 8 | 0 | 0 | 0 |
| base-2 | internal/testutil | pass | 6.81 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| base-2 | ALL (exit 0) | wall | 315 | 0 | 68 | 42 | 16 | 2 | 0 | 0 |
| branch-1 | internal/beads | pass | 90.574 | 0 | 24 | 2 | 10 | 0 | 0 | 0 |
| branch-1 | internal/cmd | pass | 270.587 | 0 | 1 | 0 | 1 | 0 | 0 | 0 |
| branch-1 | internal/convoy | pass | 167.636 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| branch-1 | internal/daemon | pass | 289.799 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| branch-1 | internal/doltserver | pass | 9.282 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| branch-1 | internal/mail | pass | 24.495 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| branch-1 | internal/polecat | pass | 146.158 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| branch-1 | internal/refinery | pass | 169.312 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| branch-1 | internal/testutil | pass | 13.587 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| branch-1 | ALL (exit 1) | wall | 308 | 1 | 25 | 2 | 11 | 2 | 0 | 0 |
| branch-2 | internal/beads | pass | 87.077 | 0 | 24 | 2 | 10 | 0 | 0 | 0 |
| branch-2 | internal/cmd | pass | 299.472 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| branch-2 | internal/convoy | pass | 174.321 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| branch-2 | internal/daemon | pass | 302.905 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| branch-2 | internal/doltserver | pass | 11.662 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| branch-2 | internal/mail | pass | 26.235 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| branch-2 | internal/polecat | pass | 146.049 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| branch-2 | internal/refinery | pass | 197.937 | 0 | 1 | 0 | 1 | 0 | 0 | 0 |
| branch-2 | internal/testutil | pass | 4.993 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| branch-2 | ALL (exit 1) | wall | 318 | 1 | 25 | 2 | 11 | 1 | 0 | 0 |

**Two known-flake waivers applied to the branch runs' failed tests (both outside the 9 tracked packages, both confirmed independently, neither is a Dolt-contention signal):**

- **branch-2**'s one failure, `internal/web` `TestFetchConvoys_TimedOutDetailReadKeepsConvoyCounted`, is a pre-existing load flake also seen on `origin/main` and other branches (it embeds the literal string `bd timed out after 300ms` as part of its own simulated-timeout scenario, which coincidentally matches the `timed out after` marker at the log-wide `ALL` level, but the test itself is about `internal/web`'s dashboard fetch code, not Dolt). Reran alone: `go test -run '^TestFetchConvoys_TimedOutDetailReadKeepsConvoyCounted$' -count=1 -v ./internal/web/` -> `PASS` (0.82s), `rc=0`.
- **branch-1**'s one failure, `internal/slot` `TestAcquire_KernelReleasesOnProcessDeath` ("the SIGKILLed holder left no history entry"), is a self-contained test (uses `t.TempDir()` as its own town root, spawns and SIGKILLs its own helper subprocess — it does not touch the shared host slot pool) that is a history-entry timing race under heavy concurrent load, not a Dolt-contention signal. Confirmed: `git diff origin/main..HEAD -- internal/slot` is empty (the branch changes nothing in this package), and it passes reliably in isolation (`go test -run '^TestAcquire_KernelReleasesOnProcessDeath$' -count=5 -v ./internal/slot/` -> 5/5 `PASS`, `rc=0`).

Both branch runs are therefore **zero Dolt-related FAILs**.

**`internal/beads` retry/refusing/init-root counts are excluded from criteria 2 and 3** (controller ruling): they come entirely from that package's own retry-wrapper unit tests, which inject failures into a stub bd (`TestBdInitRetry*`, `TestBdContainerRetry*`, `TestInitRetriesInsideOneSlot`) — the counts are mechanically identical in baseline and branch runs (22/2/8, then 24/2/10 after Task 2-4 added coverage there) and are not a signal of real Dolt container contention. Raw/including-beads `ALL` numbers are kept below for reference.

| metric | base-1 | base-2 | baseline sum | branch-1 | branch-2 | branch sum |
|---|---:|---:|---:|---:|---:|---:|
| retry notices, raw ALL | 88 | 68 | 156 | 25 | 25 | 50 |
| retry notices, excl. beads | 66 | 46 | 112 | 1 | 1 | 2 |
| refusing, raw ALL | 67 | 42 | 109 | 2 | 2 | 4 |
| refusing, excl. beads | 65 | 40 | 105 | 0 | 0 | 0 |
| init root, raw ALL | 15 | 16 | 31 | 11 | 11 | 22 |
| init root, excl. beads | 7 | 8 | 15 | 1 | 1 | 2 |

Pass bar:
1. **Zero FAILs** — PASS. Both branch runs' only failures are waived known flakes outside Dolt-contention scope (see above); zero Dolt-related FAILs.
2. **"refusing" ≈ 0 (≤ 1 summed over branch-1+branch-2), excluding internal/beads** — PASS. Branch sum = 0.
3. **Retry notices down ≥ 80%, excluding internal/beads** — PASS. Baseline sum 112 -> branch sum 2, a 98.2% reduction (well past the ≥80% bar, which required ≤ 22.4).
4. **Wall ≤ baseline + 10%** — PASS. Baseline mean (366+315)/2 = 340.5s; branch mean (308+318)/2 = 313s = 91.9% of baseline.

**Resumed migrations** is 0 in every run, including both branch runs — the env var's resume path never actually fired even though `refusing` (excl. beads) dropped to 0 and retry notices (excl. beads) dropped 98%. The contention relief the branch shows is real and large, but its mechanism doesn't match the "each non-zero resumed-migration count is a would-have-been refusal" story anticipated in the design; worth a closer look as a follow-up, not chased down here.

**Verdict: PASS.** All four criteria hold. Zero Dolt-related FAILs; refusing and retry-notice reduction both clear their bars once `internal/beads`'s own stub-retry unit-test noise is excluded per the controller's ruling; wall time is within the +10% budget.

Metrics script (reusable for the tmpfs follow-up):

```bash
#!/usr/bin/env bash
# dolt-gate-metrics.sh <go-test-json-log> [label] — per-package Dolt-contention
# metrics from one `go test -json` gate log (gt-elvf4), as markdown table rows.
# grep -a everywhere: a gate log can be classified binary, and plain grep then
# reports "no match" silently. Reads <log>.exit and <log>.wall when present.
set -uo pipefail
log="${1:?usage: dolt-gate-metrics.sh <log.json> [label]}"
label="${2:-$(basename "$log" .json)}"
mod="github.com/steveyegge/gastown"
pkgs=(internal/beads internal/cmd internal/convoy internal/daemon internal/doltserver
      internal/mail internal/polecat internal/refinery internal/testutil)
markers=(
  'bd call against the test Dolt container failed on attempt'  # gastown retry notice
  'refusing to auto-apply'                                     # bd remote-migrate gate
  'could not resolve initial root'                             # Dolt catalog snapshot race
  'timed out after'                                            # bd subprocess budget kill
  'Dolt container setup failed'                                # testutil container start
  'Warning: applying [0-9]* pending schema migration'          # bd resumed under the env var
)
[[ -s "$log" ]] || { echo "dolt-gate-metrics: empty or missing log: $log" >&2; exit 2; }
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

counts() { # <file>: the marker counts, " | "-joined
  local f="$1" pat out=""
  for pat in "${markers[@]}"; do
    out+=" | $(grep -a -c -e "$pat" "$f" || true)"
  done
  printf '%s' "${out# | }"
}

echo '| run | package | result | elapsed s | failed tests | retry notices | refusing | init root | bd timeouts | setup failed | resumed migrations |'
echo '|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|'
for p in "${pkgs[@]}"; do
  pf="$tmp/pkg"
  grep -a -F "\"Package\":\"$mod/$p\"" "$log" > "$pf" || true
  if [[ ! -s "$pf" ]]; then
    echo "| $label | $p | absent | - | - | - | - | - | - | - | - |"
    continue
  fi
  final="$(grep -a -E '"Action":"(pass|fail|skip)"' "$pf" | grep -a -v '"Test":' | tail -n 1 || true)"
  result="$(sed -n 's/.*"Action":"\([a-z]*\)".*/\1/p' <<<"$final")"
  elapsed="$(sed -n 's/.*"Elapsed":\([0-9.]*\).*/\1/p' <<<"$final")"
  grep -a -q -F '(cached)' "$pf" && result="cached"
  failed="$(grep -a -F '"Action":"fail"' "$pf" | grep -a -c -F '"Test":' || true)"
  echo "| $label | $p | ${result:-none} | ${elapsed:--} | $failed | $(counts "$pf") |"
done
exit_code="$(cat "$log.exit" 2>/dev/null || echo '?')"
wall="$(cat "$log.wall" 2>/dev/null || echo '?')"
failed="$(grep -a -F '"Action":"fail"' "$log" | grep -a -c -F '"Test":' || true)"
echo "| $label | ALL (exit $exit_code) | wall | $wall | $failed | $(counts "$log") |"
```
