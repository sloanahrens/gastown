> Status: historical (2026-10). Merged in: this bead. Not maintained.

# Every `dolt` CLI call in daemon-reachable code

Task R2 of the daemon-container plan (`docs/plans/2026-10-28-daemon-container-plan.md`). Read from `origin/main` at `aa7579bb` on 2026-10-29. No code changed, and nothing was run against Dolt or a daemon.

Method: `git grep` for every `exec` of the `dolt` binary in `internal/daemon`, `internal/doltserver` and `internal/doltbackup`; each enclosing function read; each caller traced with `git grep` over the package. Reachability from daemon code came from a name-based call graph over the two packages, which over-approximates, so every chain below was also read. Findings about runtime behaviour are marked **static hypothesis**: they come from reading the code, and a run is what would settle them.

Legend:

- **needs-local-files**: reads or writes files on this host under the town or Dolt data directory (data dir, `config.yaml`, rig dir), so a container that lacks that path cannot do it.
- **works-over-connection**: does not need the data directory: it connects with `--host/--port`, or it touches no data at all.
- **unclear**: none of the sites needed this label. The open runtime questions are listed at the end.
- **IsRemote()**: what `Config.IsRemote()` (`internal/doltserver/doltserver.go:396`) does on the path. The daemon's `DoltServerManager.isRemote()` (`internal/daemon/dolt.go:269`) uses `isLoopbackHost` instead; the two agree on empty, localhost, loopback-literal and unresolvable names.

## Summary

- 13 `exec` sites call `dolt` or build its argv: 4 in `internal/daemon`, 9 in `internal/doltserver`. Two builders (`buildDoltSQLCmd`, `buildServerSQLCmd`) carry most of the connection-based calls.
- Six of the 13 are reachable from daemon code: `daemon/dolt.go:299`, `jsonl_git_backup.go:547`, `jsonl_git_backup.go:555`, `doltserver.go:504` (through `daemon/dolt.go:1059`), `doltserver.go:4517`, `doltserver.go:5118`.
- Two daemon-reachable sites need local files: `doltserver.go:504` (start a local `dolt sql-server`) and `jsonl_git_backup.go:555` (embedded `dolt sql` against a local directory). **Neither checks `IsRemote()`**, so neither is skipped for a remote endpoint.
- The daemon's start path is gated only by `IsEnabled()` and `IsExternal()` (`daemon/dolt.go:509-521`). Static hypothesis: with a remote endpoint, `Enabled` and not `External`, every heartbeat would start a local server (Table A, row 4).
- Every connection-based call passes `--host/--port` and never reads a local path for its data, so it is already the remote-safe form. The one daemon-reachable `IsRemote()` gate is `ListDatabases` (`doltserver.go:2635`): remote uses SQL, local reads the filesystem.
- Dead code: `doltserver.go:487` (`host.buildDoltSQLCmd`) has no non-test caller, and `doltSQLScriptWithRetry` (`doltserver.go:5230`) has none either, so `doltSQLScript` (`:5196`) is unreachable in production.
- Bead correction: the bead names `doltserver.go:5097`. On `origin/main` the `exec` for the shared builder is at `:5118`, and `buildServerSQLCmd` starts at `:5107`.

## Table A: reachable from daemon code

| # | Site | argv built | Callers and path from the daemon | Class | IsRemote() |
|---|---|---|---|---|---|
| 1 | `internal/daemon/dolt.go:299` (`buildDoltSQLCmd`) | `dolt --host H --port P --user U --no-tls sql <args>`; `cmd.Dir = DataDir`; `DOLT_CLI_PASSWORD` in env | Five callers, all reached from `ensureDoltServerRunning` (`daemon.go:2033`) → `EnsureRunning` (`dolt.go:509`) → `checkHealthLocked` or the identity check: `:1283` `-q "SELECT active_branch()"`; `:1336` `-r csv -q "SELECT COUNT(*) AS cnt FROM information_schema.PROCESSLIST"`; `:1485` write probe (`USE …; CREATE TABLE IF NOT EXISTS __gt_health_probe …; REPLACE INTO …; DROP TABLE …`); `:1547` and `:1565` `-r csv -q` with `beadsql.TableRowCount(db, "issues")` and `"wisps"` | works-over-connection | Not consulted. The argv is the connection form for both local and remote. For a remote host the password is inherited from the environment when the config omits it (`dolt.go:309-323`). |
| 2 | `internal/daemon/jsonl_git_backup.go:547` (server branch) | `dolt --host H --port P --no-tls -u U sql -r json -q Q`; `cmd.Dir = dataDir`; `DOLT_CLI_PASSWORD` in env | `exportTableToJsonl` (`:612`) ← `exportDatabaseToJsonl` (`:471`) ← the JSONL backup patrol. Taken when `d.doltServer` is non-nil and `IsEnabled()` (`:629-641`) | works-over-connection | Not consulted. The branch is chosen by `IsEnabled()` alone, so a remote endpoint with the manager enabled takes this branch. |
| 3 | `internal/daemon/jsonl_git_backup.go:555` (embedded branch) | `dolt sql -r json -q Q`; `cmd.Dir = dataDir` | Same caller. Taken when the manager is nil or disabled. `dataDir` is `DataDir` when the manager is enabled, else `TownRoot/.dolt-data` (`:236-238`). The patrol skips the run when `dataDir` does not exist (`:240`) | needs-local-files | **Not skipped.** Embedded mode opens the on-disk directory. The comment inside `exportTableToJsonl` (`:621-623`) says this can lag the server. |
| 4 | `internal/doltserver/doltserver.go:504` (`NewSQLServerCommand`) | `dolt sql-server --config <DataDir>/config.yaml`; `cmd.Dir = DataDir` | `startLocked` (`daemon/dolt.go:1059`) ← `restartWithBackoff` (`:648`, `:709`) ← `EnsureRunning`'s not-running branch (`:618-630`), which has no remote check | needs-local-files | **Not skipped.** The gates on this path are `IsEnabled()` and `IsExternal()` (`:509-521`) and nothing else. Static hypothesis: `isRunning()` with no pid file returns not-running (`:399`), so with a remote endpoint and `Enabled && !External`, each `EnsureRunning` would start a local `dolt sql-server` whose `config.yaml` carries `host:` set to the remote host (`writeDaemonDoltConfig`, `daemon/dolt.go:957`), subject to restart backoff. |
| 5 | `internal/doltserver/doltserver.go:4517` (`GetActiveConnectionCount`) | `dolt --host H --port P --user U --no-tls sql -r csv -q "SELECT COUNT(*) AS cnt FROM information_schema.PROCESSLIST"`; `cmd.Dir = DataDir` | `internal/daemon/doctor_dog.go:201` | works-over-connection | Not consulted; always the connection form. |
| 6 | `internal/doltserver/doltserver.go:5118` (`buildServerSQLCmd`) | `dolt --host H --port P --user U --no-tls sql <args>`; `cmd.Dir = DataDir`; env `os.Environ()` plus `DOLT_CLI_PASSWORD` | Two daemon chains. (a) `ListDatabases` (`:2635`) → `listDatabasesCached` (`:2731`) → `listDatabasesRemote` (`:2784`), argv `-r json -q "SHOW DATABASES"` at `:2790`. Reached from `daemon/dolt.go:1530`, `:1701`, `doctor_dog.go:208`, and `EnsureAllMetadata` (`daemon.go:1273`). (b) `VerifyServerDataDir` (`:1272`), reached from `daemon/dolt.go:1513` → `VerifyDatabases` (`:2826`) → `verifyDatabasesWithRetry` (`:2904`), argv `-r json -q "SHOW DATABASES"` at `:2935` | works-over-connection | (a) is the gate: remote → this SQL path; local → `listDatabasesLocal` (`:2693`) reads the filesystem. (b) is not gated and runs in both modes. |

Static hypothesis for rows 1, 4, 5 and 6: each sets `cmd.Dir` to a data directory. If that directory is missing on the container's filesystem, the child process cannot start, so the probe fails before it reaches the server. The daemon's own start path creates the directory (`dolt.go:1032-1033`); the connection-only callers do not. This needs a run to confirm.

## Table B: in `internal/doltserver`, not reachable from the daemon

| # | Site | argv built | Callers | Class | IsRemote() |
|---|---|---|---|---|---|
| 7 | `doltserver.go:121` (`doltConfigMissing`) | `dolt config --global --get <key>` | Host `EnsureDoltIdentity` (`:69`, `:73`), exported at `:114` → `internal/cmd/install.go` only | works-over-connection (touches no data; reads the HOME-level dolt config) | n/a |
| 8 | `doltserver.go:140` (`setDoltGlobalConfig`) | `dolt config --global --unset <key>` | `EnsureDoltIdentity` (`:92`, `:104`) | works-over-connection (no data) | n/a |
| 9 | `doltserver.go:143` (`setDoltGlobalConfig`) | `dolt config --global --add <key> <value>` | as row 8 | works-over-connection (no data) | n/a |
| 10 | `doltserver.go:487` (`host.buildDoltSQLCmd`) | `dolt --host H --port P --user U --no-tls sql <args>`; `cmd.Dir = DataDir` | None outside tests (`doltserver_test.go:4449`, `:4491`, `:4531`). Dead in production | works-over-connection | Not consulted |
| 11 | `doltserver.go:2212` (`Start`, via `NewSQLServerCommand`) | `dolt sql-server --config <DataDir>/config.yaml`; `cmd.Dir = DataDir` | Exported `Start` ← `internal/doctor/rig_config_sync_check.go:551`, `internal/cmd/install.go`, `internal/cmd/dolt.go`, `internal/cmd/up.go`. Not the daemon | needs-local-files | **Not skipped.** `Start` (`:2014-2212`) has no `IsRemote` check. |
| 12 | `doltserver.go:3153` (`InitRig`) | `dolt init`; `cmd.Dir = rigDir` | Taken when `h.IsRunning` (`:739-746`) reports the server down. Callers: `rig/manager.go:662`, `RepairWorkspace` (`:4056`), `internal/cmd`. Not the daemon | needs-local-files (creates a Dolt repo under the rig directory on this host) | **Not skipped.** For a remote host, `IsRunning` dials the endpoint, so an endpoint that does not answer gets a local `dolt init`. |
| 13 | `doltserver.go:5118` callers outside the daemon | `buildServerSQLCmd` with these argv: `:2846` `-r json -q "SHOW DATABASES"` (`VerifyExpectedDatabasesAtConfig`, ← `internal/doctor/migration_check.go:287`); `:3908` `-r csv -q <query>` (`databaseHasUserTables`, ← `RemoveDatabase`); `:4754` `-q <query>` (`CheckReadOnly`, ← `GetHealthMetrics`, `RecoverReadOnly`); `:5088` `-q <query>` (`serverExecSQL`, ← `InitRig`, `RemoveDatabase`, `applyWaitTimeout`, `applyTimeZone`, `waitForCatalog`); `:5218` `--file <tmp>` (`doltSQLScript`, only ← `doltSQLScriptWithRetry`, which has no production caller) | Callers listed | works-over-connection | Not consulted |

## Table C: not counted

- `internal/daemon/dolt.go:1681` (`getDoltVersion`): `dolt version`. Class: works-over-connection (no data; needs only the binary). Reached only through `DoltServerManager.Status` (`dolt.go:362`), which the daemon package never calls. Not daemon-reachable.
- `LookPath("dolt")` at `internal/daemon/dolt.go:1038`, `internal/deps/dolt.go:36` and `internal/rig/manager.go:661`: a path check, not an exec.
- `doltserver.go:1035` runs `lsof` and is not a dolt call. The generic runner at `internal/doltserver/host.go:212` takes any binary and is not a dolt call.
- Test-only `exec` of `dolt`: `internal/daemon/maintenance_backup_integration_test.go:127`, `internal/doltserver/process_integration_test.go:292`, `internal/doltserver/sql_server_command_test.go:16`.
- Outside the daemon's dependency closure: `internal/cmd/dolt.go:848` and `:874`.
- `internal/doltbackup` has no `exec`. It holds only the backup layout (`backup.go:44-46`).

## Dependency reach

The daemon's transitive closure is 179 non-stdlib packages. Two of them call dolt-invoking `doltserver` functions: `internal/rig` (`bd.go:79-84`, `manager.go:325`, `:662`, `:681`) and `internal/polecat` (`manager.go:535`, `:562`). The daemon's direct calls into both are `WarnRigConfigOnce`, `CoerceBool`, `SpawnGrace`, `RemoveSessionHeartbeat`, `IsSubmittedWork` and `UnmergedOnAllBases`. None of them is a rig or polecat function that reaches `doltserver`'s dolt calls. That was checked one hop only, not traced further.

## Open items (need a run, not this audit)

1. Does `exec` fail before connecting when `cmd.Dir` is missing (rows 1, 4, 5, 6)? Settles whether a container without the data directory fails every probe.
2. Does a remote endpoint with the manager `Enabled` and not `External` start a local `dolt sql-server` (row 4)? Settles whether the daemon config for container mode must set `External`, or whether the start path needs an `IsRemote` gate. The fix is out of scope here; the finding is for the plan's E-tasks.
3. Which of rows 2 and 3 runs in container mode depends on whether the Mac's daemon config sets the manager `Enabled`.
