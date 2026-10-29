# Restoring the tracker database from a nightly dump

> Status: current

`trackerbackup` writes one `pg_dump` of the tracker database into
`~/gt-backups/tracker/<date>.dump` (gt-p85pw.48). This page restores one of
those files.

Before the night you need it, run step 2 once: a dump nobody has restored is a
hope, not a backup.

## What is on disk

```
~/gt-backups/tracker/
  2026-10-28.dump           complete: pg_dump exited 0 with a non-empty file
  2026-10-29.dump.partial   a run in progress, or a run that was killed
```

A `.dump` file is one PostgreSQL custom-format archive of the whole `tracker`
database. The runner publishes a night by renaming `<date>.dump.partial` to
`<date>.dump`, so a name that still ends in `.partial` is never restorable.

## 1. Pick a dump

```bash
ls -l ~/gt-backups/tracker/
```

Take the newest `.dump` from before the damage.

## 2. Restore into a scratch database and check it

This touches nothing live. The restore goes into a new database on the same
cluster, and the shell's exit trap drops that database whatever happens next.

```bash
set -euo pipefail
DUMP=~/gt-backups/tracker/2026-10-28.dump
MAINT="postgres://127.0.0.1:5433/postgres?sslmode=disable"
NAME="restorecheck_$(date +%Y%m%d%H%M%S)_$$"
SCRATCH="postgres://127.0.0.1:5433/$NAME?sslmode=disable"

createdb --maintenance-db "$MAINT" "$NAME"
trap 'dropdb --maintenance-db "$MAINT" --force "$NAME"' EXIT
pg_restore --exit-on-error --no-owner -d "$SCRATCH" "$DUMP"
```

These are the settings in trackerd's plist (`trackerplist` writes them), with
the database swapped to `postgres` for the connection that creates another one.
If your install listens elsewhere, read the port from that plist.

`--exit-on-error` stops at the first error, so a restore that would otherwise
have ignored one fails here. `--no-owner` gives the restored objects to your
role, which need not be the role the dump names as their owner.

Then read the copy the way the tracker reads the live database. The third
query needs an id the second printed, so run them in order and put one in:

```bash
psql "$SCRATCH" -c "SELECT count(*) FROM tracker.issues WHERE NOT ephemeral"

psql "$SCRATCH" -c "SELECT id, title, status FROM tracker.issues
  WHERE NOT ephemeral AND issue_type <> ALL ('{agent,role,message}')
    AND status <> 'closed'
  ORDER BY priority, created_at, id LIMIT 5"

psql "$SCRATCH" -c "SELECT id, title, status FROM tracker.issues WHERE id = 'gt-abc'"
```

The copy passes when the count is not zero, the list holds the issues you
expect, and the by-id read returns the title and status the list showed.

The package check asks the same three questions through the store's contract:
the count of durable issues, a page of the default list, and one issue by id
(`tracker/internal/restorecheck/subset.go`). The SQL above is a hand-written
equivalent for a person to read, not the check's own queries: its filters are
the default list's as written here, and if the two ever disagree the package
check is the authority. Run that check when
you change the restore path or those reads; from the repository root:

```bash
cd tracker && go test -tags integration ./internal/restorecheck/
```

It builds its own cluster and dump and needs no town.

## 3. Replace the live database (operator only)

Everything written since the dump is lost. Get the overseer's approval first.
Each line below must pass before the next one runs, and the dump has to read
back before anything is renamed: `pg_restore -l` lists an archive's contents
and fails on a file that is not a usable dump.

```bash
set -euo pipefail
DUMP=~/gt-backups/tracker/2026-10-28.dump
MAINT="postgres://127.0.0.1:5433/postgres?sslmode=disable"
ASIDE="tracker_damaged_$(date +%Y%m%d%H%M%S)"

# 1. The dump must be readable before anything else moves.
pg_restore -l "$DUMP" > /dev/null

# 2. Nothing may hold the live database while it is renamed.
launchctl bootout gui/$(id -u)/com.gastown.trackerd

# 3. Keep the damaged copy under another name. Never delete it in place.
psql "$MAINT" -c "ALTER DATABASE tracker RENAME TO $ASIDE"

# 4. Restore the night into a fresh database under the name trackerd expects.
createdb --maintenance-db "$MAINT" tracker
pg_restore --exit-on-error --no-owner -d "postgres://127.0.0.1:5433/tracker?sslmode=disable" "$DUMP"

# 5. Read it back yourself before the service does.
psql "postgres://127.0.0.1:5433/tracker?sslmode=disable" -c "SELECT count(*) FROM tracker.issues WHERE NOT ephemeral"

# 6. Bring the service back.
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.gastown.trackerd.plist
```

A line that fails stops the block there: a dump that does not list (1) never
reaches the rename, and a restore that fails (4) keeps trackerd down rather
than serving a half-filled database. When a step after (2) fails, fix it and
run (6) by hand.

The renamed copy stays on the cluster until the restored database has served
for a while. Drop it then:

```bash
dropdb --maintenance-db "postgres://127.0.0.1:5433/postgres?sslmode=disable" --force "$ASIDE"
```

## Rehearsal

Rehearsed on 2026-10-29 (PostgreSQL 17.11, `/usr/lib/postgresql/17/bin`),
recorded on bead gt-p85pw.72. The temporary cluster was created for the run
with `initdb`, the tracker schema applied with `trackerd -migrate`, and three
issues seeded: an open task and an open bug, plus a closed agent bead so the
default list filter had something to leave out. The dump was taken with
`pg_dump -Fc`.

- Step 2: the block ran as written, DSNs pointing at the temporary cluster
  (its role is `postgres@`; the town's DSN carries no user). `--exit-on-error
  --no-owner` restored, the count printed `3`, the list printed `gt-def` then
  `gt-abc`, and the by-id read of `gt-def` printed `another one | open`, the
  title and status the list showed. The trap dropped the scratch database:
  `pg_database` held only `postgres` and `tracker` afterwards.
- Step 3, the database half: `pg_restore -l` exited 0, the rename to
  `tracker_damaged_...` succeeded, the fresh `tracker` restored and the count
  read `3` back. The two `launchctl` lines did not run: the rehearsal host has
  no launchd, and the tracker is not installed there.
- The package check: `go test ./internal/restorecheck/` (unit, the order and
  every refusal) and `go test -tags integration ./internal/restorecheck/`
  (a real `pg_dump` restored into a scratch database, the subset passing, and
  an empty dump refused with its copy dropped) both pass.
