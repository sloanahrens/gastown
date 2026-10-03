# Understanding Gas Town

This document provides a conceptual overview of Gas Town's architecture, focusing on
the role taxonomy and how different agents interact.

## Why Gas Town Exists

As AI agents become central to engineering workflows, teams face new challenges:

- **Accountability:** Who did what? Which agent introduced this bug?
- **Quality:** Which agents are reliable? Which need tuning?
- **Efficiency:** How do you route work to the right agent?
- **Scale:** How do you coordinate agents across repos and teams?

Gas Town is an orchestration layer that treats AI agent work as structured data.
Every action is attributed. Every agent has a track record. Every piece of work
has provenance. See [Why These Features](why-these-features.md) for the full rationale,
and [Glossary](glossary.md) for terminology.

## Role Taxonomy

Gas Town has several agent types, each with distinct responsibilities and lifecycles.

### Infrastructure Roles

These roles manage the Gas Town system itself:

| Role | Description | Lifecycle |
|------|-------------|-----------|
| **Mayor** | Global coordinator at mayor/ | Singleton |
| **Daemon** | Go process: supervises agents, lands work | Singleton, persistent |

The daemon is not an agent. It is the only process that kills or restarts a
session, and its landing worker is the only route to main
([ADR 0003](adr/0003-one-supervisor-no-idle-llm.md),
[ADR 0004](adr/0004-daemon-lands-work.md)).

### Worker Roles

These roles do actual project work:

| Role | Description | Lifecycle |
|------|-------------|-----------|
| **Polecat** | Worker with persistent identity, ephemeral sessions | Daemon-supervised ([details](concepts/polecat-lifecycle.md)) |
| **Crew** | Persistent worker with own clone | Long-lived, user-managed |

## Tracking Work

Work is tracked by beads. A bead's status and assignee are the record of what is
in flight: `gt sling` assigns it to a worker, and the landing worker closes it
when the branch lands.

Order dependent work with `bd dep add`; a bead that depends on another stays out
of `gt ready` until its blocker closes.

```bash
# gt-def waits for gt-abc
bd dep add gt-def gt-abc

# Every bead with no open blocker, town-wide
gt ready
```

`gt ready` aggregates unblocked beads from the town and every rig; `gt show
<bead-id>` prints one bead's status, dependencies, and assignee.

The "swarm" is the set of workers currently assigned to an epic's issues.
When issues close, the epic's work is done.

## Crew vs Polecats

Both do project work, but with key differences:

| Aspect | Crew | Polecat |
|--------|------|---------|
| **Lifecycle** | Persistent (user controls) | Transient (daemon supervises) |
| **Monitoring** | None | Daemon restarts a dead session that holds work |
| **Work assignment** | Human-directed or self-assigned | Slung via `gt sling` |
| **Git state** | Works on branch, `gt done`, landing worker merges | Works on branch, `gt done`, landing worker merges |
| **Cleanup** | Manual | Automatic on completion |
| **Identity** | `<rig>/crew/<name>` | `<rig>/polecats/<name>` |

**When to use Crew**:
- Exploratory work
- Long-running projects
- Work requiring human judgment
- Tasks where you want direct control

**When to use Polecats**:
- Discrete, well-defined tasks
- Batch work (ordered with `bd dep add`)
- Parallelizable work
- Work that benefits from supervision

## Cross-Rig Work

When work belongs to another rig, dispatch it to that rig's workers:

```bash
# File the issue in the target rig's database
gt bead create --rig=beads "Fix authentication bug"

# Sling it to the rig that owns it
gt sling bd-xyz beads
```

## Directory Structure

The town root (`~/gt/`) contains the `mayor/` directory and per-project rigs.
Each rig holds a bare repo (`.repo.git/`), a canonical beads database
(`mayor/rig/.beads/`), and agent directories (`crew/`, `polecats/`).

> For the full directory tree, see [architecture.md](design/architecture.md).

## Identity and Attribution

All work is attributed to the actor who performed it:

```
Git commits:      Author: gastown/crew/joe <owner@example.com>
Beads issues:     created_by: gastown/crew/joe
Events:           actor: gastown/crew/joe
```

Identity is preserved even when working cross-rig:
- `gastown/crew/joe` working in `~/gt/beads/crew/gastown-joe/`
- Commits still attributed to `gastown/crew/joe`
- Work appears on joe's CV, not beads rig's workers

## The Propulsion Principle

All Gas Town agents follow the same core principle:

> **If you find something on your hook, YOU RUN IT.**

This applies regardless of role. The hook is your assignment. Execute it immediately
without waiting for confirmation. Gas Town is a steam engine - agents are pistons.

## Model Evaluation and A/B Testing

Gas Town's attribution system enables objective model comparison by tracking
completion time, quality signals, and revision count per agent. Deploy different
models on similar tasks and compare outcomes with `bd stats`.

See [Why These Features](why-these-features.md) for details on work history and
capability-based routing.

## Common Mistakes

1. **Confusing crew with polecats**: Crew is persistent and human-managed. Polecats are transient and daemon-supervised.
2. **Working in wrong directory**: Gas Town uses cwd for identity detection. Stay in your home directory.
3. **Waiting for confirmation when work is hooked**: The hook IS your assignment. Execute immediately.
4. **Creating worktrees when dispatch is better**: If work should be owned by the target rig, dispatch it instead.
