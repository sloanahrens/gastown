> Status: historical (2026-10). Merged in: gt-p85pw. Not maintained.

# R4 — request and response shapes for `POST /assigned` and `GET /snapshot/town`

Input to V12 (`POST /assigned`), V19 (`GET /snapshot/town`) and V14 (`bd query` /
`bd search`). Before implementing any of the three, read
`docs/plans/2026-10-25-town-tracker-design.md` §5 for the wire conventions every
endpoint shares: `/v1` prefix, bearer token, typed errors, snake_case JSON.
Shapes below are derived from the callers, not invented; every field names the
function that reads it today.

**S** below is the number of stores read: the town store `hq` plus one per rig
(`dashboard_queue.go:76`). A request scoped by rig is scoped by database,
because a rig's worktree directory and its beads database are one to one.

## 1. `POST /assigned`

### Why it is one endpoint

The patrol asks "what is on this seat's hook" once per seat, per tick, with a
subprocess per status:

| Caller | Today | File |
|---|---|---|
| `patrolScanHost.AssignedWork` | 3 × `bd list --json --status=<s> --assignee=<rig>/polecats/<name> --limit=0`, one per status (`hooked`, `in_progress`, `open`) | `internal/daemon/patrol_scan.go:485`, loop at `:470`, argv at `internal/beads/beads.go:2098` |
| `listActivePolecatWorkByName` | 1 × `bd query --json 'ephemeral=false AND (status="hooked" OR …)' --all --limit=0` per rig (five statuses), then ≤1 `bd show --json <ids>` for hooks outside the rig's store | `internal/cmd/polecat_inventory.go:944`, called from `internal/cmd/dashboard_polecats.go:402` |
| dashboard queue, live molecules | the same query with `hooked`, `in_progress`, once per store | `internal/cmd/dashboard_queue.go:173` |
| `listAssignedActiveWork` (agent hook reads) | 1 × `bd query --json 'assignee="A" AND (status="hooked" OR status="in_progress")' --all --limit=0` | `internal/cmd/hooked_work.go:56` |
| `hasAssignedWork` (`gt up`) | the same, with `pinned` added | `internal/cmd/up.go:747` |

The patrol pays 3 execs per seat, and pays them again from three call sites
(`internal/patrolscan/scan.go:861`, `:1104`, `internal/patrolscan/reap.go:88`).
One request per rig answers every seat in it.

Two of these reads also return work nobody asked about: the rig-wide query
lists every bead in five statuses and the client drops the unassigned ones
(`recordPolecatWork`, `polecat_inventory.go:984`). Keying the answer by
assignee is what removes those rows.

### Request

```json
POST /v1/assigned
{
  "assignees": ["gastown/polecats/capable", "gastown/polecats/furiosa"],
  "statuses": ["hooked", "in_progress", "open"],
  "rig": "gastown",
  "include_wisps": false
}
```

- `assignees` — required. Each is the full assignee string as stored on the
  bead (`<rig>/polecats/<name>`, `<rig>/crew/<name>`, an agent id). An empty
  array means *every* assignee, and is the form the rig-wide readers use
  (`listActivePolecatWorkByName`, the queue's live-molecule scan). At most 256
  entries; more is `invalid`.
- `statuses` — required, 1..8 entries, from the status vocabulary in the
  design §4 (`open`, `in_progress`, `hooked`, `pinned`, `blocked`, `deferred`,
  `closed`). `pinned` is here because `gt up` asks for it (`up.go:747`).
  Unknown status is `invalid`; there is no "all".
- `rig` — optional. Restricts the answer to one rig's beads. Empty means every
  rig, which is what the per-agent hook reads want. **The request's status set
  is what bounds the answer**, so no cursor exists: work in flight is tens of
  rows per rig.
- `include_wisps` — optional, default `false`. `false` reads durable issues
  only, matching the `bd list` and `ephemeral=false` callers above. `true`
  unions the wisps table, matching the two hook lookups, which omit the
  `ephemeral` predicate (`internal/beads/beads.go:2221`). See "Open item for
  V12" below.

### Response

```json
200 OK
{
  "assigned": {
    "gastown/polecats/capable": [
      {
        "id": "gt-p85pw.4",
        "status": "hooked",
        "assignee": "gastown/polecats/capable",
        "title": "Research: request and response shapes for /assigned and /snapshot/town (tracker R4)",
        "type": "task",
        "priority": 2,
        "labels": ["tracker"],
        "created_at": "2026-10-25T16:17:36Z",
        "updated_at": "2026-10-25T16:17:36Z",
        "closed_at": null,
        "ephemeral": false,
        "parent": "gt-p85pw",
        "blocked_by": [],
        "design": "",
        "notes": "",
        "description": "## Goal\n\nA report that specifies …\n\n## Constraints\n…"
      }
    ]
  },
  "count": 1
}
```

The row type is `api.Issue` (the record `GET /issues/{id}` returns) minus two
fields no caller of this endpoint reads: `acceptance_criteria` and `comments`.
The queue's bead-detail path is the only reader of those
(`internal/cmd/dashboard_queue.go:268`) and it calls `GET /issues/{id}`.
`description` stays whole: the patrol parses the attachment fields out of it
(`issueWork`, `internal/daemon/patrol_scan.go:456`) and the seat hover cuts its
`## Goal` section (`dashBeadSummary`,
`internal/cmd/dashboard_polecats.go:256`).

`count` is the number of rows across the map, for the request log and the
metrics counters (V17). An assignee with no matching row is absent, not an
empty array; a caller that needs "every seat answered" iterates the request.

**Ordering is part of the contract.** Rows within one assignee are ordered by
the position of their status in the request's `statuses` (first = highest
precedence), then `updated_at` descending, then `id` ascending. Both existing
readers take element 0 of a per-seat list as the seat's live work — the patrol
says so ("most specific status first", `patrol_scan.go:481`) and the inventory
ranks the same way (`polecatSummaryWorkStatusRank`,
`internal/cmd/polecat_inventory.go:926`) — so server-side ordering is what lets
them keep taking element 0.

### Open item for V12: does the answered set include wisps?

A hooked molecule root can be a wisp, and the patrol's `AssignedWork` cannot
see one: `List` without `Ephemeral` runs `bd list`, which reads the issues
table only (`internal/beads/beads.go:2086`). `ListAssignedIssueStatuses` was
written the other way round on purpose — it omits the `ephemeral` predicate so
a wisp assignment is found (`beads.go:2221`). The endpoint cannot keep both
conventions with one default, so the flag is explicit. **Recommendation:** the
wire default stays `false`, and the patrol's request passes `true` — a hooked
wisp is live work, and the gap is in the patrol's read, not in the contract.
That is a one-line caller change and a test in the patrol's own suite; V12
should not be the place the patrol's behaviour changes silently.

## 2. `GET /snapshot/town`

One request replaces the dashboard's two bead fan-outs (the summary poll and
the queue poll, both every 60s — `internal/dashboard/types.go:819`, `:876`) and
the beads half of `gt status`.

### Response

```json
200 OK
{
  "at": "2026-10-25T16:20:07Z",
  "schema": 1,
  "summary": {
    "ready_to_land": 3,
    "oldest_ready": "2026-10-25T15:58:12Z",
    "escalations": 2,
    "active_hooks": 7
  },
  "prefixes": {"gt": "gastown", "hq": "hq", "beads": "beads"},
  "agents": [
    {
      "id": "gt-gastown-polecat-capable",
      "rig": "gastown",
      "role": "polecat",
      "name": "capable",
      "address": "gastown/polecats/capable",
      "state": "working",
      "hook_bead": "gt-p85pw.4",
      "hook_title": "Research: request and response shapes …",
      "notification_level": "",
      "exit_type": "",
      "cleanup_status": "",
      "last_source_issue": "",
      "updated_at": "2026-10-25T16:17:36Z"
    }
  ],
  "hooks": [
    {
      "rig": "gastown",
      "role": "polecat",
      "agent": "gastown/capable",
      "has_work": true,
      "molecule": "gt-wisp-6kx1t2",
      "title": "Research: request and response shapes …"
    }
  ],
  "assigned": {
    "gastown/polecats/capable": [ { "id": "gt-p85pw.4", "status": "hooked", "…": "IssueBrief, as POST /assigned" } ]
  },
  "queue": {
    "ready": [ { "id": "gt-abc", "rig": "gastown", "title": "…", "type": "task",
                 "status": "open", "assignee": "", "priority": 2, "labels": [],
                 "created_at": "…", "updated_at": "…", "blocked_by": [] } ],
    "ready_total": 4,
    "ready_rigs": {"gastown": 4},
    "landing": [], "landing_total": 0, "landing_rigs": {},
    "blocked": [], "blocked_total": 0, "blocked_rigs": {},
    "blocked_work": 0,
    "blocked_wisps": 0,
    "unreadable": [],
    "parked_rigs": []
  }
}
```

Notes that are contract, not commentary:

- `at` is the sequence point the whole snapshot is read at. The three queue
  lists and the `assigned` map must be one read, or a bead can appear in
  `ready` and in a seat's work at once — today the dashboard's four reads per
  store are independent (`dashboard_queue.go:187`) and can disagree.
- `assigned` is `POST /assigned` with `assignees: []`, `statuses:
  ["hooked","in_progress"]` and the caller's `include_wisps`, scoped to every
  rig. It is the input to `addLiveMolecules` (`dashboard_queue.go:94`), which
  marks a molecule live only when its work bead is held by someone.
- `queue.ready` rows carry the raw bead; the dispatcher's shape verdict
  (`shape`, `shape_note`, `rig_parked`, `held`) stays in the client, because
  the lint is a pure function of the bead text and the parked table
  (`dashShape`, `dashboard_queue.go:125`). The server knows `parked_rigs` (from
  its route table) and nothing else of it.
- `blocked_work` / `blocked_wisps` stay client-side for the same reason: the
  split is `IsLiveStepWisp` over the client's live set.
- `prefixes` replaces the client's read of `routes.jsonl`
  (`storePrefixes`, `dashboard_queue.go:42`). After the last rig flips it is
  authoritative; before that the caller merges it with `routes.jsonl`.
- Every list is capped where the dashboard caps it (`queueMaxRows`), but each
  carries its exact total, so the page's chips keep counting the whole set.
  The capping is the client's: the server returns every row and the client is
  the one that knows how many fit on screen.

### 2.1 Field map

| Snapshot field | Reader that needs it | What it reads today |
|---|---|---|
| `summary.ready_to_land`, `summary.oldest_ready` | `dashboardReadyToLand` (`internal/cmd/dashboard.go:622`), also `queue.landing_*` | S × `bd list --json --label=gt:ready-to-land --limit=0` (`dashboard.go:637`) |
| `summary.escalations` | `tailOpenEscalations` (`internal/cmd/tail_sources.go:1127`) | 1 routed `bd list --label=gt:escalation --status=open --include-infra --json` (`internal/beads/beads_escalation.go:643`) |
| `summary.active_hooks` | `countActiveHooks` (`internal/townstatus/townstatus.go:453`) | derived from `agents[]`, no read of its own |
| `agents[]` | the townstatus prefetch (`townstatus.go:182`) and `dashSeats` (`dashboard_polecats.go:396`) | `bd list --label=gt:agent --include-infra --limit=0 --json --flat --no-pager` + `bd mol wisp list --json` (`internal/beads/beads_agent.go:547`, `:596`) |
| `agents[].hook_title` | `discoverRigAgents` (`internal/townstatus/agents.go:126`) | `bd show --json <hook ids>` per store (`townstatus.go:209`, `:245`; `internal/beads/beads.go:3208`) |
| `agents[].exit_type`, `.cleanup_status`, `.last_source_issue` | `patrolScanHost.AgentRecord` (`internal/daemon/patrol_scan.go:527`) | `bd show <agent bead>` per seat, from the description's agent fields |
| `hooks[]` | `discoverRigHooks` (`internal/townstatus/agents.go:19`) | `bd list --json --status=pinned --limit=0`, filtered to titles ending ` Handoff` (`internal/beads/handoff.go:56`) |
| `assigned` | `listActivePolecatWorkByName` (`polecat_inventory.go:944`), queue's live-molecule scan (`dashboard_queue.go:173`) | 1 `bd query` per rig, 1 per store |
| `queue.ready` | `dashQueueReader.read` → `ReadyAll` | `bd ready --json --exclude-label <csv> --exclude-type <csv> --limit 0` (`internal/beads/beads.go:2893`; label and type lists at `internal/constants/bookkeeping_beads.go:31`, `:64`) |
| `queue.blocked` | `read` → `Blocked` | `bd blocked --json` (`beads.go:3308`) |
| `queue.landing` | `read` → `List(Label: gt:ready-to-land)` | `bd list --json --label=gt:ready-to-land --limit=0` (`dashboard_queue.go:189`) |
| `prefixes` | `storePrefixes` (`dashboard_queue.go:42`) | `routes.jsonl` (`internal/beads/routes` via `LoadRoutes`) |

### 2.2 What the snapshot does not carry

The service holds beads, so these stay where they are; V19 must not grow
fields for them.

- tmux: session list, window activity, agent liveness (`dashSessions`,
  `dashboard_polecats.go:549`; `townstatus.go:127`).
- the filesystem: polecat and crew directories (`listPolecatDirectoryNames`,
  `dashboard_polecats.go:388`), landings files (`dashLandings`,
  `dashboard_polecats.go:23`), pause markers, seat settings.
- git probes: the per-seat worktree probe inside `buildPolecatInventoryItem`,
  and `tailMainPicture` (`tail_sources.go:1110`).
- Forgejo, gh, spend, health reports, daemon/Dolt/tmux probes, DND, overseer
  config: none is a bead read.
- mail counts (`BatchMailSummaries`, `agents.go:166`) — slice 3, the messages
  table.
- the merge-request index (`loadPolecatMRIndex`, `polecat_inventory.go:802`):
  store data, but a plain list — `POST /issues:list` with
  `label=gt:merge-request`, `status=all`, `include_wisps=true` replaces it
  (V3), not the snapshot.

### 2.3 Static finding: the escalation count caps at 50

`listEscalationsRouted` passes no `--limit` (`internal/beads/beads_escalation.go:643`)
and `bd list`'s default page is 50 (`internal/beads/beads.go:841`). Static: the
argv was read, not run. So `summary.escalations` — and the dashboard's
escalation count today — cannot exceed 50. The service answers it exactly; the
old `bd` path is gt-mm38p.

## 3. The dashboard's caches, and which ones the server retires

| Cache | Key | Lifetime | What it actually saves | After cutover |
|---|---|---|---|---|
| `dashSeatCache` (`dashboard_polecats.go:329`) | map key `rig/name`; validity key `updatedAt\|activeID\|running\|inGrace` (`:319`) | 5 min (`:344`) | the seat's worktree **git** probe, not a bead read | **keep**. Its `activeID` component today costs the `listActivePolecatWorkByName` read; from the snapshot it is a lookup, so the read goes and the cache stays |
| `dashLandings` (`:23`) | landings file path → size + mtime | until a file changes | re-reading each rig's landings JSONL | **keep**: files, not beads |
| `tailBeads` titles (`tail_sources.go:99`) | `rig/id` → title, bounded 512 (`:42`) | process lifetime | one `bd show` per feed line per poll | **keep** until the feed rows carry titles; do not add a server-side equivalent (see below) |
| `tailBeads` not-found (`:136`) | `rig/id` → first `ErrNotFound` | 5 min, 1024 entries (`:48`, `:54`) | a `show` per missing bead per poll | **keep**: a `not_found` is still a round trip |
| `PreloadBeads` snapshot (`beads.go:995`–`:1028`; warmed by `gt polecat list`, `internal/cmd/polecat.go:675`) | none; one warm-up read | process lifetime | the N+1 subprocess fan-out a single command would otherwise pay over Dolt | **drop**. Its whole reason is that every read is a process; one request per command removes it. V16 may keep the method and answer without it |
| townstatus detail lock (`townstatus.go:84`) | lock file per town | one `gt status` run | a second `gt status` downgrading itself to `--fast` so Dolt is not stormed | **drop**: one request per caller, and the lock is not a bead fact |
| dashboard queue | none | — | — | **replaced**: 4 reads per store per 60s become part of the snapshot |
| `dashboardReadyToLand` | none | — | — | **replaced**: `summary.ready_to_land` |

The server adds no response cache of its own for either endpoint. A snapshot is
read-after-write critical: `gt done` writes `gt:ready-to-land` and the landing
worker, the queue page and the patrol all read it within one refresh; a cached
answer reintroduces the disagreement the `at` field exists to remove. Caching
belongs to the store (indexes, prepared statements), not to the handler.

## 4. The `bd query` and `search` forms gt sends (input to V14)

Three query expressions and one search argv. Nothing else in gt's tree sends
`bd query` or `bd search`; the two `hm query` calls (`internal/jev/stale/wire.go:170`,
`internal/jev/dedupe/wire.go:106`) are a different binary and out of scope.
Agents may run either verb ad hoc (`internal/cmdtree/agent.go:21`), so the shim
must fail them with a clear message rather than a wrong answer; only the forms
below have to be exact.

### 4.1 Expressions

Values are Go-quoted with `strconv.Quote` (`internal/beads/beads.go:2323`), so
a value containing a quote arrives escaped. Clauses are joined with ` AND `;
status alternatives are one parenthesised ` OR ` group.

1. **Statuses, durable issues only** — `ListIssueStatuses`
   (`beads.go:2186`, argv `:2201`):
   ```
   bd query --json 'ephemeral=false AND (status="hooked" OR status="in_progress")' --all --limit=0
   ```
   Callers: `dashboard_queue.go:173`, `polecat_inventory.go:946`.

2. **One assignee, both tables** — `ListAssignedIssueStatuses`
   (`beads.go:2227`, argv `:2242`):
   ```
   bd query --json 'assignee="gastown/polecats/capable" AND (status="hooked" OR status="in_progress")' --all --limit=0
   ```
   Callers: `internal/cmd/hooked_work.go:56`, `internal/cmd/up.go:747`.
   The `ephemeral` predicate is deliberately absent, and the absence means
   "either table" (`beads.go:2221`) — the parser must give an absent
   `ephemeral` that meaning, not a default of `false`.

3. **Wisps by label** — `listEphemeral` (`beads.go:2261`, argv `:2294`), the
   clause order `ephemeral` → `label` → `created>=` → `status` → `priority` →
   `parent` → `assignee`, each present only when its `ListOptions` field is
   set:
   ```
   bd query --json 'ephemeral=true AND label="gt:merge-request"' --all --limit=0
   ```
   With `Status: "all"` the status clause is omitted and `--all` is passed;
   with any other status the clause is `status="<s>"` and `--all` is not.

**Grammar V14 must accept for these three**: `ephemeral=<true|false>`,
`status=<v>`, `assignee=<v>`, `label=<v>`, `parent=<v>`, `priority=<int>`,
`created>=<RFC3339>`, combined with `AND`, `OR`, and parentheses; flags
`--all`, `--limit=<n>`, `--json`. Anything outside that is `invalid`, per the
bead. Three things about the dialect matter more than the grammar:

- **Colons in label values.** `beads.go:2492` records that `bd query` cannot
  parse a label value containing a colon, which is why `ListMergeRequests`
  reads wisps through `bd sql` instead of the query above. Form 3 is therefore
  never sent with `gt:merge-request` from `*Beads` today — but a non-`*Beads`
  client does build it (`ListMergeRequests` in
  `internal/beads/client_helpers.go:56`, used by the fake and by the client
  V16 builds). The service's parser should accept colons; reproducing bd's
  rejection would leave the shim's `query` unable to answer a form gt itself
  sends on that path.
- **No silent cap.** `beads.go:2302` notes `bd query` caps at 50 without
  `--limit=0`; every gt call passes it. The service pages with `next_cursor`
  and reports truncation, never a 50-row answer dressed as the whole set.
- **`--all`.** In bd it means "include closed rows". In the service it maps to
  "do not default to `status=open`".

### 4.2 Search

`gt search` fans out one `bd search` per rig, pinned to that rig's database
(`internal/cmd/bead_search.go:116`):

```
bd search --json --limit=<limit+1> [--status <s>] -- <word> [<word> ...]
```

The trailing `--` is load-bearing: words after it are searched for, not parsed
as flags. The `+1` is how the caller learns the rig had more than it prints
(`bead_search.go:115`). Output is a JSON array of issues. The service's
`POST /issues:search` (or the shim's translation) must keep all three: the
limit-plus-one, an optional exact status filter, and words treated as text.
