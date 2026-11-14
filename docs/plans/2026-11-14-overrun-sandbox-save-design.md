> Status: approved by the operator, 2026-11-14 (approach A, grilling round 1 and the design). Bead: gt-wfulze.

# The overrun stop saves a sandbox seat's work from inside its sandbox

Design, 2026-11-14. Bead gt-wfulze. The plan is
`2026-11-14-overrun-sandbox-save-plan.md`.

## Problem

The overrun guard (`internal/daemon/overrun_guard_stop.go`) stops a seat after
its time limit and is meant to keep the seat's work. For a seat that runs in a
sandbox, the work is in the sandbox's clone, not in the host worktree under the
town root. The host worktree of a sandbox seat is a stub that stays at main.

Two incidents on 2026-11-14 looked like lost work and were not:

- gastown/toast on gt-74tzuk (stopped 07:41): the commit was on origin as
  `polecat/toast/gt-74tzuk+mwgfa5bs`. The guard's log line "origin does not hold
  <branch> at <tip>" named the tip of the stub.
- gastown/imperator on gt-z1cyq6 (stopped 06:08): origin held the branch when the
  guard pushed (the push was refused as non-fast-forward). The landing worker
  deleted it as stale at 06:48 after another seat landed the bead.

The gap that remains is read from the code, and no incident has shown it yet:

- `overrunSaveWork` and `overrunWorktreeDirty` resolve the seat's worktree with
  `resolvePolecatWorktree`, the host stub. For a sandbox seat they see a clean
  tree, so uncommitted edits in the sandbox are never committed.
- The only protection for a sandbox seat is `removeSandbox`, which pushes the
  sandbox clone's committed branch (`session.SandboxPusher`, `pushWork`) and then
  deletes the sandbox. Uncommitted edits die with it.
- A sandbox that is already gone maps to `session.ErrNothingToPush`, which the
  stop reads as nothing to lose, even when origin does not hold the branch.
- Several log lines describe the stub as if it were the seat's work, which is
  how the two incidents were misread.

Earlier work: gt-yegkzo (11-09) added the save step and its WIP commit on the host
worktree; gt-0361s2 and gt-mqxa26 classify a refused push; gt-ao7zh treats a
refused push that adds nothing to origin as done.

## Decision

Route the git handle. `git.Git` already runs every call through an injectable
`exec` function. For a sandbox seat the guard builds a `Git` whose `exec` runs
`git -C <clone dir>` inside the seat's sandbox, through the same exec the sandbox
pusher uses. The save step, the dirty check, the push and the post-push reset
take their handle from one function, `overrunSeatGit(seat)`, and so run the same
code for both kinds of seat. The checkpoint guards (conflict and rebase skip,
revert guard, runtime-path exclusion, unstage on refusal) apply to the sandbox
clone unchanged.

Rejected: a shell snippet in the sandbox that adds, commits and pushes (it copies
the checkpoint guards, and the copies drift), and copying the sandbox clone to the
host before running today's code (a new transfer path, double the storage, a
slower stop).

## Components

1. `overrunSeatGit(seat) (daemonGit, string, error)` in the daemon. A host seat
   gets `d.gitAt(hostWorktree)`. A seat with a sandbox record gets the in-sandbox
   handle on the record's clone directory. A record that does not parse is the
   existing `errSandboxRecordCorrupt` path.
2. `checkpointWorktreeAs` and `overrunPushBranch` take a `daemonGit` and a label
   instead of a directory, so both runners reach them. The directory forms stay
   as thin wrappers for the checkpoint dog.
3. An exported constructor in `internal/git` that builds a `Git` over a caller's
   exec function, and an in-sandbox runner in `internal/session` built from a
   `SandboxRecord`, using the existing `cloneGit` exec. A sandbox that is gone
   returns `ErrNothingToPush` from the runner.
4. The sandbox-gone rule (below) and the log wording.

## Sandbox gone

When the stop finds the sandbox gone, it asks origin whether it holds the seat's
branch. If it does, the stop proceeds as today. If it does not, the stop logs a
WARNING that names the seat, the branch and what origin holds, raises an
attention item for the overseer, and still hands the bead back, because nothing
can be saved and the bead must not stay hooked. It never writes "treating the
push as done" for a case it cannot see.

## Logs

Every line that names a tip or a worktree says which one it examined: the
sandbox clone, or the host stub. The two lines from the incidents are reworded:
"origin refused the push ... treating the push as done" and "origin does not
hold <branch> at <tip>; worktree left as it is".

## Testing

Unit tests only, through the guard's existing seams. The fake runs real git on a
temp repository that stands in for the sandbox clone, so the checkpoint and push
code under test is the production code. The reproduction is a sandbox seat with a
dirty clone and a clean host stub: today's code saves nothing and the test fails;
with the routed handle it commits a WIP commit and pushes it. A failing test
cannot land, so that test rides with the bead that fixes it and is written first
in that bead. Mutations: route a sandbox seat back to the stub, and the test
fails.

## Out of scope

The host worktree stub itself (removing it, or making it track the sandbox); the
landing worker's stale-branch deletion; the seat-never-woke problem, which is a
trace bead of its own (the plan's last task) and is not designed here.

## Open question for the trace bead

A seat that escalated for help and was answered by a bead comment plus the
escalation's close did not resume: gastown/toast sat at its prompt from 07:19 to
the 07:41 stop. The trace bead finds the actual wake path and writes what it
finds. It changes no code.
