# Town tracker: replace beads and Dolt with a town-owned issue service

Status: draft for review, 2026-10-25. Handoff bead: claude-477 (in `~/.claude`). Author: dipper.
Decisions below were grilled with Sloan on 2026-10-25 (Q1-Q16, all accepted) and the design sections were approved one by one.

## 1. Why

Dolt costs more than the town gets from it, and beads is the reason the town pays.

- Dolt (pid 1368) holds 3.9 GB resident and 172% CPU after 15 hours; the gt database is 1.2 GB, of which 933 MB is old-generation history that `dolt gc` does not reclaim. An offline `gc --full` on 2026-09-29 reclaimed almost nothing (claude-1ey.3).
- `bd` makes one Dolt commit per write. The 09-29 policy (ADR 0002) landed on the gastown side (gt-8z769.1-.5 closed: gc actor, pause marker, commit meter, nightly backup) but the beads side stayed deferred (be-c94.1, .2, .3, .5), so commits still accumulate.
- The slowness is mostly call volume, not database size. A 90-second process sample saw at least 529 `bd` processes (about 6 per second), 58% from `gt daemon run`, 10% from `gt dashboard`; each opens its own Dolt connection. A spec-dispatch tick costs about 108 reads and 25 s. Single-query latency is 1 ms.
- The town has been fixing fan-out piecemeal (gt-c9rl, gt-kvp82, gt-bqj8q, gt-9bjd3). A long-lived service with a pool and server-side batch endpoints removes the class.
- The end goal is the whole system in Kubernetes in the cloud (claude-wkb). SQLite cannot serve pods that do not share a filesystem. Postgres can, and the town already runs it (hm-postgres, Forgejo).
- What the town uses of beads is small. About 45 of ~118 commands appeared in 80,000 recorded calls; six verbs are 88% of Claude-session calls; reads outnumber writes about ten to one. Only 22 of 56 issue columns hold data in gt.

## 2. Decisions

| # | Decision |
|---|---|
| 1 | Run the program as a new wayfinder map (links claude-1ey for the Dolt decision D3 and claude-wkb for k8s). This document designs the foundation slices only. |
| 2 | Cheap Dolt fixes proceed in parallel under claude-477: verify what landed, offline flatten of gt and hq at a scheduled shutdown with a verified backup, memory cap on the server. Do not land be-c94.2 in the fork. |
| 3 | Build in the gastown repo as its own Go module (`tracker/`, own `go.mod`, no imports of gt internals). Fallback: same module with an enforced import boundary, same directory. |
| 4 | A long-lived Go service (`trackerd`) in front of Postgres, a thin Go client behind the existing `beads.Client` seam (ADR 0001). The CLI is a wrapper. |
| 5 | Postgres 17 on this Mac under launchd first, plain connection string, no extensions. Hosting and cost for k8s is a separate ticket under claude-wkb.8. SQLite is ruled out. |
| 6 | No Dolt-style history. An append-only `events` table plus backups. `AS OF`, `DOLT_REVERT`, `gt history` over `dolt_log`, and the `gt compact` AS-OF safety net go. |
| 7 | The contract test suite comes first. It runs against both `bd` and the service. Old beads tests are not ported. |
| 8 | A crew session builds schema, service core and contract suite; polecats take verb-by-verb beads afterwards. |
| 9 | One Postgres database, one `issues` table with a rig column, IDs kept verbatim. |
| 10 | Wisps share the table (`ephemeral` flag, partial indexes, TTL reaper); one dependencies table. |
| 11 | Mail and plugin-run receipts get first-class tables, designed now, built after the core. |
| 12 | HTTP+JSON with batch endpoints; a bearer token per seat carries identity. |
| 13 | A `bd` shim binary routes by ID prefix to old `bd` or the service. The long-term surface is `gt` verbs; the shim is deleted after the last rig flips. |
| 14 | No dual-write. Cutover rig by rig; smallest first; hq and gastown last. |
| 15 | See 2. |
| 16 | Skip the Dolt StatefulSet (claude-wkb.4) unless cutover slips. Postgres joins the k8s move order next to hm-postgres. The checkpoint date is set after the slice estimate. |

## 3. Architecture

```
tracker/                       own go.mod; imports nothing from gt
  cmd/trackerd/                HTTP+JSON service, pgx pool, token auth
  api/                         request/response types, JSON mirrors beads.Issue
  store/                       Postgres access: queries, batch reads, reaper
  migrate/                     schema migrations, import from Dolt, export to JSONL
  client/                      thin Go client over api/ (the only package gt links)
  contract/                    store and API behaviour suite
gt side:
  internal/beads/trackerclient implements beads.Client and beads.Admin via tracker/client
  cmd/bdshim                   the `bd` shim
```

Dependency direction is gt, then `tracker/client`, then HTTP, then `trackerd`, then Postgres. Only `trackerd` talks to Postgres, which keeps migrations, batching and the single-writer rule in one place. gt links the tracker module through a `replace` directive.

## 4. Schema

One database, one schema `tracker`. Facts below come from the live gt Dolt schema.

| Table | Notes |
|---|---|
| `rigs(prefix PK, name)` | the route table, replaces `routes.jsonl` and one-database-per-rig |
| `issues` | `id` PK; `rig` derived from the ID prefix; the 22 columns that hold data (issue_type, status, title, description, design, acceptance_criteria, notes, priority, assignee, owner, created_by, created_at, updated_at, closed_at, started_at, defer_until, close_reason, `ephemeral`, `wisp_type`, event kind, `payload jsonb`, agent-bead fields `hook_bead` / `agent_state` / `last_activity`); `parent_id` kept in step with the parent-child dependency in the same transaction; `metadata jsonb` for the rest |
| `dependencies` | one table for issues and wisps: `issue_id`, `depends_on` text (`external:<rig>:<id>` allowed, no FK), `type`, `metadata`, `thread_id`, created_at and created_by |
| `labels(issue_id, label)`, `comments` | one table each for issues and wisps |
| `events` | one append-only table with an identity `seq`: op, issue_id, actor, old and new value, comment, optional `payload jsonb`. Replaces `events`, `wisp_events`, `bd_events_journal` and `bd_events_seq`. No delete cascade. Retention prunes only payloads. |
| `kv` | `bd kv` and `remember` (`kv.memory.<slug>`, `gt.<type>.<key>`) |
| `config`, `custom_types`, `custom_statuses`, `child_counters`, `schema_migrations`, `tokens` | as today, plus hashed tokens |
| `messages`, `plugin_runs` | mail and receipts, designed with the core, built in slice 3 |

IDs are kept verbatim: `<prefix>-<5 base36>`, children `<parent>.<n>` from `child_counters` in the same transaction, wisps `<prefix>-wisp-<6>`. They are globally unique because prefixes are, so `Show` is a primary-key lookup and cross-rig batches are `id = ANY($1)`.

There is no stored `is_blocked`. `ready` and `blocked` compute blocking over `blocks` and `conditional-blocks` dependencies with partial indexes. This removes recompute-blocked and its repair class. At about 4,400 issues in gt the query is cheap; slice 1 measures it.

Types, statuses and dependency types in use today: issue types task, bug, feature, epic, chore, molecule, event, convoy, rig plus custom agent, gate, merge-request, message, queue, role, slot; statuses open, in_progress, closed, deferred, hooked plus staged_ready and staged_warnings; dependency types blocks, parent-child, tracks, related, relates-to, discovered-from, supersedes.

## 5. API (`/v1`)

Every write is one transaction that also appends its events. Errors are typed (`not_found`, `guard_failed`, `conflict`, `invalid`, `unavailable`); `if_assignee` and `if_status` are request fields. Lists return a `next_cursor` and the client has an `All` helper; there are no silent caps. Identity comes from the bearer token. The client sends its schema level and the server refuses on skew.

- Resources: `GET /issues/{id}`, `POST /issues:show-many`, `POST /issues:list` (body is a filter), `POST /issues`, `PATCH /issues/{id}`, `POST /issues:close`, `:delete`, `/{id}:release`, `:transfer`, comments, deps, labels, `GET /ready`, `GET /blocked`, `POST /issues:children-of`.
- Fan-out collapsers: `POST /assigned` (assignees and statuses to a map), `GET /board/dispatch?rigs=` (ready board, held beads, children, designs, roster work in one response), `GET /snapshot/town` (dashboard, townstatus), `POST /batch` (ordered operations in one transaction), `POST /inbox` (slice 3).
- Events: `GET /events?after=seq&limit=`, `GET /events/stream` (server-sent events from `LISTEN/NOTIFY`); a pruned-past `after` returns a typed `truncated` error with the first available seq.
- Molecules: the service stores trees (`POST /molecules`: root, steps, dependencies, created atomically; `POST /wisps`) and knows nothing about formula TOML. Cook (parse, `extends`, `compose.expand`, vars) is a Go package on the gt side. Open item: whether gt's `internal/formula` already cooks or relies on `bd cook`.
- Ops: count, stats, `/wisps:gc`, `/wisps:purge`, config and kv, `POST /export` (JSONL stream, includes wisps), `POST /import`, `/healthz`, `/readyz`, `/version` (schema level), `/metrics`.

## 6. What the new store must support

From the usage survey and the formula survey.

- Core verbs: show, list, create, update, close, comments (add and list), dep (add, remove, list, tree), label, search, query, ready, blocked, children, reopen, delete, defer and undefer, unclaim, count, stats, config, kv, remember and memories, events tail, history (as a view over `events`).
- Flags in real use: `update --status --append-notes --claim --assignee --add-label --remove-label --acceptance --body-file`; `list --status --json --limit --all --parent --type --label --include-infra`; `create --title --type --priority --description --labels --acceptance --validate --parent --body-file --graph`; `close --reason --force`; `ready --limit --parent --mol --gated --unassigned --exclude-label`.
- Molecules and wisps: cook and bond with `--var` and ephemeral, standalone wisps, `ready --mol` with correct transitive blocking, wisp list, gc, promote and purge, parent links that include wisp children, attachment fields in the bead description (`attached_molecule`, `attached_formula`, `formula_vars`), squash and burn as close or delete.
- Formulas that run: mol-polecat-work (and the monorepo variants), mol-deep-review (three scheduled slings), mol-polecat-code-review.
- Gates: a timer and GitHub gate polling stub only. No gate rows exist and no code creates one; 2,500 recorded `gate list` and `check` calls are the daemon's patrol polling.
- Raw-SQL users: `internal/beadsql/statements.go` (about 20 declared queries) is the checklist; its readers move onto API endpoints (preloaded beads, mail inbox, event tail, reaper candidates, schema level).

## 7. Tests

Three layers, each with a tier and cost under `~/.claude/docs/standards/test-engineering.md`.

1. Seam contract (gt side, integration tier). `beadsfake.RunClientContract` and `RunAdminContract` already run against the fake and against `*Beads` with bd and a Dolt test database. `trackerclient` becomes a third implementation, with `trackerd` in-process on a throwaway Postgres. New cases are added for behaviour the seam lacks.
2. Store and API contract (tracker module). Postgres semantics `bd` cannot show: atomicity, guards, ready and blocked including transitive blocks, cursor paging, `events` seq monotonicity, wisp children in `children-of`, and the batch endpoints. Service logic is unit-tested against a store interface; real-Postgres cases are integration tier.
3. CLI golden suite (shim). One golden file per verb and flag combination from the survey (about 25 verbs), recorded once from real `bd` on a fixture database and replayed against the shim. The test reads its golden files so the result cache sees changes.

Hermetic Postgres: a local `postgres` binary started per package on a unix socket in `t.TempDir()`, one template database cloned per case. No Docker. Cost is measured before the plan quotes a number.

## 8. The `bd` shim

Replaces `bd` on PATH; the real binary is kept as `bd.real`.

- A verb with an ID routes by prefix. A verb without one (`list`, `ready`, `create`) routes by the rig from cwd or `BEADS_DIR`, as `bd` does.
- Routing is a `backend` field per prefix in the route table. `dolt` execs `bd.real`; `tracker` calls the service.
- Output keeps `bd`'s JSON shapes and exit codes. An unimplemented verb on a flipped rig fails with a clear message and never falls back silently.
- Mixed state works because routing is per prefix; the 1,258 `tracks` dependencies from hq to other rigs stay external references.

## 9. Migration

No dual-write. Per rig:

1. Quiesce the rig through the procedure in `gastown-operations.md` ("Shutdown and restart"). Never touch `seat-refill.hold` or the ESTOP files.
2. Take a verified Dolt backup.
3. Import from Dolt: issues, wisps, labels, dependencies, comments, events, kv, config, child counters.
4. Verify per-table counts, sampled field equality, label sets, dependency closure and child counters.
5. Run the contract suites.
6. Flip the route entry.
7. Soak with Dolt left read-only for a set period.

Important: the JSONL backup in `.beads/backup/` does not include wisps (they are `dolt_ignore`d), yet agent beads and merge requests live there. The import and the export must read the wisp tables directly. The existing read-only `beadsql` reader does that.

Order: agentlab, the small rigs, devops, gastown, hq last (mail, escalations, convoys), rehearsed on a copy.

Rollback: flip the route back. Writes made on the service since the flip are replayed onto Dolt from the `events` log through the old CLI by a small Go tool built and proven on the first flip. There is no promise of automatic rollback beyond the soak window.

## 10. Operations

- Postgres 17 installed natively through brew (same major as hm and Forgejo), data in `~/gt/.pg-data`, two launchd jobs with `KeepAlive`: the database, then `trackerd`. The gt daemon only probes `/readyz` (ADR 0003: one supervisor, no process ownership in the daemon).
- `trackerd` holds a bounded connection pool; clients never connect to Postgres. Every client call has a bounded timeout and a capped retry (the B2-04 class). `/readyz` runs `SELECT 1` plus a write probe, and a doctor check mirrors it.
- Backup: nightly `pg_dump` at 03:00 into `~/gt-backups/tracker/<date>`, 7 kept; the git JSONL export stays as the human-readable fallback, now including wisps. A restore runbook with a proof (restore into a scratch database, run a contract subset) is a bead. No WAL archiving in slice 1.
- Maintenance in `trackerd`: wisp TTL reaper (policy as today), event-payload pruning, autovacuum defaults. No gc, flatten or pause marker.
- Resource limits: the pool bounds concurrency now; CPU and memory limits arrive with k8s requests and limits.

## 11. Security

`trackerd` binds 127.0.0.1. Every request carries a bearer token, even locally; tokens are stored hashed, issued per seat when gt creates it, kept in a 0600 file, and never printed or logged. The actor comes from the token; an admin token covers ops. Sandboxes reach the service through the same port forward as 3307 today, with the token in the environment. TLS or mTLS is added when it leaves the host.

## 12. Observability

`/metrics` exports request counts, latency histograms, rows returned, per-actor counters, pool saturation, and a slow-query log over 100 ms. A request-tag header lets one spec tick's calls be counted server-side, replacing the ps sampling and the in-process ledger.

## 13. Slices

This document designs S0 to S2 in detail and sketches S3 and S4.

| Slice | Content | Exit |
|---|---|---|
| S0 harness | `tracker/` module, gate wiring (`go test`, vet, lint, testpolicy, install), Postgres test harness, CI image change | `make gate` runs the module; a Postgres case runs in the unit-test budget |
| S1 core | Schema, issues, comments, deps, labels, events; core verbs; ready and blocked; batch reads; `trackerclient`; metrics | Seam contract green on the service and on `bd`; dispatch-board read measured on a copy of gt data; import round-trips counts |
| S2 compat | Shim and CLI goldens; import and export including wisps; kv and config; molecules, wisps and the cook port; agentlab flips | Golden suite green; agentlab running on the service through a soak |
| S3 | Mail and receipts tables and inbox; gate stub; events stream; small rigs, devops, gastown flip | Each rig's import verified and soaked |
| S4 | hq flip; delete the Dolt operational surface; retire the shim; update ADRs | Dolt no longer running |

Defaults for slice 1, to confirm at review: the dispatch tick needs at most 5 HTTP calls and 2 s on a copy of gt data (today about 108 reads and 25 s); a spec tick and a patrol scan each use one server-side aggregate per rig.

## 14. Risks

1. Porting `cook`, `bond` and `wisp` semantics. The molecule engine is 13.7k lines in beads and the largest unknown.
2. The long tail of CLI behaviour prompts and formulas rely on. The golden suite from real usage covers it; the shim fails loudly on anything unimplemented.
3. The hq cutover, rehearsed on a copy and done last.
4. Import correctness, including the meaning of `row_lock` (set on every closed issue) and `content_hash` (set on every issue), which are not yet traced in beads' source.
5. A single service is a single point of failure. launchd restarts it and bounded timeouts keep clients from hanging.
6. Test cost, and wiring a nested module into the gate.
7. Scope creep toward rebuilding all of `bd`.
8. Host change: Postgres must be installed on this Mac and on the Forgejo runner images.

## 15. Out of scope

Version history and time travel; Dolt remotes and federation; external tracker integrations (Linear, Jira, GitHub, GitLab, ADO, Notion); `swarm`, the `merge-slot` verb (gt already implements it as a `gt:merge-slot` bead), `mol current`, `squash` and `pour`, human gates, formula loops and aspects; `bd setup`, `hooks` and `onboard`; compaction; per-rig databases; Postgres high availability and managed hosting; k8s manifests.

## 16. Open items for the plan

- Whether `internal/formula` cooks without `bd cook`, and the size of the port.
- What `row_lock` and `content_hash` guard.
- Seam bypasses to bring inside the seam first: `internal/mail/bd.go`, the raw `ready` at `spec.go:2259`, the gate check in `patrol_scan`, `prime kv list`, `show` passthrough, `formula.go`, `tail config get`, the deps handshake, `metrics`, `report/hour.go`; and the seven dead seam methods.
- The exact payload of `/board/dispatch`, from `spec.go`.
- The checkpoint date for re-opening claude-wkb.4.
- ADR changes: 0002's SQLite trigger is superseded; claude-1ey.12 (due 2026-10-30) is re-scoped to this program.

## 17. References

claude-477 (notes hold the measurements); claude-1ey.3 and ADR 0002; ADR 0001 (gt-7iwy0); claude-wkb.4, .8; `~/.claude/docs/research/deep-review/beads-storage.md` and `beads-cli.md`; gt-c9rl, gt-kvp82, gt-bqj8q, gt-9bjd3 (piecemeal batching).
