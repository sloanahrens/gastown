> Status: historical (2026-10). Merged in: gt-p85pw.89 (R9). Not maintained.

# R9: every direct Dolt SQL consumer in gt, and what each must do for a tracker rig

**Bead:** gt-p85pw.89 (epic gt-p85pw, track R, research). **Read at:** gastown `origin/main` 5f3a478c.
**Method:** read-only walk of the tree, starting from `database/sql`, `sql.Open`, `go-sql-driver/mysql`,
`information_schema`, `CALL dolt_` and the `beadsql` importers. No file outside this report changed and
no consumer was run.
**Plan drift:** `docs/plans/2026-10-25-town-tracker-plan.md` has no R9 entry (it lists R1-R5). Its S4
slice does name this list ("`internal/beadsql` importers", "compactor dog", "commit meter", "Dolt doctor
checks"), and that is the task id the retirements below cite. Where no plan task or bead exists, the row
says none and the dipper session files one from this table.

## Answer

Forty-eight consumers, in six groups: the `internal/beadsql` door and the seam methods that send SQL through
`bd sql` (group 1), the daemon patrols (group 2), operator commands (group 3), doctor checks (group 4),
server-level SQL and the data-directory walks in `internal/doltserver` (group 5), and the tracker's own
Dolt reader (group 6).

The flip breaks every one of them, in one of four ways, and only the first two are loud:

- **Refused.** A read that goes through `bd sql` reaches the shim. The shim routes a tracker rig to its
  handler table (`internal/bdshim/bdshim.go:71`), which registers no handlers yet
  (`internal/bdshim/osmain.go:15`), so the command exits 1 with
  `bdshim: verb sql is not implemented for tracker rig <prefix>` (`bdshim.go:188`). No task in X3-X6
  (`docs/plans/2026-10-25-town-tracker-plan.md` section 11) implements `sql`, so this refusal is
  permanent, not a placeholder: X7 keeps it.
- **Unreadable.** A read on its own MySQL connection to a database that no longer exists fails at
  connect (`beadsql.Open`, `internal/beadsql/beadsql.go:76`); every caller below reports that as an error.
- **Silently skipped.** A consumer that enumerates databases from the Dolt data directory
  (`internal/daemon/maintenance_gc_guard.go:82`, `internal/daemon/jsonl_git_backup.go:1396`,
  `internal/doltserver/doltserver.go:2693`) or from `SHOW DATABASES` never sees the rig at all. No
  error, no log line: the JSONL backup stops containing the rig, the reaper stops reaping it, gc and
  compaction stop measuring it.
- **Read as stale data.** A Dolt database directory left behind under the rig's old name is still a
  database: it is discovered by the walks, exported by the backup, measured by the compactor and
  reaped. Nothing in this tree removes it at flip time.

The one row that stops a rig from being used at all is `gt sling` on a formula bead: the molecule-bond
read is fatal (`internal/cmd/sling.go:1007`), so a refused `bd sql` fails the sling rather than degrading
it. Every other group-1 consumer degrades: a warning and a fallback query, an empty check, or a
command that reports the refusal.

## 1. The door: `internal/beadsql` and the seam's SQL

`beadsql` is the only place a statement against bd's tables is written (`internal/beadsql/statements.go`),
and it sends them over two transports: its own MySQL connection (`beadsql.Open`), or a `beadsql.Query`
inlined into `bd sql` argv (`(*Beads).SQLCSV`, `internal/beads/cli.go:54`).

| Consumer | Reads or writes (tables) | How often | On a tracker rig today | Proposed action | Carrier |
|---|---|---|---|---|---|
| `beadsql.Open` / `New` schema check, `internal/beadsql/beadsql.go:89` | `schema_migrations` | every SQL connection | connect fails: no database of that name | the seam must not open SQL for a tracker rig; `trackerclient` answers instead | V16a-c |
| `beadsql.ReadOnly` guard, `internal/beadsql/beadsql.go:157` | none, it refuses statements | every statement | still refuses; nothing reaches a server | delete with the package at S4 | S4 |
| `(*Beads).SQLCSV`, `internal/beads/cli.go:54` | whatever `beadsql.Query` it is handed | per call | refused by the shim, exit 1 | each caller moves off SQL (rows below); the method dies with the shim | S4, X5 |
| `(*Beads).SchemaLevel` via the town start gate, `internal/beads/version.go:45`, `internal/deps/bd_handshake.go:187`, `internal/cmd/bd_handshake.go:104` | `schema_migrations` | once per process, for `gt up`, `gt sling`, `gt session start` and the other gated verbs | refused; the gate refuses the command, so no session starts | answer the gate from the tracker (readiness and schema), not from Dolt | B8, O4 |
| `(*Beads).PreloadBeads`, `internal/beads/beads_preload.go:26`, called from `internal/cmd/polecat.go:675` | `wisps`, `wisp_labels`, `wisp_dependencies`, `issues`, `labels`, `dependencies` | once per rig per `gt polecat list` | warning, then every reader falls back to its own per-call query (all of them seam verbs) | port the snapshot behind `beads.Client`, or drop it when the per-call path is the only path | none, needs a bead |
| `(*Beads).ListAgentBeadsFromWisps`, `internal/beads/beads_agent.go:569`, called from `internal/doctor/stale_agent_beads_check.go:158` | `wisps`, `wisp_labels` | per agent listing and per doctor run | refused; the check records a query error for the rig and leaves it out of the stale set | port to a seam method (`List` filtered on the wisp tier) | X3, V16a |
| `(*Beads).CountIssues`, `internal/beads/cli.go:73`, called from `internal/doctor/jsonl_bloat_check.go:126` | `issues` | per `gt doctor` run | refused; the jsonl-bloat check drops the error and skips the rig | compare the JSONL count against the tracker's count route, or retire the check | O4, S4 |

## 2. Daemon patrols

Group 2 reads run inside the daemon, on the town's Dolt server and its data directory, and one writes.

| Consumer | Reads or writes (tables) | How often | On a tracker rig today | Proposed action | Carrier |
|---|---|---|---|---|---|
| Wisp reaper, `internal/daemon/wisp_reaper.go:221` (discovery) and `:244` (open), queries in `internal/reaper/reaper.go:262` and `:563` | `wisps`, `wisp_labels`, `wisp_dependencies`, `wisp_events`, `issues`, `labels`, `information_schema` | hourly (`defaultWispReaperInterval`, `wisp_reaper.go:19`) | silently absent from `DiscoverDatabases`; if a database of that name is left behind, it is still reaped | port the age policy to `trackerd` (the plan names the same rules) | Mo3 |
| JSONL git backup, `internal/daemon/jsonl_git_backup.go:125` (issues), `:520` (six supplemental tables), `:544` (`dolt sql` command), `:1396` (data-dir walk) | `issues`, `comments`, `config`, `dependencies`, `events`, `labels`, `metadata` | every 15 minutes (`jsonl_git_backup.go:33`) | silently not exported: it is not in the data-dir walk and not in the configured list, and no alert fires | read tracker rigs through the export route | M4b, gt-p85pw.70 |
| Scheduled maintenance backup, `internal/daemon/maintenance_backup.go:179` | writes server-side with `CALL dolt_backup('sync-url', ...)` | nightly maintenance window, plus the daemon-start catch-up | not in the data-dir list, so not attempted; if named in config, the call fails and counts as a failed database | tracker backups are the nightly `pg_dump`, not this patrol | O2, S4 |
| Scheduled maintenance gc, `internal/daemon/maintenance_gc.go:480`, list from `internal/daemon/maintenance_gc_guard.go:82` | `CALL dolt_gc('--full')` | nightly window, 5-minute check ticks | not in the data-dir list, so skipped with no line | retire at S4 | S4 |
| Compactor dog, `internal/daemon/compactor_dog.go:606` (list), `:645` (`SHOW DATABASES` on a remote server), `:692` (`dolt_log` count) | `dolt_log`, database directory size | every 24 hours (`compactor_dog.go:18`), 15-minute check ticks | not in the list, so unchecked; a leftover database is measured and alarmed about | retire at S4 | S4 |
| Dolt server identity check (`checkDatabaseIdentityLocked`), `internal/daemon/dolt.go:1543` and `:1561` | row counts of `issues` and `wisps` in the first database `ListDatabases` returns | at server start and restart | names no rig: it checks whichever database is first, so a tracker rig is neither checked nor harmed | retire with `DoltServerManager` at S4 | S4 |
| Doctor dog probes, `internal/daemon/doctor_dog.go:199` | latency (`active_branch()`), connections, database list, orphan directories, backup age; no rig table | every 5 minutes (`doctor_dog.go:15`) | server-level only; it never names a rig | retire, replaced by the `tracker` doctor check | O4, S4 |

## 3. Operator commands

| Consumer | Reads or writes (tables) | How often | On a tracker rig today | Proposed action | Carrier |
|---|---|---|---|---|---|
| `gt reaper scan` and `gt reaper reap`, `internal/cmd/reaper.go:338` and `:447`, discovery at `:139` | the reaper's tables (section 2) | on demand | the rig is absent from discovery; naming it explicitly gives a connect error | port with the reaper policy | Mo3 |
| `gt history <id>`, `internal/cmd/history.go:351` | `dolt_log`, `dolt_ignore`, `issues` or `wisps`, `events` or `wisp_events` | on demand | `beadsql.Open` error, printed as a refusal | port to the events the service records; the shim's `history` is planned as a view over events | X5, S3 |
| `gt tail`, persistent connection at `internal/cmd/tail_journal.go:380`, reads at `:168` and `:188` | `bd_events_seq`, `bd_events_journal` | per poll (every few seconds while a viewer runs) | the open fails and the viewer falls back to one `bd` call per poll, which the shim refuses | port to the tracker events stream | S3 |
| `gt dep list --raw`, direct connection at `internal/cmd/dep_list_raw.go:78`, `bd sql` fallback at `:53` | `dependencies` | on demand | the direct path needs `metadata.json` to name a Dolt server (`internal/doltserver/beads_runtime.go:41`), which a tracker rig will not have, so it falls through to the `bd sql` refusal | port to a seam or route that answers raw dependency rows | none, needs a bead |
| `gt sling` with a formula, `internal/cmd/sling_helpers.go:240`, fatal at `internal/cmd/sling.go:1007` | `wisp_dependencies`, `wisps` | every formula sling | refused, and the refusal fails the sling (`checking existing molecule bonds`) | port to the seam's dependency read | X3, V16a |

> **Update 2026-10-30 (gt-p85pw.106):** the bond check now reads local-target bonds through the seam's `DepListUp`. External-target (`external:<rig>:<id>`) bonds are still read by one SQL statement on a store that answers SQL; a tracker store skips it, warns once per process and reads the local bonds only, until gt-etvxc teaches the tracker to answer them. No rig may switch to the tracker backend before gt-etvxc lands.
| `gt polecat list`, preload at `internal/cmd/polecat.go:675` | see the `PreloadBeads` row | on demand | warning only, then per-polecat seam calls | falls out of the `PreloadBeads` decision | none, needs a bead |
| `gt dolt latency`, `internal/cmd/dolt_latency.go:210` | `issues`, `labels` (three fixed probes, `:33`) | on demand | probe reports the connect failure | retire with `gt dolt` at S4 | S4 |
| `gt dolt compare`, `internal/cmd/dolt_compare.go:283` | `SHOW DATABASES`, `COUNT(*)` per database, `information_schema.tables`, `DOLT_HASHOF_DB()` | on demand | one endpoint fails to open and the comparison stops | retire at S4: it compares two Dolt servers | S4 |
| `gt dolt status`, `internal/cmd/dolt.go:486` and `:536` | latency, `information_schema.PROCESSLIST`, the write probe, `dolt_log`, data-dir size | on demand | server-level readings; the checks that name databases skip the rig | retire, replaced by `gt tracker status` | O4, S4 |
| `gt dolt list`, `internal/cmd/dolt.go:1363` via `ListDatabases`, `internal/doltserver/doltserver.go:2693` | the data directory on a local server, `SHOW DATABASES` on a remote one (`:2784`) | on demand | the rig is not listed | retire, or list tracker rigs from the routes | S4 |
| `gt dolt init-rig`, `internal/cmd/dolt.go:893`, and `gt rig add`, `internal/rig/manager.go:662` via `InitRig` | writes `CREATE DATABASE` (`internal/doltserver/doltserver.go:3134`) | every rig add | creates a Dolt database for a rig that will never use it, which then looks like a leftover database to every walk in this report | skip Dolt initialization when the rig's route backend is the tracker | none, needs a bead |
| `gt dolt cleanup`, `internal/cmd/dolt.go:1120`, and `gt rig remove`, `internal/rig/bd.go:81` via `RemoveDatabase` | writes `DROP DATABASE` and `DELETE FROM dolt_branch_control` (`doltserver.go:3856`, `:3871`), then removes the directory | on demand | a tracker rig has nothing to drop; a leftover under the old name is dropped correctly | keep as the cleanup for leftovers, retire with `gt dolt` | S4 |
| `gt dolt sql`, `internal/cmd/dolt.go:826` | whatever the operator types | on demand | a shell against a database the rig no longer has | retire at S4 | S4 |
| `gt install` preflight, `internal/cmd/install.go:531`, `:565`, and the readiness ping at `:766` | none: `PingContext` and `ListDatabases` | every install | install is town-level; the probes read the town server, not a rig | retire the Dolt probes with the server | S4 |

## 4. Doctor checks

All seven run on `gt doctor`. The daemon's doctor dog has its own probes and runs none of these
(section 2).

| Consumer | Reads or writes (tables) | How often | On a tracker rig today | Proposed action | Carrier |
|---|---|---|---|---|---|
| misclassified-wisps, `internal/doctor/misclassified_wisp_check.go:123` | `issues` | per run | the rig is not in `ListDatabases`, so it is not checked and the run is clean; a leftover database adds a "DB probe(s) failed" detail | read the tracker, or scope the check to Dolt rigs with a log line | O4, S4 |
| patrol-not-stuck, `internal/doctor/patrol_check.go:164` | `issues` | per run | the refusal is appended as a stuck-wisp detail, so a tracker rig reads as having a stuck patrol | port to the tracker's list route | O4, S4 |
| null-assignee, `internal/doctor/null_assignee_check.go:142` | `issues` | per run | the error is dropped (`continue`, "rig may not be bd-managed"), so the check reports OK having read nothing | port, or skip tracker rigs with a log line | O4, S4 |
| agent-beads repair, `internal/doctor/agent_beads_check.go:421` | `labels` | only in `gt doctor fix agent-beads`, never in the check's run | the read fails, so the repair ends with "bd update reported success but the label is absent" | port the verify to the seam's label read | O4, S4 |
| jsonl-bloat, `internal/doctor/jsonl_bloat_check.go:126` | `issues` count | per run | the rig is not in `ListDatabases` and its JSONL may still exist, so the comparison is skipped silently; a leftover database skips too (`continue`, "DB not reachable") | port, or retire with the JSONL backup | O4, S4 |
| stale-agent-beads, `internal/doctor/stale_agent_beads_check.go:158` | `wisps` | per run | refusal kept as an error; the beads it could not read stay unclassified | port with `ListAgentBeadsFromWisps` | O4, S4 |
| dolt-commit-rate, `internal/doctor/dolt_commit_rate_check.go:41` and `:72` | `dolt_log` per database, via the commit meter | per run | the rig has no reading; a leftover database reads as zero commits | retire, since the meter dies at S4 | S4 |

## 5. Server-level SQL and the data directory

These read or write the Dolt server or its data directory rather than one rig's tables. They matter to
this report because they are what a leftover tracker-rig database is still subject to, and because two of
them write.

| Consumer | Reads or writes (tables) | How often | On a tracker rig today | Proposed action | Carrier |
|---|---|---|---|---|---|
| Commit meter, `internal/doltserver/commit_meter.go:55`, called from `internal/townstatus/townstatus.go:336` and read by `internal/cmd/status.go:66` | `dolt_log` per database | every `gt status` | no reading for the rig; a leftover database counts zero commits for a day | retire, and read the tracker's commit or event rate instead | S4 |
| `GetLastCommitAge`, `internal/doltserver/doltserver.go:4923` | `MAX(date)` from each database's `dolt_log` | every health read (`gt dolt status`) | databases that fail are skipped, so the rig is absent rather than alarming | retire at S4 | S4 |
| `MeasureQueryLatency`, `internal/doltserver/doltserver.go:4877` | `SELECT active_branch()` | every health read and every doctor dog tick | server-level; unaffected by the rig | retire at S4 | S4 |
| `GetActiveConnectionCount`, `internal/doltserver/doltserver.go:4515` | `information_schema.PROCESSLIST` via `dolt sql` | every health read and every doctor dog tick | server-level | retire at S4 | S4 |
| `CheckReadOnly` write probe, `internal/doltserver/doltserver.go:4749` | **writes** `CREATE TABLE`, `REPLACE`, `DROP TABLE` of `__gt_health_probe` in the first database `ListDatabases` returns | every health read | with a leftover rig database first in the walk, it writes a table into it | retire at S4 | S4 |
| `VerifyDatabases` and `VerifyExpectedDatabasesAtConfig`, `internal/doltserver/doltserver.go:2831` and `:2846` | `SHOW DATABASES` via `dolt sql` | `gt dolt start`, `restart`, `status`, and the migration check | the rig is not served, so it is never in `missing`; nothing is flagged | retire at S4 | S4 |
| `databaseHasUserTables`, `internal/doltserver/doltserver.go:3905` | `USE <db>; SHOW TABLES` | per `RemoveDatabase` without `--force` | guards a leftover database before it is dropped | keep with the drop path | S4 |
| `InitRig`, `internal/doltserver/doltserver.go:3134` | **writes** `CREATE DATABASE` | per rig add or `gt dolt init-rig` | creates the leftover itself (section 3) | skip for a tracker rig | none, needs a bead |
| `RemoveDatabase`, `internal/doltserver/doltserver.go:3856` and `:3871` | **writes** `DROP DATABASE`, `DELETE FROM dolt_branch_control`, then removes the directory | per rig remove, `gt dolt cleanup`, and a doctor repair | drops a leftover correctly | keep while leftovers exist | S4 |
| `FindOrphanedDatabases`, `internal/doltserver/doltserver.go:3601` | the data directory against `rigs.json`, `metadata.json` and `routes.jsonl` | `gt dolt status` and `gt doctor` | a tracker rig has no directory, so it cannot be orphaned; a leftover database named after a tracker rig is reported as an orphan unless its name happens to equal a registered rig prefix (the safety net at `:3703`) | retire with `gt dolt` | S4 |
| `discoverMaintenanceDatabases`, `internal/daemon/maintenance_gc_guard.go:82` | `<data dir>/*/.dolt` | nightly maintenance and every compactor cycle | the rig is absent | retire at S4 | S4 |
| `maintenanceMeasure` and `dirSize`, `internal/daemon/maintenance_gc.go:251` and `:296` | database directory size and journal files | nightly | a leftover database is measured, and can look like a compaction candidate | retire at S4 | S4 |

## 6. The tracker's own Dolt reader

| Consumer | Reads or writes (tables) | How often | On a tracker rig today | Proposed action | Carrier |
|---|---|---|---|---|---|
| `tracker/migrate` reader, `tracker/migrate/read.go:12` (table list) and `:41` (`SELECT *` per table), driven from `tracker/cmd/trackerimport/run.go:64` | `issues`, `wisps`, `dependencies`, `wisp_dependencies`, `labels`, `wisp_labels`, `comments`, `wisp_comments`, `events`, `wisp_events`, `config`, `custom_types`, `custom_statuses`, `child_counters`, `wisp_child_counters` | once per rig, at import | this reader is the flip: it runs before the rig's route turns to the tracker | keep until the last rig is imported, then delete; it is the one consumer whose Dolt access is the point | M1, M6 |

## 7. What the flip does to a consumer that is not on this list

Three states are worth naming separately from the rows, because they decide how a miss shows up.

- **A consumer that enumerates databases silently loses the rig.** The walks in section 5 and the JSONL
  backup's discovery ask the filesystem, not the tracker, so a tracker rig is simply not in the answer.
  A missing row here does not break a run; it stops a backup, a reap or a measurement, and nothing says
  so. That is the class to re-check after a flip: run each enumerating patrol once and compare the
  database set it names with the routes.
- **A leftover database is read as live data.** Because `InitRig` creates a database before the route is
  known and nothing in this tree deletes one at flip time, a tracker rig can keep a `.<data dir>/<name>`
  directory holding its last Dolt state. The walks in section 5 and the JSONL backup then read and export
  it, and the reaper can reap and close rows in it that the tracker knows nothing about. Removing the
  directory belongs to the landing of the rig's flip; the plan's M5 keeps a replay tool for the reverse
  direction.
- **The refusal message is the only loud one.** For a `bd sql` consumer the shim exits 1 with a message
  naming the verb and the prefix (section Answer). A consumer that treats a read failure as a warning
  (the preload, the polecat summary) turns that into a quiet degradation, a consumer that drops the
  error as "not bd-managed" or "DB not reachable" (null-assignee, jsonl-bloat) reports a clean run
  having read nothing, and a consumer that treats it as fatal (the formula sling) blocks the rig.

A consumer here is a statement sent to the Dolt server or a read of its data directory. Readers of
`.beads/metadata.json` and `dolt-server.port` (`internal/beads/database.go:288`) name a database without
reading one, and the process-table walks in `internal/util` never open one; neither is listed above.

## 8. Not verified

- No consumer was run against a tracker rig; there is no flipped rig to run it against. Every "on a
  tracker rig today" cell is a static reading of the cited line plus the shim's dispatch
  (`internal/bdshim/bdshim.go:66-75`, `:184-188`, `internal/bdshim/osmain.go:15-17`).
- Whether the shim's `sql` verb is ever implemented. The plan's verb lists (X3-X6) omit it and X7 keeps
  the refusal for every unimplemented verb, which is the basis for treating every `bd sql` row as
  permanently refused rather than temporarily.
- How a call site learns a rig's backend. The routes carry a backend name
  (`internal/beads/routes.go:29`, `:33`), but none of the call sites in this table consults it; the
  "skip with a log line" actions above therefore need a lookup that does not exist yet, and its cost per
  site is not measured here.
- The patrol intervals are the shipped defaults (`wisp_reaper.go:19`, `jsonl_git_backup.go:33`,
  `compactor_dog.go:18`, `doctor_dog.go:15`). An operator's `settings/config.json` can set each
  interval and the database lists, and no configuration was read or checked.
- Whether any consumer outside this tree sends `bd sql`. A guard test is documented as failing the build
  on a `bd sql` argv built outside `internal/beadsql` (`internal/beadsql/beadsql.go:14-17`); this report
  did not run that test, so it read the call sites by grep rather than relying on the guard.
- The doctor checks that read bd's schema or version rather than a table (for example the migration
  check) were not traced; they are version reads, not data reads, and section 4 lists only checks that
  run a statement.
- How the daemon's per-database error accounting treats a refused rig once routes exist; today a named
  database that fails to open counts as a cycle error for the reaper and as a failed database for the
  JSONL backup, and a tracker rig that is absent counts as neither.
- Whether removing a rig's Dolt directory at flip time is scheduled anywhere. The plan's S4 deletes the
  Dolt surface town-wide, after the last rig flips; nothing in the tree does it per rig.
