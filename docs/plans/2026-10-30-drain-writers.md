> Status: historical (2026-10). Merged in: claude-aq9.4 (the resolution comment) and gt-6mlik, which hold the live decisions. Not maintained.

# Drain-time writers in gastown (claude-aq9.4)

Source: origin/main of gastown (clone crew/overseer), read with `git show`. Paths are under `internal/`.

## How the two brakes work

- **Hold file** (`seat-refill.hold`): `dispatch.OperatorHold` (dispatch/hold.go:43) checks the file, then the town ESTOP. `RigHold` (hold.go:77) adds the rig ESTOP and the rig dispatch hold.
- **ESTOP**: read by `estop.IsActive` / `estop.ActiveFor`. In the daemon it acts in three places.
  - `heartbeat` (daemon/daemon.go:1947) skips every step marked `lifecycle: true` in `heartbeatSteps` (daemon.go:1970).
  - The supervisor refuses `Kill`, `KillAll`, `Restart` (supervisor.go:242, `guard`) and `KillStray` (supervisor.go:595).
  - The jev passes stop asking once it is set (`newEstopGate`, daemon/jev_dedupe.go:121).
- **Drain order** (power/states.go:197 `Standby`): `setHold`, `ensureInfra`, `drain`, then `setEstop`. So the drain runs with the hold only. The ESTOP arrives after the drain ends. `drain` (states.go:352) waits for landings, working polecats and a Dolt gc.
- **Consequence**: a job that reads only the ESTOP is not braked during the drain. Only `dispatchQueuedWork`, `triggerSpecDispatch` and `runScheduledSlings` read the hold.

## Decision recorded in claude-cl7.15

- **Landing worker ignores both brakes: confirmed.**
  - `landingWorkerLoop` (daemon/landing_worker.go:297) gates a pass on `isRigOperational` (daemon.go:2619) and `upgradeRestartPending`.
  - `isRigOperational` checks parked, docked and `auto_restart` only.
  - Nothing in `internal/land` or `internal/landworker` references `estop` or `OperatorHold`.
  - It starts from `startLandingWorkers` (landing_worker.go:234), outside the heartbeat, so the lifecycle skip never reaches it.
- **Tier sweep files beads under ESTOP: confirmed, and it also ignores the hold.**
  - Its heartbeat step is `{name: "tier-sweep", run: ...}` (daemon.go:2143) with no `lifecycle` flag, so it runs under ESTOP.
  - `triggerTierSweep` (tier_sweep.go:554) has no hold or ESTOP check.
  - `runTierSweep` (tier_sweep.go:865) creates beads via `bd.Create` (tier_sweep.go:1791) and runs gate tiers in a private worktree.

## Heartbeat steps (daemon.go:1970-2159, loop at daemon.go:2261 `heartbeatWork`)

"ESTOP" means held through the lifecycle flag. Drain means hold only, standby means both set.

| Step (start) | Hold | ESTOP | Writes or acts |
|---|---|---|---|
| rigs-cache, prefix-registry | no | no | in-memory and registry reload only |
| dolt `ensureDoltServerRunning` (daemon.go:2298) | no | no | starts Dolt; intended, upkeep |
| ghost-sessions `killDefaultPrefixGhosts` (:2446) | no | yes (lifecycle + `KillStray`) | kills sessions |
| plugins `handlePlugins` (handler.go:49) | no | yes | runs script plugins, which write whatever they write |
| polecat-health `checkPolecatSessionHealth` (:3344) | no | yes (+ supervisor) | restarts polecat sessions in a drain |
| idle-reap `reapIdlePolecats` (:3950) | no | yes (+ `Kill` :4085) | kills idle polecats |
| orphan-processes `cleanupOrphanedProcesses` (:4106) | no | yes | kills processes |
| branch-prune `pruneStaleBranches` (:4134) | no | no | deletes local git branches |
| sandbox-seat-ends (sandbox_seat_end.go) | no | no | ends sandboxes; deliberate, comment at daemon.go:~2050 |
| dispatch `dispatchQueuedWork` (:4179) | **yes** `OperatorHold` :4184 | yes | slings queued beads |
| log-rotation, events-prune | no | no | disk, log files |
| git-hygiene `triggerGitHygiene` (git_hygiene.go:66) | no | no | deletes merged branches, gc of rig repos, guarded push |
| rebuild-gt `triggerRebuildGT` (rebuild_gt.go:231) | no | yes | `make install`; restarts the daemon, so a drain can be cut |
| rebuild-om `triggerRebuildOM` (rebuild_om.go) | no | yes | installs the om binary |
| 14 jev jobs (jev_dedupe.go:~60 ... jev_stale.go:27) | no | yes (lifecycle and `estopGatedAsker`) | LLM asks, bead notes and labels, per `jev_*.go` |
| tier-sweep `triggerTierSweep` (tier_sweep.go:554) | no | **no** | bead creates, gate runs |
| townhealth `writeTownHealth` (townhealth.go:167 reads the hold) | report only | no | writes health file |
| attention `writeAttention`, unhandled-escalations (:13) | no | no | writes attention files, pushes escalations |

The 14 jev jobs: dedupe, attention, landing, submission, drift, logtriage, rework, dispatch, thrash, rules, conflict, progress, stale, notes.

## Tickers and goroutines (daemon.go Run loop, 1472-1914)

All start from `Run` and none are in `heartbeatSteps`, so the lifecycle skip does not cover them.

| Job (start) | Hold | ESTOP | Writes or acts |
|---|---|---|---|
| landing worker manager, per-rig workers (landing_worker.go:234, :297) | no | no | merges PRs, runs CI and om, writes landing records. Also starts om-skew monitor and deploy watch. |
| spec dispatch `triggerSpecDispatch` (spec_dispatch.go:202, hold at :298) | **yes** | yes | slings; the only dispatcher with the full check |
| scheduled slings `runScheduledSlings` (scheduled_slings.go:460, hold :468, `RigHold` :499) | **yes** | yes | slings; deep-review schedules are entries here |
| patrol scan `runPatrolScan` (patrol_scan.go:155) | no | partial: `Restart` :725 and `Kill` :838 refused | still closes orphan molecules, writes intent records, gate checks, escalations |
| overrun guard `runOverrunGuard` (overrun_guard.go:307) | no | **no** | `overrunStopSession` (overrun_guard_stop.go:949) builds a `SessionManager` with no hooks, so it kills straight through tmux; also labels beads and writes pause state |
| checkpoint dog `triggerCheckpointDog` (checkpoint_dog.go:47) | no | no | `git commit` in polecat worktrees |
| wisp reaper `triggerWispReaper` (wisp_reaper.go:140) | no | no | closes wisps through bd |
| jsonl git backup `triggerJsonlGitBackup` (jsonl_git_backup.go:151) | no | no | git commit and push to the offsite repo |
| doctor dog and test-pollution sweep (doctor_dog.go:232, pollution.go:34) | no | no | drops orphan test databases, may restart Dolt |
| compactor dog (compactor_dog.go:108) | no | no | reads commit counts, escalates |
| scheduled maintenance `runScheduledMaintenance` (scheduled_maintenance.go:143) | no | no | `dolt_gc('--full')`; the drain waits on it but nothing stops a new one |
| disk maintenance `runDiskMaintenance` (disk_maintenance.go:310) | no | no | prunes in apply mode |
| hm sync `runHMSync` (hm_sync.go:134) | no | no | writes the history index |
| overseer report (overseer_report.go:95) | no | no | writes a digest file |
| landing infra probe (landing_infra.go:312) | no | no | probes; may escalate |
| dolt health ticker (daemon.go:1770) | no | no | restarts Dolt; intended |
| resource sampler, burst sampler, census log | no | no | append metrics rows |
| upgrade restart and heal (upgrade_restart.go:435, upgrade_heal.go) | no | yes (heartbeat returns before it at :1947; heal script checks the sentinel at :117) | restarts the daemon |

## Command paths

| Path | Hold | ESTOP | Note |
|---|---|---|---|
| `gt sling` (cmd/sling_run.go:258, `sling.Blocked` sling/run.go:683, `polecat_spawn.go:973`) | no, by design (hold.go:35) | yes, town and rig | the explicit operator sling is refused under ESTOP only |
| `gt done` (cmd/done.go) | no | no | no brake reference in done*.go; pushes the branch and submits. A working polecat can finish during a drain, which the drain wants. |
| `gt power standby` (power/states.go:197) | writes it | writes it | `setHold` then `drain` then `setEstop` (power.go:447, :484) |
| `gt mail check --inject` (cmd/mail_check.go:80) | no | prints a banner | advisory only |

## Count

The tables group jobs by family, so a row can stand for several jobs (the 14 jev passes are one row) and the rows do not add up to the job count. The counts below are the research agent's tally of 53 jobs; the sum is checked (28 + 3 + 21 + 1 = 53) but the 28/21 split was not recounted from the tables.

- 53 jobs: 34 heartbeat steps and 19 tickers or goroutines. Three command paths are listed separately.
- 28 read neither brake.
- 3 read the hold and the ESTOP: spec dispatch, scheduled slings, the heartbeat dispatch step.
- 21 read the ESTOP only (lifecycle flag): the remaining lifecycle steps and upgrade restart.
- 1 is brake-partial: patrol scan, through the supervisor.

## Candidates to stop under standby

1. **Landing worker** (landing_worker.go:297). Keep running during a drain, since the drain waits for landings. After the ESTOP it still merges. Decide whether standby should gate new passes after the drain, not mid-pass.
2. **Tier sweep** (tier_sweep.go:554, daemon.go:2143). Gate on hold or ESTOP. It creates beads and runs gate containers with nobody to read them.
3. **Overrun guard stop** (overrun_guard_stop.go:949). Route through `d.sup().Stop` or set the session hooks, so the ESTOP refuses it. Today it kills sessions under ESTOP.
4. **Scheduled maintenance gc** (scheduled_maintenance.go:143). Gate on the hold, so a gc cannot start after `drain` has seen none running.
5. **Lifecycle steps during the drain**: rebuild-gt, rebuild-om and the jev passes read only the ESTOP. A hold check on the lifecycle flag in `heartbeatWork` (daemon.go:2265) would park all 22 at once, but it must exempt polecat-health and idle-reap, which the drain relies on.
6. **Patrol scan non-restart writes**: orphan molecule closes, gate checks and escalations. Reasonable to pause under ESTOP.
7. **Checkpoint dog, wisp reaper, jsonl git backup, git-hygiene, branch-prune**: low risk disk and git writes. Stop only if standby must be quiet on disk.

Keep running by design: Dolt supervision, log rotation, townhealth and attention (the dashboard needs them), resource samplers.

## Limits

- Re-read against origin/main by the map's charter: `dispatch/hold.go:43`, `landing_worker.go:297`, `tier_sweep.go:1791` and `overrun_guard_stop.go:949`. The other citations are as the research agent reported them.
- I did not read the claude-cl7.15 bead. I confirmed both claims from code only.
- I did not read the plugin scripts that the plugins step runs, so their writes are unknown.
- I did not find an image build job in `internal/daemon`. If one exists it lives in install tooling, not the daemon.
