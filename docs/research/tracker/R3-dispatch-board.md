> Status: historical (2026-10). Merged in: gt-p85pw.3. Not maintained.

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

## The response

`POST /v1/board/dispatch`, an idempotent read (client passes `idempotent=true`).

## Fields

Why POST and not `GET /board/dispatch?rigs=...`: the request carries three lists (exclude labels,
exclude types, probe ids) that can be long, and the plan's other batch reads (`/v1/ready`,
`/v1/issues:show-many`) are POST for the same reason. It is a read, so the client passes
`idempotent=true` and retries it.

Request body `api.DispatchBoardRequest`:

| field | type | meaning | replaces |
|-------|------|---------|----------|
| `rigs` | string[] | rig prefixes to read; unknown prefix goes to `errors`, not a failure | the per-rig loop of read 1 |
| `exclude_labels` | string[] | labels a ready bead must not carry (`specReadyArgs`) | `--exclude-label` |
| `exclude_types` | string[] | issue types to leave out (`specdispatch.NonWorkBeadTypes`) | `--exclude-type` |
| `held_statuses` | string[] | statuses that mean a seat holds the bead (`specHeldStatuses`) | the status filter of read 3 |
| `probe_ids` | string[] | bead ids whose status is wanted (revert beads) | read 4 |

Response `api.DispatchBoardResponse`:

| field | type | meaning | replaces |
|-------|------|---------|----------|
| `rigs` | map[string]RigBoard | one entry per requested rig that was read | reads 1, 2, 3, 5 |
| `rigs[r].ready` | Issue[] | ready, unassigned, unexcluded issues of rig r, full rows (never null) | read 1 and read 2 |
| `rigs[r].held` | HeldBead[] | `{id string, type string, assignee string, description string}` for issues in `held_statuses` with an assignee (never null) | read 3 |
| `rigs[r].children` | map[string]ChildStatus[] | for each ready issue with children: `{id string, status string}` of its direct children, wisps included; absent when none | read 5 |
| `probes` | map[string]Probe | `{found bool, status string}` per requested id | read 4 |
| `errors` | map[string]string | rig prefix -> reason a rig could not be read | the per-rig error list of `specBoardRead` |

Request:

```json
{
  "rigs": ["gastown", "om"],
  "exclude_labels": ["..."],
  "exclude_types": ["..."],
  "held_statuses": ["hooked", "in_progress"],
  "probe_ids": ["gt-revert1"]
}
```

`rigs` is the operational rig list `gt` already computes (it decides parked and docked); the
tracker does not know rig status. The exclude lists are `specReadyArgs()`'s, passed through so the
dispatcher's exclusions stay in one place (`gt`). `held_statuses` is `specHeldStatuses`. `probe_ids`
are the revert beads named by the state files `gt` reads from disk.

Response:

```json
{
  "rigs": {
    "gastown": {
      "ready": [ { "...full Issue..." } ],
      "held": [ { "id": "gt-x", "assignee": "...", "description": "..." } ],
      "children": { "gt-parent": [ { "id": "gt-c1", "status": "open" } ] }
    }
  },
  "probes": { "gt-revert1": { "found": true, "status": "open" } },
  "errors": { "om": "..." }
}
```

- `ready`: the same set as the store's `Ready` with `Unassigned`, the exclude lists and no limit,
  but as full issues: description, design, notes, acceptance, labels, comment count, ephemeral,
  created_at (everything `specFromIssue` reads). That removes read 2 entirely. Ordering is not the
  tracker's job (`specdispatch.Order`).
- `held`: issues of the rig in `held_statuses` that have an assignee, with the description, so
  `gt` can compute `specHoldFiles` itself. Epics are filtered by `gt` (`c.IsEpic()`); the tracker
  returns the type too. This replaces read 3, which is now always cheap, so `gt` can drop its lazy
  per-rig loading.
- `children`: for every `ready` issue that has children, its direct children with their status
  (id and status only, as `childSpecs` reduces them). Wisps count as children. A ready issue with
  no children is absent. This replaces read 5.
- `probes`: status of each requested id, `found:false` when it does not exist (the caller treats
  missing as not open, as `revertBeadOpen` does). This replaces read 4.
- `errors`: a rig that could not be read is named here and the rest are still returned, like
  `specCandidates` reporting and skipping a rig.

Size: today's gt board is ~100 ready issues; with full text roughly 100-300 KB of JSON. The
response is one HTTP call, so that is acceptable; if it grows, `ready` can drop `notes` for rows the
caller already holds (the tick ledger), but do not add that until measured.

## Reads that must stay separate

- **Pre-sling re-read (read 6).** The board is a snapshot taken at the start of the tick; the
  re-read at 1194 exists to see the bead as it is when the seat is about to be spent, after the
  other candidates were processed and slings began. Folding it into the board would defeat it.
  It stays one `Show` per slung candidate, bounded by `max_per_tick`.
- **Roster (read 7).** It is town-wide and answers which sessions are live; only part of it is
  tracker state, and that part is the subject of R4 (`/assigned`).
- **Writes** (annotate, add label, comment, sling hooks) are not reads and are untouched.

## Calls before and after

Per tick, rigs R=8, candidates C (those reaching the container check), probes P (0-1 per rig):

- before: R (ready) + up to R (designs) + up to R (held) + P + C (children) = 3R+P+C reads, each
  a subprocess; ~108 measured.
- after: **1** board call per tick, plus read 6 (`s` calls, `s <= max_per_tick`, usually 0-3), plus
  read 7 (1, until R4). A tick with 8 rigs, 100 candidates and 3 slings goes from ~110 calls to 5.

Measured on 2026-10-25 against the real gt+hq data in Postgres: ready board for gt 13.5 ms, show-many
of 100 ids 2.7 ms, children of 100 parents 1.2 ms (see `claude-477`). The board is those queries
for 8 rigs, so well under 200 ms on the same data.

## Not in the board

- Roster (read 7) reads live sessions and the town store; it is not per rig and mostly not tracker
  state. Route it through `/assigned` (research bead R4), not this endpoint.
- The failed-label count (`LabeledFailed`) and skipped board-carried labels come from `ready` rows'
  labels, which the board carries; `gt` keeps counting them.
- Gates: none exist in the data (no gate rows), so no gate fields.

## Implementation notes for V13

- Build it from the existing store methods in one pass per rig: `Ready` (with `Unassigned`,
  excludes, no limit), `ListIssues` on `held_statuses` for the rig, and `ChildrenOf` for the
  ready ids. Add a `Store.DispatchBoard(ctx, DispatchBoardRequest)` that runs them in one read
  transaction (snapshot-consistent: the board must not show a bead as ready that the held list
  shows as hooked).
- The ready query must stay unlimited (`--limit 0` today); the store's page size must not truncate
  it silently, so the method pages internally and the response carries no cursor.
- Contract cases: a ready bead and a held bead in different rigs; a parent with an open and a closed
  child (wisp included); a probe for a missing id; one unreadable rig (an unknown rig name) that
  must not fail the rest; exclude lists applied; description and design present on the rows.
- Agreement test: the real client against the real route with a stub store.
