---
name: convoy
description: The definitive guide for working with gastown's convoy system -- batch work tracking, event-driven feeding, and dispatch safety guards. Use when writing convoy code, debugging convoy behavior, adding convoy features, testing convoy changes, or answering questions about how convoys work. Triggers on convoy, convoy manager, convoy feeding, dispatch, stranded convoy, feedFirstReady, feedNextReadyIssue, IsSlingableType, isIssueBlocked, CheckConvoysForIssue, gt convoy, gt sling.
---

# Gastown Convoy System

The convoy system tracks batches of work across rigs. A convoy is a bead that `tracks` other beads via dependencies. The daemon monitors close events and feeds the next ready issue when one completes.

## Architecture

```
+============================ CREATION =============================+
|                                                                   |
|                       gt convoy create ...                        |
|                              |  (explicit)                       |
|                              v                                    |
|                       +-----------+                               |
|                       |  status:  |                               |
|                       |   open    |                               |
|                       +-----------+                               |
|                                                                   |
|   Produces: CONVOY (hq-cv-*)                                      |
|             tracks: issue1, issue2, ...                           |
+===================================================================+
              |                              |
              v                              v
+= EVENT-DRIVEN FEEDER (5s) =+   +=== STRANDED SCAN (30s) ===+
|                              |   |                            |
|   GetAllEventsSince (SDK)    |   |   findStranded             |
|     |                        |   |     |                      |
|     v                        |   |     v                      |
|   close event detected       |   |   convoy has ready issues  |
|     |                        |   |   but no active workers    |
|     v                        |   |     |                      |
|   CheckConvoysForIssue       |   |     v                      |
|     |                        |   |   feedFirstReady           |
|     v                        |   |   (iterates all ready)     |
|   feedNextReadyIssue         |   |     |                      |
|   (iterates all ready)       |   |     v                      |
|     |                        |   |   gt sling <next-bead>     |
|     v                        |   |   or closeEmptyConvoy     |
|   gt sling <next-bead>       |   |                            |
|                              |   +============================+
+==============================+
```

Two creation paths (sling, create), two feed paths, same safety guards:
- **Event-driven** (`operations.go`): Polls beads stores every ~5s for close events. Calls `feedNextReadyIssue` which checks `IsSlingableType` + `isIssueBlocked` before dispatch.
- **Stranded scan** (`convoy_manager.go`): Runs every 30s. `feedFirstReady` iterates all ready issues. The ready list is pre-filtered by `IsSlingableType` in `Town.FindStranded` (`internal/convoy/stranded.go`).

## Safety guards (the three rules)

These prevent the event-driven feeder from dispatching work it shouldn't:

### 1. Type filtering (`IsSlingableType`)

Only leaf work items dispatch. Defined in `operations.go`:

```go
var slingableTypes = map[string]bool{
    "task": true, "bug": true, "feature": true, "chore": true,
    "": true, // empty defaults to task
}
```

Epics, sub-epics, convoys, decisions -- all skip. Applied in both `feedNextReadyIssue` (event path) and `Town.FindStranded` (stranded path).

### 2. Blocks dep checking (`isIssueBlocked`)

Issues with unclosed `blocks`, `conditional-blocks`, or `waits-for` dependencies skip. `parent-child` is **not** blocking -- a child task dispatches even if its parent epic is open. This is consistent with `bd ready` and molecule step behavior.

Fail-open on store errors (assumes not blocked) to avoid stalling convoys on transient Dolt issues.

### 3. Dispatch failure iteration

Both feed paths iterate past failures instead of giving up:
- `feedNextReadyIssue`: `continue` on dispatch failure, try next ready issue
- `feedFirstReady`: `for range ReadyIssues` with `continue` on skip/failure, `return` on first success

## CLI commands

### Create and manage

```bash
gt convoy create "Auth overhaul" gt-task1 gt-task2 gt-task3
gt convoy create "Deploy v2.0" gt-abc bd-xyz
gt convoy create "Feature rollout" gt-a gt-b --owner mayor/ --notify ops/
gt convoy create --from-epic gt-epic-abc         # discover an epic's slingable children
gt convoy create "Manual deploy" gt-abc --owned  # caller-managed lifecycle
gt convoy add hq-cv-abc gt-task4                 # add issues (reopens if closed)
```

`--owner` sets who receives the completion notification (defaults to `created_by`); `--notify` adds subscribers; `--merge` sets the merge strategy (`mr` merge-queue default, or `local` for upstream PRs / human review).

### Check and monitor

```bash
gt convoy check hq-cv-abc       # auto-closes if all tracked issues done
gt convoy check                  # check all open convoys
gt convoy check --dry-run        # preview what would close
gt convoy status hq-cv-abc       # single convoy detail
gt convoy status                 # status of all active convoys
gt convoy list                   # open convoys (default)
gt convoy list --all             # include closed
gt convoy list --status=closed   # recently landed
gt convoy list --tree            # convoy + child status tree
gt convoy list --json
```

### Find stranded work

```bash
gt convoy stranded               # ready work with no active workers
gt convoy stranded --json        # machine-readable
```

### Close

```bash
gt convoy close hq-cv-abc                               # Close (all items must be done)
gt convoy close hq-cv-abc --force                       # Force close abandoned convoy
gt convoy close hq-cv-abc --reason "no longer needed" --force
gt convoy close hq-cv-xyz --notify mayor/
```

Closing verifies all tracked issues are closed by default; `--force` skips that check. The close is idempotent.

### Convoy status model

Convoys use `open` and `closed` as their primary lifecycle statuses:

- `open` — active; the daemon feeds tracked work as beads close.
- `closed` — complete or cancelled.

The legacy staged statuses `staged_ready` and `staged_warnings` remain valid
(`EnsureKnownStatus` accepts them) and the daemon treats staged convoys as
inert — but the `gt convoy stage` and `gt convoy launch` commands that created
and transitioned them were removed. There is no CLI path to create or launch a
staged convoy today.

## Batch sling behavior

`gt sling <bead1> <bead2> <bead3>` creates **one convoy** tracking all beads. The rig is auto-resolved from the beads' prefixes (via `routes.jsonl`). The convoy title is `"Batch: N beads to <rig>"`. Each bead gets its own polecat, but they share a single convoy for tracking.

The convoy ID and merge strategy are stored on each bead, so `gt done` can find the convoy via the fast path (`getConvoyInfoFromIssue`).

### Rig resolution

- **Auto-resolve (preferred):** `gt sling gt-task1 gt-task2 gt-task3` -- resolves rig from the `gt-` prefix. All beads must resolve to the same rig.
- **Explicit rig (deprecated):** `gt sling gt-task1 gt-task2 gt-task3 myrig` -- still works, prints a deprecation warning. If any bead's prefix doesn't match the explicit rig, errors with suggested actions.
- **Mixed prefixes:** If beads resolve to different rigs, errors listing each bead's resolved rig and suggested actions (sling separately, or `--force`).
- **Unmapped prefix:** If a prefix has no route, errors with diagnostic info (`cat .beads/routes.jsonl | grep <prefix>`).

### Conflict handling

If any bead is already tracked by another convoy, batch sling **errors** with detailed conflict info (which convoy, all beads in it with statuses, and 4 recommended actions). This prevents accidental double-tracking.

```bash
# Auto-resolve: one convoy, three polecats (preferred)
gt sling gt-task1 gt-task2 gt-task3
# -> Created convoy hq-cv-xxxxx tracking 3 beads

# Explicit rig still works but prints deprecation warning
gt sling gt-task1 gt-task2 gt-task3 gastown
# -> Deprecation: gt sling now auto-resolves the rig from bead prefixes.
# -> Created convoy hq-cv-xxxxx tracking 3 beads
```

## Testing convoy changes

### Running tests

```bash
# Full convoy suite (all packages)
go test ./internal/convoy/... ./internal/daemon/... ./internal/cmd/... -count=1

# By area:
go test ./internal/convoy/... -v -count=1                       # feeding logic
go test ./internal/daemon/... -v -count=1 -run TestConvoy       # ConvoyManager
go test ./internal/daemon/... -v -count=1 -run TestFeedFirstReady
go test ./internal/cmd/... -v -count=1 -run TestSlingCreatesNoConvoy
go test ./internal/cmd/... -v -count=1 -run TestBatchSling
go test ./internal/cmd/... -v -count=1 -run TestResolveRig      # rig resolution
go test ./internal/daemon/... -v -count=1 -run Integration      # real beads stores
```

### Key test invariants

- `feedFirstReady` dispatches exactly 1 issue per call (first success wins)
- `feedFirstReady` iterates past failures (sling exit 1 -> try next)
- Parked rigs are skipped in both event poll and feedFirstReady
- hq store is never skipped even if `isRigParked` returns true for everything
- High-water marks prevent event reprocessing across poll cycles
- First poll cycle is warm-up only (seeds marks, no processing)
- `IsSlingableType("epic") == false`, `IsSlingableType("task") == true`, `IsSlingableType("") == true`
- `isIssueBlocked` is fail-open (store error -> not blocked)
- `parent-child` deps are NOT blocking
- Batch sling creates exactly 1 convoy for N beads (not N convoys)
- `resolveRigFromBeadIDs` errors on mixed prefixes, unmapped prefixes, town-level prefixes
- Daemon does NOT feed issues from staged convoys (status prefixed `staged_`) — both feed paths skip

## Common pitfalls

- **`parent-child` is never blocking.** This is a deliberate design choice, not a bug. Consistent with `bd ready`, beads SDK, and molecule step behavior.
- **Batch sling errors on already-tracked beads.** If any bead is already in a convoy, the entire batch sling fails with conflict details. The user must resolve the conflict before proceeding.
- **The stranded scan has its own blocked check.** `isReadyIssue` in cmd/convoy.go reads `t.Blocked` from issue details. `isIssueBlocked` in operations.go covers the event-driven path. Don't consolidate them without understanding both paths.
- **Empty IssueType is slingable.** Beads default to type "task" when IssueType is unset. Treating empty as non-slingable would break all legacy beads.
- **`isIssueBlocked` is fail-open.** Store errors assume not blocked. A transient Dolt error should not permanently stall a convoy -- the next feed cycle retries with fresh state.
- **Explicit rig in batch sling is deprecated.** `gt sling beads... rig` still works but prints a warning. Prefer `gt sling beads...` with auto-resolution.
- **Staged statuses are inert.** Convoys with `staged_ready`/`staged_warnings` status are ignored by the daemon. No CLI command creates or launches them anymore, so don't document a stage/launch workflow that no longer exists.

## Key source files

| File | What it does |
|------|-------------|
| `internal/convoy/operations.go` | Core feeding: `CheckConvoysForIssue`, `feedNextReadyIssue`, `IsSlingableType`, `isIssueBlocked` |
| `internal/daemon/convoy_manager.go` | `ConvoyManager` goroutines: `runEventPoll` (5s), `runStrandedScan` (30s), `feedFirstReady` |
| `internal/cmd/convoy.go` | `gt convoy` flag parsing and output |
| `internal/convoy/check.go`, `stranded.go`, `tracked.go` | `Town.CheckOne`/`CheckAll`, `Town.FindStranded`, `Town.TrackedIssues`: the logic behind `gt convoy check`, `stranded` and `status`, called in process by the daemon and deacon |
| `internal/cmd/sling.go` | Batch detection at ~242, auto-rig-resolution, deprecation warning |
| `internal/cmd/sling_batch.go` | `runBatchSling`, `resolveRigFromBeadIDs`, `allBeadIDs`, cross-rig guard |
| `internal/cmd/sling_convoy.go` | `createAutoConvoy`, `createBatchConvoy`, `printConvoyConflict` |
| `internal/daemon/daemon.go` | Daemon startup -- creates `ConvoyManager` at ~237 |
