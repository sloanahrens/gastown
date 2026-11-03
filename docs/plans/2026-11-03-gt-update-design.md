> Status: historical (2026-11). Merged in: gt-2tgau (design only; the six sections were approved by the operator on 2026-11-03, and nothing is built except the archive limits, gt-s6srv). Not maintained.

# gt update: one job for install, image and maintenance

## Goal

The town today needs quiet for three separate things that each have their own trigger: installing a new gt binary and its sandbox image (the `rebuild_gt` daemon job calling `cmd/gt-installer`), the nightly Dolt maintenance window (backup, history archive, gc, all inside the daemon), and a manual hold-drain-install routine the overseer runs by hand. They collide, they are invisible to the operator ("why is nothing dispatching?"), and the maintenance has starved before (gt-mdejo: the gt database went 17 days without a gc). The operator wants the daemon small and the heavy work in a separate process that the daemon can start on a schedule and a person can start by hand, as the tier sweep already works.

This design adds one command, `gt update`, that does install, image and maintenance as one job with no modes, and a small daemon trigger that decides when it is due and shows what it is doing.

## Facts this rests on

- The tier sweep is the pattern: the daemon decides, a runner does the work, a state file (`.runtime/tier-sweep/<rig>.json`) and a `result.json` record it, the dashboard reads the state (internal/daemon/tier_sweep.go, internal/sweeprunner).
- `rebuild_gt` only decides (interval, install request marker, quiet gate); `cmd/gt-installer` builds, publishes the sandbox image before the binary swap, and swaps. The daemon restarts itself onto the new binary when idle (upgrade-restart).
- The maintenance code is about 3,300 lines in internal/daemon (scheduled_maintenance*.go, maintenance_*.go) with about 180 uses of the daemon's logger, config, state and Dolt manager. It starts only from the daemon's window tick.
- Measured durations (daemon.log, 2026-11-01 to 11-03): nightly backup 0.5 to 2.7 minutes, gt gc 1.4 minutes (it reclaimed nothing: the size is history), install about 4 minutes, image publish 2 seconds when cached. The first history archive of all 11 databases took 36 seconds, but it copies at most 5,000 commits and 5,000 events rows per database per cycle (before gt-s6srv made the bounds settings), so gt (23,523 commits and 138,430 events behind) needed about 28 cycles.
- The hold file stops the sweep and the maintenance gc as well as dispatch (gt-6mlik.6, 2026-10-31), so a hold for an install also cancels that night's gc and archive.
- Every rig's beads are still in Dolt (server mode). The Postgres tracker (ADR 0006, epic gt-p85pw) is the destination but no rig has flipped, so Dolt maintenance is worth building.

## Decisions already made

- Dolt is interim and runs on this host (a process now, a Docker container next). No Dolt in Kubernetes. A hosted Dolt is an unresearched fallback.
- The Ring work (gt-6mlik) is paused. Note on gt-6mlik.9: deleting the hold file would remove the dispatch-only lever.
- The dispatch hold means "no new polecats" and nothing else (gt-5cl4i).
- Clusters per claude-2or.1: control, work, tenant. The gate runner and Forgejo moves are dependencies and later tracks, not part of this build. Endpoints stay configuration.
- Approach: a wrapper job (A). A daemon-resident state machine (B) is rejected. A full extraction before shipping (C) is reached in version 2.

## Design

### 1. Who does what

`gt update` is its own process. It does all sequencing and heavy work. An exclusive lock file `.runtime/update/lock` stops two runs overlapping; a second run exits at once and names the one in progress.

The daemon keeps one small job, the update trigger: decide when an update is due, launch `gt update` as a subprocess, read its state, alert on a stale job. It refuses to launch under the town ESTOP or a waned Ring. The self-restart onto a new binary, the Dolt health tick and the hold's effect on the dispatcher stay in the daemon.

Files under `.runtime/update/`: `state.json` (rewritten atomically at every step), `run-<id>.log`, `history.jsonl` (one line per run), `maintenance-request` (version 1 only), `lock`.

The job owns the hold only if it created the file: it sets `~/gt/seat-refill.hold` with a note ("gt update <run id>: draining") when none exists, and releases that file on every exit path. A hold already present when the job starts is never released by it.

### 2. The steps

Each step decides for itself whether it has work. The plan, with a reason per step, is printed first; `--dry-run` stops there.

1. Plan: take the lock; compare the installed build with main; check whether the archive is behind, whether today's backup exists, whether a gc is due by the existing rules.
2. Drain, only if an install is planned: set the hold with a note if none exists; wait up to 30 minutes for working polecats to finish; a polecat silent and idle for 5 minutes counts as drained; a working polecat is never killed; if a seat is still working at the end, abort, release the hold if the job owns it, and name the seat. The drain fails closed: if the seat list cannot be read, that poll never counts as "no seat blocking"; it retries until the timeout and then aborts with the error named.
3. Maintenance (archive, backup, gc) runs beside work with no drain. When an install is planned, the hold step 2 leaves in place is present here, so this step depends on the hold decoupling track (§5, gt-5cl4i): until the sweep and the maintenance gc stop reading the hold file, the hold cancels the cycle the job requests. Version 1 requests one daemon maintenance cycle through `maintenance-request`, logs the cycle's stage lines and waits for completion; if nothing is due it skips the request.
4. Build, image, install: delegate to `cmd/gt-installer`; skip when the installed build equals main and the image is present and published.
5. Wait up to 15 minutes for the daemon's self-restart. On timeout the new binary is still installed: release the hold if the job owns it, record "installed, restart pending", escalate.
6. Verify: `gt version`, the daemon's reported version, the image tag, town health.
7. Release the hold if the job owns it.

Failure rules: a failed maintenance stage does not block the install (the run ends "partial" with an escalation); a failed install aborts and the town keeps running on the old binary; the hold-release rule in §1 covers Ctrl-C and SIGTERM; a job killed outright is caught by the stale heartbeat and the hold's note names the run. Re-running after a crash replans, because finished steps skip themselves; the state file is a record, not a resume script. Flags for debugging only: `--dry-run`, `--skip <step>`, `--only <step>`.

### 3. State, dashboard, stuck-job detection

`state.json` is versioned (`v: 1`) and holds the run id, start time, target build, the plan (per step "will run" or "skipped" with a reason), the current step, each step's start, end, result and detail, whether the job owns the hold and its note, a heartbeat time and the outcome (running, ok, partial, aborted, failed, interrupted). A separate timer refreshes the heartbeat every 15 seconds, so a hung job looks different from a slow step. `history.jsonl` gets one line per run with every step's duration.

The daemon raises one escalation `update:stale` when the outcome is "running" and the heartbeat is more than 3 minutes old, and one when a run exceeds 2 hours. A failed or partial outcome becomes an attention item. The daemon never edits the file.

The dashboard state feed gains an `update` section read from `state.json`: running or not, run id, elapsed, current step, compact step list, last outcome and time, and the hold note. The banner reads like "gt update: install, draining, 8 min, 2 polecats left. Dispatch held". When only a hand-set hold exists, the banner shows that hold's note and age. `gt update status` prints the same record, human-readable or `--json`.

### 4. Triggers and coexistence

The trigger is configured under `daemon.patrols.update`: `enabled`, `window` (03:00), `min_interval` (3h), `max_runtime` (2h). An update is due when the nightly window opens (once per day), or when main differs from the installed build, at least one landing waits to deploy and `min_interval` has passed since the last run ended. It launches only with no ESTOP or waned Ring, no run in progress and no install marker pending.

The trigger ships disabled. When `update.enabled` is true, `rebuild_gt` stops making install decisions and the maintenance window tick stops starting cycles by itself; the cycle code still runs inside the daemon in version 1. One key switches it on; switching it off is the rollback. The old triggers are deleted only after version 2.

The run-now request: at its 5-minute tick, if `maintenance-request` exists, the daemon runs the cycle regardless of the window and the 20-hour gap. It still honors the ESTOP, the Ring and the gc's quiet guard. If the guard defers, the file stays and the next tick retries; the job waits up to 30 minutes, then moves on and marks maintenance "deferred" (it never blocks the install). Under the ESTOP or a waned Ring the daemon answers at once with a "deferred" result naming the standby and deletes the file, so the job does not sit out that wait. When the cycle finishes, even with failures, the daemon records the result and deletes the request it served — re-reading the file first and deleting it only while it still holds the run id the cycle started with, so a request another job wrote meanwhile survives to its own cycle. A request that arrives while a cycle is still running is answered "deferred": its file is deleted when that cycle is serving it, so the job does not wait out the running cycle, and kept when it is not, so a later tick runs it. A request with no configured window runs the cycle and records no deferral streak. A tick that cannot run the cycle at all (no databases, no local server) answers "failed" with the reason and deletes the file, so no request is left unanswered; a failed request does not set the scheduled gap. The run id is untrusted: over 64 characters or outside `[A-Za-z0-9._-]` is answered with an id-less failed result and is never logged. The last request-driven answer is stored in the maintenance state file before the result is written, so a completed cycle whose result write failed is re-answered by a later tick, not rerun; when that state cannot be read, a present request is answered failed and no cycle runs, because an unreadable state cannot prove the answer was not already given.

### 5. Other tracks, in dependency order

| Track | What | Size |
|---|---|---|
| Hold decoupling | gt-5cl4i: sweep and maintenance gc and archive stop reading the hold file | S |
| Archive limits | gt-s6srv: batch bounds become `archive_commit_limit` and `archive_events_limit` (default 5000) | landed |
| Archive loop | keep taking batches per database within one cycle until nothing is behind or a 10-minute budget ends | S |
| gt update core | about 11 beads: state package, plan, drain, hold ownership and cleanup, installer wrapper and verify, request file (job side and daemon side), daemon trigger and config, stale alert, commands, dashboard feed and banner, runbook | S to M each |
| Dolt container | a tested Go helper that only prints the `docker create` command (same-path bind mounts of `.dolt-data` and `~/gt-backups/dolt`, 12 GiB memory limit to start, loopback port), a runbook, a rehearsal on a restored copy compared with `gt dolt compare`, then cutover with the hold on; rollback is removing `dolt.external_container` | S, S, then overseer-run |
| Squash | the hand-run rolling squash (keep 7 days) from gt-2conu after every database reads 0 behind; no daemon step | no code |
| Version 2 | extract archive, backup and gc into a shared package with watermark state under a file lock; the daemon's maintenance and `rebuild_gt` become thin triggers | L, own plan |
| Cluster-runner backend | build and image on a cluster runner, as the sweeps have | design only |

Order: hold decoupling and archive loop first; then an install; then the archive catches up and the squash runs; the gt update core is a chain (several beads touch the daemon, so each lands before the next dispatches); the Dolt container is an independent parallel track; the trigger goes live last. Optional shortcut: once the limit settings are live, raise both to about 50,000 and run the cycle a few times to catch gt up without waiting for the loop, watching memory.

### 6. Testing, rollout, risks

Tests: `gt update` lives in its own package with the clock, files, commands, the hold and the daemon probe behind interfaces; table tests per step; no real Docker, network or sleeps. Daemon tests use narrow `-run` filters (container-suite package). Dashboard feed and banner text have fixture tests. Mutation proofs for the guards that matter: a foreign hold is never cleared, a stale heartbeat is detected, the drain never kills. On the live town: a read-only `gt update --dry-run` after the install, then the first real run with the operator watching; measured step times land in `history.jsonl`.

Rollout: ships disabled; two clean hand runs; then `enabled: true`. Rollback: `enabled: false`.

Risks: in version 1 the maintenance still lives in the daemon. The daemon's idle restart waits on hard holds, so the 15-minute wait can time out under heavy landings (handled as "installed, restart pending"). A hold that already exists when the job starts is governed by the ownership rule in §1. The archive loop and squash timings are unmeasured. "Image published" depends on the registry setting being present.

Out of scope: the Ring, Dolt in Kubernetes, the cluster-runner backends, the gate-runner and Forgejo moves, the tracker flip.

## References

gt-s6srv, gt-5cl4i, gt-mdejo, gt-6mlik.6, gt-6mlik.9, gt-xeb1x, gt-2conu, gt-p85pw; ADR 0006 (docs/adr/0006-tracker-replaces-dolt-backed-beads.md); claude-2or.1, claude-2or.9, claude-2or.11 (control, work and tenant clusters; resident order); docs/dolt-history-offline.md; docs/design/dolt-rolling-squash.md; docs/design/dolt-storage.md.
