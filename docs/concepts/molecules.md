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
    └─▶ bd mol wisp --root-only ──▶ Root Wisp (ephemeral) ─── Vapor
```

**Root-only wisps** (default): Formula steps are NOT materialized as database rows.
Only a single root wisp is created. Agents read steps inline from the embedded
formula at prime time. This prevents wisp accumulation (~6,000+ rows/day → ~400/day).

**Poured wisps** (`pour = true`): Steps ARE materialized as sub-wisps with
checkpoint recovery. If a session dies, completed steps remain closed and work
resumes from the last checkpoint. Use pour for expensive, low-frequency workflows
where losing progress would be costly (e.g., release workflows).

## Core Concepts

| Term | Description |
|------|-------------|
| **Formula** | Source TOML template defining workflow steps |
| **Protomolecule** | Frozen template ready for instantiation |
| **Molecule** | Active workflow instance (root wisp only) |
| **Wisp** | Ephemeral molecule for patrols and polecat work (never synced) |
| **Root-only** | Only root wisp created; steps read from embedded formula |
| **Pour** | Formula flag (`pour = true`); steps materialized as sub-wisps with checkpoint recovery |

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
bd mol list                  # Available protos
bd mol show <id>             # Proto details
bd mol wisp <proto>          # Create wisp (root-only by default)
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

| Type | Storage | Use Case |
|------|---------|----------|
| **Root-only Wisp** (`pour = false`) | `.beads/` (ephemeral) | Polecat work, patrols — high frequency, cheap steps |
| **Poured Wisp** (`pour = true`) | `.beads/` (sub-wisps) | Releases, long workflows — low frequency, expensive steps |

**Heuristic**: If you would curse losing the progress after a crash, set `pour = true`.
High frequency + cheap steps = inline (default). Low frequency + expensive steps = pour.

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
3. **File discovered work** — `bd create` for bugs found, don't fix them yourself
