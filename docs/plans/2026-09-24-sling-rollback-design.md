> Status: design approved (2026-09-24), implemented in claude-i1m MR-G1.

# Sling rollback and nuke restore the polecat-to-bead binding

Date: 2026-09-24. Beads: gt-7evi4 (rollback skipped on early returns), gt-vm5g4
(nuke leaves the bead hooked to a missing polecat).

## Problem

Two teardown paths leave the bead and the polecat out of step.

- `gt polecat nuke` removes the sandbox but never releases the work bead. The
  bead stays `hooked` with `assignee=<rig>/polecats/<name>`, so
  `gt session restart` fails with "polecat not found". The bead waits until a
  witness patrol runs `resetAbandonedBead`.
- `gt sling` spawns or reuses a polecat inside `resolveTarget`, before a dozen
  guards. Eleven of those guards return without a rollback (four in
  `runSling`, seven in `runSlingFormula`). Each one leaves behind a polecat with
  no session. The rollback that does run assumes this sling created
  everything. It removes a reused persistent sandbox (gt-4ac) and deletes a
  resumed `--branch` ref, which may hold the only copy of the work.

## Rules

1. **One shared helper.** `releasePolecatWork` releases the hooked bead and,
   when asked, resets the polecat's slot (clears `hook_bead`, sets
   `agent_state=idle`). Both nuke and sling rollback call it. It is
   compare-and-release: it re-reads the bead and returns it to `open` only
   while the status is `hooked` or `in_progress` and the assignee is still
   this polecat. A bead that was re-slung, closed or released stays as it is.
   Nuke calls it after the preserve gate and before removal, because removal
   resets the agent bead itself.
2. **One deferred guard per sling.** `runSling` and `runSlingFormula` arm a
   guard right after `resolveTarget`. They mark success at a single commit
   point: the work is hooked and any polecat this sling spawned has a running
   session. Any other exit runs the rollback exactly once, whether it returns
   an error or `nil`. Early returns set a reason and nothing else. A dry run
   never rolls back. With no polecat spawned, the guard owns nothing once the
   hook lands.
3. **Undo only what this sling created.** `SpawnedPolecatInfo` records
   `FreshSpawn` and `BranchCreated`. Both default to false, which means "not
   ours, keep it". Rollback, and `cleanupSpawnedPolecat` for batch and queue
   dispatch, follow the same rules:
   - Remove the sandbox only when `FreshSpawn` is true. A reused sandbox is
     kept, and its slot is reset to idle.
   - Delete the branch only when the sandbox was fresh and `BranchCreated` is
     true. `deletePolecatBranch` also keeps any branch whose tip is not on a
     remote.
   - Release or burn bead state only after this sling has written to that
     bead. Before that point the rollback bead is empty.

## Invariants the tests pin

- Each post-spawn early exit runs the rollback exactly once, and the success
  path runs none. `runSling` covers the molecule read, burn and refuse, the
  cross-rig guard, formula instantiation, the assignee lock, raw metadata,
  hook and session. `runSlingFormula` covers the formula lookup, the mode
  update, the existing-formula no-op, cook, wisp, hook and session.
- A reused polecat that fails keeps its worktree, has its hook released and
  its slot reset.
- A fresh spawn that fails has its worktree removed.
- A resumed branch is never deleted. A created branch is handed to
  `deletePolecatBranch`, which keeps a tip that has unpushed commits.
- Nuke and rollback release a bead only if its assignee still matches the
  polecat and its status is held.
- A failure before the sling writes to the bead neither burns the bead's
  molecules nor releases its hook.
