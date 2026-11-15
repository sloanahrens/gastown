> Status: proposed (2026-11). Approved by the operator in a grilling on 2026-11-15; nothing here is implemented yet; the beads filed from the plan track it (gt-whj9al).

# Rolling Dolt squash v2: no operator, no drain, no hold

Approved by the operator in a grilling of 2026-11-15 (Q1 to Q8) and
measured on scratch databases the same day. It replaces the "First manual run" of
[the 2026-11-01 design](../design/dolt-rolling-squash.md), whose preconditions
and ceremony it removes. Nothing here is implemented; the plan is
[2026-11-15-dolt-squash-plan.md](2026-11-15-dolt-squash-plan.md).

## Why it changes

The 11-01 design squashes the `gt` database once, by hand, with the operator at
the swap, the town drained, a dispatch hold set, writers paused, a backup under
six hours old, the history archive at zero commits behind for every database, on
a morning after the 03:00 window and on a day with no tracker flip. Measured on
2026-11-15, most of that is ceremony, and one step does not work:

- **Step 5 squashes nothing.** Inside a rebase session `dolt_log` shows only the
  base, so `commit_hash IN (SELECT commit_hash FROM dolt_log WHERE date <= cut)`
  selects no row and every commit stays a `pick`. Reading the list from
  `dolt_log('main')` works: 12,002 commits became 6,002 with identical table hashes.
- **"Zero behind" cannot be met by day.** Behind counts everything written since
  the last pass (`gt-p2yaf6` says so). The squash discards only commits older
  than seven days, so only those need to be archived.
- **Maintenance starves.** The scheduled maintenance defers backup and archive
  while the daemon has a landing pass in flight, and about 300 landings a day
  leave almost no quiet moment (`gt-y6ovz`, `gt-2m8sj`, `gt-mdejo` fixed the same
  shape for the catch-up backup and the gc; it recurred on 2026-11-14 and the
  archive has not run since 2026-11-13).
- **Presence adds nothing.** The swap is two renames; what proves it right is the
  per-table hash comparison, which a machine does exactly.

## What stays and what goes

| Barrier of the 11-01 design | Verdict | Replacement |
|---|---|---|
| Operator present at the swap | removed | automatic verification, a retained `main-pre-squash` for 48 hours, a second verification before it is dropped |
| Dispatch hold and a drained town | removed | catch-up replay and a swap at a quiet tip (below) |
| Writers paused | removed | no server-side pause exists (a global `read_only` does not stop a root writer, measured); the replay makes one unnecessary |
| Archive at 0 behind for every database | replaced | the archive reaches the cut date for the database being squashed |
| Fresh backup under six hours | replaced | the last nightly backup finished within 24 hours (the restore path) and `main-pre-squash` is the first restore path |
| A morning after the 03:00 window, a day with no tracker flip, after the bd container is the only bd path | removed | none needed; a tracker flip only needs to not run in the same minutes, which the state file's run marker enforces |
| Server must be local for the gc | removed | the gc and the squash are SQL (`CALL dolt_gc(...)`, `DOLT_REBASE`), so the pod runs them over its connection; the size trigger that read the wrong disk is dropped |
| `gt dolt archive-now` needs the hold | removed | the archive only reads the Dolt log and writes files; it stays bounded |
| Backup and archive deferred behind landing work | removed | only the gc and the squash wait, and only on hard holds |
| Data safety: a verified copy of the old history exists until the new one is verified | **kept** | `main-pre-squash` and the nightly backup |
| Never rebase `main` in place; work on a branch and swap | **kept** | `compact-work` |
| One run at a time; a dead run's branches are not deleted blindly | **kept** | run marker and the dead-run check |

## Trigger (Q1)

The squash is not scheduled for a chosen day. A daily check, outside the
landing-pass gate, reads four numbers per enabled database and does nothing
until one trips:

- the last nightly backup's size over 5 GiB,
- its duration over 5 minutes,
- the daemon's `bd`-style query p95 over 200 ms,
- the commit count (a proxy for the first when the size cannot be read).

All thresholds and `keep_days` (7) are settings. Measured on 2026-11-15 none is
near: `gt` is 1.6 GB, a backup takes 7 seconds, latency is 4 ms, growth is 0.1 to
0.15 GB a day.

## The run

Per database, `gt` first. The daemon runs it over its Dolt connection, one SQL
statement at a time from Go, so a statement's error is handled where it occurs
(a SQL batch stops at its first error, which the lab hit).

1. **Decide.** A trigger tripped, no run in the last 24 hours, no `compact-*` or
   `main-pre-squash` branch left by a dead run (a dead run is recovered by state,
   never deleted blindly), the run marker free, the nightly backup finished within
   24 hours.
2. **Archive to the cut.** The cut date is now minus `keep_days`. The archive pass
   for the database runs until it reaches that date. It is bounded and needs no hold.
3. **Record BEFORE.** The main tip, per versioned table the row count and
   `dolt_hashof_table`, the commit count.
4. **Branch and rebase.** `compact-base` at the initial commit, `compact-work` at the
   recorded tip `T`. `DOLT_REBASE('--interactive','compact-base')` on `compact-work`;
   mark `squash` every plan row after the first whose commit is in
   `dolt_log('main')` with date at or before the cut; `--continue`. Writers keep
   writing to `main` throughout (22.5 s for 12,002 commits in the lab, so about a
   minute for the real database).
5. **Catch up.** Commits that landed on `main` meanwhile are cherry-picked onto
   `compact-work` in passes bounded by recorded tips: pass `n` picks
   `P..tip_n` and sets `P = tip_n`. A pick that reports `nothing to commit`
   is benign; one that reports `uncommitted changes` is retried (the working set
   belongs to the branch, so a writer's in-flight insert blocks a pick). Passes
   repeat until the unpicked tail is empty at a tip that has held still for 300 ms;
   the town commits about once every 25 seconds, so such a tip is nearly always there.
6. **Verify.** On `compact-work`, per table the row count and hash equal `main`'s at
   the last picked tip; exactly one commit at or before the cut remains; the commits
   newer than the cut are present one for one. Any difference: drop the work
   branches, leave `main` untouched, raise an item.
7. **Swap.** `DOLT_BRANCH('-m','--force','main','main-pre-squash')`, then
   `DOLT_BRANCH('-m','--force','compact-work','main')`. Dolt refuses the first
   rename while another session is on `main` unless forced; forced, it took 0.1 s in
   the lab and a connected writer carried on to the new `main` with no error.
8. **Reconcile.** Any commit that reached the old `main` after the verified tip is the
   tail of `main-pre-squash`. Replay it onto the new `main` one commit at a time, with
   the same retry. At 25 commits a second (about 600 times the town's rate) the lab lost
   11 of 626 commits without this step and none with it. A replay that conflicts
   (the same row changed on both sides in that instant) renames back only if no
   commit has landed on the new `main` since; otherwise it keeps the new `main`,
   keeps `main-pre-squash`, and raises a high item naming the commits.
9. **Retain.** `main-pre-squash` stays 48 hours. Then a second verification: for the
   twenty newest commits present on both, the table hashes `AS OF` each are equal.
   Only then drop `main-pre-squash` and `compact-base`.
10. **Reclaim.** The existing gc step (hard holds only) runs `dolt_gc('--full')`;
    the squash frees nothing until it does (370 MB to 101 MB in 2.3 s in the lab).

Recorded per run: a ledger line (commits before and after, durations of rebase,
catch-up and swap, tail length, retries) and `maintenance_state.json`.

## Recovery by state

| State | Recovery |
|---|---|
| Before the swap (steps 3 to 6) | drop `compact-work` and `compact-base`; `main` is untouched |
| After the first rename, before the second | rename `main-pre-squash` back to `main` |
| After the swap, no commit yet on the new `main` | rename the two back; nothing is lost |
| After the swap with commits on the new `main` | keep the new `main`; the old history is `main-pre-squash` until the retention check passes; restore from the nightly backup only on a failed second verification |
| A dead run found at the next check | recover by the branch state above, then raise a high item |

## Rollout (Q7)

Off by default. It is enabled for `gt` only after it has run on a restored copy of
the last nightly backup in a scratch container (the restore path in
`docs/dolt-restore.md`), with table hashes equal and the timings and sizes written on
its bead. The small databases wait for their own trigger.

## Starvation fix, separately and first (Q5)

Backup and archive only read. They stop waiting behind landing passes; only the gc and
the squash, which write, wait, and only on the hard holds. This is its own bead and
needs nothing else.

## Out of scope

Squashing other databases before they trip a trigger; an unattended gc of another
kind; the bd container program (`gt-2zigh5`), which does not gate any of this.

## Open

Whether `bd` clients behave across the rename exactly as the lab writer did (a
persistent session continued on the new `main` by name) is the one thing the lab
cannot show; the rehearsal on the restored copy runs the real `bd` against it.
