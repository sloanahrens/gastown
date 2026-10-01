# Property Layers: Multi-Level Configuration

> Implementation guide for Gas Town's configuration system.
> Created: 2025-01-06

## Overview

Gas Town uses a layered property system for configuration. Properties are
looked up through multiple layers, with earlier layers overriding later ones.
This enables both local control and global coordination.

## The Four Layers

```
┌─────────────────────────────────────────────────────────────┐
│ 1. WISP LAYER (transient, town-local)                       │
│    Location: <rig>/.beads-wisp/config/                      │
│    Synced: Never                                            │
│    Use: Temporary local overrides                           │
└─────────────────────────────┬───────────────────────────────┘
                              │ if missing
                              ▼
┌─────────────────────────────────────────────────────────────┐
│ 2. RIG BEAD LAYER (persistent, synced globally)             │
│    Location: <rig>/.beads/ (rig identity bead labels)       │
│    Synced: Via git (all clones see it)                      │
│    Use: Project-wide operational state                      │
└─────────────────────────────┬───────────────────────────────┘
                              │ if missing
                              ▼
┌─────────────────────────────────────────────────────────────┐
│ 3. TOWN DEFAULTS                                            │
│    Location: ~/gt/config.json or ~/gt/.beads/               │
│    Synced: N/A (per-town)                                   │
│    Use: Town-wide policies                                  │
└─────────────────────────────┬───────────────────────────────┘
                              │ if missing
                              ▼
┌─────────────────────────────────────────────────────────────┐
│ 4. SYSTEM DEFAULTS (compiled in)                            │
│    Use: Fallback when nothing else specified                │
└─────────────────────────────────────────────────────────────┘
```

## Lookup Behavior

### Override Semantics (Default)

For most properties, the first non-nil value wins:

```go
func GetConfig(key string) interface{} {
    if val := wisp.Get(key); val != nil {
        if val == Blocked { return nil }
        return val
    }
    if val := rigBead.GetLabel(key); val != nil {
        return val
    }
    if val := townDefaults.Get(key); val != nil {
        return val
    }
    return systemDefaults[key]
}
```

### Stacking Semantics (Integers)

For integer properties, values from wisp and bead layers **add** to the base:

```go
func GetIntConfig(key string) int {
    base := getBaseDefault(key)    // Town or system default
    beadAdj := rigBead.GetInt(key) // 0 if missing
    wispAdj := wisp.GetInt(key)    // 0 if missing
    return base + beadAdj + wispAdj
}
```

This enables temporary adjustments without changing the base value.

### Blocking Inheritance

You can explicitly block a property from being inherited:

```bash
gt rig config set gastown auto_restart --block
```

This creates a "blocked" marker in the wisp layer. Even if the rig bead
or defaults say `auto_restart: true`, the lookup returns nil.

## Rig Identity Beads

Each rig has an identity bead for operational state:

```yaml
id: gt-rig-gastown
type: rig
name: gastown
repo: git@github.com:steveyegge/gastown.git
prefix: gt

labels:
  - status:operational
  - priority:normal
```

These beads sync via git, so all clones of the rig see the same state.

## Two-Level Rig Control

### Level 1: Park (Local, Ephemeral)

```bash
gt rig park gastown      # Stop services, daemon won't restart
gt rig unpark gastown    # Allow services to run
```

- Stored in wisp layer (`.beads-wisp/config/`)
- Only affects this town
- Disappears on cleanup
- Use: Local maintenance, debugging

### Level 2: Dock (Global, Persistent)

```bash
gt rig dock gastown      # Set status:docked label on rig bead
gt rig undock gastown    # Remove label
```

- Stored on rig identity bead
- Syncs to all clones via git
- Permanent until explicitly changed
- Use: Project-wide maintenance, coordinated downtime

### Daemon Behavior

The daemon checks both levels before auto-restarting:

```go
func shouldAutoRestart(rig *Rig) bool {
    status := rig.GetConfig("status")
    if status == "parked" || status == "docked" {
        return false
    }
    return true
}
```

## Configuration Keys

| Key | Type | Behavior | Description |
|-----|------|----------|-------------|
| `status` | string | Override | operational/parked/docked |
| `auto_restart` | bool | Override | Daemon auto-restart behavior |
| `max_polecats` | int | Override | Max concurrent polecats in this rig; `0` = uncapped (town-wide cap: `scheduler.max_polecats`) |
| `priority_adjustment` | int | **Stack** | Scheduling priority modifier |
| `maintenance_window` | string | Override | When maintenance allowed |
| `dnd` | bool | Override | Do not disturb mode |

## Commands

### View Configuration

```bash
gt rig config show gastown           # Show effective config (all layers)
gt rig config show gastown --layer   # Show which layer each value comes from
```

### Set Configuration

```bash
# Set in wisp layer (local, ephemeral)
gt rig config set gastown key value

# Set in bead layer (global, permanent)
gt rig config set gastown key value --global

# Block inheritance
gt rig config set gastown key --block

# Clear from wisp layer
gt rig config unset gastown key
```

Values are parsed according to the key's type from the table above. Integer keys
(`max_polecats`, `priority_adjustment`) require a whole number, and boolean keys
(`auto_restart`, `dnd`, `auto_start_on_up`) accept `true`/`false`/`1`/`0`. A value
that does not fit the key's type is rejected rather than stored in a form the
key's reader would ignore; keys with no declared type are still guessed
(number, then boolean, then string).

### Rig Lifecycle

```bash
gt rig park gastown          # Local: stop + prevent restart
gt rig unpark gastown        # Local: allow restart

gt rig dock gastown          # Global: mark as offline
gt rig undock gastown        # Global: mark as operational

gt rig status gastown        # Show current state
```

## Examples

### Temporary Priority Boost

```bash
# Base priority: 0 (from defaults)
# Give this rig temporary priority boost for urgent work

gt rig config set gastown priority_adjustment 10

# Effective priority: 0 + 10 = 10
# When done, clear it:

gt rig config unset gastown priority_adjustment
```

### Local Maintenance

```bash
# I'm upgrading the local clone, don't restart services
gt rig park gastown

# ... do maintenance ...

gt rig unpark gastown
```

### Project-Wide Maintenance

```bash
# Major refactor in progress, all clones should pause
gt rig dock gastown

# Syncs via git - other towns see the rig as docked
bd sync

# When done:
gt rig undock gastown
bd sync
```

### Block Auto-Restart Locally

```bash
# Rig bead says auto_restart: true
# But I'm debugging and don't want that here

gt rig config set gastown auto_restart --block

# Now auto_restart returns nil for this town only
```

## Implementation Notes

### Wisp Storage

Wisp config stored in `.beads-wisp/config/<rig>.json`:

```json
{
  "rig": "gastown",
  "values": {
    "status": "parked",
    "priority_adjustment": 10
  },
  "blocked": ["auto_restart"]
}
```

### Rig Bead Labels

Rig operational state stored as labels on the rig identity bead:

```bash
bd label add gt-rig-gastown status:docked
bd label remove gt-rig-gastown status:docked
```

### Daemon Integration

The daemon reads rig status before starting a rig's agents: a parked or docked
rig is skipped.

## Role Directives and Formula Overlays

Directives and overlays extend the property layer model to agent behavior.
They follow the same rig > town > system precedence as other config.

### Directives (Behavioral Policy)

Per-role Markdown files that modify agent behavior at prime time:

```
SYSTEM LAYER:   Embedded role template (compiled in)
                        │ if directive exists
                        ▼
TOWN LAYER:     ~/gt/directives/_common.md      (every role)
                ~/gt/directives/<role>.md
                        │ concatenated with
                        ▼
RIG LAYER:      ~/gt/<rig>/directives/_common.md (every role)
                ~/gt/<rig>/directives/<role>.md
```

Every file that exists concatenates, broadest first: town `_common`, rig
`_common`, town `<role>`, rig `<role>`. The most specific file appears last and
wins conflicts (same as CSS specificity — later rules override earlier ones).

A file named for no role loads for no one. `gt directive list` marks it
`UNUSED (no such role)`, `gt doctor` warns, and `gt prime` prints the list;
none of them delete it.

### Overlays (Formula Modifications)

Per-formula TOML files that modify individual steps:

```
SYSTEM LAYER:   Embedded formula (compiled in)
                        │ if overlay exists
                        ▼
TOWN LAYER:     ~/gt/formula-overlays/<formula>.toml
```

Unlike directives, overlays have no rig layer: there is one overlay dir
(gt-fd2cu.3).

### Precedence Summary

| Config Type | Town + Rig Interaction | Rationale |
|-------------|----------------------|-----------|
| Rig properties | First non-nil wins (override) | Standard config lookup |
| Integer properties | Values stack (additive) | Allows adjustments |
| Role directives | Concatenate (rig last) | Additive policy; rig gets last word |
| Formula overlays | Rig replaces town | Step mods can conflict; full replacement is safer |

See [directives-and-overlays.md](directives-and-overlays.md) for the full
reference with TOML format, examples, and `gt doctor` integration.

## Related Documents

- `~/gt/docs/hop/PROPERTY-LAYERS.md` - Strategic architecture
- `wisp-architecture.md` - Wisp system design
- `agent-as-bead.md` - Agent identity beads (similar pattern)
- [directives-and-overlays.md](directives-and-overlays.md) - Full reference
