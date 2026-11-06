# Molecules

Molecules are workflow templates that coordinate multi-step work in Gas Town.

## Molecule Lifecycle

```
Formula (source TOML) ─── "Ice-9"
    │
    ▼ bd cook
Protomolecule (frozen template) ─── Solid
    │
    ├─▶ bd mol pour ──▶ Mol (persistent) ─── Liquid
    │
    └─▶ bd mol wisp ──▶ Wisp (ephemeral, with step wisps) ─── Vapor
```

**Step wisps:** `bd` always materializes a formula's steps as child wisps of the
root wisp. Each step is its own issue with its own status, so closed steps stay
closed if a session dies. Gastown agents still read the checklist from `gt prime`,
which renders the steps inline. `gt mol step done <step-id>` closes the
completed step and assigns the next ready step to this session.

**Pour** (`bd mol pour`): creates a persistent mol. Its steps are stored as beads
and sync like any other bead (`bd dolt push` / `bd dolt pull`). Use it for work
you want to keep in the database, such as feature work that spans sessions.

## Core Concepts

| Term | Description |
|------|-------------|
| **Formula** | Source TOML template defining workflow steps |
| **Protomolecule** | Frozen template ready for instantiation |
| **Molecule** | Active workflow instance: a root issue plus its step children |
| **Wisp** | Ephemeral molecule for patrols and polecat work (never synced) |
| **Step wisps** | Child wisps of the root, one per formula step; `gt prime` renders them as the checklist |
| **Pour** | `bd mol pour`: persistent mol whose steps sync like any other bead |

## How Agents See Steps

Agents do NOT use `bd mol current` or `bd close <step-id>` for formula workflows.
Instead, formula steps are rendered inline when the agent runs `gt prime`:

```
**Formula Checklist** (10 steps from mol-polecat-work):

### Step 1: Load context and verify assignment
Initialize your session and understand your assignment...

### Step 2: Set up working branch
Ensure you're on a clean feature branch...
```

The agent works through the checklist and runs `gt done` when complete.

## Molecule Commands

### Beads Operations (bd)

```bash
# Formulas
bd formula list              # Available formulas
bd formula show <name>       # Formula details
bd cook <formula>            # Formula → Proto

# Molecules (data operations)
bd mol show <id>             # Proto details
bd mol wisp <proto>          # Create wisp (root plus step wisps)
bd mol bond <proto> <parent> # Attach to existing mol
```

### Agent Operations (gt)

```bash
# Hook management
gt hook                    # What's on MY hook?
gt prime                   # Shows inline formula checklist
gt mol attach <bead> <mol>   # Pin molecule to bead
gt mol detach <bead>         # Unpin molecule from bead

# Patrol dispatch
gt sling mol-pr-feedback-patrol <rig>   # Dispatch a patrol formula run
```

## Polecat Workflow

Polecats receive work via their hook — a root wisp attached to an issue.
They see the formula checklist inline when they run `gt prime` and work
through each step in order.

### Polecat Workflow Summary

```
1. Spawn with work on hook
2. gt prime               # Shows formula checklist inline
3. Work through each step
4. Persist findings: bd update <issue> --append-notes "..."
5. gt done                # Submit, preserve handoff metadata, exit
```

### Molecule Types

| Type | Command | Storage | Use Case |
|------|---------|---------|----------|
| **Wisp** | `bd mol wisp` | `.beads/` (ephemeral, never synced) | Polecat work, patrols, operational loops |
| **Pour** | `bd mol pour` | `.beads/` (persistent, synced) | Feature work you want to keep in history |

**Heuristic**: If the work should stay in the database and sync with git, pour it.
Operational work that is frequent or disposable belongs in a wisp.

## Patrol Workflow

A patrol formula (`wisp_type = "patrol"`, `scan_interval_seconds`) is dispatched
by the daemon: its `scheduled_slings` patrol slings the formula on an interval
(internal/daemon/scheduled_slings.go), and `patrol_scan` restarts a session that
died still holding the run. The session works the checklist and starts the next
cycle by reopening the cycle's own step beads
(internal/formula/formulas/mol-pr-feedback-patrol.formula.toml, `loop-or-exit`); a
session near its context limit hands off instead (`gt handoff`).

## Best Practices

1. **Persist findings early** — `bd update <issue> --append-notes "..."` before session death
2. **Run `gt done` when complete** — mandatory for polecats (pushes, submits to MQ/PR path, exits)
3. **Report discovered work** — `gt done --finding`; the town files the bead, don't fix it yourself
