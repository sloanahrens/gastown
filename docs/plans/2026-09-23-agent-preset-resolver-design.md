# Agent preset resolver — design

Date: 2026-09-23. Tracking: claude-9a8 (handoff bead), gastown parent bead TBD at filing.

## Problem

Town agents are custom presets defined in `settings/config.json` under `agents`
(for example `deepseek-flash`: `provider=claude`, `command=claude`). Sessions
carry the custom name in `GT_AGENT`. `config.GetAgentPresetByName` reads
`globalRegistry`, which holds only the built-in presets plus
`settings/agents.json`. Nothing copies `config.json` agents into it, so every
lookup by a custom name returns nil. A throwaway test proved this against the
real loading path.

The consequence that started this work: `tmux.effectiveSkipEscape` never sees
the `claude` preset's `EscapeCancelsRequest`, so the gt-cyyg rule ("never send
Escape to Claude Code") protects no town agent. The only remaining guard is the
busy-indicator scrape, which fails open. On 2026-09-22 23:12:51 UTC a nudge's
Escape landed mid-tool-call in `gastown/witness`. Claude Code reported it as
"[Request interrupted by user for tool use]", and the witness held for 20 hours
waiting for an operator who does not exist.

The same blind spot affects every caller that looks up a preset by an agent
name:

| Caller | Effect of nil today |
|---|---|
| `tmux.effectiveSkipEscape` | Escape not suppressed for custom Claude agents |
| `tmux.NudgePane` | No identity check at all; scrape only |
| `tmux.readyPromptPrefixForSession` | Falls back to the default prefix (correct only by accident) |
| `cmd/nudge_poller.go` | `SkipEscape` and prompt detection not set |
| `cmd/nudge.go` wait-idle | Degrade-to-queue check skipped |
| `cmd/sling_helpers.go shouldAcceptPermissionWarning` | Permission warning never accepted |
| `crew/manager.go buildResumeArgs` | "agent does not support session resume" |
| `crew/manager.go` startup dialogs | Keys on `rc.Provider`; wrong for `provider=deepseek command=claude` |
| `runtime.SessionIDFromEnv` | Falls back to `CLAUDE_SESSION_ID` (correct only by accident) |
| `cmd/hooks_sync.go`, `doctor/hooks_sync_check.go` | Skip; a no-op for Claude harnesses, wrong for custom non-Claude harnesses |
| `rig/manager.go` polecat scaffold | Custom `default_agent` gets no scaffold |
| `templates/commands/provision.go` | Custom agent gets no config dir |

## Decisions (settled by grilling)

1. The harness is named by the **command** (basename, wrappers unwrapped).
   Provider is a fallback. This matches `isClaudeAgent` and `ResolveProcessNames`.
2. An **explicit resolver** is called by each caller. Custom agents are not
   injected into `globalRegistry` (process-global state; see gt-5v82).
3. The resolver returns the **harness preset unchanged**. Per-agent trait
   overrides (such as gt-3vfs `CLAUDE_CONFIG_DIR`) are out of scope.
4. **Fail safe**: an agent that cannot be resolved gets no Escape. Only a
   positively identified harness without `EscapeCancelsRequest` (codex today)
   keeps the scrape-gated Escape.
5. **Three MRs**: MR1 adds the resolver and fixes the nudge path. MR2 moves the
   remaining callers and adds a guard test. MR3 updates the role templates.
6. `tmux` reads `GT_ROOT` and `GT_RIG` from the target session's environment,
   then falls back to `NudgeOpts.TownRoot`, then to the process `GT_ROOT`.
7. `NudgePane` maps its pane to a session and shares one Escape decision with
   `NudgeSessionWithOpts`.
8. Tests build a town in `t.TempDir()` with unique agent names, only read the
   registry, and never reset it.
9. The poller resolves once at startup; `gt nudge` resolves once per process.
10. Recovery guidance goes into town directives now (done 2026-09-23) and into
    the role templates (MR3).

## Design

### 1. Resolver (`internal/config/preset_resolve.go`), MR1

```go
func ResolveAgentPreset(name, townRoot, rigPath string) (*AgentPresetInfo, bool)
func harnessPresetName(command string, args []string, provider string, presets map[string]*AgentPresetInfo) string
func customAgentFor(name string, town *TownSettings, rig *RigSettings) *RuntimeConfig
```

`ResolveAgentPreset` tries these in order:

1. An empty name returns `(nil, false)`.
2. If the name is in the registry, return that preset (built-ins plus
   `agents.json`; `LoadAgentRegistry(DefaultAgentRegistryPath(townRoot))`
   runs first, as in `ResolveAgentConfigByName`).
3. If the name is a custom agent (rig settings win over town settings), pass
   its command, args and provider to `harnessPresetName` and return that
   registry preset.
4. Otherwise return `(nil, false)`.

`harnessPresetName` picks the harness:

- a. Normalize the command: basename, extension stripped, `gt-` prefix
  stripped. For a wrapper (`env`, `sudo`, `nohup`, …), use the binary that
  `extractWrappedBinary` finds in the args.
- b. If a preset's name equals the binary, use it (canonical first). This
  matters because the built-in `groq-compound` also has `command=claude`, and
  `claude` must win. Otherwise take the first preset, in sorted name order,
  whose command basename matches.
- c. With an empty command, use the provider if it names a preset.
- d. With an empty command and an empty provider, use `claude`. This is the
  existing default in `isClaudeAgent` and `fillRuntimeDefaults`.
- e. If the command is set but not recognized, use the provider when it names
  a preset. Otherwise return `""` (unresolvable).

`isClaudeAgent(rc)` is reimplemented as
`harnessPresetName(...) == "claude"`, and its existing tests must pass
unchanged. The command-match loop in `ResolveProcessNames` switches to the same
canonical-first, sorted lookup, which removes a latent map-order
nondeterminism.

Settings are read with `LoadOrCreateTownSettings` and `LoadRigSettings`. Both
are read-only; neither creates a missing file. A read error means the agent is
unresolvable, not an error.

### 2. Nudge path (`internal/tmux`, `internal/cmd`), MR1

- `(t *Tmux) sessionPreset(session, townRootHint string) (*config.AgentPresetInfo, bool)`
  reads `GT_AGENT`, `GT_ROOT` and `GT_RIG` from the session environment. The
  town root comes from the session `GT_ROOT`, then `townRootHint`, then the
  process `GT_ROOT`. The rig path is `<townRoot>/<GT_RIG>` when `GT_RIG` is
  set. It then calls `config.ResolveAgentPreset`.
- The pure function `escapeAllowed(agentName string, preset *AgentPresetInfo, ok bool) bool`
  returns false when `!ok`, when the name is `copilot` (kept for
  compatibility), or when `preset.EscapeCancelsRequest` is set. Otherwise it
  returns true. It replaces `effectiveSkipEscape`.
- `(t *Tmux) escapeSafe(target, session, townRootHint string) bool` returns
  `escapeAllowed(...) && t.shouldSendEscape(target)`. The identity gate runs
  first, so no pane capture happens for Claude sessions.
- `NudgeSessionWithOpts` uses
  `sendEscape := !opts.SkipEscape && t.escapeSafe(target, session, opts.TownRoot)`.
- `NudgePane` gets its session name from
  `display-message -p -t <pane> '#{session_name}'` and uses the same
  `escapeSafe`. If that lookup fails, the session name is empty, the agent is
  unresolvable, and no Escape is sent.
- `readyPromptPrefixForSession` uses `sessionPreset`.
- `cmd/nudge_poller.go` uses `config.ResolveAgentPreset(agentName, townRoot, rigPathFromSessionEnv)`
  for `SkipEscape` and prompt detection. An unresolved agent sets
  `SkipEscape=true`.
- In `cmd/nudge.go` wait-idle, a set but unresolvable `GT_AGENT`, or a
  resolved preset with no `ReadyPromptPrefix`, degrades to queue mode. An
  empty `GT_AGENT` keeps today's behavior.

### 3. Remaining callers, MR2

Each caller switches from `GetAgentPresetByName(agentName)` to
`ResolveAgentPreset(agentName, townRoot, rigPath)`, using the town root and rig
path it already has, or the process `GT_ROOT`/`GT_RIG` in
`runtime.SessionIDFromEnv`. In `crew/manager.go`, the startup-dialog branch
resolves the worker agent's name instead of `rc.Provider`. The nil-handling in
each caller does not change: an unresolved agent behaves as a nil preset does
today.

`seance.go`, `config.go` (agent list) and `runtime.go:42` iterate built-in
names or key on the hooks provider, so they stay as they are.

Guard test (`internal/config/preset_lookup_guard_test.go`): parse every
non-test Go file under `internal/` outside `internal/config` with `go/parser`
and find each call to `config.GetAgentPresetByName`. Any call that is not on
the allow-list fails the test with a message pointing at
`ResolveAgentPreset`. The allow-list is keyed by file and enclosing function,
and names only lookups whose argument is a built-in or registry name: the
`seance.go resolveSeanceCommand` loop, the `config.go` agent-list loops, and
`runtime.go`'s hooks-provider lookup.

### 4. Role templates, MR3

New `internal/templates/roles/partial-interrupts.md.tmpl` holding
`{{ define "interrupt-policy" }}…{{ end }}`, with the same text as the town
directive: no human types into the pane; a bare interrupt is a delivery
artifact; a hold covers only what it names; cite the order or resume. The
witness, deacon, refinery and polecat templates include it with
`{{ template "interrupt-policy" }}` near their patrol or work-loop sections.
Every role file is parsed into one set and rendered by name, so a define-only
file adds no role. A template test asserts that the four roles render the
section and that mayor and crew do not.

## Error handling

The resolver never returns an error. It returns ok=false, and each caller
applies its existing nil behavior. The one change in behavior is the Escape
decision, where an unresolved agent now suppresses Escape (decision 4). The
cost is that a vim-mode composer on an unknown harness may keep a nudge
unsubmitted; `submitComposer` verification already reports that case.

## Testing

- `harnessPresetName`: table test over claude, `/path/to/claude`, `gt-claude`,
  `env -u X claude`, empty command with provider claude/codex/unknown, empty
  command and provider, unknown command with provider claude, unknown command
  with no provider, and command `claude` with provider `deepseek`. It uses a
  local preset map, not the global registry, so it can run in parallel.
- `ResolveAgentPreset`: in a TempDir town, a custom `test-9a8-flash`
  (command claude) resolves to `claude` with `EscapeCancelsRequest`; a rig
  override beats the town definition; an unknown name returns false; built-in
  `codex` resolves to itself.
- `escapeAllowed`: a table over unresolved, copilot, claude, gemini, codex, and
  a custom agent resolved to claude.
- Live tmux end-to-end, not parallel: a session with `GT_AGENT=test-9a8-flash`
  and `GT_ROOT=<tempTown>` makes `escapeSafe` return false even on an idle
  pane, while `GT_AGENT=codex` on an idle pane returns true.
- The existing `isClaudeAgent` and `ResolveProcessNames` tests pass unchanged.
  In `TestEffectiveSkipEscape`, the rows for an empty or unknown agent flip to
  fail-safe and move into the `escapeAllowed` test.

## Out of scope

- Per-agent trait overrides on custom presets (gt-3vfs class).
- Stamping `GT_HARNESS` at spawn (approach B, rejected).
- Making the busy scrape itself more robust (#4240).
