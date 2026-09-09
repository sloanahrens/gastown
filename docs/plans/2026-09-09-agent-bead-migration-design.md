# Agent-bead migration completion: one authoritative row per agent

Date: 2026-09-09
Status: approved design (overseer, mayor session)
Tracking: hq-kt9y1 (sweep), gt-8we (prior migration), gt-abj, gt-8po
Scope: gastown only. No change to `bd`.

## Problem

Rig-scoped agent beads (`gt-gastown-polecat-*`, `gt-gastown-witness`, …)
exist twice: a legacy row in the town database (`hq`) and a canonical row in
the rig database (`gt`). gt-8we made creation rig-canonical and gave
`ForAgentBead()` a rig-first / town-fallback resolver, but the legacy rows
were never retired and some writers still reach the town row:

- `bd` resolves an ID against the *local* store before consulting
  `routes.jsonl` (`cmd/bd/routed.go`). Any shell `bd update` run from `~/gt`
  therefore lands on the `hq` row. `internal/witness/handlers.go:2774` does
  exactly this for agent beads.
- `ListAgentBeads` and other list-style operations on the agent-scoped
  wrapper still query the town database.

Census on 2026-09-09: all 30 gastown polecats duplicated, 10 diverged.
Consequences observed: garnet and shale carry a ghost `active_mr` on the
canonical row (the clearing write went to `hq`), leaking two slots; jade's
canonical row reports `spawning` while `hq` says `done`; `gt agents resolve`
and `gt agents list` disagree.

## Decisions

1. **The rig database is the single authoritative store for rig-prefixed
   agent beads.** `hq` holds only `hq-` agents (mayor, deacon, dogs).
2. **`bd` is not changed.** Local-first resolution stays; the fix is that no
   shadow row exists and no gastown code path can create one.
3. **Legacy `hq` rows are reconciled into the rig row, archived, then
   deleted** — one ID per command, reviewed, reversible via Dolt history and
   the archive file. Closing or relabelling is insufficient because a closed
   local row still shadows `bd show`.
4. Out of scope, tracked separately: om/beads witness and refinery bead
   naming (`om-witness` vs `om-om-witness`); the `done`→`idle` state
   machine (`TransitionPolecatToIdle` has no callers); route-first
   resolution in `bd`.

## Components

### 1. Resolution contract — `internal/beads`

- `ForAgentBead()` remains the only door for agent-bead operations. For an
  ID whose prefix belongs to a rig, every operation (Show, Update, Create,
  the `AgentField` helpers, and lists) targets that rig's database. The town
  fallback for rig-prefixed IDs is removed, along with the resolver cache it
  needed. For `hq-` IDs the wrapper targets the town database as today.
- `ListAgentBeads` / `ListAgentBeadsFromWisps` take the scope from the
  wrapper: rig-prefixed roles list from the rig database; town roles from
  `hq`. Callers that need "every agent in the town" iterate rigs explicitly.
- `NewRigLocal(workDir)` stays as the pinned wrapper for doctor.

Interface: no signature changes. Behaviour change is limited to where a
rig-prefixed ID is looked up when the rig row is missing: it is now a
not-found error instead of a silent read of the legacy town row. Callers
that currently rely on that fallback (if any are found in the audit) are
fixed to create or read the rig row.

### 2. Writer audit and structural guard — `internal/witness`, `internal/cmd`

- `handlers.go:2774` (completion-metadata clearing) uses
  `ForAgentBead().UpdateAgentDescriptionFields` instead of shell `bd update`.
- Audit every call site that touches an agent bead (grep for
  `agentBeadID`, `AgentBeadID`, `WitnessBeadID*`, `RefineryBeadID*`,
  `PolecatBeadID*`, `CrewBeadID*`) and route it through the wrapper.
- Structural test (model: `internal/polecat/workstate_constructor_test.go`)
  that walks non-test Go files outside `internal/beads` and fails when
  either pattern appears:
  (a) an argument matching an agent-bead identifier expression is passed to
  `bd.Run`, `BdCmd`, or `exec.Command("bd", …)`;
  (b) an agent-bead helper (`UpdateAgentState`, `UpdateAgentCleanupStatus`,
  `UpdateAgentDescriptionFields`, `CreateAgentBead`,
  `CreateOrReopenAgentBead`, `ResetAgentBeadForReuse`, `ListAgentBeads`) is
  called on a wrapper that is not agent-scoped (`ForAgentBead()` /
  `NewRigLocal`).
  The test is approximate by construction; it must catch every pattern found
  in the audit and the two seed cases above.

### 3. Shadow-row detection — `internal/doctor`

New check `agent-beads-shadow`. For each rig, list `gt:agent`-labelled IDs
in `hq` whose prefix is that rig's. Each hit is reported with both rows'
`updated_at` and the set of differing agent fields. The check has no
`--fix`; its message names the reconcile command. `agent-beads-exist --fix`
continues to create rig-local rows only.

### 4. Data repair — `gt polecat identity reconcile <rig>/<name>`

One ID per invocation. `--dry-run` (default) prints a table:

    field           rig(gt)                     hq                      winner
    agent_state     done   @16:36Z              done   @01:07Z          rig
    active_mr       gt-wisp-0yhh (MISSING)      null                    clear

Winner rule, per field: the copy with the newer `updated_at` wins, except
that `active_mr` and `hook_bead` values referencing a bead that no longer
exists are cleared, never copied. `--apply`:

1. writes the merged fields to the rig row through `ForAgentBead()`;
2. appends the full `hq` row (JSON) to
   `~/gt/.beads/archive/agent-bead-legacy.jsonl`;
3. deletes the `hq` row;
4. re-reads both stores and fails loudly if the rig row does not carry the
   merged fields or the `hq` row still exists.

If step 3 fails after step 2, the command reports the archive line and
stops; re-running is safe because step 1 is idempotent and step 2 appends
a duplicate line at worst. Deletion goes through `bd delete` against the
town store; Dolt history retains the row.

### 5. Sequencing

1. Components 1–3 land through the refinery and the om editorial gate.
2. Binary installed; `gt doctor` shows `agent-beads-shadow` reporting the
   30 gastown IDs (and any om/be hits).
3. Reconcile the 10 diverged IDs first, each `--dry-run` reviewed by the
   overseer before `--apply`; then the identical ones (`--apply` is
   delete-only when the table has no differing fields).
4. `agent-beads-shadow` reports 0. Live acceptance below passes.

Repairs run from the mayor session, one command per ID, never from a
patrol formula.

## Error handling

- Resolver: a rig-prefixed ID with no rig row is an error surfaced to the
  caller; no silent town read.
- Reconcile: refuses to run on an ID with no `hq` row (nothing to do) or no
  rig row (would be a create, not a reconcile — points at `doctor --fix`).
- Doctor: a store that cannot be opened is reported as a warning for that
  rig, not a clean result.

## Testing

Unit (`internal/beads`): rig-prefixed ID never reaches the town store on
Show/Update/Create/List; `hq-` ID never reaches a rig store; missing rig row
is an error. Unit (`internal/cmd`): winner rule including ghost-reference
clearing; archive-before-delete ordering; post-apply verification failure
is reported. Structural: the guard test fails on a fixture containing each
banned pattern and passes on the tree. Doctor: fixture with a shadow row is
reported with the differing fields; none → clean.

Live acceptance, pre-registered:

- `bd show gt-gastown-polecat-garnet` from `~/gt` and from
  `~/gt/gastown/refinery/rig` return the same `updated_at`.
- `gt polecat list gastown` shows garnet and shale without `active_mr`;
  `gt sling` can reuse them.
- `gt agents resolve --role witness --rig om` and `gt agents list` agree.

## Rollback

Code: revert the merge. Data: each deleted `hq` row is in
`agent-bead-legacy.jsonl` and in Dolt history; restoring one is a single
`bd create`/import of that line into `hq`, after which the shadow returns
and doctor reports it.
