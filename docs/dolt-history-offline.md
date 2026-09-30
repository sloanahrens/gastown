# Dolt history rewrites: offline procedures

Gas Town has no CLI for rewriting Dolt history. `gt dolt flatten`, `gt dolt
rebase`, `gt dolt rollback` and `gt dolt recover` were removed (gt-638go.6,
wayfinder D3/D7): each one rewrote or replaced shared state, none ran in the
three weeks before removal, and a command an agent can reach is a command an
agent will eventually run. These are operator procedures. Run them by hand,
with the town parked, after a backup.

Compaction that runs unattended is a separate path and is not covered here:
the compactor-dog plugin (`plugins/compactor-dog/run.sh --compact`) and the
maintain patrol.

## Before any rewrite

1. Park the town, or at least every rig that writes to the database. A
   rebase fails on a concurrent commit, and a flatten is only safe against
   concurrent writes because Dolt merges the in-flight transaction.
2. Take a backup (`gt dog dispatch --formula mol-dog-backup`, or copy the
   database directory while the server is stopped).
3. Record row counts for every table so you can verify afterwards:

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
SELECT COUNT(*) FROM dolt_log;                                   -- after
```

Re-count every table. A table that lost rows or disappeared means stop and
restore the backup. A table that gained rows took a concurrent write, which
is safe.

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
