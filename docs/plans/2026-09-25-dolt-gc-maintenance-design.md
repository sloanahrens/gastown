> Status: historical (2026-09). Merged in: gt-pmawc. Not maintained.

# Scheduled Dolt maintenance: gc mode — design

Date: 2026-09-25. Tracking: claude-05o (handoff bead, item b). Sloan approved
building it after the one-off operator gc on 2026-09-24.

## Problem

`patrols.scheduled_maintenance` had two modes. `monitor` escalates commit
counts and changes nothing. `flatten` runs `gt maintain --force`, which squashes
history and can force-push. Nightly flatten was turned off on 2026-09-19 after
it left the gt database diverged from its remote. The 2026-09-17 03:05 Dolt
panic (nil deref in `prolly/tree`) came from a live reader during the nightly
run's final step, which was gc.

The investigation (claude-05o notes, evidence in
`~/.claude/docs/research/claude-05o/`) found:

- Most on-disk growth is unreferenced chunk data, not history. A scratch
  restore of hq went from 2.6G to 102M under `dolt gc` with all 4993 commits
  kept. Flattening saved only a further ~4%.
- Normal bd reads do not slow down as history grows. Only history-walking
  queries do (`bd history`, `dolt_history_*`), at about 44us per commit.
- Dolt's gc is generational. Auto-gc and plain `dolt_gc()` collect only the
  new generation, so old-generation chunks stay until a `--full` pass.
- The operator's prod run of `CALL dolt_gc('--full')` per database on
  2026-09-24 took 0-2s per database and cut the total from 1.57G to about
  660M. No commits were lost and no panic occurred.

The town needs a periodic, unattended, history-preserving gc. It must not
flatten and must not race live work.

## Design

A third mode, `gc`, in `internal/daemon/maintenance_gc.go`.

1. **Mode resolution.** `maintenanceMode` matches only the exact word
   `flatten` or `gc`, ignoring case and surrounding space. Anything else,
   including typos, is `monitor`. A binary built before gc mode reads `gc` as
   `monitor`, so a rollback degrades to escalating and never flattens.
2. **Never flattens.** The gc branch returns before commit counting and never
   reaches `maintenanceExecFn` (`gt maintain`). A test asserts this.
3. **Size trigger, per database.** The trigger is the sum of regular files under
   `<dataDir>/<db>`, from stat calls only, with `oldgen` included. A database
   is gc'd when
   `size >= gc_min_bytes` and either no baseline exists or
   `size >= gc_growth_ratio x baseline`. The baseline is the size measured just
   after the database's last patrol gc. It is stored in
   `<town>/daemon/maintenance_state.json` and written atomically. Defaults are
   256MiB and 2.0. After the 2026-09-24 gc, gt (480M) re-triggers at about
   960M and hq (97M) at 256M.
4. **Quiet-window guard, re-checked before each database.** The guard
   (`maintenanceQuiet`) requires all of the following:
   - `daemonWorkIdle()`, which is the upgrade-restart predicate from
     `upgrade_idle.go` without the gc cycle's own flag;
   - no `main_branch_test`, including one still waiting for a slot;
   - no container-gate slot or in-flight marker held by any role, read with
     `slot.StatusPoolLocksOnly` (no `docker ps`);
   - no polecat whose session heartbeat says `working` and is less than 15
     minutes old (file reads only).

   A probe that cannot answer counts as busy. When the town is busy the
   cycle logs the reason and the remaining databases, then stops. The next
   5-minute tick in the window retries. Databases already gc'd have new
   baselines and drop out.
5. **Execution.** Eligible databases run smallest first, one at a time.
   Each runs `CALL dolt_gc('--full')` over a single SQL connection to the
   running server. The read timeout is 10 minutes, and the context is bounded
   by 10 minutes and the daemon's context. The cycle runs on its own goroutine
   so the heartbeat loop never stalls (gt-uvxy). `maintenanceGCRunning` blocks
   a second cycle and makes `isIdleForUpgrade` false, so an upgrade-restart
   never kills a gc midway.
6. **Diagnostics.** Each database logs its size before, its size after and
   the duration. A gc error stops the run, logs the error, and escalates once
   through `maintenanceEscalateFn` (`d.escalate`, which calls `gt escalate`
   with retries and a timeout). The failure sets the run time, so the patrol
   does not retry until the next interval. The patrol never restarts Dolt.
7. **Config.** `gc_min_bytes` (int64) and `gc_growth_ratio` (float64) sit
   under `patrols.scheduled_maintenance`. The daemon replaces invalid file
   values with the defaults and logs a warning. `gt config set` refuses
   them and now accepts `maintenance.mode gc` and both `maintenance.gc_*` keys.

## Fix round 1 (review, 2026-09-25)

The review approved the mode, trigger, baseline and config. It asked for
four fixes before prod, all of them now built:

1. **Health restart during gc.** `DoltServerManager.EnsureRunning` stops and
   restarts a server that fails its health, write or identity probe. A new
   `restartSuppressed` hook, wired to `doltRestartHeldForGC`, defers that
   restart while a gc call is in flight and within its timeout plus a
   2-minute grace. Past the grace it escalates once and lets the restart
   proceed, because a gc stuck past its own context timeout means the server
   is stuck. A dead server is still started.
2. **The daemon's own Dolt work.** `d.doltMaintMu` is an RWMutex that no
   caller ever blocks on.
   - The gc takes the write side with `TryLock` after each database's quiet
     check and holds it for that one call. If the lock is taken, the gc
     defers with the reason "daemon Dolt task in flight".
   - `syncDoltBackups`, `pushDoltRemotes`, `reapWisps`, `syncJsonlGitBackup`
     and the compactor cycle take the read side with `TryRLock`. They skip
     their tick while a gc holds the lock and log "skipped: gc in flight".
     Using a read side keeps these tasks from excluding each other.
   - The ConvoyManager gets a `pollGate`. Event-poll ticks and stranded
     scans take its read side with `TryRLock` and skip while it is paused.
     `Pause(timeout)` and `Resume()` wrap each database's gc; if a pause
     times out, the gc defers with "convoy poll busy". While `Pause` waits,
     a `pausing` flag stops new ticks from starting, so back-to-back ticks
     cannot starve it; it still polls `TryLock` because a blocking `Lock`
     cannot be abandoned on timeout.
3. **Database set.** Databases are discovered from the data dir: every
   directory that has a `.dolt` subdirectory. This replaces the
   compactor_dog and wisp_reaper lists, which fall back to `["hq"]` alone.
   There is no config key; discovery is the only source.
4. **Skipped-window escalation.** A window that closes with gc still
   deferred and at least one eligible database increments
   `consecutive_deferred_windows` in `maintenance_state.json`. The file also
   keeps the pending deferral, the last deferral reason and a count per
   reason.
   - The first escalation fires at 3 windows for a daily interval, or 2 for
     weekly, monthly or an interval of 6 days or more.
   - After that it escalates at most once every further 3 windows.
   - A completed or failed run resets the count.

Minor fixes:
- The DSN uses `doltServerHost()`, and the database name is validated
  before the DSN is built.
- gc mode is skipped, with a log line, when the server is external or on a
  host that is not loopback.
- The docs now state that `gc_min_bytes` must be a plain JSON integer.
- The docs now state that a failed re-measure records no baseline, so the
  next interval gc's that database again.

## compactor_dog.threshold

The compactor dog only escalates; it never compacts. With gc mode handling
disk by size, its commit threshold only guards history-query latency (see
Problem). At 2000 it escalates daily on
every busy database without a real problem behind it. A town running gc mode
sets it to 20000.

## Enabling (after this lands and is installed)

`mayor/daemon.json`:

```json
"scheduled_maintenance": {
  "enabled": true, "window": "03:00", "interval": "daily", "threshold": 1000,
  "mode": "gc", "gc_min_bytes": 268435456, "gc_growth_ratio": 2.0
},
"compactor_dog": { ..., "threshold": 20000 }
```

## Not in scope

- A `gt maintain --gc-only` operator command. The patrol covers the recurring
  case, and the one-off case used a script (`run-gc.sh` in the evidence dir).
- Rebuilding `.dolt-backup` stores and running `git gc` in `.dolt-archive`
  (separate operator steps, claude-05o item d).
- Flatten stays off. Revisit it only if history queries degrade, and take a
  pre-flatten backup ref first.

## Tests

`internal/daemon/maintenance_gc_test.go` uses fakes for the size, gc, quiet,
slot, polecat, escalation and dispatch seams. It never touches Dolt, `~/gt`
or Docker. Coverage:

- mode resolution;
- config defaults and validation;
- the trigger table;
- state round-trip and repair of a corrupt state file;
- the size walk, with oldgen counted and path escape refused;
- the cycle: smallest-first order, baselines, deferral midway, and
  stop-and-escalate-once on error;
- wiring through `runScheduledMaintenance`: the gc path never flattens, a
  deferral is retried on the next tick, a failure is not retried, and a
  running cycle is skipped;
- the quiet guard's individual probes;
- the gc flag blocking an upgrade but not the guard itself;
- detection of working polecats from heartbeats, and a rig whose polecats
  cannot be listed counting as busy.

`internal/daemon/maintenance_gc_dolt_test.go` runs `doltGCFull`'s real
`CALL dolt_gc('--full')` against the package's Docker-gated Dolt container
and checks that commits and rows survive. It skips without Docker
(`GT_TEST_DOCKER=0`).
