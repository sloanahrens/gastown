---
status: accepted
date: 2026-09-30
---

# The witness and deacon patrols become one Go tick

ADR 0003 decided that an unattended town runs no long-running LLM session. The witness LLM was
the only path that restarted a crashed polecat: the daemon's crash check and the stuck-agent dog
both only mailed it. The deacon LLM ran 28 formula steps every few minutes, most of which a daemon
job already did. Both sessions spent tokens while idle, and their judgement calls were the source
of the restart loops, re-slings and mail storms the backlog audit catalogued.

We decided that the daemon runs a `patrol_scan` tick (`internal/patrolscan`, host in
`internal/daemon/patrol_scan.go`). Per rig, every two minutes, it does four things:

- **Restart.** A polecat whose session the liveness function has found Dead on two consecutive
  samples, while it holds hooked or in_progress work, is restarted through the supervisor
  (`gt session restart --force`, worktree preserved). Nothing is restarted when the intent record
  is parked, frozen or `submitted`; the work carries `gt:ready-to-land` or a dispatch hold; the
  work was hooked within the spawn grace; the agent bead says `stuck`, `awaiting-gate`, `paused`,
  `done` or `nuked`; or the heartbeat says `stuck` or a fresh `exiting`. The supervisor still
  enforces e-stop, shutdown and the 3-per-hour budget.
- **Idle seats.** A polecat Dead on two consecutive samples which holds no hooked or in_progress
  work has its intent record retired to `stop` (gt-613vw): the town needs no session for a seat
  with nothing to run, and a record left at `desired=run` was what made townhealth report the
  seat dead forever. A hold and a `submitted` record are left alone, and the seat's next dispatch
  goes through the supervisor's Respawn, which sets the record back to `run`.
- **Orphaned molecules.** For hooked work whose polecat has neither a session nor a directory,
  the bonded `mol-polecat-work` root and its step wisps are force-closed, read with
  `bd show --children` so ephemeral steps are seen (gt-22hdp.36).
- **Stranded work.** The same beads get one comment per window (default 24h), naming the branch
  that carries unlanded work or saying none does. The tick never re-slings, resets, unassigns or
  passes `--force`.

Town-wide it also resolves elapsed timer gates in the town and scanned rigs' databases, and once
an hour walks agent worktrees for a rogue executable `bd`, which it neutralizes and escalates.

Every read that fails is Unknown and nothing acts on Unknown. A failed `bd list` is not "no
work", a failed `bd show` is not "bead gone", and an unreadable intent record is a hold. The
agent bead is read only to refuse a restart, never to cause one. The tick never reads, drains or
sends mail; the operator hears from it through `gt escalate` (budget exhaustion, rogue `bd`).

## What was dropped and why

Anything that needed judgement is gone, not ported. Stalled seats are logged, not nudged or
restarted, until the stall threshold is measured (ADR 0003 item 7). Dirty-worktree recovery is
dropped because it meant an LLM authoring commits in another agent's worktree. The restart keeps
the worktree, and the stranded comment names the surviving branch. Resetting an orphaned bead
for re-dispatch is dropped: git cannot tell work that landed under another bead from work never
started, so the bead is reported, not released. HELP triage belongs to the operator inbox.

### Witness, `mol-witness-patrol` (10 steps)

| # | Step | Disposition |
|---|---|---|
| 1 | inbox-check | Dropped. Mail hygiene is self-maintenance; HELP routing is judgement; MERGED and SWARM_START have no live sender. |
| 2 | process-cleanups | Dropped (judgement). Restarts preserve worktrees; `checkpoint_dog` commits WIP. |
| 3 | check-refinery | Dropped. The refinery is gone (ADR 0004); mayor and deacon liveness are daemon ensure paths. |
| 4 | survey-workers | **Ported**: dead-seat restart, orphaned molecules, stranded report. Nudges, completion routing (the landing worker owns it) and bead reset dropped. |
| 5 | state-collapse | Dropped. Its MR half reads merge-request wisps, which ADR 0004 deleted; a live stranded bead is the tick's report, a closed one has a landing record. |
| 6 | check-timer-gates | **Ported** (timer gates). |
| 7 | check-swarm-completion | Dropped: nothing sends SWARM_START. |
| 8-10 | patrol-cleanup, context-check, loop-or-exit | Dropped: they exist only because the session exists. |

### Deacon, `mol-deacon-patrol` (28 steps)

| # | Step | Disposition |
|---|---|---|
| 1 | heartbeat | Dropped (self). |
| 2 | ack-probes | Dropped (self). The doctor dog's deacon self-probe now runs only while the deacon patrol is enabled. |
| 3 | inbox-check | Wisp gc: existing `wisp_reaper`. RECOVERED_BEAD re-dispatch: the landing worker re-dispatches rejections itself (ADR 0004). HELP: operator inbox, dropped as judgement. |
| 4 | orphan-process-cleanup | Existing daemon job (`cleanupOrphanedProcesses`, every heartbeat). |
| 5 | test-pollution-cleanup | Dolt orphans and test dirs: existing `doctor_dog`. The residual (imposter kill, test tmux sockets, PID files, dog worktrees) is follow-up gt-4k3fj.6.2. |
| 6 | gate-evaluation | **Ported** (timer gates). GitHub gates were evaluated by no step before and none now; follow-up gt-4k3fj.6.2. |
| 7 | dispatch-gated-molecules | Dropped to follow-up gt-4k3fj.6.2: slinging is the spec dispatcher's, and usage was never measured. |
| 8 | check-convoy-completion | Existing ConvoyManager. |
| 9 | resolve-external-deps | Dropped: beads unblocks dependents itself. |
| 10 | fire-notifications | Dropped: `gt convoy check` already notifies. |
| 11 | heartbeat-mid | Dropped (self). |
| 12 | health-scan | Existing daemon ensure paths through the supervisor; the LLM's restart authority is gone. |
| 13 | dolt-health | Existing Dolt ensure and health ticker, `doctor_dog`, `compactor_dog`, `dolt_backup`. |
| 14 | zombie-scan | Dropped (report-only). Polecat death is the tick's; strays are the supervisor's `KillStray`. |
| 15 | plugin-run | Existing plugin dispatch; on-request runs are an operator action. |
| 16 | dog-pool-maintenance | Existing (`gt sling` grows the pool, `reapIdleDogs` shrinks it). |
| 17 | dog-health-check | Existing (`cleanupStuckDogs`, `detectStaleWorkingDogs`). |
| 18 | orphan-check | **Ported**: the tick's orphan pass is now the single owner. |
| 19 | rogue-bd-check | **Ported** (hourly). |
| 20 | session-gc | Existing (`killDefaultPrefixGhosts`, `cleanupOrphanedProcesses`); the `gt doctor --fix` residual is dropped. |
| 21 | wisp-compact | Existing `wisp_reaper` deletion; stuck-wisp promotion dropped (nothing reads it). |
| 22 | compact-report | Dropped (report). |
| 23 | costs-digest | Dropped (already disabled). |
| 24 | patrol-digest | Dropped: no patrol molecules remain to digest. |
| 25 | log-maintenance | Existing (`rotateOversizedLogs`). |
| 26-28 | patrol-cleanup, context-check, loop-or-exit | Dropped (self). |

## How to enable it, and how to turn the LLM patrols off

Install a binary that carries this change first. `mayor/daemon.json` is decoded strictly, so an
older daemon refuses the new keys. Then hand one rig at a time to the tick:

```json
"patrols": {
  "patrol_scan": {"enabled": true, "interval": "2m", "rigs": ["gastown"]},
  "witness": {"enabled": true, "disabled_rigs": ["gastown"]}
}
```

A rig in `witness.disabled_rigs` gets no witness: the daemon kills a leftover session through the
supervisor, `gt up` skips it, and the patrol watchdog stops checking it. In a rig the tick covers,
the daemon's crash check stops mailing CRASHED_POLECAT. Optional keys are `dead_samples`
(default 2) and `report_window` (default `24h`). Tickers are built at daemon start, so restart the
daemon after editing. The tick logs one `patrol_scan:` summary line per rig per tick; its
supervisor actions carry actor `daemon/patrol-scan` in `.runtime/supervisor/actions.jsonl`.

When every rig is covered, set `"witness": {"enabled": false}` and `"deacon": {"enabled": false}`.
The daemon kills leftover sessions and neither `gt up` nor the ensure paths start them. Before
turning the deacon off, check that `doctor_dog`, `wisp_reaper`, `compactor_dog` and `dolt_backup`
are enabled, since several dispositions above lean on them. Turning the deacon off also stops
Boot, so `gt warrant` filings are no longer executed; the supervisor's `Kill` is the replacement.

Rollback is deleting the keys. Deleting `internal/witness`, `internal/deacon`, their formulas and
roles is gt-4k3fj.6.1, after the tick has run for a day.

Decision record: epic gt-4k3fj, bead gt-4k3fj.6; step enumeration in
`~/.claude/docs/research/deep-review/patrol-steps-vs-daemon.md` (claude-1ey.13).
