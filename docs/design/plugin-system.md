# Plugin System Design

> **Status: Original design proposal (2026-01-11, crew/george session).**
>
> The execution model shipped differently: the daemon runs cooldown script
> plugins itself (see [Execution Model](#execution-model-daemon-runs-script-plugins)).
> The Deacon and the dog worker pool this proposal relied on have been retired.

## Problem Statement

Gas Town needs extensible, project-specific automation that runs on daemon heartbeats. The immediate use case is rebuilding stale binaries (gt, bd, wv), but the pattern generalizes to any periodic maintenance task.

Current state:
- Plugin infrastructure exists conceptually (patrol step mentions it)
- `~/gt/plugins/` directory exists with README
- No actual plugins in production use
- No formalized execution model

## Design Principles Applied

### Discover, Don't Track
> Reality is truth. State is derived.

Plugin state (last run, run count, results) lives on the ledger as wisps, not in shadow state files. Gate evaluation queries the ledger directly.

### ZFC: Zero Framework Cognition
> Agent decides. Go transports.

The plugin's own script decides whether there is work (and can report `[plugin-result skipped]`). The daemon only evaluates the gate, runs the script, and records the result.

### MEOW Stack Integration

| Layer | Plugin Analog |
|-------|---------------|
| **M**olecule | `plugin.md` - work template with TOML frontmatter |
| **E**phemeral | Plugin-run wisps - high-volume, digestible |
| **O**bservable | Plugin runs appear in `bd activity` feed |
| **W**orkflow | Gate → Dispatch → Execute → Record → Digest |

---

## Architecture

### Plugin Locations

```
~/gt/
├── plugins/                      # Town-level plugins (universal)
│   └── README.md
├── gastown/
│   └── plugins/                  # Rig-level plugins
│       └── rebuild-gt/
│           └── plugin.md
├── beads/
│   └── plugins/
│       └── rebuild-bd/
│           └── plugin.md
└── wyvern/
    └── plugins/
        └── rebuild-wv/
            └── plugin.md
```

**Town-level** (`~/gt/plugins/`): Universal plugins that apply everywhere.
**Rig-level** (`<rig>/plugins/`): Project-specific plugins.

The daemon scans both locations on each heartbeat.

### Execution Model: Daemon Runs Script Plugins

**Key insight**: Plugin execution should not depend on an agent being awake.

On each heartbeat the daemon scans plugins and, for every open cooldown gate,
runs the plugin in-process:

```
Daemon heartbeat
─────────────────
1. Scan town + rig plugin dirs
2. Evaluate cooldown gates (plugin-run receipts on the ledger)
3. For each open gate on a script plugin
   ([execution] type = "script" plus run.sh):
   └─ run run.sh with a timeout
4. Record a plugin-run receipt (success / skipped / failure)
5. On failure: log it and raise `gt escalate` (severity high) under the
   fingerprint plugin:<name>:failed, with the output tail.
   The plugin's next good run (success or skipped) closes that escalation.
```

A cooldown plugin that is not a script plugin with a `run.sh` is logged as
skipped every heartbeat: nothing runs markdown-instruction plugins any more.
Manual-gate plugins are never auto-run; run them with `gt plugin run`.

The old top-level `agent` key (per-plugin agent preset) is ignored.

### State Tracking: Wisps on the Ledger

Each plugin run creates a wisp:

```bash
gt plugin record-run --plugin rebuild-gt --result success --rig gastown \
  --title "Plugin: rebuild-gt [success]" \
  --description "Rebuilt gt: abc123 → def456 (5 commits)"
```

**Gate evaluation** queries wisps instead of state files:

```bash
# Cooldown check: any runs in last hour?
bd list --all --label type:plugin-run --label plugin:rebuild-gt --created-after 1h -n 1
```

**Derived state** (no state.json needed):

| Query | Command |
|-------|---------|
| Last run time | `bd list --all --label=plugin:X --limit=1 --json` |
| Run count | `bd list --all --label=plugin:X --json \| jq length` |
| Last result | Parse `result:` label from latest wisp |
| Failure rate | Count `result:failure` vs total |

### Digest Pattern

Like cost digests, plugin wisps accumulate and get squashed daily:

```bash
gt plugin digest --yesterday
```

Creates: `Plugin Digest 2026-01-10` bead with summary
Deletes: Individual plugin-run wisps from that day

This keeps the ledger clean while preserving audit history.

---

## Plugin Format Specification

### File Structure

```
rebuild-gt/
└── plugin.md      # Definition with TOML frontmatter
```

### plugin.md Format

```markdown
+++
name = "rebuild-gt"
description = "Rebuild stale gt binary from source"
version = 1

[gate]
type = "cooldown"
duration = "1h"

[tracking]
labels = ["plugin:rebuild-gt", "rig:gastown", "category:maintenance"]
digest = true

[execution]
timeout = "5m"
notify_on_failure = true
+++

# Rebuild gt Binary

What run.sh does, and the manual procedure if it fails...
```

### TOML Frontmatter Schema

```toml
# Required
name = "string"           # Unique plugin identifier
description = "string"    # Human-readable description
version = 1               # Schema version (for future evolution)

[gate]
type = "cooldown|cron|condition|event|manual"
# Type-specific fields:
duration = "1h"           # For cooldown
schedule = "0 9 * * *"    # For cron
check = "gt stale -q"     # For condition (exit 0 = run)
on = "startup"            # For event

[tracking]
labels = ["label:value", ...]  # Labels for execution wisps
digest = true|false            # Include in daily digest

[execution]
type = "script"           # Daemon runs run.sh; required for auto-run
timeout = "5m"            # Max execution time
notify_on_failure = true  # Escalate on failure
severity = "low"          # Escalation severity if failed
```

### Gate Types

| Type | Config | Behavior |
|------|--------|----------|
| `cooldown` | `duration = "1h"` | Query wisps, run if none in window |
| `cron` | `schedule = "0 9 * * *"` | Run on cron schedule |
| `condition` | `check = "cmd"` | Run check command, run if exit 0 |
| `event` | `on = "startup"` | Run on Deacon startup |
| `manual` | (no gate section) | Never auto-run, dispatch explicitly |

### Instructions Section

The markdown body after the frontmatter documents what `run.sh` does and how an operator runs or debugs it by hand. The daemon executes `run.sh`, not the markdown.

Standard sections:
- **Detection**: Check if action is needed
- **Action**: The actual work
- **Record Result**: Create the execution wisp
- **Notification**: On success/failure

---

## New Commands Required

- **`gt stale`** -- Expose binary staleness check (human-readable, `--json`, `--quiet` exit code)
- **`gt plugin list|show|run|digest|history`** -- Plugin management and execution history

---

## Implementation Plan

### Phase 1: Foundation

1. **`gt stale` command** - Expose CheckStaleBinary() via CLI
2. **Plugin format spec** - Finalize TOML schema
3. **Plugin scanning** - Daemon scans town + rig plugin dirs

### Phase 2: Execution

4. **Script execution** - Daemon runs the plugin's run.sh in-process
5. **Failure escalation** - `gt escalate` under `plugin:<name>:failed`
6. **Wisp creation** - Record results on ledger

### Phase 3: Gates & State

7. **Gate evaluation** - Cooldown via wisp query
8. **Other gate types** - Cron, condition, event
9. **Plugin digest** - Daily squash of plugin wisps

### Phase 4: Escalation

10. **`gt escalate` command** - Unified escalation API
11. **Escalation routing** - Config-driven multi-channel
12. **Stale escalation patrol** - Check unacknowledged

### Phase 5: First Plugin

13. **`rebuild-gt` plugin** - The actual gastown plugin
14. **Documentation** - So Beads/Wyvern can create theirs

---

## Open Questions

1. **Plugin discovery in multiple clones**: If gastown has crew/george, crew/max, crew/joe - which clone's plugins/ dir is canonical? Probably: scan all, dedupe by name, prefer rig-root if exists.

2. **Plugin dependencies**: Can plugins depend on other plugins? Probably not in v1.

3. **Plugin disable/enable**: How to temporarily disable a plugin without deleting it? Label on a plugin bead? `enabled = false` in frontmatter?

---

## References

- PRIMING.md - Core design principles
- mol-deacon-patrol.formula.toml - Patrol step plugin-run
- ~/gt/plugins/README.md - Current plugin stub
