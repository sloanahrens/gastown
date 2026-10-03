# Gastown/Beads Cleanup Commands Reference

A comprehensive catalog of all cleanup-related commands in the gastown/beads ecosystem, organized by scope and severity.

---

## Process Cleanup

| Command | What it does |
|---------|-------------|
| `gt down --all` | Full shutdown with orphaned-process cleanup and verification |
| (daemon, automatic) | The daemon's orphan cleanup kills orphaned Claude processes on its heartbeat |

## Polecat (Agent Sandbox) Cleanup

| Command | What it does |
|---------|-------------|
| `gt polecat remove <rig>/<polecat>` | Removes polecat worktree/directory (fails if session running) |
| `gt polecat nuke <rig>/<polecat>` | Nuclear: kills session, deletes worktree, deletes branch, closes bead |
| `gt polecat nuke <rig> --all` | Nukes all polecats in a rig |
| `gt polecat gc <rig>` | GC stale polecat branches (orphaned, old timestamped) |
| `gt polecat stale <rig>` | Detects stale polecats; `--cleanup` auto-nukes them |
| `gt polecat check-recovery` | Pre-nuke safety check (SAFE_TO_NUKE vs NEEDS_RECOVERY) |
| `gt polecat identity remove <rig> <name>` | Removes a polecat identity |
| `gt done` | Polecat self-cleaning: pushes branch, marks the bead `gt:ready-to-land`, preserves handoff metadata, kills own session. No landing for `--status ESCALATED\|DEFERRED` |

## Rig-Level Cleanup

| Command | What it does |
|---------|-------------|
| `gt rig reset` | Resets handoff content, stale mail, orphaned in_progress issues |
| `gt rig reset --handoff` | Clears handoff content only |
| `gt rig reset --mail` | Clears stale mail only |
| `gt rig reset --stale` | Resets orphaned in_progress issues |
| `gt rig remove <name>` | Unregisters rig from registry, cleans up beads routes |
| `gt rig shutdown <rig>` | Stops all polecat sessions in the rig |
| `gt rig stop <rig>...` | Stop one or more rigs |

## Town-Wide Shutdown

| Command | What it does |
|---------|-------------|
| `gt down` | Stops all infrastructure (crew, mayor, daemon, dolt) |
| `gt down --polecats` | Also stops all polecat sessions |
| `gt down --all` | Full shutdown with orphan cleanup and verification |
| `gt down --nuke` | Kills entire tmux server (DESTRUCTIVE - kills non-GT sessions too) |
| `gt shutdown` | "Done for the day" - stops agents AND removes polecat worktrees/branches. Flags control aggressiveness (`--graceful`, `--force`, `--nuclear`, `--polecats-only`, etc.) |

## Crew Workspace Cleanup

| Command | What it does |
|---------|-------------|
| `gt crew stop [name]` | Stops crew tmux sessions |
| `gt crew restart [name]` | Kills and restarts crew fresh ("clean slate", no handoff mail) |
| `gt crew remove <name>` | Removes workspace, closes agent bead |
| `gt crew remove <name> --purge` | Full obliteration: deletes agent bead, unassigns beads, clears mail |
| `gt crew pristine [name]` | Syncs workspaces with remote (`git pull`) |

## Ephemeral Data / Event Cleanup

| Command | What it does |
|---------|-------------|
| `gt compact` | TTL-based compaction: promotes/deletes wisps past their TTL |

## Dolt Database Cleanup

| Command | What it does |
|---------|-------------|
| `gt dolt cleanup` | Removes orphaned databases from `.dolt-data/` |
| `gt dolt stop` | Stops the Dolt SQL server |

## Bead / Hook Cleanup

| Command | What it does |
|---------|-------------|
| `gt unsling` / `gt unhook` | Removes work from agent's hook, resets bead status to "open" |
| `gt hook clear` | Alias for unsling |

## Mail Cleanup

| Command | What it does |
|---------|-------------|
| `gt mail delete <msg-id>` | Deletes specific messages |
| `gt mail archive <msg-id>` | Archives messages (`--stale` for stale ones) |
| `gt mail clear [target]` | Deletes all messages from an inbox (town quiescence) |

## Misc State Cleanup

| Command | What it does |
|---------|-------------|
| `gt doctor fix <check>` | Repairs one named check: orphan sessions, stale redirects, worktree validity, … |

## System-Level Cleanup

| Command | What it does |
|---------|-------------|
| `gt config agent remove <name>` | Removes custom agent definition |
| `make clean` | Removes compiled `gt` binary |

## Internal (Automatic / Side-Effect)

| Function | Where | What it does |
|----------|-------|-------------|
| `cleanupOrphanedProcesses()` | `polecat.go` | Auto-runs after nuke/stale cleanup |
| `retirePolecatSessionAfterDone()` | `done.go` | Self-terminates tmux session with PID exclusion after durable handoff |
| `rollbackSlingArtifacts()` | `sling.go` | Cleans up partial sling failures |
| `cleanStaleHookedBeads()` | `unsling.go` | Repairs beads stuck in "hooked" state |
| `gt signal stop` | `signal_stop.go` | Clears stop-state temp files at turn boundaries |
| `make install-local` | `Makefile` | Removes stale `~/go/bin/gt` and `~/bin/gt` binaries (also run by `make install`) |

---

## Cleanup Layers (Low to High Severity)

| Layer | Scope | Key Commands |
|-------|-------|-------------|
| **L0** | Ephemeral data | `gt compact` (TTL-based lifecycle) |
| **L1** | Processes | `gt down --all`, daemon orphan cleanup |
| **L2** | Git artifacts | `gt polecat gc` |
| **L3** | Agents/sessions | `gt polecat nuke`, `gt done`, `gt shutdown`, `gt down` |
| **L4** | Workspace | `gt rig reset`, `gt doctor fix <check>`, `gt dolt cleanup` |
| **L5** | System | `make clean` |

**Total: ~62 commands/functions** across the cleanup ecosystem.
