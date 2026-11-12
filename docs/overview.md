# Understanding Gas Town

Gas Town is an orchestration layer that treats AI agent work as structured data.
Every action is attributed. Every agent has a track record. Every piece of work
has provenance.

As AI agents become central to engineering workflows, teams face new challenges:
accountability (who did what, which agent introduced this bug), quality (which
agents are reliable, which need tuning), efficiency (how to route work to the
right agent), and scale (how to coordinate agents across repos and teams).

The daemon is not an agent. It is a Go process that supervises agents and lands
work: it is the only process that kills or restarts a session, and its landing
worker is the only route to main
([ADR 0003](adr/0003-one-supervisor-no-idle-llm.md),
[ADR 0004](adr/0004-daemon-lands-work.md)).

| Role | Description | Lifecycle |
|------|-------------|-----------|
| **Polecat** | Worker with persistent identity, ephemeral sessions | Daemon-supervised ([details](concepts/polecat-lifecycle.md)) |
| **Crew** | Persistent worker with own clone | Long-lived, user-managed |

## Where to read next

- [architecture.md](architecture.md) - every moving part of the town and how one piece of work travels from an idea to a published commit.
- [glossary.md](glossary.md) - the terms: MEOW, GUPP, NDI, town and rig.
- [why-these-features.md](why-these-features.md) - the rationale: the work ledger, attribution, work history and capability-based routing.
- [concepts/polecat-lifecycle.md](concepts/polecat-lifecycle.md) - the polecat's three lifecycle layers and its operating states.
- [reference.md](reference.md) - technical reference for Gas Town internals.
