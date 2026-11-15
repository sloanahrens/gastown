> Status: proposed (2026-11). Approved by the operator in a grilling on 2026-11-15; nothing here is implemented yet; the plan is [2026-11-15-agent-bead-retirement-plan.md](2026-11-15-agent-bead-retirement-plan.md).

# Retiring the agent identity beads

The daemon keeps one bead per seat name (`<prefix>-<rig>-polecat-<name>`,
`<prefix>-<rig>-crew-<name>`), typed `task` with the label `gt:agent`. It carries
`role_type`, `rig`, `agent_state`, `hook_bead`, `cleanup_status`, `active_mr`,
`notification_level` and the exit facts `gt done` writes. ADR 0003 decided that a
seat's intended state lives in one file per seat, "never in the store", and that
agent beads are mirrors of it. This design finishes that decision: the facts the
readers still take from the agent bead move to the intent record, each reader is
moved over with proof that its decision does not change, the writers are stopped,
and the records are deleted once.

## Why

Two sources of truth for "what is this seat doing" have caused a run of incidents,
each a reader trusting a proxy that had drifted: a done seat restarted forever
(gt-bd68); a live seat restarted because `hook_bead` named a closed bead (deep
review G1-03); a dead seat counted against the pool cap (gt-bc9b6, gt-tldj4); a
nuke or a failed start leaving the work bead hooked to a dead seat (gt-in9l9,
gt-pzq5e); a missing `cleanup_status` blocking cleanup rig-wide (gt-624w); a
reaper that closed 102 durable beads, every agent bead among them (gt-2qzr).
The records also show up in `bd list` and `bd ready` as open work, because they
are `task` beads, which has misled backlog counts.

## What is true today (read from origin/main at c604db7e)

- **The intent record is a file, not a store row.** `internal/intent` holds
  `desired` (run, stop, park, submitted), `work_bead`, the agent, `frozen`,
  restarts, liveness progress, `sandbox_owed` and the pause fields. Its package
  doc says agent beads are "display mirrors, written after it, never read for a
  decision".
- **The code does not match that sentence.** `patrolscan.AgentRecord` reads the
  agent bead's state, exit type, cleanup status, hook and last source issue to
  refuse a restart (`internal/daemon/patrol_scan.go`, the host method
  `AgentRecord`). `gt done` writes those facts to the agent bead. Other decision
  readers: the pool's dead-hooked count (`internal/cmd/pool_seats.go`), the seat
  cap (`internal/daemon/patrol_scan_seatcap.go`), the sandbox seat end
  (`internal/daemon/sandbox_seat_end.go`), the polecat manager
  (`internal/polecat`), the reaper's active-MR protection, and the kept-seat
  attention rule (gt-jw437c).
- **The intent record lacks the finished-turn facts.** It has no exit type, no
  cleanup status, no turn state (done, stuck, awaiting-gate) and no last work
  bead. `work_bead` and `submitted` it already has.
- **One choke point covers most readers.** `AgentRecord` on the patrol host feeds
  the scan, the daemon's crash path, the overrun guard's escalation check and the
  kept-seat rule. Moving it moves about six readers at once.
- **Call sites.** Outside `internal/beads`, non-test: `internal/cmd` about 38
  (polecat, identity reconcile, pool seats, sling helpers, `gt done`, agent
  pause, mail directory), `internal/polecat` 11, `internal/daemon` 5, and a few in
  `done`, `doctor`, `mail` and `townstatus`.
- **Live data, 2026-11-15.** 34 polecat records (29 done, 4 nuked, 1 spawning) and
  12 crew records (idle). Names are reused within minutes, so the reuse path
  (`CreateOrReopenAgentBead`, `ResetAgentBeadForReuse`) is live.

What the first plan task must still establish, field by field: which readers use
`notification_level`, `role_type` and `active_mr`, and whether any reader uses a
field this document does not name. Nothing below depends on those answers except
the field table.

## Decisions (the operator, 2026-11-15)

1. **Extend the intent record.** The finished-turn facts go into the intent
   record: turn state, exit type, cleanup status, last work bead. It is a file, it
   works when Dolt is down, and it keeps one writer per fact. `gt done` writes it
   first; the agent bead stays a mirror until its writers are stopped.
2. **Shadow-read the decision readers.** For each reader that feeds a liveness,
   restart or capacity decision, read both sources and log any disagreement for a
   window of at least two days before the reader is cut over. Display-only readers
   are cut directly.
3. **Hide the records from work lists in gastown's own tools now.** A default
   exclusion in the beads fork is a later, cross-rig change; the records will be
   deleted before it matters.
4. **Delete the rows once, at the end.** After the writers have stopped, the
   finished records are removed with `gt polecat identity reconcile --delete-only`.
   Deleting earlier would race the name-reuse path.
5. **Polecats first, crew last,** in the same plan.

## Target state

A seat's state is the intent record plus the work bead's assignee and status.
Nothing reads or writes an agent bead. `role_type` and `rig` come from the seat
name. The records, `CreateOrReopenAgentBead`, `ResetAgentBeadForReuse`, the
agent-bead listing helpers and the doctor checks that audit them are gone. The
no-bead-access polecat design is unaffected: the host-side stop hook that reads
the agent bead moves to the intent record like any other reader.

## Field map

| Field | Replacement | Verified |
|---|---|---|
| `hook_bead` | intent `work_bead` | yes, it exists and `gt sling` sets it |
| `agent_state` paused / submitted / parked | intent `desired`, `frozen` | yes |
| `agent_state` done / stuck / awaiting-gate | new intent `turn_state` | to build |
| exit type | new intent `exit_type` | to build |
| `cleanup_status` | new intent `cleanup_status` | to build |
| `last_source_issue` | new intent `last_work_bead` | to build |
| `active_mr` | to decide in task P2.4 (the reaper protects MR wisps by it) | open |
| `notification_level`, `role_type`, `rig` | none: display or derivable from the seat name | to confirm in P1.1 |

## Safety

- **Additive first.** Wave 1 adds fields and writes them beside the agent bead; no
  reader changes. A rollback at this point is deleting the new fields.
- **No cut without a measured window.** Wave 2 is gated by the shadow log: the
  overseer reads the disagreements and flips each reader only when every one is
  explained. A flip is one small change that reads the intent record and falls
  back to the bead only when the intent record has no finished-turn facts, so a
  seat that predates the change still reads right.
- **Fail closed stays.** Every intent reader already fails closed (an unreadable
  record reads as held); the new readers keep that rule, and an unreadable hook is
  never read as a re-sling (gt-jw437c).
- **Rows last.** Stopping the writers (wave 3) and deleting the rows (wave 5) are
  separate steps; the delete is an attended step on live data, with a dry run
  first.

## Out of scope

The tracker program, the no-bead-access polecat plan, the beads fork's default
filters, and any change to what gt done decides. This plan moves where the facts
live, not what is decided from them.
