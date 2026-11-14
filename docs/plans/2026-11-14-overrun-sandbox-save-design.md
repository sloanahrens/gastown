> Status: historical (2026-11). Merged in: gt-wfulze. Not maintained.

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

The in-sandbox runner passes argv to the sandbox client as separate elements, with no shell and no joined string. It cannot tell which argument is a branch, so it refuses a clone directory that is empty, relative, or holds `..` or a newline, a first argument outside the list of git verbs the overrun save uses, and any argument that is an option able to change what git runs (`-c`, `-C`, `--git-dir`, `--work-tree`, `--exec-path`, `--upload-pack`, `--receive-pack`, `--exec`, `--config-env`) or holds a newline or NUL, before anything is sent. The branch name is checked by the overrun guard with `validCloneBranch` before any `Git` call.

The sandbox handle is injected through a `Daemon` field shaped like `openGitFn`, so tests never steer behavior through a package variable. An `ErrNothingToPush` from the sandbox handle during the save or the dirty check is never read as clean: it becomes `errSandboxGone`, which the stop routes to the sandbox-gone rule below.

## Sandbox gone

When the stop finds the sandbox gone, it asks origin whether it holds the seat's
branch. There are three outcomes:

- Origin holds the branch: the stop proceeds as today. Nothing is raised.
- Origin answered that it does not hold the branch: the stop logs a WARNING that
  names the seat, the branch and what origin holds, raises an attention item for
  the overseer, and hands the bead back, because nothing can be saved and the
  bead must not stay hooked.
- The read of origin itself failed (network, auth, timeout): the answer is
  unknown, so the stop logs the WARNING with the error, raises the attention
  item, and does NOT release the bead. The retry is counted in the stop's persisted state like `SaveFailures` and capped at `overrunSaveMaxAttempts`; at the cap the stop hands the bead back with a note and raises the attention item at high severity. `originBranchSHA` returns an empty string for both an absent branch and a failed read, so the stop uses a new helper, `originBranchRead`, that returns the error separately; `originBranchSHA` stays for its other callers. A silent
  hand-back on an unreadable origin is the incident this design exists to
  prevent.

It never writes "treating the push as done" for a case it cannot see.

## Logs

Every line that names a tip or a worktree says which one it examined: the
sandbox clone, or the host stub. The two lines from the incidents are reworded:
"origin refused the push ... treating the push as done" and "origin does not
hold <branch> at <tip>; worktree left as it is".

## Testing

Unit tests only, through the guard's existing seams, and in-process: no git binary runs. The sandbox clone is a scripted fake exec (a `git.ExecFunc` that answers each git call from a table and records the calls), so the checkpoint and push code under test is the production code and the test asserts the exact calls it makes. The reproduction is a sandbox seat with a
dirty clone and a clean host stub: today's code saves nothing and the test fails;
with the routed handle it commits a WIP commit and pushes it. A failing test
cannot land, so that test rides with the bead that fixes it: it is written first and seen to fail, and the bead submits one squashed commit, so the red state never reaches the landed tree. Mutations: route a sandbox seat back to the stub, and the test
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
