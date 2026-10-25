> Status: historical (2026-10). Merged in: gt-p85pw.3 (R3). Not maintained.

# R3: the dispatch board, one response for the spec dispatch tick

Question: what single response would replace the bd reads of the spec dispatch tick
(`runSpecDispatchAt`, `internal/cmd/spec.go`), so the tracker's V13 bead can build it?
Sources: `internal/cmd/spec.go` at origin/main (lines cited are from that file) and the measurements
of 2026-10-25 in `claude-477`.

## What the tick reads today

Per tick, for every operational rig with a beads database (8 rigs today):

| # | Read | Where (spec.go line) | How often | What it needs |
|---|------|-------|-------|---------------|
| 1 | Ready board | `specReadyBoard`, `bd ready --json --unassigned --exclude-label L --exclude-type T --limit 0`; args at 2236, run at 2258, called from the Candidates read at 940 | once per operational rig per tick | the ready issues of the rig, with labels |
| 2 | Missing designs | `specBoardDesigns` (2169) | once per rig that has a candidate with an empty design | `design` is not on a board row |
| 3 | Held beads | `specHeldBeads` (1542) -> `heldBeadsFrom` (1519) -> `listDuplicateCandidates` (`sling_duplicate.go:625`), lazily per rig from `specHeldHold` (1461) when a candidate names a path | once per rig, at most, per tick (cached in `heldReads`) | the rig's `hooked` and `in_progress` beads with assignee and description |
| 4 | Revert probe | `rigRevertInFlight` (2023, called at 1006) -> `revertBeadOpen` (2044) | once per rig whose red-main state file names a revert bead | the status of one named bead |
| 5 | Children | `env.Children` (1104) | once per candidate that reaches the container check | the status of each direct child of the candidate |
| 6 | Pre-sling re-read | `showFresh` (1194) | once per candidate about to be slung (at most `max_per_tick`) | the full current bead; deliberately bypasses the tick ledger |
| 7 | Roster | `specRosterSnapshot` (1954, called from the roster read at 914) | once per tick, town store | the beads the live polecat sessions hold |

The tick already counts these (`specTickReads`, the "tick cost" line): about 108 bd read calls and
~25 s measured on 2026-10-25. Reads 1-5 are the board. Read 6 must stay a separate fresh read: its
purpose is to see a bead as it is now, after the board was taken. Read 7 is town-wide and not
rig-shaped, and is left out of the board (see below).

Everything else the tick does with these (eligibility, ordering, shape lint, path overlap, the
container rule, budget, seats) is Go in `internal/specdispatch` and stays in `gt`. The tracker
returns raw material and never decides what to dispatch.

## Decision: one POST, not a GET

The bead's acceptance wording named `GET /board/dispatch?rigs=...`. This report decides **`POST /v1/board/dispatch`**
instead, and V13 builds that. The request carries three lists (exclude labels, exclude types, probe ids) that can
be long, and the plan's other batch reads (`/v1/ready`, `/v1/issues:show-many`) are POST for the same reason.
It only reads, so the client passes `idempotent=true` and retries it like any read. Plan task V13 is updated to
say POST.

## Request

```json
{
  "rigs": ["gastown", "om"],
  "exclude_labels": ["..."],
  "exclude_types": ["..."],
  "held_statuses": ["hooked", "in_progress"],
  "probe_ids": ["gt-revert1"]
}
```

`rigs` is the operational rig list `gt` already computes (it decides parked and docked); the tracker does not
know rig status. The exclude lists are `specReadyArgs()`'s, passed through so the dispatcher's exclusions stay in
one place (`gt`). `held_statuses` is `specHeldStatuses`. `probe_ids` are the revert beads named by the state files
`gt` reads from disk.

| field | type | meaning | replaces |
|-------|------|---------|----------|
| `rigs` | string[] | rig prefixes to read; an unknown prefix goes to `errors`, not a failure; empty is invalid | the per-rig loop of read 1 |
| `exclude_labels` | string[] | labels a ready issue must not carry | `--exclude-label` |
| `exclude_types` | string[] | issue types to leave out | `--exclude-type` |
| `held_statuses` | string[] | statuses that mean a seat holds an issue; empty means no held list | the status filter of read 3 |
| `probe_ids` | string[] | issue ids whose status is wanted | read 4 |

## Response

```json
{
  "rigs": {
    "gastown": {
      "ready": [ { "...full Issue..." } ],
      "held": [ { "id": "gt-x", "type": "task", "assignee": "...", "description": "..." } ],
      "children": { "gt-parent": [ { "id": "gt-c1", "status": "open" } ] }
    }
  },
  "probes": { "gt-revert1": { "found": true, "status": "open" } },
  "errors": { "om": "unknown rig" }
}
```

| field | type | meaning | replaces |
|-------|------|---------|----------|
| `rigs` | map[string]RigBoard | one entry per requested rig that was read | reads 1, 2, 3, 5 |
| `rigs[r].ready` | Issue[] | ready, unassigned, unexcluded issues of rig r, ordered by priority, created_at, id; never null | read 1 and read 2 |
| `rigs[r].ready[]` | Issue | the full row, every field `specFromIssue` reads: id, title, issue_type, status, assignee, priority, created_at, labels, description, design, notes, acceptance_criteria, comment_count, ephemeral | read 2 (design) |
| `rigs[r].held` | HeldBead[] | assigned issues of rig r in `held_statuses`; never null | read 3 |
| `rigs[r].held[].id` / `.type` / `.assignee` / `.description` | string | `gt` computes `specHoldFiles` from the description and drops epics by `type` | read 3 |
| `rigs[r].children` | map[string]ChildStatus[] | for each ready issue that has children: its direct children, wisps included; absent when none; never null | read 5 |
| `rigs[r].children[p][].id` / `.status` | string | all `childSpecs` reads | read 5 |
| `probes` | map[string]Probe | one entry per requested id; never null | read 4 |
| `probes[id].found` | bool | false when the id does not exist (the caller treats that as not open, as `revertBeadOpen` does) | read 4 |
| `probes[id].status` | string | the status when found | read 4 |
| `errors` | map[string]string | rig prefix -> reason that rig was not read; never null | the per-rig error list of `specBoardRead` |

Ordering stays the tracker's `priority, created_at, id`, which equals `specdispatch.Order`'s primary key; `gt`
still applies `Eligible` and `Order` (cross-rig) itself. The tracker returns raw material and never decides what
to dispatch.

Size: today's gt board is ~100 ready issues; with full text roughly 100-300 KB of JSON. One HTTP call, so that is
acceptable; do not trim fields until measured.

## Reads that must stay separate

- **Pre-sling re-read (read 6).** The board is a snapshot taken at the start of the tick; the
  re-read at 1194 exists to see the bead as it is when the seat is about to be spent, after the
  other candidates were processed and slings began. Folding it into the board would defeat it.
  It stays one `Show` per slung candidate, bounded by `max_per_tick`.
- **Roster (read 7).** It is town-wide and answers which sessions are live; only part of it is
  tracker state, and that part is the subject of R4 (`/assigned`).
- **Writes** (annotate, add label, comment, sling hooks) are not reads and are untouched.

## Calls before and after

Before, measured: the tick's own cost line on 2026-10-25 counted about 108 bd read calls and ~25 s. They are the
sum of the reads above: read 1 once per rig (R=8), read 2 up to once per rig, read 3 up to once per rig, read 4 once per rig
with a red-main state file, read 5 once per candidate that reaches the container check (C), read 6 once per
slung candidate, read 7 once. C is the term that grew the count to ~108.

After: **1** board call replaces reads 1-5 entirely, so the count no longer depends on R or C. What remains is
read 6 (`s` calls, `s <= max_per_tick`, usually 0-3) and read 7 (1, until R4 replaces it): `1 + s + 1`, which is
at most 5 for `max_per_tick = 3`. A tick that slings three beads goes from ~108 measured reads to 5.

Latency, measured 2026-10-25 on the real gt+hq data in Postgres (`claude-477`): ready board for gt 13.5 ms,
show-many of 100 ids 2.7 ms, children of 100 parents 1.2 ms. The board is those queries for 8 rigs in one
transaction, so a few tens of milliseconds server-side; the exit measurement (S1) replays the real call mix and
replaces this estimate.

## Not in the board

- Roster (read 7) reads live sessions and the town store; it is not per rig and mostly not tracker
  state. Route it through `/assigned` (research bead R4), not this endpoint.
- The failed-label count (`LabeledFailed`) and skipped board-carried labels come from `ready` rows'
  labels, which the board carries; `gt` keeps counting them.
- Gates: none exist in the data (no gate rows), so no gate fields.

## Implementation notes for V13

- `Store.DispatchBoard(ctx, DispatchBoardRequest) (DispatchBoardResponse, error)`, built from the same queries
  as `Ready` (with `Unassigned`, the excludes, no limit), a held query (`rig`, `status = ANY(held_statuses)`,
  `assignee <> ''`) and a children query for the ready ids.
- **One snapshot.** All of it runs in one `REPEATABLE READ`, read-only transaction. The failure mode this
  prevents: an issue that a polecat claims between the ready query and the held query would otherwise appear
  as ready in one list and held in another, and the dispatcher could sling it again. This is an implementation
  requirement checked in review; no test can interleave a writer inside the call without a hook, so the contract
  does not claim to test it.
- The ready list must not stop at a page: the method pages internally (1000 per page) until the cursor is empty,
  and the response carries no cursor. A contract case creates 1003 ready issues and expects all of them.
- Contract cases: ready, held and children across two rigs in one call (children include a closed child and a
  wisp); exclude lists applied and rows carry description and labels; an unknown rig is reported in `errors`
  and the rest answered; probes found, closed and missing; an empty `rigs` is invalid; more than one page of
  ready issues.
- Route `POST /v1/board/dispatch`, client `DispatchBoard(ctx, req)`, agreement test with a stub store.
