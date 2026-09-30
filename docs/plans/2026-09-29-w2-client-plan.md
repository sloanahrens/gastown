> Status: implemented on crew/sloan/w2-client (gt-7iwy0.2). Historical once merged; not maintained.

# W2: move the library sites to beads.Client (gt-7iwy0.2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans with superpowers:test-driven-development. Steps use checkbox (`- [ ]`) syntax for tracking. Every task starts with a failing test.

**Goal:** The three library write sites and the library reads named on gt-7iwy0.2 reach beads only through bd, via typed methods on `*beads.Beads` that the fake models and the contract pins, so gt-7iwy0.3 can delete the library.

**Architecture:** Each site swaps an in-process `beadsdk.Storage` call for a `*beads.Beads` method that runs bd. Hot reads (the journal tail, the board read) run in machine mode (`BD_MACHINE=1`) and read the envelope `{schema_version, contract_version, data, pagination, error}`. Writes keep the legacy `--json` path the rest of `*Beads` uses. New interface methods land with their `beadsfake` implementation and a contract case.

**Tech Stack:** Go, bd CLI (beads fork origin/main 92d15f7), `internal/beads` runner seam (`recorder`, `NewWithBeadsDirAndRunner`), `beadsfake` contracts.

**Spec:** bead gt-7iwy0.2 and its notes; epic gt-7iwy0 (rules 1, 4, 7); ADR `docs/adr/0001-cli-only-beads-access.md`; beads `engdocs/design/d1-machine-surface.md`; deep review G5-05 in `~/.claude/docs/research/deep-review/gastown-cross-cutting.md`. Seams reused from `docs/plans/2026-09-29-w1-handshake-plan.md` (`deps.ParseBDVersionJSON`, the DB-level read, `beads.BDReportedNotFound`, `RequireJSON`) and `docs/plans/2026-09-29-w2-liveness-plan.md` (`NewWithBeadsDirAndRunner` fakes in daemon tests).

## Global Constraints

- Do not delete the library, `store.go`/`store_open.go`, or go.mod entries (gt-7iwy0.3). Do not migrate raw argv sites these sites do not need (gt-7iwy0.4).
- Machine-mode exit codes: not_found 20, refused 21, partial 22, truncated 23, route_unreachable 24, store_unavailable 25, schema_skew 26, invalid_args 27.
- Every list-shaped read passes `--limit 0` or reads `pagination` under machine mode; a truncated page is an error, never a short answer.
- No test reaches the bd on PATH: unit tests use `recorder` / `NewWithBeadsDirAndRunner`; the contract's integration tier runs bd against the test Dolt container.
- The bd built from beads origin/main lives only in the scratchpad; it is never installed on PATH.
- No AI attribution in commits.

## Findings that shape the plan

1. **The events journal is off in production.** `bd config get events-journal` answers `false` for hq and gastown. It is a config.yaml key (`bd config set events-journal true` writes `.beads/config.yaml`), or `BD_EVENTS_JOURNAL=1` in the writer's environment. A rig's canonical config.yaml is git-tracked in `mayor/rig`, so the daemon must not write it. Decision: every bd process gastown spawns carries `BD_EVENTS_JOURNAL=1` (added in `SuppressBDSideEffects`, which every gastown bd env passes through). The daemon's startup check reports a store whose config.yaml leaves the journal off, because bd calls agents make directly do not journal there. The stranded scan stays the backstop for any close the journal missed.
2. **The journal is not the audit events table.** Records are `{seq, ts, op, issue_id, actor, issue}` with the issue's state after the mutation. Close = `op close`, or `op update` whose issue status is closed. Reopen = `op update` whose issue is not closed, for an issue this manager saw closed. The cursor is the seq (gapless, per replica): no time lookback and no lifecycle-event-ID dedup are needed.
3. **No bd verb seeds an unset `issue_prefix` on an existing database.** `bd config set issue_prefix` is refused ("use bd init / bd bootstrap / bd rename-prefix"), and `bd rename-prefix` fails when the current prefix is empty. `bd init --prefix` sets it on a database with no schema, and `bd rename-prefix` repairs a stale value when every row already carries the new prefix.
4. The daemon compat check's schema read goes through `bd sql --json 'SELECT MAX(version) ...'` in machine mode, the query the handshake already makes (`deps` exports it). Its event-table probe becomes a one-record journal tail.

## Call sites (origin/main da18a0cf)

| Site | Library call | Replacement | Machine mode |
|---|---|---|---|
| `internal/beads/beads_agent.go:230,277-301` | `store.CreateIssue` (createAgentBeadViaStore) | the existing `bd create --json --id --labels=gt:agent` path only | no |
| `internal/cmd/tracking_relations.go:44-76` | `store.AddDependency` / `store.RemoveDependency` type tracks | `(*Beads).AddTypedDependency` / `RemoveTypedDependency` (`bd dep add/remove A B --type=tracks`) | no |
| `internal/doltserver/doltserver.go:2880-2886` | `store.SetConfig("issue_prefix")` (EnsureRigIssuePrefix, opens with CreateIfMissing) | `bd config get issue_prefix`; the write stays on the library when the value differs (no bd verb; see Execution notes) | no |
| `internal/doltserver/doltserver.go:2905-2911` | `store.SetConfig("issue_prefix")` (SetRigIssuePrefix, doctor fix) | same helper as above | no |
| `internal/daemon/convoy_manager.go:735,857-880` | `store.GetAllEventsSince` + audit event types | `(*Beads).EventsTail(since, limit)` = `bd events tail --since --limit`, seq cursor per store | yes |
| `internal/cmd/daemon_dispatch.go:448-481` | `b.OpenStore` + store `GetReadyWork` (board count) | `(*Beads).ReadyAll()` = `bd ready --json --limit 0 --exclude-label --exclude-type` | yes |
| `internal/daemon/daemon.go:1692-1735` | `store.DB()` schema_migrations read and events/wisp_events probe | `deps.ReadDBSchemaLevel` via bd per store dir; `EventsTail(0, 1)` probe; journal-config warning | yes |

Out of scope and still on the library after this change (for gt-7iwy0.3/.4): `internal/daemon/daemon.go:1924-1945` hasActiveWork (`SearchIssues` status filter, bd verb: `bd list --status in_progress,hooked --limit 1` in machine mode), convoy operations (`internal/convoy/*` GetDependencyRecords, GetIssuesByIDs, comments; bd verbs exist: `bd show --json`, `bd dep list`, `bd comments`), the mail store, and the `*Beads` store branches in `beads.go`/`store.go`.

## File map

- `internal/beads/client.go`: `Client` gains `AddTypedDependency`, `RemoveTypedDependency`, `ReadyAll`; `Admin` gains `EventsTail`.
- `internal/beads/events.go` (new): `EventRecord`, `EventsPage`, `EventsTruncatedError`, `(*Beads).EventsTail`, envelope + legacy JSONL parsing.
- `internal/beads/machine.go` (new): `machineEnvelope` decode shared by EventsTail and ReadyAll.
- `internal/beads/beads.go`: `ReadyAll`, typed dependency methods.
- `internal/beads/database.go`: `BD_EVENTS_JOURNAL=1` in `SuppressBDSideEffects`.
- `internal/beads/beads_agent.go`: delete the store create.
- `internal/beads/beadsfake/{fake.go,admin.go,contract.go,admin_contract.go}`: fake methods, journal, contract cases.
- `internal/beads/library_free_test.go` (new): the migrated files import no library.
- `internal/cmd/tracking_relations.go`, `internal/cmd/daemon_dispatch.go` + tests.
- `internal/daemon/convoy_manager.go`, `daemon.go`, `memstore_test.go`, `convoy_manager_test.go`, `daemon_compatibility_test.go`.
- `internal/deps/bd_handshake.go`: export `ReadDBSchemaLevel`.
- `internal/doltserver/doltserver.go` + test.

---

### Task 1: library-free guard and agent-bead create

**Files:** create `internal/beads/library_free_test.go`; modify `internal/beads/beads_agent.go`; test `internal/beads/beads_agent_test.go`.

- [ ] Failing test `TestMigratedFilesImportNoBeadsLibrary`: parse each file in a table (starts with `beads_agent.go`; later tasks append theirs) with `go/parser` ImportsOnly and fail on `github.com/steveyegge/beads`.
- [ ] Failing test `TestCreateAgentBead_UsesBDCreateOnly`: recorded Beads answering `create` with `{"id":"gt-r-polecat-x","title":"t"}`; the returned issue has that ID and the only non-probe call is `create --json --id=... --labels=gt:agent`.
- [ ] Delete `createAgentBeadViaStore` and its call; drop the `beadsdk` import.
- [ ] `go test ./internal/beads/ -run 'MigratedFiles|CreateAgentBead'`; commit `refactor(beads): create agent beads through bd only`.

### Task 2: typed dependencies and tracking relations

**Files:** `internal/beads/client.go`, `beads.go`, `beadsfake/fake.go`, `beadsfake/contract.go`, `internal/cmd/tracking_relations.go` + `tracking_relations_test.go`.

**Produces:** `AddTypedDependency(issue, dependsOn, depType string) error`, `RemoveTypedDependency(issue, dependsOn, depType string) error` on `Client`.

- [ ] Contract case `typed dependencies`: tracks from a new issue to `external:rig:rig-x` (which need not exist) and to a local issue; `Show` lists both with type tracks; a tracks edge does not block `Ready`; remove drops only that edge; missing source issue is an error.
- [ ] Fake: `external:` targets need not exist; edges keep their type; `openBlockers` already filters to blocks.
- [ ] `*Beads`: `dep add A B --type=T` / `dep remove A B --type=T` through `run`.
- [ ] cmd failing test: `trackingDepsFor` seam returns a recording client; `addTrackingRelation` calls `AddTypedDependency(tracker, "external:gt:gt-abc", "tracks")` for a routed target; a client error is returned wrapped, with no second write path.
- [ ] Replace the store path and the BdCmd fallback with the client pinned to `beads.ResolveBeadsDir(townRoot)`; append the file to the guard table.
- [ ] Commit `refactor(cmd): write tracks edges through bd dep add/remove`.

### Task 3: board count through bd ready --limit 0 in machine mode

**Files:** `internal/beads/machine.go`, `beads.go`, `client.go`, `beadsfake/fake.go`, `beadsfake/contract.go`, `internal/cmd/daemon_dispatch.go` + test.

**Produces:** `ReadyAll() ([]*Issue, error)` on `Client`; `decodeMachineEnvelope(out []byte, what string) (machineEnvelope, error)`.

- [ ] beads failing test: recorded call argv is `ready --json --limit 0 --exclude-label ... --exclude-type ...`, env has `BD_MACHINE=1`; a 373-issue envelope returns 373; `pagination.truncated=true` is an error; a bare array (pre-D1 bd) is accepted; prose is an error.
- [ ] Contract: in `ready filter`, `ReadyAll` returns the same IDs as `Ready`.
- [ ] cmd: `readyBoardFor` seam (replaces `openDispatchReadyStore`) returning `interface{ ReadyAll() ([]*beads.Issue, error) }`; the 373-board test drives it.
- [ ] Commit `refactor(cmd): count the dispatch board through bd ready --limit 0`.

### Task 4: bd events tail on the client

**Files:** `internal/beads/events.go`, `events_test.go`, `client.go`, `database.go`, `beadsfake/admin.go`, `beadsfake/admin_contract.go`.

**Produces:**
```go
type EventRecord struct { Seq int64; TS, Op, IssueID, Actor string; Status string } // Status from issue.status, "" on delete
type EventsPage struct { Records []EventRecord; NextSince int64; More bool }
type EventsTruncatedError struct { Since, Floor, Head int64 }
func (b *Beads) EventsTail(since int64, limit int) (*EventsPage, error) // on Admin
```
- [ ] Failing tests: argv `events tail --since 5 --limit 100`, `BD_MACHINE=1`; envelope with records parses; `next_cursor` present sets More; exit 23 with `error.kind=truncated` and detail `{floor, head}` returns `*EventsTruncatedError`; legacy JSONL parses; legacy `{"code":"events_journal_truncated"}` is truncated; prose is an error; `SuppressBDSideEffects` output has `BD_EVENTS_JOURNAL=1`.
- [ ] Fake journals every create/update/close/dep/comment; admin contract case `events journal`: create, close, reopen; tail from 0 shows create, close, update with statuses; tail from NextSince is empty; limit 1 pages with More.
- [ ] Commit `feat(beads): read the events journal through bd events tail`.

### Task 5: convoy event polling on the journal

**Files:** `internal/daemon/convoy_manager.go`, `memstore_test.go`, `convoy_manager_test.go`.

**Consumes:** `beads.EventsPage`, `EventRecord`, `EventsTruncatedError`.

- [ ] `var newEventJournal = func(townRoot, name string) eventJournal` (bd for `beadsDirForStore(townRoot, name)`); test init swaps it to read the memstore's journal by name.
- [ ] memStore appends a journal record per mutation; poll tests move to it; new failing tests: truncated resumes at floor-1, logs the gap and sets recovery mode; a poll pages until More is false; the warm-up cycle advances cursors without processing; an update on an already-closed issue does not re-fire.
- [ ] Replace `lastEventIDs` time marks with seq cursors; delete `eventPollLookback`, `processedLifecycleEvents`, `isInfNaNError`, `isCloseEvent`/`isReopenEvent` over `beadsdk.Event`.
- [ ] Commit `refactor(daemon): poll convoy closes from the bd events journal`.

### Task 6: daemon compat check through bd

**Files:** `internal/deps/bd_handshake.go`, `internal/daemon/daemon.go`, `daemon_compatibility_test.go`.

- [ ] Failing tests with a per-store fake: level equal passes; unequal names both integers; bd exit 26 is refused as schema ahead; tail probe failure is refused; journal disabled in config.yaml passes with a logged warning naming `bd config set events-journal true`.
- [ ] `checkBeadsStoreCompatibility` takes store names, reads through `storeProbeFor(townRoot, name)`; delete `readStoreSchemaLevel`, `probeStoreEventSchema`, `probeEventTable`.
- [ ] Commit `refactor(daemon): check store schema and journal through bd`.

### Task 7: rig issue_prefix through bd

**Files:** `internal/doltserver/doltserver.go` + test.

- [ ] Failing tests with a fake prefix client: equal prefix makes no write; config-get failure on a schema-less DB runs `init --prefix p --database rig --server --server-port N`; an unset prefix on an initialized DB is refused naming the missing bd verb; SetRigIssuePrefix on a stale prefix runs `rename-prefix p` only after a sample row shows no other prefix, and refuses when a row carries another.
- [ ] Delete `openRigStoreFromConfig` and its env mutex; drop the `beadssdk` import; add the file to the guard.
- [ ] Commit `refactor(doltserver): seed issue_prefix through bd init and rename-prefix`.

### Task 8: measure and gate

- [ ] Latency against the scratch Dolt server: `GetAllEventsSince` in process vs `bd events tail` (bd-client); `GetReadyWork` vs `bd ready --limit 0`. Record in Execution notes.
- [ ] `make lint`, `go build ./...`, package tests, `gt slot run -- make test` with wall time, all by exit code.
- [ ] `om review -base origin/main`; fix blockers and majors.
- [ ] Attribution grep empty; `git push origin crew/sloan/w2-client`.

## Execution notes

- **Task 2:** `bd dep remove` has no `--type` flag in either bd build (537accb installed, 92d15f7 origin/main): the old raw-argv fallback for removal always failed. Removal was untyped in the store too, so the existing `RemoveDependency` covers it and only `AddTypedDependency` is new. `bd show --json` and `bd dep list` omit `external:` targets; the fake models that and the contract pins it.
- **Task 4:** both bd builds honor `BD_EVENTS_JOURNAL=1` with the journal off in config.yaml; the contract cases pass against both (legacy JSON lines on 537accb, the envelope on 92d15f7), run against a scratch Dolt server with `-parallel 1`.
- **Task 7 (changed):** bd has no verb that sets `issue_prefix` on an existing database. `bd config get` on an empty database creates the schema without a prefix; after that `bd init` refuses ("already initialized" once metadata.json exists), `bd rename-prefix` fails ("failed to get current prefix"), and `bd bootstrap` names the prefix after the database and leaves `issue_prefix` unset. So both doltserver sites read through bd and skip a matching prefix; the write stays on the library in one helper (`writeRigIssuePrefixViaStore`). Needed on the beads side for gt-7iwy0.3: a verb that sets `issue_prefix` on a database whose prefix is unset or stale without rewriting IDs (for example `bd config set issue_prefix` allowed when unset, or `bd rename-prefix --config-only`).
- **Task 6:** the daemon package's hermetic TestMain renames databases, so the real probe was checked by running its exact argv by hand against the scratch server (level 66, tail, config get).
- **Latency (scratch Dolt, 20 journal records, 10 ready issues, 20 runs, median / p90):**

| Read | Library in process | bd subprocess |
|---|---|---|
| Journal poll (GetAllEventsSince vs events tail) | 0.95 ms / 1.4 ms | 156 ms / 170 ms |
| Board (GetReadyWork vs ready --limit 0) | 3.8 ms / 4.6 ms | 179 ms / 338 ms |

  The cost is process start, not the query. At the 5 s poll interval with six stores that is about 0.9 s of bd per tick, run serially; the library also paid a 1.1 s open per store that bd does not keep. Not slow enough to revisit read-only library access (epic rule 7); a batched multi-store tail on the beads side would cut it to one process per tick.
