# Restoring a Dolt database from the nightly backup

The daemon's `scheduled_maintenance` window takes a backup of every Dolt
database each night, before its gc (gt-8z769.5,
`internal/daemon/maintenance_backup.go`). This page is how to get one back.

## What is on disk

```
~/gt-backups/dolt/
  2026-10-01/
    backup.json      manifest: started, finished, databases, method
    hq/              one Dolt backup per database
    gt/
    ...
  2026-09-30/
  ...                seven nights are kept
```

- The root is outside the town (`~/gt`), so nothing that cleans or tests the
  town touches it.
- A directory named for a date is complete: the daemon writes a night into
  `<date>.partial` and renames it only once every database and the manifest
  are in. A `.partial` directory is a night that failed or is in progress;
  never restore from one.
- Each `<db>` directory is a Dolt backup written by
  `CALL dolt_backup('sync-url', 'file://...')` through the running server. It
  holds every branch, the uncommitted working set and dolt_ignored tables
  (events), as of that database's copy. Databases are copied one after
  another, so two databases in one night are minutes apart at most.
- A Dolt backup is not a data directory you can serve as is. It is read back
  with `dolt backup restore`, which builds a normal database directory from it.

The JSONL export each repo commits (`.beads/issues.jsonl`) is the
human-readable current state; the nightly backup is the full-fidelity copy.

## 1. Pick a night

```bash
ls ~/gt-backups/dolt/
cat ~/gt-backups/dolt/2026-10-01/backup.json
```

Pick the newest night from before the damage. `backup.json` lists the
databases it holds and when it finished.

## 2. Restore into a scratch directory and check it

This touches nothing live. Do it first, every time.

```bash
NIGHT=2026-10-01
DB=hq
SCRATCH=$(mktemp -d -t dolt-restore)
cd "$SCRATCH"
dolt backup restore "file://$HOME/gt-backups/dolt/$NIGHT/$DB" "$DB"
```

Serve it on a free port (never 3307, the live server) and query it:

```bash
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
dolt sql-server --host 127.0.0.1 --port "$PORT" --data-dir "$SCRATCH" &
SERVER_PID=$!

dolt --host 127.0.0.1 --port "$PORT" --user root --password "" --no-tls \
  --use-db "$DB" sql -q "SELECT COUNT(*) FROM issues; SELECT * FROM dolt_log LIMIT 3"

kill "$SERVER_PID"
```

Check the rows you care about are there. To recover a few rows rather than a
whole database, stop here: read them from this scratch server and write them
back through `bd`.

## 3. Replace a live database (operator only)

Replacing a database under the live server loses every write since that
night. Do it only with Sloan's approval, with the town down. Collect
diagnostics first if Dolt is misbehaving (`gt dolt status`, `gt dolt dump`;
see `~/gt/CLAUDE.md`).

```bash
NIGHT=2026-10-01
DB=hq
ASIDE=~/gt-backups/replaced-$(date +%Y%m%d-%H%M%S)

gt dolt pause --reason "restore $DB from $NIGHT" --until 1h
gt dolt stop

# Keep the damaged copy, outside the town. Never delete it in place.
mkdir -p "$ASIDE"
mv ~/gt/.dolt-data/"$DB" "$ASIDE/"

cd ~/gt/.dolt-data
dolt backup restore "file://$HOME/gt-backups/dolt/$NIGHT/$DB" "$DB"

gt dolt start
gt dolt status
dolt --host 127.0.0.1 --port 3307 --user root --password "" --no-tls \
  --use-db "$DB" sql -q "SELECT COUNT(*) FROM issues"
gt dolt unpause
```

Then bring the town back. The nightly gc's baselines in
`~/gt/daemon/maintenance_state.json` still describe the old copy; the old-gen
trigger may gc the restored database on the next window, which is harmless.

## What the backup does not cover

- It is on this machine. Losing the disk loses it too; the JSONL export in
  each repo, pushed to GitHub, is the off-host copy.
- Writes made after a night's copy are only in the live database and the
  next night's backup.
- The Dolt server's own config (`~/gt/.dolt-data/config.yaml`) and the town's
  files are not in it.

## Rehearsal

Rehearsed on 2026-10-01 (dolt 2.3.2), recorded on bead gt-8z769.5:

- `TestIntegrationNightlyBackupRestores` runs the daemon's real backup path
  (`maintenanceBackup`, `CALL dolt_backup('sync-url', ...)`) against a private
  `dolt sql-server` on a temp data dir holding a committed row, an
  uncommitted row and a dolt_ignored `events` table, then `dolt backup
  restore` into a new dir, serves that copy on a free port and queries it:
  all three survive, history intact.
- Steps 1-2 above were run by hand against a scratch server: the restored
  copy served on a free port returned both rows, the ignored table and the
  full `dolt_log`.

Step 3 has not been run against the live town.
