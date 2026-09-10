# Gas Town Hooks Management

Centralized hook management for Gas Town workspaces.

## Overview

Gas Town manages context injection for all supported agents. The mechanism varies by agent:

| Agent | Hook mechanism | Managed file |
|-------|---------------|-------------|
| Claude Code, Gemini | `settings.json` lifecycle hooks | `<role>/.claude/settings.json` |
| OpenCode | JS plugin | `workDir/.opencode/plugins/gastown.js` |
| GitHub Copilot | JSON lifecycle hooks | `workDir/.github/hooks/gastown.json` |
| Codex, others | Startup nudge fallback | *(no file — nudge only)* |

> **GitHub Copilot note**: Copilot CLI supports full executable lifecycle hooks
> (`sessionStart`, `userPromptSubmitted`, `preToolUse`, `sessionEnd`) via
> `.github/hooks/gastown.json`. This is the same lifecycle coverage as Claude Code,
> delivered in Copilot's JSON format rather than Claude's `settings.json` format.
> The `gt hooks` commands below apply to Claude Code (and Gemini) only.

Gas Town manages `.claude/settings.json` files in gastown-managed parent directories
and passes them to Claude Code via the `--settings` flag. This keeps customer repos
clean while providing role-specific hook configuration. The hooks system provides
a single source of truth with a base config and per-role/per-rig overrides.

## Architecture

```
~/.gt/hooks-base.json              ← Shared base config (all agents)
~/.gt/hooks-overrides/
  ├── crew.json                    ← Override for all crew workers
  ├── witness.json                 ← Override for all witnesses
  ├── gastown__crew.json           ← Override for gastown crew specifically
  └── ...
```

**Merge strategy:** `base → role → rig+role` (more specific wins)

For a target like `gastown/crew`:
1. Start with base config
2. Apply `crew` override (if exists)
3. Apply `gastown/crew` override (if exists)

## Generated targets

Each rig generates settings in shared parent directories (not per-worktree):

| Target | Path | Override Key |
|--------|------|--------------|
| Crew (shared) | `<rig>/crew/.claude/settings.json` | `<rig>/crew` |
| Witness | `<rig>/witness/.claude/settings.json` | `<rig>/witness` |
| Refinery | `<rig>/refinery/.claude/settings.json` | `<rig>/refinery` |
| Polecats (shared) | `<rig>/polecats/.claude/settings.json` | `<rig>/polecats` |

Town-level targets:
- `mayor/.claude/settings.json` (key: `mayor`)
- `deacon/.claude/settings.json` (key: `deacon`)
- `deacon/dogs/boot/.claude/settings.json` (key: `boot`, when the boot dir exists)
- `deacon/dogs/<name>/.claude/settings.json` (key: `dog`, one per kennel with a
  `.dog.json`; all dogs share the `dog` override key)

The `dog` override adds a PreToolUse guard on every Bash command
(`gt tap guard formula-allowlist`, gt-9iv): when the dog's assigned formula
declares a `command_allowlist` in its TOML, commands outside that list (plus a
built-in lifecycle baseline) are blocked before they run.

Settings are passed to Claude Code via `--settings <path>`, which loads them as
a separate priority tier that merges additively with project settings.

## Commands

### `gt hooks sync`

Regenerate all `.claude/settings.json` files from base + overrides.
Preserves non-hooks fields (editorMode, enabledPlugins, etc.).

```bash
gt hooks sync             # Write all settings files
gt hooks sync --dry-run   # Preview changes without writing
```

### `gt hooks diff`

Show what `sync` would change, without writing anything.

```bash
gt hooks diff             # Show differences
gt hooks diff --no-color  # Plain output
```

### `gt hooks base`

Edit the shared base config in `$EDITOR`.

```bash
gt hooks base             # Open in editor
gt hooks base --show      # Print current base config
```

### `gt hooks override <target>`

Edit overrides for a specific role or rig+role.

```bash
gt hooks override crew              # Edit crew override
gt hooks override gastown/witness   # Edit gastown witness override
gt hooks override crew --show       # Print current override
```

### `gt hooks list`

Show all managed settings.local.json locations and their sync status.

```bash
gt hooks list             # Show all targets
gt hooks list --json      # Machine-readable output
```

### `gt hooks scan`

Scan the workspace for existing hooks (reads current settings files).

```bash
gt hooks scan             # List all hooks
gt hooks scan --verbose   # Show hook commands
gt hooks scan --json      # JSON output
```

### `gt hooks init`

Bootstrap base config from existing settings.local.json files. Analyzes all
current settings, extracts common hooks as the base, and creates overrides
for per-target differences.

```bash
gt hooks init             # Bootstrap base and overrides
gt hooks init --dry-run   # Preview what would be created
```

Only works when no base config exists yet. Use `gt hooks base` to edit
an existing base config.

### `gt hooks registry` / `gt hooks install`

Browse and install hooks from the registry.

```bash
gt hooks registry                  # List available hooks
gt hooks install <hook-id>         # Install a hook to base config
```

## Current Registry Hooks

The registry (`~/gt/hooks/registry.toml`) defines 7 hooks, 5 enabled by default:

| Hook | Event | Enabled | Roles |
|---|---|---|---|
| pr-workflow-guard | PreToolUse | Yes | crew, polecat |
| session-prime | SessionStart | Yes | all |
| pre-compact-prime | PreCompact | Yes | all |
| mail-check | UserPromptSubmit | Yes | all |
| costs-record | Stop | Yes | crew, polecat, witness, refinery |
| clone-guard | PreToolUse | No | crew, polecat |
| dangerous-command-guard | PreToolUse | Yes | crew, polecat |

Additional hooks exist in settings.json files but are not yet in the registry:

- **bd init guard** (gastown/crew, beads/crew) - blocks `bd init*` inside `.beads/`
- **mol patrol guards** (gastown roles) - blocks persistent patrol molecules
- **tmux clear-history** (gastown root) - clears terminal history on session start
- **SessionStart .beads/ validation** (gastown/crew, beads/crew) - validates CWD

## Design Decision: Registry as Catalog vs Source of Truth

> **Decision: The registry is a catalog, not the source of truth.**
>
> The registry (`registry.toml`) lists available hooks. The base/overrides system
> (`~/.gt/hooks-base.json` + `~/.gt/hooks-overrides/`) defines what is active.
> `gt hooks install` copies from the registry into the base/overrides config.
>
> This separation provides:
> - Per-machine customization (PATH differences across machines)
> - Per-role overrides without polluting the shared registry
> - Clear distinction between "what hooks exist" and "what hooks are active where"
>
> The registry is the menu. The base/overrides are the order.

## Known Gaps

1. **Registry doesn't cover all active hooks** — Several hooks in settings.json
   files are not in `registry.toml` (bd-init-guard, mol-patrol-guard, tmux-clear,
   cwd-validation). These should be added so `gt hooks install` can manage them.

2. **No `gt tap` commands beyond pr-workflow** — The tap framework has only one
   guard implemented. `gt tap guard dangerous-command` is referenced in the
   registry but does not exist yet. Priority order: dangerous-command, bd-init,
   mol-patrol, then audit git-push.

3. **No `gt tap disable/enable` convenience commands** — Per-worktree
   enable/disable is possible via the override mechanism (`gt hooks override`
   with empty hooks list), but there is no convenience wrapper yet.

4. **Private hooks (settings.local.json)** — Claude Code supports
   `settings.local.json` for personal overrides. Gas Town doesn't manage
   these yet. Low priority since Gas Town is primarily agent-operated.

5. **Hook ordering** — No action needed currently. The merge chain
   (base -> override) produces deterministic order, and per-matcher merge
   ensures one entry per event type.

## Integration

### `gt rig add`

When a new rig is created, hooks are automatically synced for all the
new rig's targets (crew, witness, refinery, polecats).

### `gt doctor`

The `hooks-sync` check verifies all settings.local.json files match what
`gt hooks sync` would generate. Use `gt doctor --fix` to auto-fix
out-of-sync targets.

## Per-matcher merge semantics

Different matchers are always appended. An override entry with an empty
hooks list **removes** that matcher, regardless of matcher kind. What
happens when an override has the *same* matcher as a base entry depends on
the matcher kind (gt-5ihs):

- **Permission-pattern matcher** (contains `(`, e.g. `Bash(git push*)`, or
  the empty `""` matcher used by non-PreToolUse event types) — the override
  **replaces** the base entry entirely.
- **Bare tool-name matcher** (no parentheses, e.g. `Bash`, `Edit|Write`) —
  the override's hooks are **unioned** into the base entry's hooks instead,
  keyed by `(command, if)` so re-merging stays idempotent. Every PreToolUse
  guard must route through a bare tool-name matcher post-gt-5ihs (Claude
  Code's matcher only ever matches the tool name), so pr-workflow,
  dangerous-command, and per-role guards like formula-allowlist commonly
  all share matcher `"Bash"`, discriminated by each hook's `if` field (or
  by self-inspecting the command) instead of by matcher. Whole-entry
  replace would silently drop one layer's guards whenever another layer
  also targets `"Bash"`.

Example base (bare matcher — unions):
```json
{
  "PreToolUse": [
    { "matcher": "Bash", "hooks": [
      { "type": "command", "command": "gt tap guard dangerous-command" }
    ] }
  ]
}
```

Override for dog:
```json
{
  "PreToolUse": [
    { "matcher": "Bash", "hooks": [
      { "type": "command", "command": "gt tap guard formula-allowlist" }
    ] }
  ]
}
```

Result: dog sessions get **both** `dangerous-command` and
`formula-allowlist` on the same `"Bash"` matcher — not just the override's
hook. To disable a single one of several bare-matcher guards, there is
currently no per-hook removal: an override entry with an empty hooks list
removes the *entire* matcher's hooks from every layer, not just the
override layer's own contribution.

Example base (empty `""` matcher — replaces):
```json
{
  "SessionStart": [
    { "matcher": "", "hooks": [{ "type": "command", "command": "gt prime" }] }
  ]
}
```

Override for witness:
```json
{
  "SessionStart": [
    { "matcher": "", "hooks": [{ "type": "command", "command": "gt prime --witness" }] }
  ]
}
```

Result: The witness gets `gt prime --witness` instead of `gt prime`
(same `""` matcher = replace).

## Default base config

When no base config exists, the system uses sensible defaults:

- **SessionStart**: PATH setup + `gt prime --hook`
- **PreCompact**: PATH setup + `gt prime --hook`
- **UserPromptSubmit**: PATH setup + `gt mail check --inject`
- **Stop**: PATH setup + `gt costs record`
