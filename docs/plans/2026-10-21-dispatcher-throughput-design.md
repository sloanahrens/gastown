> Status: approved 2026-10-21 (Sloan, claude-rh7). Implemented by gt-ckz0r (landed), gt-t7k92, gt-uoez5, gt-aiby1, be-gf9, be-yn5 and the crew landing gt-bcmlj.

# Spec dispatcher throughput

Date: 2026-10-21. Owner: Sloan's load session (handoff bead claude-rh7). Status: approved design.

## Problem

With 9 seats and 20-60 shaped candidates, the town runs 2-4 polecats at a time. Measured on 2026-10-21, 09:00-11:45:

- A tick's time is its slings, not its reads. Ticks by polecats slung: 0 slung 0.4 min, 1 slung 1.8 min, 2 slung 2.9 min, 3 slung 3.7 min (medians). A reads-only dry run of the same tick takes 15-35 s.
- Each sling costs 45-70 s, run one after another inside the tick. `gt sling` re-reads every polecat directory in the town (`occupied()` in `sling_pool.go`, through the per-seat reader `poolPolecatSeatState`): at least 2 `bd` subprocesses per directory, idle seats included, 34 directories. `bd mol bond --ephemeral` adds 7.6 s per sling (0.32 s dry).
- Ticks serialize (the 60 s ticker skips while one runs) and `max_per_tick` was 3; it bound 10 of 32 daytime ticks. Seats turn over fast (5.8 min of work plus 1.1 min of landing), so the pool settles at 4-6 seats counted and 2-4 live sessions.
- The daemon's own patrols make every `bd` call slow. A 123 s trace of daemon subprocesses held 1,633 processes: 602 `bd` calls (`patrol_scan` runs `check-recovery-batch` for 9 rigs every 2 min, 5-9 `bd` calls and about 10 git commands, two of them network `ls-remote`, per polecat directory, with no skip for empty seats) and 193 git processes (the `hm-sync` plugin fetching every repo every 15 min). A `bd show` costs 0.15 s quiet and 0.7-1.9 s when idle drops under 10%, while bd's own CPU stays at 0.06 s and a raw Dolt query at 0.07 s: the time is queueing on Dolt round trips.
- 33 daemon upgrade restarts in one day, one per `gt` rebuild after a landing (41 builds, 104 min of go build). At least four cut a tick mid-sling ("a spec dispatch tick is still in flight after 2m0s; restarting anyway").
- One seat of nine was dead for 23 h: devops/mariner, parked "overrun (20 minute rule)", skipped by crash detection because parked, yet counted toward capacity.
- 145 Dolt serialization failures against 777 commits (19%), retried with backoff. Deferred: a symptom of concurrent writers, not a dispatch gate.

Data: `~/gt/.runtime/loadprof/` (`tick-calls.tsv`, `sling-trace.txt`, `tickprobe.tsv`, `bdprobe.tsv`, `bdcpu.tsv`).

## Target

Daytime, measured from the daemon log by the owner after each landing (never by a polecat):

- the median tick shows at least 7 of 9 seats live;
- a tick that slings 3 polecats finishes in under 60 s;
- a freed seat is refilled within 2 minutes.

Baseline: 3.7 min per 3-sling tick, 4-6 seats counted, refill 1-5 min.

## Already applied (config, 2026-10-21)

- `daemon.patrols.spec_dispatch.max_per_tick` 3 -> 5 (read per tick).
- `daemon.patrols.patrol_scan.interval` 2m -> 10m (read at boot; applies at the next upgrade restart).
- `~/gt/plugins/hm-sync/plugin.md` cooldown 15m -> 60m.
- devops/mariner nuked (`--force`, branch preserved on origin).

Backups under `~/gt/.runtime/loadprof/`.

## Design

### 1. Sling reads through the batched seat picture (bead a)

`poolRouterFor` wires `disposition` to the per-seat reader (`sling_pool.go:1131-1133`), so `occupied()` costs 2+ `bd` calls per polecat directory. The spec dispatcher already reads the same picture through `poolSeatBatch` (`spec.go:1611`, `pool_seats.go:189-198`): two `bd` calls per rig (`ShowMultiple` of the agent beads, `List` of open/in_progress/hooked) and then no per-seat agent or assigned-issue read.

Change: `poolRouterFor` builds a `poolSeatBatch` for the town and feeds `disposition` from it. Hook and hint shows stay per seat (`pool_seats.go:160`). No behaviour changes; a hand-run `gt sling` gets the same picture it gets today, cheaper.

Test: through the fake store, a town of N idle polecat directories costs 2 reads per rig and no read per directory.

### 2. Tick snapshot and parallel slings (crew session)

The tick holds the batched roster (`specRosterSessions`). Changes, all in the dispatcher and the pool router:

- `env.Sling` receives the tick's occupancy snapshot. `route(requested, true)` takes an `occupied` source; the dispatcher's source answers from the snapshot and the ledger (`poolSeatLedger.begin`) re-reads only the chosen seat's claim under the decision lock. The in-flight claim record a sling writes before its session exists (gt-t8q5) is what keeps two concurrent slings off one seat.
- The loop in `runSpecDispatchCycle` first collects eligible candidates up to the free-seat count (after holds, the `showFresh` re-read and the shape gate), then runs their slings concurrently with a bounded worker set the size of that count. `RetryOnContention`, the rollback on failure (gt-moxlb) and the per-sling log lines stay; the report lists entries in candidate order.
- `max_per_tick` stays as an optional ceiling; 0 or absent means the free-seat count. The config value set today (5) is removed once this lands.
- A hand-run `gt sling` is untouched beyond section 1.

Expected tick: reads (15-25 s) plus the slowest sling (about 15 s after section 1 and the bond work).

Tests: a fake lister and store with 9 free seats and 12 candidates slings 9 in one tick with no seat claimed twice (`go test -race`); a sling failure leaves its seat free for the next tick; a tick with `max_per_tick` 2 slings 2.

### 3. Parked seats stop counting (bead b)

The overrun guard pauses the agent with reason `overrun (20 minute rule)` (`overrun_guard_stop.go:26`). Crash detection skips a parked polecat (`daemon.go:2728`) and the pool counts it (`poolDeadHookedSessions`, `pool_seats.go:368-390`; `workstate.go` sets `CountsTowardCapacity` for `not-idle`).

Change: in `poolDeadHookedSessions`, a sessionless polecat whose park is older than 30 minutes is reported with `CountsTowardCapacity=false` and the roster line shows `(1 parked-stale)` instead of `(1 dead-hooked)`. The crash-detection skip logs once per polecat that the seat was released and that recovery is the operator's (`gt polecat check-recovery`, then `nuke`); it does not restart a parked polecat.

Test: a parked record 31 minutes old frees the seat; 29 minutes keeps it; a parked polecat with a live session is unaffected.

### 4. Recovery batch skips empty seats (bead c)

`runPolecatCheckRecoveryBatch` (`polecat.go:1530`) preloads agent beads and merge requests per rig, then runs the full check on every polecat directory (`checkRecoveryForPolecat`: 5-9 `bd` calls, `git status`, two `stash list`, two preservation checks with `ls-remote`).

Change: after the preloads, one tmux listing. A polecat with no session, no hook in its preloaded agent bead and agent state `done` or `idle` is reported from that evidence (`buildPolecatInventoryItemFromEvidence`) with no `mgr.Get` and no git. Everything else runs the full check as today. The patrol interval stays at 10m.

Test: a rig with 10 idle directories and 2 live seats runs the full check twice and reports 12 items.

### 5. Install and restart coalescing (bead d)

`rebuild_gt` arms a sticky request on every drained landing queue (`requestRebuildGTInstall`) and installs; the upgrade restart waits up to 2 min for an in-flight tick, then restarts anyway (`upgrade_restart.go:574`).

Change: the patrol runs the install only when the last install is at least `rebuild_gt.min_interval` old (default 30m) or the landing carried `gt:install-now` (`requestRebuildGTInstallNow`). The upgrade restart waits for "no spec tick and no landing pass in flight", still capped at 2 min, and the log line names what it waited for. Net: at most two builds and restarts per hour, none mid-sling under normal tick lengths.

Test: three landings in 10 minutes produce one install; `gt:install-now` installs at once; a restart with a tick in flight waits until the tick's done flag flips.

### 6. Bond cost (bead e, beads rig)

`bd mol bond --ephemeral` for an 8-step formula writes 9 wisps and up to 16 deps in one transaction (`template.go:654-757`), yet costs 7.6 s under load. Polecats use the molecule (540 of 1,040 step wisps today were closed before their root), so the bond stays.

Change: profile the bond's statements (the per-row inserts, the second-connection ignored-table transaction) and batch them into few statements. Acceptance: a statement-count test (under 10 statements for a 9-wisp bond) and a benchmark under 1 s against the test server.

### 7. Measurement

After each landing the owner records on claude-rh7, from `~/gt/daemon/daemon.log`: tick duration by slings dispatched, live seats at the tick, and refill latency (seat freed to next sling). The target in "Target" is checked against the 2026-10-21 baseline. Polecat acceptance boxes never carry these numbers.

## Order and ownership

1. bead a (sling reads), 2. bead b (parked seats), 3. bead c (recovery batch), 4. bead d (install/restart), 5. bead e (bond, beads rig), each a shaped bead for the dispatcher; a, b and d touch `sling_pool.go` or the daemon, so they land one at a time with `bd dep add` between them. The crew session for section 2 starts after bead a lands, in `~/gt/gastown/crew/sloan-dispatch` on branch `crew/sloan/dispatch`, with its own handoff bead, and lands through `gt done`.

## Out of scope

Write-conflict retries, om review and landing-review load, the Docker disk size, Spotlight and Bitdefender, and the bd NOT-FOUND path (landed as be-mud).
