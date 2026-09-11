# Gas Town Glossary

Gas Town is an agentic development environment for managing multiple Claude Code instances simultaneously using the `gt` and `bd` (Beads) binaries, coordinated with tmux in git-managed directories.

## Core Principles

### MEOW (Molecular Expression of Work)
Breaking large goals into detailed instructions for agents. Supported by Beads, Epics, Formulas, and Molecules. MEOW ensures work is decomposed into trackable, atomic units that agents can execute autonomously.

### GUPP (Gas Town Universal Propulsion Principle)
"If there is work on your Hook, YOU MUST RUN IT." This principle ensures agents autonomously proceed with available work without waiting for external input. GUPP is the heartbeat of autonomous operation.

### NDI (Nondeterministic Idempotence)
The overarching goal ensuring useful outcomes through orchestration of potentially unreliable processes. Persistent Beads and oversight agents (Witness, Deacon) guarantee eventual workflow completion even when individual operations may fail or produce varying results.

## Environments

### Town
The management headquarters (e.g., `~/gt/`). The Town coordinates all workers across multiple Rigs and houses town-level agents like Mayor and Deacon.

### Rig
A project-specific Git repository under Gas Town management. Each Rig has its own Polecats, Refinery, Witness, and Crew members. Rigs are where actual development work happens.

## Town-Level Roles

### Mayor
Chief-of-staff agent responsible for initiating Convoys, coordinating work distribution, and notifying users of important events. The Mayor operates from the town level and has visibility across all Rigs.

### Deacon
Daemon beacon running continuous Patrol cycles. The Deacon ensures worker activity, monitors system health, and triggers recovery when agents become unresponsive. Think of the Deacon as the system's watchdog.

### Dogs
The Deacon's crew of maintenance agents handling background tasks like cleanup, health checks, and system maintenance.

### Boot (the Dog)
A special Dog that checks the Deacon every 5 minutes, ensuring the watchdog itself is still watching. This creates a chain of accountability.

## Rig-Level Roles

### Polecat
Worker agents with persistent identity but ephemeral sessions. Each polecat has a permanent agent bead, CV chain, and work history that accumulates across assignments. Sessions and sandboxes are ephemeral — spawned for specific tasks, cleaned up on completion — but the identity persists. They work in isolated git worktrees to avoid conflicts.

### Refinery
Manages the Merge Queue for a Rig. The Refinery intelligently merges changes from Polecats, handling conflicts and ensuring code quality before changes reach the main branch.

### Witness
Patrol agent that oversees Polecats and the Refinery within a Rig. The Witness monitors progress, detects stuck agents, and can trigger recovery actions.

### Crew
Long-lived, named agents for persistent collaboration. Unlike ephemeral Polecats, Crew members maintain context across sessions and are ideal for ongoing work relationships.

## Work Units

### Bead
Git-backed atomic work unit stored in Dolt. Beads are the fundamental unit of work tracking in Gas Town. They can represent issues, tasks, epics, or any trackable work item.

### Formula
TOML-based workflow source template. Formulas define reusable patterns for common operations like patrol cycles, code review, or deployment.

### Protomolecule
A template class for instantiating Molecules. Protomolecules define the structure and steps of a workflow without being tied to specific work items.

### Molecule
Durable chained Bead workflows. Molecules represent multi-step processes where each step is tracked as a Bead. They survive agent restarts and ensure complex workflows complete.

### Wisp
Ephemeral Beads destroyed after runs. Wisps are lightweight work items used for transient operations that don't need permanent tracking.

### Hook
A special pinned Bead for each agent. The Hook is an agent's primary work queue - when work appears on your Hook, GUPP dictates you must run it.

## Workflow Commands

### Convoy
Primary work-order wrapping related Beads. Convoys group related tasks together and can be assigned to multiple workers. Created with `gt convoy create`.

### Slinging
Assigning work to agents via `gt sling`. When you sling work to a Polecat or Crew member, you're putting it on their Hook for execution.

### Nudging
Real-time messaging between agents with `gt nudge`. Nudges allow immediate communication without going through the mail system.

### Handoff
Agent session refresh via `/handoff`. When context gets full or an agent needs a fresh start, handoff transfers work state to a new session.

### Seance
Communicating with previous sessions via `gt seance`. Allows agents to query their predecessors for context and decisions from earlier work.

### Patrol
Ephemeral loop maintaining system heartbeat. Patrol agents (Deacon, Witness) continuously cycle through health checks and trigger actions as needed.

## Supervision

### Canary settings file
A single settings file synced first, before a rollout to every other target. Catches a sync round-trip that silently drops or corrupts fields on the one canary before it ever reaches everyone else.

### Field provenance (Live / Recorded / Unknown)
The tag on every field of a Summary: Live (measured this cycle from the real artifact), Recorded (read from a store another actor wrote, such as a bead status), or Unknown (could not be measured). A Recorded value is a count of the store, never of reality.

### Pair probe
A live-fire check that exercises both a shape that must be blocked and a shape that must be allowed, so a guard that has started denying (or permitting) everything is caught rather than only the one-sided case.

### Probe-path rule
A probe's result counts only if the probe entered through the same production entry point, with the same inherited environment, as the real system. A probe that takes a shortcut path proves nothing about the real one.

### Progress counter
A monotonic counter carried alongside a heartbeat timestamp (e.g. HeartbeatCount) so readers can tell "fresh" from "advancing" — a clock heartbeat alone proves only that the clock still ticks.

### Self-probe
A check that injects a known event through a component's own production path and verifies the expected reaction, rather than trusting a clock-only heartbeat.

### Summary
Any status report a human or acting role consumes to make a decision — a doctor report, a patrol heartbeat, a status mail. Its fields are provenance-tagged Live, Recorded, or Unknown.

### Supervisor
Any producer of a Summary: monitor scripts, doctor checks, patrol steps, mayor/refinery status mails. Provenance and liveness rules apply to all of them, not just a dedicated watchdog role.

---

*This glossary was contributed by [Clay Shirky](https://github.com/cshirky) in [Issue #80](https://github.com/steveyegge/gastown/issues/80).*
