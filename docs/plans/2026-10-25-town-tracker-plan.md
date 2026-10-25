> Status: historical (2026-10). Merged in: gt-p85pw. Not maintained.

Approved by Sloan 2026-10-25 (claude-477). The beads under epic gt-p85pw are the live work; crew foundation handoff is claude-pwu. Design: 2026-10-25-town-tracker-design.md.

# Town tracker implementation plan (slices S0-S2, milestones S3-S4)

> **For agentic workers:** every task below becomes ONE Gas Town bead (polecat tasks) or is built in a crew session (crew tasks, marked `CREW`). File polecat beads from `~/gt/gastown/mayor/rig` with the filing template in section 2. Do not run tasks out of dependency order. Steps inside a bead are the polecat's to choose; the acceptance boxes are the contract.

**Goal:** a town-owned issue service (`trackerd`, Postgres 17) that replaces `bd` and Dolt for the verbs the town uses, with a `bd` shim for migration, rig by rig.

**Architecture:** `tracker/` is a nested Go module in the gastown repo (service, store, client, migrate, contract). gt reaches it through a `trackerclient` that implements the existing `beads.Client` and `beads.Admin` seam. A `bd` shim routes by ID prefix to old `bd` or the service. No dual-write; cutover per rig from a Dolt import.

**Tech stack:** Go 1.26, `net/http`, `pgx/v5` (pool), Postgres 17 (native, launchd), server-sent events over `LISTEN/NOTIFY`. No Postgres extensions.

**Spec:** `docs/plans/2026-10-25-town-tracker-design.md`. Handoff bead: claude-477 (in `~/.claude`).

## Global constraints

Every task inherits the design spec: its decisions (section 2), schema (4), API (5), operations and security (10, 11) and out-of-scope list (15). Tests follow `~/.claude/docs/standards/test-engineering.md`.

## 1. File map

```
tracker/go.mod                         module github.com/steveyegge/gastown/tracker
tracker/api/                           wire types (mirror beads.Issue JSON), error kinds, ListFilter, UpdatePatch, Guard
tracker/store/                         Store interface + postgres implementation (pgx), migrations runner
tracker/store/schema/NNNN_*.sql        embedded migrations
tracker/store/storetest/               fake Store (in-memory) for the unit tier
tracker/server/                        HTTP handlers, auth, error mapping, metrics
tracker/cmd/trackerd/                  main
tracker/client/                        Go client over api/ (bounded timeout, capped retry, All helper)
tracker/migrate/                       Dolt reader -> import; export; verify
tracker/contract/                      store/API behaviour suite (layer 2)
internal/beads/trackerclient/          beads.Client + beads.Admin over tracker/client (gt side)
internal/beads/beadsfake/              existing seam contract (layer 1): gains the trackerclient run
cmd/bdshim/                            the bd shim (router + verb handlers + goldens under testdata/)
```

Tiers: **unit** = in-process with `storetest` fake or `httptest`, target under 100 ms per case. **integration** = real `postgres` on a unix socket in `t.TempDir()`, one template database cloned per case (cost measured by task C2 before any number is quoted). Integration cases run only where the polecat image or the runner has the binary (task E1).

## 2. Filing template (applies to every polecat bead)

Create from `~/gt/gastown/mayor/rig` with `bd create --type=task --priority=2 --validate --parent <epic>`; then `gt spec lint <id>` (exit 0 required). Each bead's description is these sections, filled from its entry below:

```
## Goal         <the Goal line>
## Constraints  <Files line>; no schema change; no new dependency; plus the entry's constraints
## Out of scope <the Out line>
## Gate         make gate; plus <the Gate line if any>
## Size         one worker, one landing
## Acceptance   <3-6 boxes from the entry>
```

Rules every entry inherits: boxes are ones the polecat ticks alone (package tests with a narrow `-run` filter; never a whole container-suite package such as `internal/beads`, `internal/cmd`, `internal/daemon`, `internal/mail`, `internal/polecat`; never `make gate`; never a live-town or wall-time measurement). Beads touching `internal/beads/beads.go`, `internal/beads/client.go` or `internal/beads/beadsfake` are chained in the order given. Tracker-module beads add to an existing package only; the crew tasks create the packages, the schema, and the dependencies first.

## 3. Epic and tracks

File one epic in the gastown rig: "Town tracker: replace beads and Dolt (S0-S2)", linking claude-477. Children by track:

| Track | Who | Purpose |
|---|---|---|
| R | polecat, read-only, start now | research with a written answer on the bead |
| B | polecat | bring seam bypasses inside `beads.Client`; delete dead methods |
| C | CREW | module, gate wiring, Postgres harness, schema, contract skeleton, server skeleton |
| E | polecat + overseer | polecat image gets Postgres |
| V | polecat | verbs: store + API + client + contract cases, one deliverable each |
| M | polecat | import, verify, export |
| Mo | polecat | wisps, molecules, cook port |
| X | polecat | the `bd` shim and goldens |
| O | polecat + crew | ops: launchd, backup, restore proof, health, tokens |
| A | polecat | ADRs |
| D | overseer/crew | interim Dolt track (not polecat) |

## 4. Crew tasks (build these in a crew session first)

Run as one `/handoff` bead for the crew session (suggested title: "Town tracker foundation: module, harness, schema, server skeleton"), worktree `~/gt/gastown/crew/sloan-tracker`, branch per task `crew/sloan/<bead>`, landed with `gt done`. The crew session writes the code TDD, reads the design spec, and updates its handoff bead after each task. Polecat tracks V, M, Mo, X wait for C1 to C6.

### C1 CREW: module skeleton and gate wiring
Files: create `tracker/go.mod`, `tracker/doc.go`, packages above with one passing test each; modify `Makefile` (new `tracker-test`, `tracker-vet`, `tracker-lint`, wired into `gate-test` and `gate-lint`), `internal/testpolicy` (include the module), `.golangci.yml` if per-module config is needed, `scripts/install-gt.sh` only if it must build `trackerd`; add `replace github.com/steveyegge/gastown/tracker => ./tracker` and the require to the root `go.mod`.
Acceptance: `make gate` runs the module's tests, vet and lint; a deliberately failing test in `tracker/` fails the gate (prove by a scratch commit, then drop it); `go test ./...` at the root still passes; an import of a gt internal package from `tracker/` fails a policy test (add that test).
Fallback if wiring fights the landing pipeline: same module, same directory, enforced import-boundary test (spec section 2, decision 3).

### C2 CREW: Postgres test harness, measured
Files: `tracker/store/pgtest/` (start `postgres` on a unix socket in a temp dir, `initdb` once per package, template database, `New(t) *pgxpool.Pool` cloning the template).
Precondition owed by Sloan: `brew install postgresql@17` on this Mac (a host change, not mine to do unasked). Acceptance: 50 cases clone-and-drop inside a measured budget; the measured per-case cost and the one-time cost go in the plan and on the bead before any other task quotes a number; the harness never listens on TCP; teardown leaves no process (test asserts the pid is gone); tests are parallel-safe.

### C3 CREW: schema v1 and migrations runner
Files: `tracker/store/schema/0001_core.sql` (rigs, issues, dependencies, labels, comments, events, kv, config, custom_types, custom_statuses, child_counters, tokens, schema_migrations), `tracker/store/migrate.go`.
Schema as in the design spec section 4: 22 populated issue columns, `metadata jsonb`, `parent_id`, partial indexes (`status` open and in_progress, `ephemeral`, `rig`), `dependencies(issue_id, depends_on, type)` indexed both ways, `events(seq identity)`, no cascade from issues to events. Acceptance: migrations apply on an empty database and are idempotent; a test asserts every table, the partial indexes and the absence of a cascade; the runner refuses a database with a higher schema level than the binary knows; mail and receipts tables are NOT created here (slice 3).

### C4 CREW: Store interface, fake, and contract skeleton
Files: `tracker/store/store.go`, `tracker/store/storetest/fake.go`, `tracker/contract/contract.go`.
Interfaces produced (later tasks fill them in, they do not rename them):
```go
type Store interface {
    CreateIssue(ctx context.Context, p api.CreateParams) (api.Issue, error)
    GetIssues(ctx context.Context, ids []string) (map[string]api.Issue, error)
    ListIssues(ctx context.Context, f api.ListFilter) (api.IssuePage, error)
    UpdateIssue(ctx context.Context, id string, patch api.UpdatePatch, g api.Guard) error
    CloseIssues(ctx context.Context, ids []string, reason string, force bool) error
    DeleteIssues(ctx context.Context, ids []string) error
    AddComment(ctx context.Context, id, author, text string) error
    Comments(ctx context.Context, id string) ([]api.Comment, error)
    AddDependency(ctx context.Context, issue, dependsOn, depType string) error
    RemoveDependency(ctx context.Context, issue, dependsOn string) error
    Dependencies(ctx context.Context, id, depType string) ([]api.IssueDep, error)
    Ready(ctx context.Context, f api.ReadyFilter) (api.IssuePage, error)
    Blocked(ctx context.Context, f api.ReadyFilter) (api.IssuePage, error)
    ChildrenOf(ctx context.Context, parents []string) (map[string][]api.Issue, error)
    EventsAfter(ctx context.Context, after int64, limit int) (api.EventsPage, error)
}
func RunStoreContract(t *testing.T, newStore func(t *testing.T) store.Store) // layer 2
```
Acceptance: the fake and the (stub) Postgres store both compile against the interface; `RunStoreContract` has one real case (create then get) passing on the fake; all methods the interface lists exist in `api/` types; a test pins the method set so a rename shows in review.

### C5 CREW: server skeleton and typed errors
Files: `tracker/server/`, `tracker/cmd/trackerd/main.go`, `tracker/api/errors.go`.
Acceptance: `/healthz`, `/readyz` (SELECT 1 plus a write probe), `/version` (schema level) work; bearer-token middleware resolves actor from the hashed `tokens` table and rejects unknown tokens with 401; errors map to the five kinds with a stable JSON body; the listener binds 127.0.0.1 only (test); a request carrying a schema level above the server's is refused.

### C6 CREW: client skeleton
Files: `tracker/client/client.go`.
Acceptance: bounded per-call timeout and a capped retry on `unavailable` only (tested with `httptest` and a fake clock, no sleeps); `All` pages through `next_cursor`; the token is read from a file path or env and never appears in an error message (test).

## 5. Research beads (polecat, read-only, start immediately)

Each bead's deliverable is a written comment on the bead. No code.

- **R1 cook port sizing.** Goal: answer whether `internal/formula` cooks without `bd cook` and size the port. Files: read `internal/formula/`, `internal/beads/formula.go`, `internal/cmd/sling_helpers.go`, beads `cmd/bd/mol*.go` and `cook*.go` at `~/gt/beads/mayor/rig` (read via `git show origin/main:`). Acceptance: comment lists, for each of the 21 formulas' features (needs, vars with defaults, extends, compose.expand, condition), whether gt already implements it; names the functions in beads that gt would have to port with line counts; states a size per piece in bead units; states what `bd mol bond` does to the vars that gt backfills (`backfillFormulaDefaultVars`).
- **R2 row_lock and content_hash.** Acceptance: comment states what each column guards, which verbs write and read it, whether the new store must carry it, and the import rule (copy, recompute, or drop), citing file:line in beads.
- **R3 dispatch board payload.** Goal: derive the exact `/board/dispatch` response from `internal/cmd/spec.go` (spec tick at ~1772, `specReadyBoard`, held beads, children per candidate, missing designs at ~2188, roster, the ledger at ~335-440). Acceptance: comment contains the JSON shape with every field and the list of bd calls it replaces; states what stays a separate call.
- **R4 patrol_scan and dashboard aggregates.** Acceptance: comment gives the request and response shapes of `POST /assigned`, `GET /snapshot/town` and the town-status call set, with the `bd` calls each replaces and the cache keys the dashboard uses today (`dashSeatCache`).
- **R5 polecat image and runner readiness.** Goal: find how the polecat image and the Forgejo runner images are built (`plugins/polecat-image/run.sh`, `deploy/`, `Dockerfile`) and what adding `postgresql@17` binaries takes. Acceptance: comment lists the files to change, the image size cost, and whether k8s seats use the same recipe.

## 6. Seam bypass beads (track B, polecat)

Goal of the track: every `bd` call goes through the seam, so one adapter can switch it. Chain order matters (hot files `internal/beads/client.go`, `beads.go`, `beadsfake`). Each bead: add the method to `beads.Client` or `beads.Admin`, implement it in `*Beads` with the exact `bd` argv the caller used, add it to `beadsfake` and to `RunClientContract` or `RunAdminContract`, replace the call site, no behaviour change. Boxes use narrow `-run` filters.

- **B1** delete the seven dead methods (`ReleaseWithReason`, `PreloadAgentBeads`, `CreateProbeBead`, `TableExists`, `Export`, `Init`, `LogDetachAudit`) after confirming no non-test caller; fix `method_set_test.go`.
- **B2** `ReadyFiltered(ReadyOptions)` for `spec.go:2259` (`--unassigned --exclude-label --exclude-type --limit 0`).
- **B3** `GateCheck(types ...string)` for `daemon/patrol_scan.go:446`, stripping the stray `null` once, inside the seam.
- **B4** `KVList()` and `KVGet(key)` on `Admin` for `cmd/prime.go:876`.
- **B5** `ShowRaw(id)` for the `cmd/show.go:123` and `show_unix.go:18` passthrough.
- **B6** formula verbs (`FormulaShow`, `FormulaList`) used by `cmd/formula.go:277`.
- **B7** `ConfigGet` use in `cmd/tail.go:477` and `tail_sources.go:308`.
- **B8** version and schema handshake in `internal/deps/bd_handshake.go` and `deps/beads.go` moved behind an `Admin.Version()` method.
- **B9** `metrics/source_host.go:99` and `source_queue.go:252` through the seam.
- **B9b** `report/hour.go:82` (the generic `Run` escape hatch) through `List`.
- **B10** `internal/mail/bd.go` runner (`runBdCommand`, used by `mailbox.go`, `router.go`, `delivery.go`) replaced by seam calls. Largest of the track: split into B10a (list and show paths), B10b (label add and remove, close, reopen), B10c (create, message thread, `mol wisp list`). Each is one landing.
- **B11** a policy test that fails when any non-test file outside `internal/beads` execs `bd` directly (allow-list shrinks only). Depends on B1 to B10.

## 7. Postgres in the polecat image (track E)

- **E1** (depends on R5): add `postgresql@17` binaries to the polecat image and the Forgejo runner recipe. Owed by the overseer: image rebuild and rollout. Acceptance for the polecat: the recipe change lands with a unit test of the recipe; the live check (a polecat can run `initdb`) is in Gate, owed by the overseer.

## 8. Verb beads (track V, polecat; depend on C1 to C6 and E1)

Each bead delivers the endpoint end to end: Postgres store method, handler, client method, and contract cases (layer 2 against the Postgres store and `storetest` fake; layer 1 via `trackerclient` once V16 exists). Tier is stated per bead. Keep each in `tracker/store`, `tracker/server`, `tracker/client` and the matching tests.

- **V1** ID generation: random base36 with collision retry (`ON CONFLICT`), child IDs from `child_counters` in the same transaction, wisp IDs. Unit tier for the generator, integration for the transaction.
- **V2** `CreateIssue` and `GetIssues` (`POST /issues`, `POST /issues:show-many`, `GET /issues/{id}`), event append. Integration.
- **V3** `ListIssues` with the filters in `beads.ListOptions` (status set, label and labels, priority, parent, assignee, no_assignee, type, ids, ephemeral, created_after, closed_after, include_infra, limit) and `next_cursor`. Integration.
- **V4** `UpdateIssue`: title, status, priority, description, assignee, add, remove and set labels, acceptance replace, append-notes, force, promote (`--persistent`). Integration.
- **V5** guards: `Release`, `ReleaseIfAssignee`, `TransferIfAssignee`, claim, `guard_failed` without a write; concurrent claim test with two clients. Integration.
- **V6** `CloseIssues` (reason, force past open children and blockers), `Reopen`, `DeleteIssues` all-or-nothing with comments, labels and dependencies in both directions. Integration.
- **V7** comments: add, add-as, list oldest first. Integration.
- **V8** dependencies: add, typed add, remove, list by type with relation; `external:<rig>:<id>` targets; parent-child maintains `parent_id` in the same transaction. Integration.
- **V9** `Ready` and `Blocked`: transitive blocking over `blocks` and `conditional-blocks`, `--unassigned`, `--exclude-label`, `--exclude-type`, `--parent`, `--mol`, cursor paging with no silent cap. Integration; this is the query V9 times on a copy of gt data in its Gate (owed by the overseer).
- **V10** `ChildrenOf` including wisp children. Integration.
- **V11** events: `GET /events?after=` with the `truncated` error and first seq; `GET /events/stream` over `LISTEN/NOTIFY` with a bounded test (context deadline, no sleep). Integration.
- **V12** `POST /assigned` (assignees by statuses, one call; R4). Integration.
- **V13** `GET /board/dispatch` (payload from R3). Integration.
- **V14** `query` and `search` forms gt uses (`ephemeral=true AND status=...`, text search). Scope fixed by R4 and the transcript survey; unsupported forms return `invalid`. Integration.
- **V15** admin: count, stats, config get and set, kv (also `remember` keyspaces), custom types and statuses. Integration.
- **V16** `trackerclient`: implements `beads.Client` and `beads.Admin` over `tracker/client`; `RunClientContract` and `RunAdminContract` run against it with `trackerd` in-process (integration tier, shared database per run). Split into V16a (read methods; after V2, V3, V9, V10, V12, V13 and V14), V16b (write methods; after V4 to V8 and V18) and V16c (admin and events; after V11 and V15).
- **V17** metrics: `/metrics` with request count, latency histogram, rows, per-actor counters, pool saturation, slow-query log over 100 ms, `X-Request-Tag` counted per tag. Unit tier.
- **V18** `POST /batch` (ordered operations in one transaction). Integration.
- **V19** `GET /snapshot/town` (R4). Integration.

## 9. Migration beads (track M, polecat)

- **M1** Dolt reader: read issues, wisps, labels, wisp_labels, dependencies, wisp_dependencies, comments, events, wisp_events, kv, config, custom types, child counters through `internal/beadsql` (read-only, schema-checked) into `api` import records. Wisps included. Unit tier with a recorded fixture; no live Dolt in the bead.
- **M2** `POST /import` and the writer: idempotent by ID, one transaction per rig, keeps timestamps and `seq` order, carries `row_lock` and `content_hash` per R2. Integration.
- **M3** verify: per-table counts, sampled field equality, label sets, dependency closure, child counters, wisp counts; prints a diff that names the first mismatch. Unit tier over two fixtures.
- **M4** export: `POST /export` JSONL stream including wisps, and the nightly JSONL git backup's SQL source rewritten to call it (`internal/daemon/jsonl_git_backup.go`, `statements.go:526,543`). Integration.
- **M5** replay tool for rollback: replay the service's `events` since a seq onto Dolt through `bd` (Go, with a fake runner in tests). Unit tier. Built for the first flip.
- **M6** import rehearsal command `gt tracker import --rig <r> --from-dolt --dry-run` that prints the verify report; the live run is owed by the crew session (Gate).

## 10. Wisps, molecules, cook (track Mo, polecat; Mo2 onward depends on R1)

- **Mo1** `POST /molecules` and `POST /wisps`: root, steps and dependencies created atomically; attachment fields in the description; ephemeral flag. Integration.
- **Mo2** `Ready` with `--mol` and `--gated`; parent links that include wisps (V9, V10 contract cases extended).
- **Mo3** wisp list, `wisps:gc` (dry-run candidates), `wisps:purge`, promote; TTL reaper in `trackerd` (policy as `internal/reaper`, same age rules). Integration.
- **Mo4** cook port per R1 into a gt Go package (path chosen by R1); its unit tests use the 21 formulas as fixtures. One bead per piece R1 names; bond and wisp calls go through the seam.
- **Mo5** gate stub: `gate check --type=timer|gh:run|gh:pr` polling on the service side or the client side as R1 decides; empty `gate list`. Unit tier.

## 11. Shim beads (track X, polecat; depend on V16 and the goldens)

- **X1** record goldens: a Go tool that runs real `bd` against a fixture database and writes `cmd/bdshim/testdata/golden/<verb>-<flags>.json` plus exit code, for the verb and flag combinations in the survey. Done once, results committed; the tool is the only script-like piece and is Go.
- **X2** router: route by ID prefix, or by rig from cwd and `BEADS_DIR`; `backend` field read from the route table; unknown prefix falls through to `bd.real`.
- **X3** read verbs: `show`, `list`, `ready`, `blocked`, `children`, `search`, `query`, `count` (goldens).
- **X4** write verbs: `create`, `update`, `close`, `reopen`, `delete`, `defer`, `undefer`, `unclaim` (goldens, exit codes: update 1 or 13, close 0 on partial).
- **X5** `comments`, `dep`, `label`, `kv`, `remember`, `memories`, `config`, `events`, `history` (as a view over `events`), `stats`.
- **X6** `mol`, `cook`, `formula`, `gate` verbs the town runs.
- **X7** a verb the service lacks on a flipped rig exits non-zero with a message naming the verb (test per unimplemented verb in the survey).

## 12. Ops beads (track O)

- **O1** launchd jobs `com.gastown.tracker-db` and `com.gastown.trackerd` (`KeepAlive`), data in `~/gt/.pg-data`; the plist generator is Go with a golden test (no hand-written plist script). The install is owed by the crew session and needs `postgresql@17`.
- **O2** nightly `pg_dump` at 03:00 to `~/gt-backups/tracker/<date>`, 7 kept, failure escalates; Go with a fake runner and clock.
- **O3** restore runbook and proof: `docs/tracker-restore.md` plus a Go check that restores a dump into a scratch database and runs a contract subset. The live restore proof is in Gate, owed by the crew session.
- **O4** doctor check `tracker` (readyz plus write probe) and `gt tracker status`.
- **O5** token issuance: gt creates a token when it creates a seat, stores it 0600, injects `TRACKER_TOKEN`, revokes on seat removal; never prints it (test).
- **O6** endpoint config: `TRACKER_URL` replaces the hardcoded 3307 for tracker rigs in `internal/agentconfig`, `power/states.go`, `hoststat.go`, `session/backend_openshell.go` (sandbox port forward); Dolt keys remain for Dolt rigs.

## 13. ADR beads (track A)

- **A1** ADR 0006 "Tracker replaces Dolt-backed beads": records the decisions; supersedes the SQLite trigger in ADR 0002 (note SQLite is ruled out by the k8s goal); links claude-1ey.3.
- **A2** re-scope claude-1ey.12 (due 2026-10-30) from "evaluate SQLite" to "tracker program checkpoint" (a bead comment plus `bd update`, run from `~/.claude`).

## 14. Interim Dolt track (D, not polecat)

Owned by this crew session with the overseer gt-b7 (reach it by `gt escalate` or a comment on claude-5dp, never `gt mail`).

- **D1** done: verified what landed (gastown gt-8z769.1 to .5 closed; beads be-c94.1, .2, .3, .5 deferred). Dolt: 3.9 GB resident, 172% CPU, gt 1.2 GB (933 MB old-generation).
- **D2** find the supported memory cap mechanism for the server (GOMEMLIMIT in the launchd or daemon env, `dolt-boot`'s start path, `config.yaml` keys) and write it on claude-477. Read-only research first.
- **D3** schedule: `gt escalate` to the overseer proposing a window; follow `gastown-operations.md` "Shutdown and restart" and `gt power`; never stop the town while `~/gt/devops/wizard-src/cloud-check.sh` lists a deploy, staging or teardown run; never touch `seat-refill.hold` or the ESTOP files.
- **D4** before the window: `gt dolt dump` and `gt dolt status` captured; verified backup (`diff -rq` identical, restore proof into a scratch location under `~/gt`).
- **D5** in the window: offline flatten of gt and hq per `docs/dolt-history-offline.md` (never SIGQUIT, never touch `.dolt` internals), restart under launchd with kickstart only.
- **D6** after: re-measure RSS, CPU, database size, spec-tick time, and write the numbers on claude-477. Skip D5 if D2's cap plus a re-measure shows no need.

## 15. Dependency graph

```
R1 R2 R3 R4 R5   (start now, parallel, read-only)
B1 -> B2 -> B3 -> B4 -> B5 -> B6 -> B7 -> B8 -> B9 -> B10a -> B10b -> B10c -> B11   (chain: hot seam files)
C1 -> C2 -> C3 -> C4 -> C5 -> C6              (CREW, sequential)
R5 -> E1
{C1..C6, E1} -> V1 -> V2 -> {V3, V4, V5, V6, V7, V8, V15, V17}
V3,V8 -> V9 -> V10 -> V12 ; R3 -> V13 ; R4 -> V12, V19 ; V2 -> V11, V14, V18
{V2, V3, V9, V10, V12, V13, V14} -> V16a ; {V4..V8, V18} -> V16b ; {V11, V15} -> V16c ; V16a -> V16b -> V16c
V16c -> X3..X7 ; X1 and X2 after C1 ; V16c -> Mo1..Mo3
R1 -> Mo4, Mo5 ; R2 -> M2
{V16c, M1..M3} -> import rehearsal M6 -> agentlab flip (crew) -> soak
O1..O6 after C5 (O2, O3 after C3); A1, A2 anytime
D1..D6 independent of everything above
```

## 16. Slice exits

The exit criteria are in the design spec, section 13. This plan only maps them to tasks: S0 is C1 and C2; S1 is V1 to V16c and V17 plus the M2 and M3 round-trip; S2 is X1 to X7, M1 to M6, Mo1 to Mo5 and O1 to O6. The overseer owes the dispatch-board measurement in V13's Gate.

## 17. Milestones only (re-plan after S2)

- **S3:** mail and receipts tables (design in the wayfinder ticket WF3), `POST /inbox`, events stream consumers (`gt tail`), the small rigs then devops then gastown flip, the mail runner retired.
- **S4:** hq flip rehearsed on a copy, then done in a full-town window; delete the Dolt operational surface (`gt dolt` lifecycle and gc, `DoltServerManager`, compactor dog, commit meter, Dolt doctor checks, `internal/beadsql` importers, test container pool, port 3307 plumbing); retire the shim after the last rig flips; update ADRs and the handbook.

## 18. Wayfinder map: open decision tickets

Create a new map (`wayfinder:map` epic) "Town tracker program", linked to claude-1ey and claude-wkb; one ticket per open decision, resolved one per session:

- **WF1** checkpoint date for re-opening claude-wkb.4 (Dolt StatefulSet), set from the S1 estimate.
- **WF2** Postgres hosting and cost for k8s (in-cluster operator vs managed; sizing and price table; extends claude-wkb.8; separate budget decision).
- **WF3** mail and receipts schema details (columns, TTL, label-compatible mapping until built, inbox with CC).
- **WF4** formula port scope (from R1: port everything the 21 formulas use or a subset; ownership of cook).
- **WF5** `gt` verb naming and the shim retirement date (long-term surface after the last rig flips).
- **WF6** token issuance in k8s seats and rotation (extends O5).
- **WF7** how prompts, skills and other rigs migrate from `bd` to `gt` verbs after the shim.

## 19. Self-review against the spec

- Spec section 2 decisions 1 to 16: 1 (wayfinder) section 18; 2 and 15 (Dolt interim) section 14; 3 and 4 (module, service) C1, C5, C6; 5 (Postgres) C2, O1; 6 (no history) V11, X5; 7 (contract first) C4, V16; 8 (crew then polecats) section 4; 9 and 10 (schema) C3, V1, Mo1; 11 (mail, receipts) section 17; 12 (API, tokens) C5, O5, V12 to V19; 13 (shim) X; 14 (cutover) M, S2 exit; 16 (k8s order) WF1, WF2.
- Spec open items: formula cook (R1, Mo4), `row_lock` and `content_hash` (R2, M2), seam bypasses (B), dispatch payload (R3, V13), checkpoint date (WF1), ADR changes (A1, A2).
- Operations (spec section 10): O1 to O4; security (section 11): C5, O5; observability (section 12): V17.
- Known soft spots: V9's query cost and the C2 harness cost are unmeasured until C2 and V9 run; E1 depends on R5's finding about image recipes; the `query` language scope (V14) is bounded by what gt actually sends and may grow.
