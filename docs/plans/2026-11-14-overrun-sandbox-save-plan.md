> Status: historical (2026-11). Merged in: gt-wfulze (the operator approved the design and the plan on 2026-11-14). Not maintained.

# Overrun sandbox save: implementation plan

> **For agentic workers:** every task below is one Gas Town bead of at most twenty minutes of one polecat's work; the spec text under each is the bead's description.

**Goal:** the overrun stop saves a sandbox seat's work from inside its sandbox.

**Architecture:** route the git handle (design, Decision): one seam, `overrunSeatGit`, gives the host handle or an in-sandbox handle, and the existing save, dirty check, push and reset code runs over it.

**Spec:** `docs/plans/2026-11-14-overrun-sandbox-save-design.md`

## Order

- Parallel from the start: T1, T3, T4, T8.
- T2 after T1. T5 after T2, T3 and T4. T6 and T7 after T5.
- Every task depends on the docs bead gt-wfulze landing first (the design is on main).
- A failing test cannot land, so T5 writes its reproduction test first, watches it fail, fixes the code, and submits one squashed commit: the red state never reaches the landed tree.

## Global constraints

- Tests only through the guard's existing seams; no sleeps, no network, no raised timeouts (docs/standards/test-engineering.md).
- Host seats keep today's behavior byte for byte until T5.
- Commits are authored by the agent that makes them, with no attribution lines.

## T1: overrun save: the checkpoint takes a git handle, not a directory

Depends on: docs bead only

### Goal
checkpointWorktreeAs in internal/daemon/checkpoint_dog.go opens its own git handle from a directory, so a caller cannot run it over any other git runner. Add checkpointWith(g daemonGit, rigName, polecatName, message string) holding the whole body, and make checkpointWorktreeAs a one-line wrapper that opens the handle with d.gitAt(workDir) and calls it. No behavior change. Design: docs/plans/2026-11-14-overrun-sandbox-save-design.md (Components, item 2).

### Constraints
Files: internal/daemon/checkpoint_dog.go and its tests only. The checkpoint dog and overrunSaveWork keep calling checkpointWorktreeAs. Every guard in the body (conflict and rebase skip, revert guard, runtime-path exclusion, unstage on refusal) stays exactly as it is. Tests use the existing openGitFn fake; no sleeps, no network.

### Out of scope
The overrun guard's own functions (later beads), any sandbox code, log wording.

### Gate
go test ./internal/daemon -run Checkpoint -count=1

### Size
one worker, one landing

### Acceptance

- [ ] Every existing checkpoint test passes unchanged against the new wrapper
- [ ] A test calls checkpointWith with a fake handle that is not the one d.gitAt opens, and the result and the staged calls show the fake was used
- [ ] A test shows checkpointWorktreeAs still opens its handle through d.gitAt for the directory it is given
- [ ] Mutating checkpointWorktreeAs to ignore the handle it opened makes the second test fail

## T2: overrun guard: one seam picks the git handle for the save, dirty check, push and reset

Depends on: T1

### Goal
overrunSaveWork, overrunWorktreeDirty, overrunPushBranch and overrunResetWorktree in internal/daemon/overrun_guard_stop.go each resolve the host worktree with resolvePolecatWorktree and open a handle on it. Add overrunSeatGit(seat overrunSeat) (daemonGit, string, error) that returns the handle and a label for the seat, and make the four functions take their handle from it. For every seat it returns the host handle today, so nothing changes yet. Design: docs/plans/2026-11-14-overrun-sandbox-save-design.md (Components, items 1 and 2).

### Constraints
Files: internal/daemon/overrun_guard_stop.go and its tests only. Uses checkpointWith from the checkpoint bead. A worktree that cannot be resolved stays an error, never nothing to save. The label names the worktree kind (host worktree) so a later bead can add the sandbox kind. No behavior change; log lines keep their text.

### Out of scope
Any sandbox handle, the sandbox-gone rule, log wording.

### Gate
go test ./internal/daemon -run OverrunGuard -count=1

### Size
one worker, one landing

### Acceptance

- [ ] Every existing overrun guard save, push and reset test passes unchanged
- [ ] A table test shows overrunSeatGit returns the host handle and a label for a host seat, and an error for a seat whose worktree cannot be resolved
- [ ] A test shows overrunSaveWork, overrunWorktreeDirty, overrunPushBranch and overrunResetWorktree each use the handle overrunSeatGit returns, by swapping in a recording fake
- [ ] Mutating overrunWorktreeDirty to open its own handle again makes the third test fail

## T3: git: build a Git over a caller's exec function

Depends on: docs bead only

### Goal
internal/git/exec.go runs git through an unexported runFunc, so only code inside the package can swap the runner. Add an exported constructor NewGitWithExec(workDir string, exec ExecFunc) *Git and the exported call and result types it needs (a name for gitCall, and an ExecFunc type), so a caller outside the package can run every Git method through its own runner, for example one that execs inside a sandbox. Design: docs/plans/2026-11-14-overrun-sandbox-save-design.md (Components, item 3).

### Constraints
Files: internal/git/exec.go and its tests only. Existing constructors and the nil-exec default (the real git on PATH) do not change. The exported types carry the same fields the unexported ones do, nothing added. No sleeps, no network, no real remote.

### Out of scope
Any sandbox code, any caller outside internal/git.

### Gate
go test ./internal/git -run Exec -count=1

### Size
one worker, one landing

### Acceptance

- [ ] A test builds a Git with NewGitWithExec and a recording ExecFunc, calls Rev and Status, and the recorder sees the git arguments and the working directory
- [ ] A test shows an error returned by the ExecFunc comes back from the Git method unchanged in meaning (a non-zero exit keeps its exit code)
- [ ] A test shows NewGit and the other existing constructors still run the real git and are unchanged
- [ ] Mutating NewGitWithExec to drop the exec function makes the first test fail

## T4: session: a git runner that executes inside a sandbox record's clone

Depends on: docs bead only

### Goal
internal/session/backend_openshell_ops.go has cloneGit, which runs one git command inside a seat's sandbox. Expose it as a runner for a SandboxRecord: SandboxGitExec(townRoot string, rec SandboxRecord) (func(dir string, args ...string) ([]byte, error)), set through a hook variable beside SandboxPusher in sandbox_hooks.go. A sandbox that is already gone returns ErrNothingToPush. Design: docs/plans/2026-11-14-overrun-sandbox-save-design.md (Components, item 3).

### Constraints
Files: internal/session/sandbox_hooks.go, internal/session/backend_openshell_ops.go and their tests only. The runner reuses cloneGit, so the timeout, the error shape and the not-found mapping are the ones the pusher already has. It passes argv to the sandbox client with no shell and no joined string. The directory it runs in is the record's clone dir (cloneDir); the runner refuses a dir that is empty, relative, or holds .. or a newline, and a branch argument that fails validCloneBranch. Tests use the existing fake sandbox client; no network, no sleeps.

### Out of scope
The git package constructor, anything in internal/daemon.

### Gate
go test ./internal/session -run SandboxGit -count=1

### Size
one worker, one landing

### Acceptance

- [ ] A test shows the runner passes git -C <clone dir> and the arguments to the sandbox client and returns its output
- [ ] A test shows a sandbox the client reports as gone makes the runner return ErrNothingToPush
- [ ] A test shows any other client error comes back wrapped with the git verb and the sandbox name
- [ ] A test shows the hook is nil until the implementation is linked in, and a caller that finds it nil gets a clear error
- [ ] A table test shows a clone dir that is empty, relative, or holds .. or a newline, and a branch that fails validCloneBranch, are refused before anything is sent, and the argv reaches the client as separate elements

## T5: overrun guard: a sandbox seat's save, dirty check and push run on its sandbox clone

Depends on: T2, T3, T4

### Goal
For a seat with a sandbox record, overrunSeatGit returns a Git built with git.NewGitWithExec over the in-sandbox runner on the record's clone dir, and a label that names the sandbox. The save step then commits a WIP commit of uncommitted sandbox edits and pushes the branch from the sandbox, and the dirty check reads the sandbox clone, so a stop never deletes a sandbox holding uncommitted work. Host seats are unchanged. Design: docs/plans/2026-11-14-overrun-sandbox-save-design.md (Decision, Testing).

### Constraints
Files: internal/daemon/overrun_guard_stop.go and its tests, and create internal/daemon/overrun_guard_sandbox_test.go, only. Write the failing test FIRST and watch it fail before the fix: a sandbox seat with a dirty clone and a clean host stub, where the clone is a scripted fake exec (a git.ExecFunc that answers each git call from a table and records the calls), never a real git or a temp repository. Submit ONE squashed commit, so the red state never reaches the landed tree. A record that does not parse stays the errSandboxRecordCorrupt path. The host stub is not read for a sandbox seat. No sleeps, no network.

### Out of scope
The sandbox-gone rule, log wording on lines this bead does not touch, the host worktree stub.

### Gate
go test ./internal/daemon -run OverrunSandbox -count=1

### Size
one worker, one landing

### Acceptance

- [ ] A test with a dirty sandbox clone and a clean host stub shows the save step commits a WIP commit named for the bead and pushes it, and the stop proceeds to remove the sandbox only after the push
- [ ] A test shows the dirty check reports a sandbox seat with uncommitted edits dirty even when the host stub is clean
- [ ] A test shows a host seat takes exactly the path it took before (the existing save tests pass unchanged)
- [ ] A test shows a clone mid-rebase is refused by the same guard the host path uses, and the sandbox is kept
- [ ] Mutating overrunSeatGit to return the host handle for a sandbox seat makes the first two tests fail

## T6: overrun guard: a vanished sandbox with the branch missing from origin is a warning, not nothing to lose

Depends on: T5

### Goal
When removeSandbox finds the sandbox already gone (session.ErrNothingToPush) the stop reads it as nothing to lose. If origin does not hold the seat's branch, log a WARNING that names the seat, the branch and what origin holds, raise an attention item for the overseer, and still hand the bead back. If the read of origin itself fails, treat it as the alarming case: log the WARNING with the error, raise the attention item, and do NOT release the bead. If origin holds the branch the stop proceeds as today. Never write 'treating the push as done' for a case the guard cannot see. Design: docs/plans/2026-11-14-overrun-sandbox-save-design.md (Sandbox gone).

### Constraints
Files: internal/daemon/overrun_guard_stop.go and its tests, plus the one attention-item kind the daemon already has a constructor for (reuse it; no new kind unless none fits). The origin read uses originBranchSHA. The bead is handed back exactly once. No sleeps, no network.

### Out of scope
The save step, the host path, log wording on other lines.

### Gate
go test ./internal/daemon -run OverrunSandboxGone -count=1

### Size
one worker, one landing

### Acceptance

- [ ] A test shows a gone sandbox with the branch absent from origin logs the WARNING with seat, branch and what origin holds, raises the attention item, and hands the bead back once
- [ ] A test shows a gone sandbox with the branch on origin proceeds as before and raises nothing
- [ ] A test shows an origin read that returns an error logs the WARNING with the error, raises the attention item, and does not hand the bead back
- [ ] A test shows the words treating the push as done are not written on the gone-sandbox path
- [ ] Mutating the stop to treat ErrNothingToPush as success again makes the first test fail

## T7: overrun guard: log lines name the worktree they examined

Depends on: T5

### Goal
Two overrun guard log lines describe the host stub as if it were the seat's work: 'origin refused the push of <branch> as non-fast-forward, but its tip <tip> adds nothing origin lacks; treating the push as done' (overrunPushBranch) and 'origin does not hold <branch> at <tip>; worktree left as it is' (overrunResetWorktree). Reword every line in overrun_guard_stop.go that names a tip or a worktree so it carries the label overrunSeatGit returns (host worktree or sandbox clone). Design: docs/plans/2026-11-14-overrun-sandbox-save-design.md (Logs).

### Constraints
Files: internal/daemon/overrun_guard_stop.go and its tests only. Wording and the label only; no control-flow change. Tests assert the label appears in the line for a host seat and for a sandbox seat.

### Out of scope
Any logic, the gone-sandbox warning (its own bead).

### Gate
go test ./internal/daemon -run OverrunGuardLog -count=1

### Size
one worker, one landing

### Acceptance

- [ ] A test shows the non-fast-forward line for a host seat names the host worktree and for a sandbox seat names the sandbox clone
- [ ] A test shows the origin-does-not-hold line names the same label
- [ ] A grep-style test lists every Printf in overrun_guard_stop.go that mentions a tip or a worktree and fails when one lacks the label
- [ ] No existing test is changed except to carry the new wording

## T8: trace: why a seat paused on an answered escalation did not resume

Depends on: docs bead only

### Goal
gastown/toast on gt-74tzuk escalated for help at 07:21, the overseer answered with a bead comment and closed the escalation at 07:24, and the seat stayed idle at its prompt until the overrun stop at 07:41. Find, from the code and the daemon log, which path is meant to deliver an answer to a paused or escalated seat (gt escalate close, the attention ack, the agent resume, the sandbox seat's session), where it stops for a sandbox seat, and why. Write the finding; change no code. Design: docs/plans/2026-11-14-overrun-sandbox-save-design.md (Open question).

### Constraints
Output is a comment on this bead that names each function and log line followed and the exact point the answer is dropped, plus a recommended fix with its size. Read code from origin/main. Do not nudge, mail or resume any live seat.

### Out of scope
Any code change, any fix.

### Gate
A written finding on this bead

### Size
one worker, one landing

### Acceptance

- [ ] A comment names the code path that is meant to deliver an answer to an escalated seat, with file and function names
- [ ] The comment names the point where a sandbox seat's answer is not delivered, with the daemon log line that shows it
- [ ] The comment recommends one fix and says how many beads of twenty minutes it is
