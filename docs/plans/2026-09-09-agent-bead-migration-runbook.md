# Agent-bead migration — Task 6 runbook (2026-09-09)

Epic gt-a6g · Task bead gt-7vy · Spec `2026-09-09-agent-bead-migration-design.md` · Plan `2026-09-09-agent-bead-migration-plan.md`

## Preconditions met
- T1 `bb9c373`, T3 `5fcba19`, T4 `f0a0258`, T5 `19deb3b` on main; T2 landed inside T3.
- Follow-up gt-1361 `162fb8d` (routing-safe verify/delete, `--delete-only`, nuked-incarnation auto delete-only, `--id`).
- Binary installed via the self-serving rebuild chain: `31d5af2 -> 162fb8d`.
- `gt doctor agent-beads-shadow` before: gastown 24, om 7, beads 2.

## Runs (one ID per command, dry-run reviewed before every `--apply`)

| Set | IDs | Mode | Result |
|---|---|---|---|
| gastown, rig row newer | amber basalt flint garnet granite jasper marble obsidian onyx opal pearl quartz ruby shale topaz | `--apply` | 15 hq rows archived+deleted; garnet and shale ghost `active_mr` cleared |
| gastown, nuked legacy incarnation | agate malachite mica pyrite slate | auto delete-only | 5 deleted (fields ignored) |
| gastown, stuck respawn | jade | force-nuked first (clean, 0 unmerged, stash dropped, gt-911 merged); hq row then `--apply` (rig wins) | 1 deleted |
| gastown roles | gt-gastown-witness/refinery/crew-sloan | `--id --apply` (identical) | 3 deleted |
| om, rig newer | jasper obsidian onyx | `--apply` | 3 deleted |
| om, stale idle incarnation | opal quartz topaz | `--delete-only` (severity would have injected unknown) | 3 deleted |
| om/beads roles | om-crew-sloan be-beads-crew-sloan be-beads-refinery | `--id --apply` | 3 deleted |

Archive: `~/gt/.beads/archive/agent-bead-legacy.jsonl` — 34 lines (33 unique hq rows + one duplicate line from the incident below).

## Incident during the first pass (before gt-1361)
The shipped tool's "pinned" town wrapper was not pinned at the `bd` level: once an hq row was deleted, `bd -C ~/gt show/delete` routed the ID to the rig row. Every first apply deleted its hq row correctly but false-failed step 4; a re-run on garnet read the rig row as "town" and deleted it (issues row + `gt:agent` label, two Dolt commits). Restored exactly with `CALL DOLT_REVERT` on both commits; garnet verified `done/clean/SAFE_TO_NUKE`. gt-1361 fixed the tool; its safety probe (`reconcile gastown/garnet` with no hq row) now refuses.

## Live acceptance
- `bd show gt-gastown-polecat-<x>` from `~/gt` and from the rig return the same `updated_at` (garnet, shale, amber, agate, mica checked).
- `gt polecat list gastown`: garnet and shale reusable (leaked slots recovered); pool 27 reusable before the afternoon wave.
- `gt doctor agent-beads-shadow`: 0 in gastown, om, beads.
- hq holds zero rig-prefixed agent rows.

## Deferred (own beads)
- done->idle promotion design: gt-glfh. Reclaim of missing-worktree slot: gt-2h6. Formula sync flags: gt-7qbj.
