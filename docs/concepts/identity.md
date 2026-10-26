# Agent Identity and Attribution

> Canonical format for agent identity in Gas Town

## Why Identity Matters

When you deploy AI agents at scale, anonymous work creates real problems:

- **Debugging:** "The AI broke it" isn't actionable. *Which* AI?
- **Quality tracking:** You can't improve what you can't measure.
- **Compliance:** Auditors ask "who approved this code?" - you need an answer.
- **Performance management:** Some agents are better than others at certain tasks.

Gas Town solves this with **universal attribution**: every action, every commit,
every bead update is linked to a specific agent identity. This enables work
history tracking, capability-based routing, and objective quality measurement.

## BD_ACTOR Format Convention

The `BD_ACTOR` environment variable identifies agents in slash-separated path format.
This is set automatically when agents are spawned and used for all attribution.

### Format by Role Type

| Role Type | Format | Example |
|-----------|--------|---------|
| **Crew** | `{rig}/crew/{name}` | `gastown/crew/joe` |
| **Polecat** | `{rig}/polecats/{name}` | `gastown/polecats/toast` |

### Why Slashes?

The slash format mirrors filesystem paths and enables:
- Hierarchical parsing (extract rig, role, name)
- Consistent mail addressing (`gt mail send gastown/crew/joe`)
- Path-like routing in beads operations
- Visual clarity about agent location

## Attribution Model

Gas Town uses three fields for complete provenance:

### Git Commits

```bash
GIT_AUTHOR_NAME="gastown/crew/joe"      # Who did the work (agent)
GIT_AUTHOR_EMAIL="steve@example.com"    # Who owns the work (overseer)
```

Result in git log:
```
abc123 Fix bug (gastown/crew/joe <steve@example.com>)
```

**Interpretation**:
- The agent `gastown/crew/joe` authored the change
- The work belongs to the workspace owner (`steve@example.com`)
- Both are preserved in git history forever

### Beads Records

```json
{
  "id": "gt-xyz",
  "created_by": "gastown/crew/joe",
  "updated_by": "gastown/polecats/toast"
}
```

The `created_by` field is populated from `BD_ACTOR` when creating beads.
The `updated_by` field tracks who last modified the record.

### Event Logging

All events include actor attribution:

```json
{
  "ts": "2025-01-15T10:30:00Z",
  "type": "sling",
  "actor": "gastown/crew/joe",
  "payload": { "bead": "gt-xyz", "target": "gastown/polecats/toast" }
}
```

## Environment Setup

Gas Town uses a centralized `config.AgentEnv()` function to set environment
variables consistently across all agent spawn paths (managers and the daemon).

### Example: Polecat Environment

```bash
# Set automatically for polecat 'toast' in rig 'gastown'
export GT_ROLE="polecat"
export GT_RIG="gastown"
export GT_POLECAT="toast"
export BD_ACTOR="gastown/polecats/toast"
export GIT_AUTHOR_NAME="gastown/polecats/toast"
export GT_TOWN_ROOT="/home/user/gt"
export BEADS_DIR="/home/user/gt/gastown/.beads"
export BEADS_AGENT_NAME="gastown/toast"
```

### Example: Crew Environment

```bash
# Set automatically for crew member 'joe' in rig 'gastown'
export GT_ROLE="crew"
export GT_RIG="gastown"
export GT_CREW="joe"
export BD_ACTOR="gastown/crew/joe"
export GIT_AUTHOR_NAME="gastown/crew/joe"
export GT_TOWN_ROOT="/home/user/gt"
export BEADS_DIR="/home/user/gt/gastown/.beads"
export BEADS_AGENT_NAME="gastown/joe"
```

### Manual Override

For local testing or debugging:

```bash
export BD_ACTOR="gastown/crew/debug"
bd create --title="Test issue"  # Will show created_by: gastown/crew/debug
```

See [reference.md](../reference.md#environment-variables) for the complete
environment variable reference.

## Identity Parsing

The format supports programmatic parsing:

| Input | Parsed Components |
|-------|-------------------|
| `gastown/crew/joe` | rig=gastown, role=crew, name=joe |
| `gastown/polecats/toast` | rig=gastown, role=polecat, name=toast |

## Audit Queries

Attribution makes each issue's history auditable:

```bash
# Every state change on one issue, with the actor that made it
bd history gt-xyz --events

# Git history by agent
git log --author="gastown/crew/joe"
```

`bd` filters issues by assignee, not by the actor who created or changed one,
and it has no actor wildcard; the actor of each event is reported per issue.
`bd --help` lists the reports it does provide.

## Design Principles

1. **Agents are not anonymous** - Every action is attributed
2. **Work is owned, not authored** - Agent creates, overseer owns
3. **Attribution is permanent** - Git commits preserve history
4. **Format is parseable** - Enables programmatic analysis
5. **Consistent across systems** - Same format in git, beads, events

## CV and Skill Accumulation

### Human Identity is Global

The global identifier is your **email** - it's already in every git commit. No separate "entity bead" needed.

```
steve@example.com                ← global identity (from git author)
├── Town A (home)                ← workspace
│   ├── gastown/crew/joe         ← agent executor
│   └── gastown/polecats/toast   ← agent executor
└── Town B (work)                ← workspace
    └── acme/polecats/nux        ← agent executor
```

### Agent vs Owner

| Field | Scope | Purpose |
|-------|-------|---------|
| `BD_ACTOR` | Local (town) | Agent attribution for debugging |
| `GIT_AUTHOR_EMAIL` | Global | Human identity for CV |
| `created_by` | Local | Who created the bead |
| `owner` | Global | Who owns the work |

**Agents execute. Humans own.** The polecat name in `completed-by: gastown/polecats/toast` is executor attribution. The CV credits the human owner (`steve@example.com`).

### Polecats Have Persistent Identities

Polecats have **persistent identities but ephemeral sessions**. Like employees who
clock in/out: each work session is fresh (new tmux, new worktree), but the identity
persists across sessions.

- **Identity (persistent)**: Agent bead, CV chain, work history
- **Session (ephemeral)**: Claude instance, context window
- **Sandbox (ephemeral)**: Git worktree, branch

Work credits the polecat identity, enabling:
- Performance tracking per polecat
- Capability-based routing (send Go work to polecats with Go track records)
- Model comparison (A/B test different models via different polecats)

See [polecat-lifecycle.md](polecat-lifecycle.md#polecat-identity) for details.

### Skills Are Derived

Your CV emerges from querying work evidence:

```bash
# All work by owner (across all agents)
git log --author="steve@example.com"

# Beads records an owner on each issue but has no owner filter;
# --assignee lists the issues assigned to an agent
bd list --assignee=gastown/crew/joe

# Skills derived from evidence
# - .go files touched → Go skill
# - issue tags → domain skills
# - commit patterns → activity types
```

### Multi-Town Aggregation

A human with multiple towns has one CV:

```bash
# Future: a federated CV query that discovers all towns, aggregates work,
# and derives skills. bd today reads one town's database; bd --help lists
# the reports it has.
```

See `~/gt/docs/hop/decisions/008-identity-model.md` for architectural rationale.

## Enterprise Use Cases

### Compliance and Audit

```bash
# Who touched this file in the last 90 days?
git log --since="90 days ago" -- path/to/sensitive/file.go

# All changes by a specific agent since a date
git log --author="gastown/polecats/toast" --since=2025-01-01

# The actor-attributed history of one issue
bd history gt-xyz --events
```

### Performance Tracking

```bash
# Completed work by agent
bd count --status=closed --by-assignee

# Completions in a window
bd count --status=closed --closed-after=2025-01-01
```

### Model Comparison

A bead's `agent:<alias>` label pins it to the seat that works it (the pin is read
in `poolPinnedAgent`, `internal/cmd/sling_pool.go`), and `gt config agent list`
names the model each alias runs. Count closed beads per label to compare models:

```bash
# Completed work per model: the agent: label rows
bd count --status=closed --by-label

# One model: completed beads, then those a landing sent back (the rework label)
bd count --status=closed --label=agent:claude-sonnet
bd count --status=closed --label=agent:claude-sonnet --label=rework
```

The rework rate for a model is its rework-labelled count divided by its completed
count. Compare the rates, not the raw counts: a model that ran more beads has more
rework-labelled beads in absolute terms.

The rework label is a proxy, not a measure of first-pass quality. Handback
(`internal/polecat/handback.go`) and the landing worker (`internal/land/work.go`)
set it when a landing sends a bead back. It records that a landing did so, not how
many attempts a bead took or which attempt failed, so a rate says how often a
model's beads were sent back, not how often its first attempt was wrong.
