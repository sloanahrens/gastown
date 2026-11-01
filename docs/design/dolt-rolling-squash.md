# Rolling Dolt history squash

Status: design and first-run runbook (gt-2conu). Nothing here is implemented. The operator approved, on 2026-11-01, a guarded daemon step that squashes Dolt history older than 7 days and keeps the last 7 days of commits, nightly after the backup and before the gc, off by default. The first run is manual, with the operator present, and is the 2 GB flatten (docs/dolt-history-offline.md, "When to flatten"). The method is the surgical rebase of docs/design/dolt-storage.md ("Surgical compaction"): a work branch and a swap, never a rebase of main in place.

Related: gt-9fcui (14 nightly backups, landed), gt-hk21d (events retention, landed). With events kept 30 days, `bd history <id> --events` reaches back 30 days; older changes survive only in the nightly backups and the JSONL backup repository's git history.

## Facts (measured 2026-11-01)
gt database: 24,182 commits in dolt_log, 21,951 since 10-26 (3.4k a day), about 41 KB of store per commit (estimate), 1.69 GiB on disk by du, post-gc 1.149 GB with oldgen 603 MB, growth +0.10 to 0.15 GiB a day. The history is the issues, comments and labels rows; events and wisps are dolt_ignore'd. Backups: nightly in the 03:00 window, 7 kept (14 after gt-9fcui). A rebase replays the commits in its plan; the cost of replaying about 24k commits is NOT measured and the first manual run measures it.

## Steps (daemon step, per enabled database; first target gt only)
0. Switch: `squash` block under the scheduled-maintenance config, `enabled` false by default, `databases` list, `keep_days` 7, `min_old_commits` (do nothing below it), `max_runtime`. Off means the step is not even constructed.
1. Preconditions, all or skip with a logged reason: inside the window; no gate slot held and no landing pass, sling cycle or install in flight (the gc's own conditions); the daemon's Dolt maintenance mutex acquired; no pause marker held by someone else; THIS NIGHT's backup of the database completed with a manifest naming it (doltbackup.LastFor), finished within the last 6 hours; free disk at least 2 x the database size; commits older than keep_days exceed min_old_commits.
2. Park writers: write the pause marker (internal/doltpause, reason "history squash", until = max_runtime + slack) and wait until the processlist shows no non-daemon connection writing (bounded wait; skip if it does not clear).
3. Record BEFORE: the main tip hash; for every versioned table the row count (`SELECT COUNT(*)`) and `dolt_hashof_table(t)`; the commit count; the cut commit C = newest commit with date < now - keep_days.
4. Branch: `compact-base` at the initial commit, `compact-work` from main. Any leftover compact-* branch from a dead run aborts the step with an escalation (do not delete another run's branches blindly).
5. `CALL DOLT_REBASE('--interactive','compact-base')` on compact-work; `UPDATE dolt_rebase SET action='squash' WHERE rebase_order > (SELECT MIN(rebase_order) FROM dolt_rebase) AND commit_hash IN (SELECT commit_hash FROM dolt_log WHERE date < <cut time>)`; `CALL DOLT_REBASE('--continue')`.
6. Verify on compact-work: every table's row count AND dolt_hashof_table equal the BEFORE values; the main tip is still the BEFORE tip (no writer moved it); commits in the last keep_days survive one for one (count of commits newer than C unchanged); exactly one commit older than C.
7. Swap: checkout compact-work; `DOLT_BRANCH('-D','main')`; `DOLT_BRANCH('-m','compact-work','main')`; `DOLT_BRANCH('-D','compact-base')`; `DOLT_CHECKOUT('main')`. Re-run step 6's counts and hashes on main.
8. Unpark: remove the pause marker. Log one summary line: commits before and after, duration, tables verified. Write the result to daemon/maintenance_state.json.
9. The existing gc step then runs (`dolt_gc('--full')`): the squash itself frees nothing until the gc reclaims the old chunks. Record the size before and after the gc.

## Abort conditions (each: drop compact-work and compact-base, remove the marker, escalate high, leave main untouched)
a precondition fails after step 2 began; the BEFORE and AFTER row count or table hash of any table differs; the main tip moved during the run; DOLT_REBASE reports a conflict or any error; the run passes max_runtime; the free disk drops under 1 x the database size; the process dies (the marker lapses on its own; the next start finds compact-* branches and escalates, step 4). After the swap (step 7) there is no abort: a failed post-swap check restores from the nightly backup by hand (docs/dolt-restore.md) and escalates critical.

## Test plan
1. Unit, fake SQL executor and injected clock: each precondition skips with its reason; each abort condition runs the cleanup and the escalation exactly once; the sequence of statements is exactly steps 3 to 7; a leftover compact-* branch aborts at step 4; the switch off constructs nothing.
2. Integration, throwaway Dolt container under the gate slot (like the beads recorder tests), narrow `-run`: create 300 commits with `--date` spread over 14 days over three tables; run the step; assert one old commit plus the commits of the last 7 days, identical table hashes, no compact-* branches, main exists; then a second run does nothing (below min_old_commits).
3. Failure integration: a writer commit injected between steps 3 and 6 aborts with main untouched; a table hash forced to differ aborts before the swap.
4. Mutation proofs: drop the hash comparison, drop the pause marker, rebase main directly instead of the work branch, skip the backup precondition; each turns a named test red.
5. Measured in the first manual run, not assumed: rebase time for about 24k commits, peak disk use, whether bd clients honor the pause marker or fail while main is absent during the swap.

## First manual run (2026-11-02 morning, after L3's first checks), operator present
Owner of each step in brackets.
R0 [operator, overseer] Pick the time (a window with the fewest seats busy). Measure and write down: `du -sh ~/gt/.dolt-data/gt`, `du -sh ~/gt/.dolt-data/gt/.dolt/noms/oldgen`, `SELECT COUNT(*) FROM dolt_log`.
R1 [overseer] Backup: use the 03:00 nightly backup if it finished within 12 hours and names gt (`~/gt-backups/dolt/<date>/backup.json`); otherwise run the same `dolt backup sync-url` the daemon runs (internal/daemon/maintenance_backup.go maintenanceBackupOne) into a dated directory outside the rotation. Check the restore page (docs/dolt-restore.md) lists the restore steps for this database.
R2 [operator] Dispatch hold: the operator sets `~/gt/seat-refill.hold` (agents never create it); the overseer stops landings and slings, waits for the seats to drain, and tells the birds to stop writing. `gt dolt pause --reason "history squash" --until 90m`. Confirm with the processlist that only the daemon is connected.
R3 [overseer] BEFORE record: for gt, per versioned table `SELECT COUNT(*)` and `SELECT dolt_hashof_table('<t>')`, the main tip (`SELECT dolt_hashof('main')`), the commit count, and the time of the cut commit. Save them in ~/gt/.runtime/overseer/squash-before.txt.
R4 [overseer] The SQL of steps 4 to 5 in a dolt sql session against gt, keeping the session open between statements (the rebase state lives in the session): compact-base, compact-work, the interactive rebase, the UPDATE on dolt_rebase with the 7-day cut, the --continue. Time it. On any error: `CALL DOLT_REBASE('--abort')`, checkout main, drop both branches (docs/dolt-history-offline.md), unpause, stop here.
R5 [overseer] Verify as in step 6 against the R3 file. Any difference: abort as above. Only an exact match goes on.
R6 [operator present] Swap (step 7), then repeat the verification on main, then `bd list --limit 1` from a rig directory and `bd show` of two known beads.
R7 [overseer] `gt dolt unpause`; the operator removes the hold; the seats resume. Watch one dispatcher tick and one landing.
R8 [overseer] gc: either wait for the 03:00 window (the old-gen growth rule may not fire after a rewrite, so run it by hand) or, under a fresh pause marker, `CALL dolt_gc('--full')` once; record du and oldgen before and after. Expect the oldgen to drop towards the size of the last 7 days of history (the target to compare against: post-gc 1.149 GB today).
R9 [overseer] Record the measured numbers (rebase duration, disk peak, commits before and after, size before and after) in docs/design/dolt-rolling-squash.md and on this bead. Rollback if something is wrong after R6: stop Dolt per the town's rules, restore gt from the backup of R1 (docs/dolt-restore.md), and escalate.

## Backup plan for the first run

1. The restore path is docs/dolt-restore.md; read it before the run and confirm the nightly backup of the day holds the database: `cat ~/gt-backups/dolt/<date>/backup.json` lists `gt` and `finished` is under 12 hours old. Fourteen nights are kept (gt-9fcui), so the night before the run is still there after the run.
2. If the nightly backup is older than 12 hours, take one by the daemon's own method into a directory outside the rotation (so Rotate never removes it): `CALL dolt_backup('sync-url', 'file://<dir>/gt')` through the running server (internal/daemon/maintenance_backup.go, doltBackupSyncURL), under a pause marker (`gt dolt pause --reason "pre-squash backup" --until 30m`, then `gt dolt unpause`).
3. Check the copy is whole before going on: its `du` is within 10% of the database's backup size of the night before (about 1.2 GiB for gt on 2026-11-01), and `dolt_hashof_table` of `issues` on the source equals the recorded BEFORE value.
4. Keep the pre-run copy until the squash has run for a week.

## Why not today

Today (2026-11-01) the L3 window forbids Dolt maintenance, installs, restarts and slings. The run moves to the next morning after L3's first checks, with the operator present.
