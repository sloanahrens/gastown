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
   compare-and-release: it reads the bead and releases it only while the
   status is `hooked` or `in_progress` and the assignee is still this
   polecat. The write is `bd update --status=open --assignee=
   --if-assignee=<polecat>`. That write is atomic, and it is the claim
   transfer bd allows on an `in_progress` bead. bd exit 13 means the guard no
   longer held, so the helper skips the bead. A bead that was re-slung,
   closed or released stays as it is.
   - Nuke calls the helper after the preserve gate and before removal,
     because removal resets the agent bead itself.
   - A polecat reaped before its nuke has no record of its work. Nuke reads
     the work bead off the agent bead's `hook_bead` instead.
   - Before any release, every path asks the work-survival predicate (rule 4).
     Surviving work keeps the hook.
2. **One deferred guard per sling.** `runSling` and `runSlingFormula` arm a
   guard right after `resolveTarget`. They mark success at a single commit
   point: the work is hooked and any polecat this sling spawned has a running
   session. Any other exit runs the rollback exactly once, whether it returns
   an error or `nil`. Early returns set a reason and nothing else. A dry run
   never rolls back. With no polecat spawned, the guard owns nothing once the
   hook lands.
   - The auto-convoy stays open (gt-yg24). The one exception is a
     raw-metadata failure, which closes it.
   - A formula wisp this sling created and did not commit is burned.
   - A wisp still hooked to a just-spawned polecat is stale. It is burned
     before dispatch, where the old code reported "already hooked, no-op".
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

4. **One work-survival predicate.** Work survives for bead B when two things
   hold. A generated `polecat/*` branch for B exists, either in the rig repo
   or on origin. And `git cherry origin/<default> <branch>` shows at least one
   `+` line, meaning a patch that is not on main. Patch identity stays correct
   under rebase and squash merges, where an ancestry check reports merged work
   as unmerged. A branch equal to main, fully merged or empty does not survive.
   - The predicate is `polecat.WorkSurvival` / `SurvivingWorkForIssue`.
   - Every path that releases a hooked bead asks it first:
     - `gt polecat nuke`;
     - polecat removal (`unassignWorkBeads`);
     - the witness `resetAbandonedBead` and `DetectOrphanedBeads`;
     - the witness formula's orphan step, through
       `gt polecat surviving-work <bead>` (exit 0 = branch printed, 1 = none,
       2 = unknown);
     - sling's re-sling guard.
   - Surviving work keeps the hook. An unknown answer keeps it too, except in
     sling, which proceeds so a network outage cannot block dispatch. A rig
     with no git repo has no branch to protect.
   - Every release, including removal's and the witness's, is the guarded
     `--if-assignee` write.
   - Nuke writes the "resume with `--branch`" comment only after removal, and
     only if the bead is still hooked to the nuked polecat.

## Invariants the tests pin

- Each post-spawn early exit that a test can reach runs the rollback exactly
  once, and the success path runs none.
  - `runSling`: the molecule read, burn and refuse, formula instantiation,
    the assignee lock, raw metadata (which also closes the convoy), hook and
    session.
  - `runSlingFormula`: the formula lookup, the stale-wisp burn, admission,
    cook, wisp, hook and session. After the hook, the wisp is burned.
  - The guard also covers exits that no test reaches with a spawned polecat:
    the cross-rig check after a spawn, the dog-only exits, and the
    existing-formula mode update.
- A reused polecat that fails keeps its worktree, has its hook released and
  its slot reset.
- A fresh spawn that fails has its worktree removed.
- A resumed branch is never deleted. A created branch is handed to
  `deletePolecatBranch`, which keeps a tip that has unpushed commits.
- Nuke and rollback release a bead only if its assignee still matches the
  polecat and its status is held. This holds for `in_progress` beads under
  bd's write fence, and when the bead is re-assigned between the read and
  the write.
- The predicate pins these cases against a local bare origin:
  - an unmerged branch survives;
  - a branch equal to main does not;
  - a merged branch does not;
  - a rebase-merged branch does not;
  - a local-only branch with unpushed work survives;
  - an unreachable origin is unknown.
- Polecat removal keeps a bead whose work survives and releases one whose
  branch is merged, using the guarded write.
- The witness keeps surviving and unknown work hooked, and makes a guarded
  reset otherwise.
- The nuke flow runs decide, remove, report in order. Kept work gets exactly
  one comment and no release attempts. Merged work is released. A polecat
  reaped before its nuke is handled through its `hook_bead`.
- A failure before the sling writes to the bead neither burns the bead's
  molecules nor releases its hook.
