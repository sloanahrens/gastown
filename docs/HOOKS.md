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

Copilot's JSON hooks cover the same lifecycle as Claude Code's. The `gt hooks`
commands below apply to Claude Code (and Gemini) only.

Gas Town writes `.claude/settings.json` into its own parent directories and
passes it to Claude Code via `--settings`, so customer repos stay clean. One
base config plus per-role/per-rig overrides is the single source of truth.

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
| Polecats (shared) | `<rig>/polecats/.claude/settings.json` | `<rig>/polecats` |

Town-level target:
- `mayor/.claude/settings.json` (key: `mayor`)

The `polecats` override adds the polecat-paths guard (`gt tap guard
polecat-paths`, gt-hmaf) on the `Bash|Monitor` and
`Edit|Write|MultiEdit|NotebookEdit` matchers: a cross-worktree edit corrupts a
branch its owner cannot see. File-writing tools are limited to the polecat's
own worktree (plus temp dirs and the session scratchpad), and shell writes —
interpreters, `curl -o`, redirections, `cd` and `git -C` targets — are blocked
when they name a town path outside it, its polecat directory, or its rig's
`.repo.git`. `$HOME/.local/bin`, the `gt`/`bd` install directory shared by all
agents, is denied to every write (gt-tnts5). Reads stay allowed anywhere; an
unresolvable target is blocked.

The `polecats` override denies permission prompts nobody can answer
(`gt tap guard permission-request`, gt-8stz); attended roles carry no entry.
They deny the question tool too (`gt tap guard question-tool`, gt-163k8, matcher
`AskUserQuestion`): it parks the session while it reads as running — a polecat
sat 4h26m on one.

The `bd-close-invariant` guard (`gt tap guard bd-close-invariant`, gt-arno) runs
on the `Bash|Monitor` matcher for every role, from `DefaultBase()`. It is the
town-wide half of the gt-6hmz close-time invariant: `gt done` applies that
invariant to its own self-close, but `bd` is an external binary, so a raw
`bd close <id>` never reaches gt's Go code. This guard parses the command from
the hook payload and evaluates the same predicate, refusing exactly what
`gt done` refuses — plus the `supersede:`/`cancel:` operator override a raw
close can carry. Only ids naming the bead the current branch was cut for are
judged; other closes and unresolvable input pass.

The `boot` override adds the raw-tmux-send-keys guard (`gt tap guard
boot-sendkeys`, gt-3mp1) on the `Bash|Monitor` matcher: a raw `tmux send-keys`
into another pane can leave text staged but unsubmitted, so boot uses
`gt nudge --mode=immediate` instead. The guard reads `tool_input.command` off
stdin and blocks only a genuine tmux send-keys invocation.

Settings are passed to Claude Code via `--settings <path>`, which loads them as
a separate priority tier that merges additively with project settings.

## Commands

Each takes `--help` for its flags.

| Command | Does |
|---|---|
| `gt hooks sync [--dry-run]` | Regenerate every managed settings file from base + overrides, keeping non-hooks fields |
| `gt hooks diff` | Show what `sync` would change |
| `gt hooks base [--show]` | Edit (or print) the shared base config |
| `gt hooks override <target> [--show]` | Edit (or print) a role or rig+role override |
| `gt hooks list` / `gt hooks scan` | Show managed targets and their sync status / the hooks in current settings files |
| `gt hooks init [--dry-run]` | Bootstrap base and overrides from existing settings files; only when no base exists |
| `gt hooks registry` / `gt hooks install <id>` | Browse the registry / copy a hook into the base config |

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
- **polecat-paths guard** (polecats) - what it blocks: the `polecats` override
  paragraph above
- **tmux clear-history** (gastown root) - clears terminal history on session start
- **SessionStart .beads/ validation** (gastown/crew, beads/crew) - validates CWD

## Registry is a catalog, not the source of truth

`registry.toml` lists the hooks that exist; base + overrides decide which are
active where. `gt hooks install` copies from one to the other. The split keeps
per-machine differences (PATH) and per-role overrides out of the shared registry.

## Known Gaps

1. The registry does not cover all active hooks (the list above).
2. No `gt tap disable/enable` wrapper: per-worktree disable is an override with
   an empty hooks list.
3. Gas Town does not manage Claude Code's `settings.local.json`.

## Integration

`gt rig add` syncs hooks for the new rig's targets. The `hooks-sync` doctor
check flags any settings file that differs from what `gt hooks sync` would
write; `gt doctor --fix` rewrites it.

## Per-matcher merge semantics

Different matchers are always appended. An override entry with an empty
hooks list **removes** that matcher, regardless of matcher kind. What
happens when an override has the *same* matcher as a base entry depends on
the matcher kind (gt-5ihs):

- **Permission-pattern matcher** (contains `(`, e.g. `Bash(git push*)`, or
  the empty `""` matcher used by non-PreToolUse event types) — the override
  **replaces** the base entry entirely.
- **Bare tool-name matcher** (no parentheses, e.g. `Edit|Write`) — the
  override's hooks are **unioned** into the base entry's hooks instead,
  keyed by `(command, if)` so re-merging stays idempotent. Every
  shell-executing PreToolUse guard shares one bare matcher,
  `"Bash|Monitor"`, because Claude Code's matcher only ever matches the
  tool name (gt-5ihs) and Monitor runs the same command shape as Bash
  (gt-vx2mm): bare `"Bash"` would leave `rm -rf`, a force push, or an
  unwrapped suite unblocked there. The guard discriminates by
  self-inspecting the command, not by matcher — `Bash|Monitor` is
  `shellExecutingToolMatcher` (`internal/hooks/config.go`), and a guard
  branching on `tool_name` instead needs its own case per named tool.
  Whole-entry replace would silently drop one layer's guards whenever
  another layer also targets that matcher.

  Built-in hooks do not use the `if` field at all (gt-3mp1). Claude Code's
  `if` evaluator resolves a command it cannot statically analyze — a brace
  group holding a quoted string (`echo x{"a"}y`, any JSON/dict literal), or
  an argument-position `$(...)` substitution — as matching ANY pattern, so
  a deny hook gated by a leading-`*` glob fires on unrelated commands. A
  guard that needs to discriminate on the command text reads
  `tool_input.command` off stdin and exits 2 only when it recognizes its
  own forbidden shape (`tap_guard_boot_sendkeys.go`,
  `tap_guard_pr_workflow.go`, `tap_guard_dangerous.go`). The field remains
  available for operator-supplied overrides;
  `TestBuiltinHooksNeverUseIf` asserts no built-in config sets it.

Example base (bare matcher — unions):
```json
{
  "PreToolUse": [
    { "matcher": "Bash|Monitor", "hooks": [
      { "type": "command", "command": "gt tap guard dangerous-command" }
    ] }
  ]
}
```

Override for polecats:
```json
{
  "PreToolUse": [
    { "matcher": "Bash|Monitor", "hooks": [
      { "type": "command", "command": "gt tap guard polecat-paths" }
    ] }
  ]
}
```

Result: polecat sessions get **both** `dangerous-command` and
`polecat-paths` on the `"Bash|Monitor"` matcher. There is no per-hook removal:
an override entry with an empty hooks list removes the *entire* matcher's hooks
from every layer.

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
