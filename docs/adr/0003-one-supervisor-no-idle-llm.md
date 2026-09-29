---
status: accepted
date: 2026-09-29
---

# One supervisor, and no long-running LLM sessions

The town had at least ten code paths that could kill, restart or unhook a worker, each reading a
different mix of agent-bead fields, work-bead assignee, heartbeat files, tmux state, pane
processes, pause and estop markers, and an LLM's memory. They disagreed. The daemon read agent
beads from the wrong database, the witness restarted working polecats on a stale pointer that
nothing wrote any more, stall evidence lived in memory and vanished on every daemon restart, and
the tmux respawn hook looped every three seconds outside every guard.

We decided that the daemon is the only process that kills or restarts, through one `Kill` and one
`Restart` that enforce pause, estop, a persisted restart budget and actor logging in one place.
A worker's intended state lives in one fail-closed file per seat under the town runtime
directory, never in the store, so liveness decisions work when Dolt does not. Agent beads are
mirrors of that file. Every other detector becomes a finding fed to the supervisor or is deleted.
Liveness is one function returning Alive, Dead, Stalled or Unknown; Unknown is never acted on,
and progress is measured by change, never by elapsed turn time. E-stop stops dispatch and
restarts and lets sessions finish; a separate `kill-all` exists for the other case.

We also decided that an unattended town runs no long-running LLM session. The witness becomes a
Go tick, the deacon's patrol steps are ported to Go or dropped, the mayor becomes a planner the
daemon spawns for a spec that needs decomposition and that exits when the plan is filed, dogs
whose job is a script become daemon jobs, and the refinery leaves with the landing-path
redesign. A ready `spec`-labelled bead is dispatched by a Go ticker within the seat classes and
budget. The Claude CLI, with backend wrappers, is the only agent runtime, so no session can be
spawned without its hooks.

## Considered options

- **Keep the witness's restart authority for its rig.** Rejected: it is the actor behind the
  false restarts, and its detection already runs in Go.
- **Keep the mayor as a resident chat surface with no authority.** Rejected: an incoming nudge
  once answered a permission dialog on the operator's behalf; the operator session is the
  operator.
- **A true freeze with SIGSTOP.** Rejected: tmux sends SIGCONT to pane children, so the freeze
  is undone by the tool that hosts the process.

Decision record: wayfinder map claude-1ey, ticket claude-1ey.4; evidence in
`~/.claude/docs/research/deep-review/gastown-control-plane.md`. Absorbs the host-supervision map
claude-41j.
