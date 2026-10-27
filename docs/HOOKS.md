# Gas Town Hooks Management

Centralized hook management for Gas Town workspaces.

## Overview

Every agent runs the Claude Code CLI, so every session gets the same
`settings.json` lifecycle hooks (`<role>/.claude/settings.json`), whatever
backend the agent points the CLI at.

Gas Town writes `.claude/settings.json` into its own parent directories, so
customer repos stay clean. One base config plus per-role/per-rig overrides is
the single source of truth.

## Architecture

```
~/.gt/hooks-base.json              ← Shared base config (all agents)
~/.gt/hooks-overrides/
  ├── crew.json                    ← Override for all crew workers
  ├── polecat.json                 ← Override for all polecats
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
branch its owner cannot see. File-writing tools are checked against the
polecat's own worktree, and shell writes — interpreters, `curl -o`,
redirections, `cd` and `git -C` targets — are blocked when they name a town
path outside it, its polecat directory, or its rig's `.repo.git`. Work stays
under `~/gt`: scratch files, trace logs and test data go in the worktree,
never `/tmp`, `/private/tmp`, `HOME` or a session scratchpad (gt-3n4e0).
`$HOME/.local/bin`, the `gt`/`bd` install directory shared by all agents, is
denied to every write (gt-tnts5). Reads stay allowed anywhere; an
unresolvable target is blocked, as is a write target reached through a variable
the command line itself assigns or sets from `command -v` — including one
reached inside an interpreter's `-c` payload. A command keeps that rule
wherever a construct puts it: a subshell, a brace group and a `case` body are
read through, not skipped (gt-tt8sg).

The `polecats` override denies permission prompts nobody can answer
(`gt tap guard permission-request`, gt-8stz); attended roles carry no entry.
They deny the question tool too (`gt tap guard question-tool`, gt-163k8, matcher
`AskUserQuestion`): it parks the session while it reads as running — a polecat
sat 4h26m on one.

It also carries `gt tap nudge-drain` (gt-drwph) as a `PostToolUse` hook on the
empty `""` matcher, so it runs after every tool call: it drains the session's
queued nudges, hands them to the model once as hook `additionalContext` (a
drained nudge leaves the queue) and exits 0. The `UserPromptSubmit` hook below
is the only other drain and never fires while a seat runs tool turns, so the
overrun guard's queued split-proposal nudge expired unseen. Hooks arrive only in
the `--settings` file, a Claude CLI flag: the Claude CLI is the only runtime
(gt-be0z).

The `bd-close-invariant` guard (`gt tap guard bd-close-invariant`, gt-arno) runs
on the `Bash|Monitor` matcher for every role, from `DefaultBase()`. It is the
town-wide half of the gt-6hmz close-time invariant: `gt done` applies that
invariant to its own self-close, but `bd` is an external binary, so a raw
`bd close <id>` never reaches gt's Go code. This guard parses the command from
the hook payload and evaluates the same predicate, refusing exactly what
`gt done` refuses — plus the `supersede:`/`cancel:` operator override a raw
close can carry. Only ids naming the bead the current branch was cut for are
judged; other closes and unresolvable input pass.

The `bd-create-store` guard refuses a polecat's `bd create` that would file into the town's `hq` store, judged from the effective cwd.

Settings are passed to Claude Code via `--settings <path>`, which loads them as
a separate priority tier that merges additively with project settings.

## Commands

| Command | Does |
|---|---|
| `gt hooks sync` | Regenerate every managed settings file from base + overrides, keeping non-hooks fields |
| `gt hooks diff` | Show what `sync` would change |
| `gt hooks base` | Edit the shared base config |
| `gt hooks override <target>` | Edit a role or rig+role override |
| `gt hooks list` / `gt hooks scan` | Show managed targets and their sync status / the hooks in current settings files |
| `gt hooks init` | Bootstrap base and overrides from existing settings files; only when no base exists |
| `gt hooks registry` / `gt hooks install <id>` | Browse the registry / install one of its hooks |

## Shipped hooks

No registry file ships with the repo: `~/gt/hooks/registry.toml` is an optional
town-local catalog. A settings file's hooks come from `DefaultBase()` for every
role plus `DefaultOverrides()` (`internal/hooks/config.go`):

| Hook | Event | Roles |
|---|---|---|
| pr-workflow, dangerous-command, container-suite, bd-close-invariant, bd-create-store guards | PreToolUse | all |
| session-prime (`gt prime --hook`) | SessionStart | all |
| pre-compact-prime (`gt prime --hook`) | PreCompact | all |
| mail-check (`gt mail check --inject`) | UserPromptSubmit | all |
| nudge-drain | PostToolUse | polecats |
| polecat-stop-check | Stop | polecats |
| polecat-paths, question-tool guards (polecats); script-files (polecats, crew) | PreToolUse | polecats, crew |
| permission-request guard | PermissionRequest | polecats |
| crew compaction handoff (`gt handoff --cycle --reason compaction`) | PreCompact (replaces) | crew |

Not in that table: guards the tap surface carries that no default installs
(bd-init, mol-patrol), and any hook a town adds to its own settings files.
`gt tap list` and the settings files on disk are the live list.

## Registry is a catalog, not the source of truth

`registry.toml` lists the hooks that exist; base + overrides decide which are
active where. `gt hooks install` changes only settings files; base and overrides
are unchanged. That split keeps per-machine differences (PATH) and per-role
overrides out of the shared registry.

## Known Gaps

1. The registry does not cover all active hooks (the list above).
2. No `gt tap disable/enable` wrapper: per-worktree disable is an override with
   an empty hooks list.
3. Gas Town does not manage Claude Code's `settings.local.json`.

## Integration

`gt rig add` syncs hooks for the new rig's targets. The `hooks-sync` doctor
check flags any settings file that differs from what `gt hooks sync` would
write; `gt doctor fix hooks-sync` rewrites it.

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
  own forbidden shape (`tap_guard_polecat_paths.go`, `tap_guard.go`,
  `tap_guard_dangerous.go`). The field remains
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

Override for polecat:
```json
{
  "SessionStart": [
    { "matcher": "", "hooks": [{ "type": "command", "command": "gt prime --explain" }] }
  ]
}
```

Result: Polecats get `gt prime --explain` instead of `gt prime`
(same `""` matcher = replace).
