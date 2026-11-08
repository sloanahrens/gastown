# Dolt history rewrites: offline procedures

Gas Town has no CLI for rewriting Dolt history. `gt dolt flatten`, `gt dolt
rebase`, `gt dolt rollback` and `gt dolt recover` were removed (gt-638go.6,
wayfinder D3/D7): each one rewrote or replaced shared state, none ran in the
three weeks before removal, and a command an agent can reach is a command an
agent will eventually run. These are operator procedures. Run them by hand,
with the town parked, after a backup.

Nothing rewrites history unattended. The routine flatten paths, the
compactor-dog plugin among them, were deleted with gt-8z769.3, and the daemon's
`scheduled_maintenance` patrol is now the town's one GC actor: a
history-preserving `CALL dolt_gc('--full')`, never a rewrite (see "The GC
actor" below).

## When to flatten: one trigger, 2 GB

Flatten only when a database directory under `~/gt/.dolt-data` passes
**2 GB** on disk. Nothing else is a reason: commit count is not (the
compactor_dog patrol advises on it to keep history-query latency visible on the
status line, and raises nothing, gt-3mu6q), and neither is slow growth that the
weekly GC handles. The 2026-09-29 measurement (gt-8z769) put gt at 775 MB, of
which 307 MB was old-gen history for 19,746 commits; a full GC reclaimed almost
nothing, because the size is history, not garbage.

Measure without touching the store (a read of sizes is safe; never write
inside `.dolt/`):

```bash
du -sh ~/gt/.dolt-data/*/                 # whole database
du -sh ~/gt/.dolt-data/*/.dolt/noms/oldgen # old generation, the growth
```

`daemon/maintenance_state.json` also records each database's size and
old-gen size after its last GC, and what that GC reclaimed.

A flatten discards the history `bd history` reads; see
[reference.md](reference.md), "Reading a bead's history".

## The GC actor

`scheduled_maintenance` (internal/daemon/scheduled_maintenance.go and
maintenance_gc.go) runs in the window set by `gt config set
maintenance.window HH:MM` (default 03:00, one hour long). Per database it
runs `CALL dolt_gc('--full')` when either holds:

- **weekly**: its last patrol GC is a week old, or none is recorded;
- **old-gen growth**: `.dolt/noms/oldgen` grew more than 20% since right
  after its last GC.

It runs only while no gate slot is held and the daemon has no landing pass,
scheduled slings cycle or install in flight; a working polecat does not defer
it. It defers to a pause someone else holds, and writes the
pause marker `~/gt/daemon/dolt.pause` before each GC call and removes it
after. A daemon that dies mid-GC leaves a marker that lapses on its own 15
minutes after it was written. Dolt's automatic GC stays on.

Never run a manual `dolt gc` or `CALL dolt_gc()` alongside it. If you need
one, pause first (`gt dolt pause --reason "manual gc" --until 30m`) and
`gt dolt unpause` after, so clients and the daemon know.

## Plan the flatten for the morning after a window

Plan a flatten for the morning after a maintenance window. That window's
archive pass archives what was committed before it and `gt dolt archive-now`
archives the rest, so the pair is what brings every database's archive to 0
behind before the flatten.

The window hour is the daemon's clock, not the reader's: read it from the
`scheduled_maintenance: in window HH:MM` line in `~/gt/daemon/daemon.log`,
which is stamped in the daemon container's own timezone. A container with no
timezone database runs the window in UTC, which was the case from the
2026-11-04 cutover until gt-gg9hv.34, so confirm the clock with `date` in the
daemon container (`docker exec <daemon container> date`) and with that log
line before choosing the morning. Wait until the window has logged
`cycle completed` — the line comes after that window's archive, backup and
gc — before parking the town.

## Rehearse on a copy first

Rehearse the flatten on a copy of the database before you run it on the town.
The 2026-11-06 rehearsal of gt, on a restore of the nightly backup, is the one
that found the three statements the flatten section now corrects; these steps
are the procedure it ran. Nothing here touches the live server on port 3307.

1. Copy the backup, then restore from the copy. Dolt writes a `LOCK` file into
   the backup it restores, so never mount the nightly backup itself. The
   restore uses the image's own `dolt`, so the rehearsal needs no host install:

   ```bash
   SCRATCH=$(mktemp -d -t dolt-rehearsal)
   mkdir -p "$SCRATCH/data"
   cp -a ~/gt-backups/dolt/<night>/gt "$SCRATCH/backup"
   docker run --rm --entrypoint sh \
     -v "$SCRATCH/backup:/backup/gt" -v "$SCRATCH/data:/data" -w /data \
     dolthub/dolt-sql-server:2.7.2 \
     -c 'dolt config --global --add user.name <name> && dolt config --global --add user.email <email> && dolt backup restore file:///backup/gt gt'
   ```

   Expect the restored `gt` to hold 33,010 commits and 30 tables (the 2026-11-06
   rehearsal's counts).

2. Serve the copy with the town's server config, in a container capped at 8 GiB
   with `DOLT_ROOT_HOST=%`. Copy `~/gt/.dolt-data/config.yaml` and mount the
   copy, so auto GC stays on and `archive_level` stays 0 as in the town. Publish
   a free port on 127.0.0.1, never 3307:

   ```bash
   cp ~/gt/.dolt-data/config.yaml "$SCRATCH/config.yaml"
   docker run -d --name gt-rehearsal --memory 8g -e DOLT_ROOT_HOST=% \
     -p 127.0.0.1:13306:3306 \
     -v "$SCRATCH/data:/var/lib/dolt" -v "$SCRATCH/config.yaml:/config.yaml" \
     dolthub/dolt-sql-server:2.7.2 --config /config.yaml
   ```

3. Count rows for every table, before the flatten, after it and after the full
   GC. The counts must be identical at all three points; a table that lost rows
   means the rehearsal failed. Run the queries inside the container:

   ```bash
   docker exec gt-rehearsal dolt --host 127.0.0.1 --port 3306 --no-tls \
     -u root -p "" --use-db gt sql -q "SELECT COUNT(*) FROM <table>"
   ```

4. Run the flatten SQL from the Flatten section below on the copy's `gt`, then
   `CALL dolt_gc('--full')`. Measure the size and the memory: read the database
   directory on the host and the container's cgroup `memory.peak` (cgroup v2,
   `/sys/fs/cgroup/memory.peak` inside the container). The 2026-11-06 rehearsal
   read a 1,664 MB database before, 1,668 MB after the flatten, 363 MB after the
   GC, and a `memory.peak` of 1.73 GiB over the whole run:

   ```bash
   du -sh "$SCRATCH/data/gt"
   docker exec gt-rehearsal cat /sys/fs/cgroup/memory.peak
   ```

5. Run `bd` only inside a container that mounts nothing but a scratch metadata
   file, and joins the copy's network namespace so `bd` reaches the copy's
   server. Step 2 publishes that server on 127.0.0.1 of the host, and a
   container on the default bridge cannot reach that loopback port, so do not
   point `bd` at the host's address. `--network container:gt-rehearsal` puts
   `bd` in the copy's namespace, where the server listens on 3306. Write the
   metadata file (modelled on `.beads/metadata.json`) with the fields `bd`
   reads to find a server-mode store:

   ```bash
   mkdir -p "$SCRATCH/beads"
   printf '%s\n' '{"backend": "dolt", "dolt_mode": "server", "dolt_database": "gt", "dolt_server_host": "127.0.0.1", "dolt_server_port": 3306}' > "$SCRATCH/beads/metadata.json"
   docker run --rm --network container:gt-rehearsal \
     -v "$SCRATCH/beads/metadata.json:/work/.beads/metadata.json:ro" -w /work \
     <image with bd> bd show <id>
   ```

   `<image with bd>` is any image that carries the `bd` binary, such as the
   daemon's image. A scratch metadata file on the host does not isolate a `bd`
   run: `bd` on the host still reads the live store. Print `bd show` for five
   beads before the flatten and after it, and compare the bytes; the
   rehearsal's five matched. Then read
   `bd history <id>`: the 2026-11-06 rehearsal went from 33,009 entries (3.7 MB
   of output) to one.

6. Remove the container and the copy when you have the numbers:

   ```bash
   docker rm -f gt-rehearsal
   ```

   Then remove the scratch directory you made with `mktemp`.

The rehearsal does not cover `gt dolt archive-now`, which needs the live town
(the dispatch hold, the daemon and the archive), and it does not cover
concurrent writes: nothing writes to the copy. The flatten's safety against
concurrent writes is step 1 of "Before any rewrite".

The live flatten of 2026-11-07 is a second measurement, run under this runbook
with the town held and drained and the archive brought to 0 behind. It took gt
from 35,300 commits to 2. The on-disk size was unchanged right after the
flatten and fell from 1.89 GB to 353 MiB only after the full GC, which ran under
the pause marker. All 30 tables kept their row counts, and 11 databases were 0
behind in the archive. That is one day's measurement, not a promise, and it
agrees with the rehearsal.

## Before any rewrite

Before step 1, check the archive level the server is configured with. The
daemon writes level 0, and the design doc records the gc peak measured at each
level (see [Dolt storage](design/dolt-storage.md), "Dolt GC"):

```bash
grep -n -B1 'archive_level' ~/gt/.dolt-data/config.yaml
```

The matching line must read `archive_level: 0`, and the line before it must
read `enable: true`. Anything else means stop. The overseer owns the restart
window that rewrites that file; neither the operator nor a bird edits it. Come
back to this runbook once the overseer's restart has run and the check passes.

1. Park the town, or at least every rig that writes to the database. A
   rebase fails on a concurrent commit, and a flatten is only safe against
   concurrent writes because Dolt merges the in-flight transaction.
2. Bring the archive up to date. The overseer creates the dispatch hold
   `~/gt/seat-refill.hold`; no bird and no operator session creates or lifts
   it. With the hold set and the town parked, run:

   ```bash
   gt dolt archive-now
   ```

   Stop if it exits 1, 2 or 3, and read the exit code as what happened to the
   pass: 1 is a failed pass: the daemon answered failed and printed why, so the
   archive is not caught up. Fix the cause that reason names before you run it
   again. 2 is still pending, so run `gt dolt archive-now` again.
   3 is a refusal, either for want of the dispatch hold or because an
   archive request is already pending. Then read `gt dolt status` again until
   every database's commit line and events line read `0 commits behind` and
   `0 events behind`.

   Check that every database's archive line in `gt dolt status` is the line
   you just read: the archive is the copy of `dolt_log` that outlives a
   flatten. Stop if the status prints any `ARCHIVE CHECK UNAVAILABLE` line: it
   could not check the archive, so it has checked nothing.
   Stop too when no database line shows an archive line, and when any
   database line says `archive state unreadable`.
3. Stop the server, then take the backup: copy the database directory while
   the server is stopped. Take the copy only after step 2's checks pass, so
   the copy and the archive describe the same moment. The nightly backups
   under `~/gt-backups/dolt` are the other copy, see `docs/dolt-restore.md`.

   Then start the server again with `gt dolt start`, and read `gt dolt status`
   until it shows the server running and lists every database. The flatten
   runs on the live server, so do not run it until that status shows them all.
4. Record row counts for every table so you can verify afterwards:

   ```sql
   USE `<db>`;
   SELECT table_name FROM information_schema.tables WHERE table_schema = '<db>';
   SELECT COUNT(*) FROM `<table>`;   -- once per table
   ```

## Flatten: squash all history into one commit

This is what `gt dolt flatten` did. It runs on the live server and needs no
downtime.

```sql
USE `<db>`;
SELECT COUNT(*) FROM dolt_log;                                   -- before
SET @root = (SELECT commit_hash FROM dolt_log ORDER BY date ASC LIMIT 1);
CALL DOLT_RESET('--soft', @root);   -- moves the parent pointer; data stays staged
CALL DOLT_COMMIT('-Am', 'flatten: squash history');
SELECT COUNT(*) FROM dolt_log;                                   -- after: 2, not 1
```

The flatten does not shrink the database. The commits go, but the space they
held stays on disk until a full GC. The rehearsal on a copy measured this (see
"Rehearse on a copy first", step 4, for the sizes). The space comes back only
at that next full GC. The full GC is the `scheduled_maintenance` window's
(weekly, or when old-gen grew more than 20%; see "The GC actor"). Run one by
hand only under the pause from "The GC actor": `gt dolt pause --reason "manual
gc" --until 30m`, then
`gt dolt unpause` after it.

After the flatten `dolt_log` holds 2 commits, not 1: the original root and the
flatten commit. `DOLT_RESET('--soft', @root)` keeps the root, so the count after
is 2 and is not a failure. The flatten commit is the one not yet archived, which
is why the status below reads behind by one.

After the flatten, `bd history <id>` shows one entry for every bead, authored
"Dolt System Account". The history before the flatten is gone from bd. It
survives in the archive that step 2 brought to 0 behind, and in the nightly
backup (see [docs/dolt-restore.md](dolt-restore.md)).

Each flattened database now reads `1 commits behind` in `gt dolt status`,
because its flatten commit is not archived yet. Run `gt dolt archive-now`
again, with the dispatch hold still set and the town still parked. Read its
exit code and the status the way step 2 does: exit 1, exit 3 and a status step
2 would refuse all stop the flatten here too. Run the pass again only while it
exits 2, re-reading `gt dolt status` after each pass until every database reads
`0 commits behind` as step 2 checks it. Stop after three passes that all exited
2: a fourth asks the daemon for work three passes did not finish, so report the
databases still behind to the overseer instead.

Re-count every table against the counts recorded in step 4 of "Before any
rewrite". A table that lost rows or disappeared means stop. See
[docs/dolt-restore.md](dolt-restore.md), section 3, "Replace a live database
(operator only)", for the restore. A table that gained rows took a concurrent
write, which is safe.

## Surgical rebase: squash old history, keep recent commits

This is what `gt dolt rebase` did. The SQL, the rebase actions, and the
concurrent-write hazard are in [Dolt storage](design/dolt-storage.md), under
"Surgical compaction". Never rebase `main` in place: rebase a work branch and
swap it in, as that section shows. If `DOLT_REBASE('--continue')` fails, abort
and drop the work branches before anything else touches the database:

```sql
CALL DOLT_REBASE('--abort');
CALL DOLT_CHECKOUT('main');
CALL DOLT_BRANCH('-D', 'compact-work');
CALL DOLT_BRANCH('-D', 'compact-base');
```

Dolt's automatic GC reclaims the dropped chunks once the journal passes its
threshold; a rewrite does not need a manual `dolt gc`.

## Rollback: restore `.beads` from a migration backup

This is what `gt dolt rollback` did. It applies only to backups made by the
migration formula's backup step, laid out as:

```
migration-backup-YYYYMMDD-HHMMSS/
├── town-beads/        -> <town>/.beads
└── <rig>-beads/       -> <town>/<rig>/.beads
```

1. `gt dolt stop`.
2. For each directory in the backup, move the live `.beads` aside (do not
   delete it) and copy the backup's directory into its place. The backup's
   `metadata.json` is the pre-migration one, so copying the directory resets
   it too.
3. `gt dolt start`, then check each restored rig with `bd list` from that
   rig's directory.

Never touch anything inside a `.dolt/` directory while doing this.

## Recover: a server stuck read-only

This is what `gt dolt recover` did, and the daemon already does it on its
health tick. To do it by hand, collect diagnostics first (town CLAUDE.md,
"If you detect Dolt trouble"), then run `gt dolt restart` and confirm a write
succeeds, for example with `bd create` on a scratch bead that you then close.
