# Polecat Lifecycle

> Understanding the three-layer architecture of polecat workers

## Overview

Polecats have three distinct lifecycle layers that operate independently. The
key design principle: **clean completion retires the live polecat session**.
The agent identity and landing evidence persist, but completed sessions do not
return to the idle reuse pool.

## Operating States

Polecats use these primary operating states:

| State | Description | How it happens |
|-------|-------------|----------------|
| **Working** | Actively doing assigned work | Normal operation after `gt sling` |
| **Idle** | Available before assignment | Spawned or explicitly prepared for work |
| **Done** | Work completed and session retired | After `gt done` completes successfully |
| **Stalled** | Session stopped mid-work | Interrupted, crashed, or timed out without being nudged |
| **Zombie** | Completed work but failed to exit | `gt done` failed during cleanup |

**State cycle (happy path):**

```
         ┌──────────┐
         │  IDLE    │──── gt sling
         └────┬─────┘
              v
         ┌──────────┐
         │ WORKING  │<──── session active, hook set
         └────┬─────┘
              │ gt done
              v
         ┌──────────┐
         │  DONE    │──── branch and bead evidence preserved, session exits
         └──────────┘
```

No idle reuse in the happy path. Polecats move: IDLE -> WORKING -> DONE.

**Key distinctions:**

- **Working** = actively executing. Session alive, hook set, doing work.
- **Idle** = not yet assigned and safe to use.
- **Done** = work done, session killed, waiting for the landing worker and cleanup.
- **Stalled** = supposed to be working, but stopped. The daemon restarts it.
- **Zombie** = finished work, tried to exit, but cleanup failed. Stuck in limbo.

## The Retired Completion Model

**Polecat identity persists after completing work, but the live session does not.**
When a polecat finishes its assignment:

1. Signals completion via `gt done`
2. Rebases onto the target, runs the local gate, pushes the branch and labels
   the work bead `gt:ready-to-land`
3. Clears its hook (work is done)
4. Sets agent state to "done"
5. Kills its own session using PID-excluding cleanup
6. Leaves the branch and the READY TO LAND block on the bead for the landing worker

The next `gt sling` allocates available capacity without reusing a completed
session that still has unlanded work or cleanup state attached.

### Why Retire Sessions?

- **Preserved identity** — The polecat's agent bead, CV chain, and work history persist
- **Simpler lifecycle** — Clean completion has one terminal session path
- **Done means retired** — Session dies, the landing worker and cleanup own remaining state

### What About Pending Landings?

The daemon's landing worker owns landing ([ADR 0004](../adr/0004-daemon-lands-work.md)).
Once `gt done` submits work:
- The branch is pushed to origin
- The work bead carries `gt:ready-to-land`; the work no longer lives in the polecat
- The landing worker merges, gates and pushes, then writes the landing record to the bead
- A rejection is written to the bead and the bead is re-dispatched to a fresh
  polecat at most twice, then parked

## The Three Layers

### The Problem: Three Concepts Were Conflated

Early designs treated polecats as monolithic. This caused recurring issues:

| Concept | Lifecycle | Old behavior |
|---------|-----------|-----------------|
| **Identity** | Long-lived (name, CV, ledger) | Destroyed on nuke |
| **Sandbox** | Per-assignment (worktree, branch) | Destroyed on nuke |
| **Session** | Ephemeral (Claude context window) | = polecat lifetime |

Separating these three layers keeps completed sessions out of the idle reuse pool,
preserves capability records (CV, completion history), and lets the landing
worker and cleanup own branch and worktree state after handoff.

### Layer Summary

| Layer | Component | Lifecycle | Persistence |
|-------|-----------|-----------|-------------|
| **Identity** | Agent bead, CV chain, work history | Permanent | Never dies |
| **Sandbox** | Git worktree, branch | Per active assignment/cleanup window | Created for work, retired after cleanup |
| **Session** | Claude (tmux pane), context window | Ephemeral per step | Cycles per step/handoff |

### Identity Layer

The polecat's **identity is permanent**. It includes:

- Agent bead (created once, never deleted)
- CV chain (work history accumulates across all assignments)
- Mailbox and attribution record

Identity survives all session cycles and sandbox resets. In the HOP model, this IS
the polecat — everything else is infrastructure that comes and goes. See
[Polecat Identity](#polecat-identity) below for details.

### Session Layer

The Claude session is **ephemeral**. It cycles frequently:

- After each molecule step (via `gt handoff`)
- On context compaction
- On crash/timeout
- After extended work periods

**Key insight:** Session cycling is **normal operation**, not failure. The polecat
continues working—only the Claude context refreshes.

```
Session 1: Steps 1-2 → handoff
Session 2: Steps 3-4 → handoff
Session 3: Step 5 → gt done
```

All three sessions are the **same polecat**. The sandbox persists throughout.

### Sandbox Layer

The sandbox is the **git worktree**—the polecat's working directory:

```
~/gt/gastown/polecats/Toast/
```

This worktree:
- Exists while the polecat is active or awaiting cleanup
- Survives handoff/session cycles during an assignment
- Is not synced to main or branch-deleted by `gt done`
- Contains uncommitted work, staged changes, branch state during active work

Cleanup owns sandbox retirement after durable handoff. Explicit
`gt polecat nuke` remains the manual destructive path.

#### Branch Preservation (After Completion)

When work completes, `gt done` leaves the feature branch and the bead's landing metadata intact:

```bash
# Handled by gt done
git push origin polecat/<name>/<issue>@<suffix>
# Branch and metadata remain available to the landing worker and cleanup
```

When new work is slung:
```bash
# Create fresh branch from current main
git checkout -b polecat/<name>/<new-issue>+<timestamp>
# Start working
```

Completed sandboxes are not treated as reusable idle worktrees while unlanded
work or cleanup state remains attached.

### Slot Layer

The slot is the **name allocation** from the polecat pool:

```bash
# Pool: [Toast, Shadow, Copper, Ash, Storm...]
# Toast is allocated to work gt-abc
```

The slot:
- Determines the sandbox path (`polecats/Toast/`)
- Maps to a tmux session (`gt-gastown-Toast`)
- Appears in attribution (`gastown/polecats/Toast`)
- Persists until explicit nuke

## Correct Lifecycle

```
┌─────────────────────────────────────────────────────────────┐
│                        gt sling                             │
│  → Find idle polecat OR allocate slot from pool (Toast)    │
│  → Create/repair sandbox (worktree on new branch)          │
│  → Start session (Claude in tmux)                          │
│  → Hook molecule to polecat                                │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                     Work Happens                            │
│                                                             │
│  Session cycles happen here:                               │
│  - gt handoff between steps                                │
│  - Compaction triggers respawn                             │
│  - Crash → daemon restarts the session                     │
│                                                             │
│  Sandbox persists through ALL session cycles               │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                  gt done (retired model)                     │
│  → Rebase onto target, run local gate                      │
│  → Push branch, label bead gt:ready-to-land                │
│  → Set agent state to "done"                               │
│  → Kill session                                            │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                 Daemon: landing worker                      │
│  → Merge onto target in a throwaway worktree               │
│    (main or integration branch — see below)                │
│  → Gate and review the merged tree, push, read back        │
│  → Close the bead with the landing record                  │
│  → If rejected: record why, re-dispatch (at most twice)    │
└─────────────────────────────────────────────────────────────┘
```

## What "Recycle" Means

**Session cycling**: Normal. Claude restarts, sandbox stays, slot stays.

```bash
gt handoff  # Session cycles, polecat continues
```

**Sandbox setup**: For active work. `gt sling` prepares a fresh branch for the assignment.

```bash
gt sling gt-xyz gastown  # Allocates capacity and prepares branch
```

Session cycling happens constantly. Sandbox setup/cleanup is tied to assignments.

## Anti-Patterns

### Manual State Transitions

**Anti-pattern:**
```bash
gt polecat done Toast    # DON'T: external state manipulation
gt polecat reset Toast   # DON'T: manual lifecycle control
```

**Correct:**
```bash
# Polecat signals its own completion:
gt done  # (from inside the polecat session)

# Only explicit nuke destroys polecats:
gt polecat nuke Toast  # (destroys sandbox, identity persists)
```

Polecats manage their own session lifecycle. External manipulation bypasses verification.

### Sandboxes Without Work (Idle vs Done vs Stalled)

An idle polecat has no hook, no session, and no unlanded work or cleanup state —
this is **available capacity**.

A **done** polecat has completed work and exited, but unlanded work or cleanup state
may still be attached. It is not reusable until cleanup resolves that state.

A **stalled** polecat has a hook but no session — this is a **failure**:
- The session crashed and has not been restarted yet
- The hook was lost during a crash
- State corruption occurred

**Recovery for stalled:**
```bash
# The daemon's patrol_scan tick restarts the session in the existing sandbox
# Or, if unrecoverable:
gt polecat nuke Toast        # Clean up the stalled polecat
gt sling gt-abc gastown      # Respawn with fresh polecat
```

### Confusing Session with Sandbox

**Anti-pattern:** Thinking session restart = losing work.

```bash
# Session ends (handoff, crash, compaction)
# Work is NOT lost because:
# - Git commits persist in sandbox
# - Staged changes persist in sandbox
# - Molecule state persists in beads
# - Hook persists across sessions
```

The new session picks up where the old one left off via `gt prime`.

## Session Lifecycle Details

Sessions cycle for these reasons:

| Trigger | Action | Result |
|---------|--------|--------|
| `gt handoff` | Voluntary | Clean cycle to fresh context |
| Context compaction | Automatic | Forced by Claude Code |
| Crash/timeout | Failure | Daemon restarts |
| `gt done` | Completion | Session exits, polecat goes done |

All except `gt done` result in continued work. Only `gt done` signals completion
and retires the completed polecat session.

## Supervision

The daemon is the only process that kills or restarts a polecat
([ADR 0003](../adr/0003-one-supervisor-no-idle-llm.md)). Its `patrol_scan` tick
([ADR 0005](../adr/0005-patrol-scan-tick.md)) restarts a polecat whose session
is dead on two consecutive samples while it holds work, closes the molecules
of hooked work whose polecat is gone, and comments on stranded work. It does
not force session cycles, interrupt a live session, re-sling or reset work.
A polecat that needs help runs `gt escalate`.

## Polecat Identity

**Key insight:** Polecat *identity* is permanent; sessions are ephemeral, sandboxes are persistent.

In the HOP model, every entity has a chain (CV) that tracks:
- What work they've done
- Success/failure rates
- Skills demonstrated
- Quality metrics

The polecat *name* (Toast, Shadow, etc.) is a slot from a pool — persistent until
explicit nuke. The *agent identity* that executes as that polecat accumulates a
work history across all assignments.

```
POLECAT IDENTITY (permanent)      SESSION (ephemeral)     SANDBOX (assignment-scoped)
├── CV chain                      ├── Claude instance     ├── Git worktree
├── Work history                  ├── Context window      ├── Branch
├── Skills demonstrated           └── Dies on handoff     └── Retired after cleanup
└── Credit for work                   or gt done              by gt sling
```

This distinction matters for:
- **Attribution** - Who gets credit for the work?
- **Skill routing** - Which agent is best for this task?
- **Cost accounting** - Who pays for inference?
- **Federation** - Agents having their own chains in a distributed world

## Key Files

- `internal/cmd/done.go` — work submission, done-state handoff, session retirement
- `internal/cmd/sling.go` + `polecat_spawn.go` — capacity allocation, branch setup
- `internal/cmd/handoff.go` — session cycling for all roles
- `internal/patrolscan/` — dead-session restart, orphaned molecules, stranded work
- `internal/landworker/`, `internal/land/` — the landing worker
- `internal/polecat/manager.go` — stale detection, done-state projection, pool management

## Related Documentation

- [Overview](../overview.md) - Role taxonomy and architecture
- [Molecules](molecules.md) - Molecule execution and polecat workflow
- [Propulsion Principle](propulsion-principle.md) - Why work triggers immediate execution
- [Persistent Polecat Pool](../design/persistent-polecat-pool.md) - Pool management design and shipping status
