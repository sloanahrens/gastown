> Status: current (2026-11). The page a person reads to ask the town's history questions: where the daemon's archive files are, what each column holds, how the metrics tables read them, and four worked DuckDB queries.

# Querying the town's history archive

The daemon's maintenance cycle appends each database's new Dolt commits and
events to gzip CSV files, one file per database per UTC day. This is the town's
complete event record: a history flatten discards `dolt_log` and every AS OF
reading with it, and the events table is in Dolt's `dolt_ignore` list, so no
commit and no backup carries it. Before a flatten, read
[Dolt history rewrites: offline procedures](../dolt-history-offline.md): it says
when a flatten is allowed and what it discards.

## Where the files are

The directory is `patrols.scheduled_maintenance.archive_dir` in the town's
`~/gt/mayor/daemon.json`. The town's is `~/gt/.runtime/dolt-archive/`. An unset
setting means archiving is off and no default path is invented, so a town
without the setting has no archive rather than one in a guessed directory.

| File | Holds |
|------|-------|
| `events-<database>-<YYYYMMDD>.csv.gz` | every event bd recorded, appended as the day's runs archive more |
| `dolt-log-<database>-<YYYYMMDD>.csv.gz` | the `dolt_log` rows no archive file already held: the commits |
| `events-<database>.csv.gz` | a one-off whole-table export; the town's was written 2026-11-02 |

The date in a file's name is the UTC day the daemon wrote it, not the day of
its rows: a commit backdated into Dolt lands in the file of the day it was
exported. A file carries one header row, at the top, and the daemon appends
gzip members under it as more rows arrive.

## The columns

The events files start with a header row naming the events table's columns, in
the table's own column order. The daemon holds no list of those names: it writes
the columns its `SELECT *` returns, so a column added to the events table reaches
the next file's header without a daemon change. A file's header is fixed once
written, and the daemon refuses to append a member under a different one, so on
the day the columns change that database's export fails until the next day's
file starts. The line below is the shape the queries here assume; a file's own
first line is what that file holds.

```
issue_id,event_type,actor,old_value,new_value,comment,created_at,id
```

| Column | Holds |
|--------|-------|
| `issue_id` | the bead the event happened to |
| `event_type` | one of the types `archiveEventTypes` accepts, below |
| `actor` | who caused it: an agent, a command, or a person |
| `old_value`, `new_value` | the change the event records |
| `comment` | the sentence bd writes with the event: `Added label: <label>` on a label add, the reason on a close, the body on a `commented` |
| `created_at` | when it happened, UTC, as `YYYY-MM-DD HH:MM:SS` |
| `id` | the event's key; an event is one row however many files hold it |

On an `updated` row, `new_value` is a JSON object of the fields the update set
with their new values, and `old_value`, when present, a JSON object of the
values they replaced; that pair is what the metrics rollup parses. The other
types' pairs are bd's own shape, so select that type's rows before a query
reads them.

The commit files start with this header:

```
commit_hash,committer,date,message
```

| Column | Holds |
|--------|-------|
| `commit_hash` | the commit's hash in `dolt_log` |
| `committer` | who committed |
| `date` | when, as RFC 3339 with a zone, such as `2026-11-14T23:59:04Z` |
| `message` | the commit message; a work commit usually ends with its bead id in parentheses |

## Read each event once

A dated file never repeats an event: the daemon skips an id its watermark
already holds before it appends a row. A hand export is the exception: it is
the whole table when it was written, and the town's was written 2026-11-02. The
first dated read looks back a day, so events from that eve sit in both files.
Collapse the archive on `id` before counting anything:

```sql
CREATE OR REPLACE VIEW events AS
  SELECT *
  FROM read_csv('events-*.csv.gz', all_varchar => true, union_by_name => true)
  QUALIFY row_number() OVER (PARTITION BY id ORDER BY created_at) = 1;
```

## The event types the rollup knows

The metrics rollup accepts the `event_type` values `archiveEventTypes` holds in
[the events-archive loader](../../internal/metrics/loaders_events_archive.go),
the one place that set lives; read it there before you treat a type as known.
Any other type names a row the rollup cannot read, which it refuses rather than
counts as nothing.

## How the archive reaches the metrics tables

The daily analysis (`gt metrics analyse`, which `gt metrics run` also calls)
reads every events file, folds one bead's events into one row, and publishes
`~/gt/.runtime/metrics/latest/bead-changes.csv`: one row per database and
issue id, with the creation, first-update, claim and last-close stamps, the
counts, and the description the bead was filed with. `BeadChangeColumns` in
[the events-archive loader](../../internal/metrics/loaders_events_archive.go)
is the column order, and `BeadChangeRow` above it says what each one means;
read that comment rather than a copy of it. The rows the load refuses or has
already counted are gone from the table but reported, in one line on the
analysis's report.

Use `bead-changes.csv` for per-bead questions. Read the events files themselves
only for event-level ones: the order of what one actor did, or what one day
holds.

## Worked queries

DuckDB reads `.csv.gz` itself, so these run over the files as they sit. Run
`duckdb` from the archive directory, or give `read_csv` the absolute path:
DuckDB does not expand `~`. Both views read every column as text, so the casts
below, and nothing DuckDB inferred from a sample, decide what a value is. The
commit files are one view away:

```sql
CREATE OR REPLACE VIEW commits AS
  SELECT *
  FROM read_csv('dolt-log-*.csv.gz', all_varchar => true, union_by_name => true);
```

The rows do not name their database; the file name does. The globs above read
every database at once, which is what a town-wide count wants, because each
rig's issue ids carry that rig's prefix. A question about one rig reads that
rig's files: `events-gastown-*.csv.gz`, plus `events-gastown.csv.gz` if that
database has a hand export.

### Events per bead

```sql
SELECT issue_id, count(*) AS events
FROM events
GROUP BY issue_id
ORDER BY events DESC, issue_id
LIMIT 20;
```

### Time from created to closed

A bead counts as closed at its latest close, the rollup's own rule, so a bead
closed, reopened and closed again keeps its last close.

```sql
WITH per_bead AS (
  SELECT issue_id,
         min(created_at::TIMESTAMP) FILTER (WHERE event_type = 'created') AS created_at,
         max(created_at::TIMESTAMP) FILTER (WHERE event_type = 'closed')  AS closed_at
  FROM events
  GROUP BY issue_id
)
SELECT issue_id, created_at, closed_at,
       date_diff('minute', created_at, closed_at) AS minutes_to_close
FROM per_bead
WHERE created_at IS NOT NULL AND closed_at IS NOT NULL
ORDER BY minutes_to_close DESC;
```

### Rejections by week

This counts reopens, not verdicts: a refusal reopens its bead, and so does a
bead a person reopens. The archive holds no rejection verdict; that is the
`MERGE REJECTION` block in the bead's notes.

```sql
SELECT date_trunc('week', created_at::TIMESTAMP) AS week,
       count(*) AS reopens
FROM events
WHERE event_type = 'reopened'
GROUP BY week
ORDER BY week;
```

### Commits per day

Group on the commit's own `date`, not the file name: the file is the day the
daemon wrote the row. The first ten characters of `date` are its UTC calendar
day, as the daemon writes the stamp.

```sql
SELECT substr("date", 1, 10) AS day,
       count(*) AS commits
FROM commits
GROUP BY day
ORDER BY day;
```

## DuckDB is not installed in the town's Python environment

The town's Python environment has no `duckdb` module, so no town Python script
runs these queries. The supported path to the town's own answers is
`gt metrics`: `gt metrics analyse` publishes `bead-changes.csv` from this
archive, and its other subcommands read the tables around it. The queries above
are for a machine where DuckDB is installed, or as a statement of how the
archive's columns are read.
