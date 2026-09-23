> Status: in progress (2026-09-23). Implemented across three MRs (resolver + nudge path, remaining callers, role templates). Tracked in claude-9a8.

# Agent Preset Resolver Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Resolve custom town agents (`settings/config.json` `agents`) to the preset of the harness that actually runs, so preset-keyed behavior works for them. The first such behavior is the gt-cyyg rule that Claude Code never receives a nudge Escape.

**Architecture:** One read-time resolver in `internal/config` (`ResolveAgentPreset`, `HarnessPreset`, and the pure `harnessPresetName`) shares the command-wins identity rule with `isClaudeAgent` and `ResolveProcessNames`. `internal/tmux` gets one Escape decision (`escapeAllowed` and `escapeSafe`) used by both nudge functions. Callers move from `GetAgentPresetByName(name)` to the resolver.

**Tech Stack:** Go, tmux, `text/template`.

**Spec:** `docs/plans/2026-09-23-agent-preset-resolver-design.md`

## Global Constraints

- Three MRs. MR1 (Tasks 1–3) is on `crew/sloan/claude-9a8`. MR2 (Tasks 4–5) is on `crew/sloan/claude-9a8-callers`, stacked on MR1. MR3 (Task 6) is on `crew/sloan/claude-9a8-templates`, off `origin/main`.
- Tests never call `ResetRegistryForTesting`, `RegisterAgentForTesting` or fixture `LoadAgentRegistry` (gt-5v82). Custom agent names in tests start with `test-9a8-`.
- An unresolvable agent never receives Escape.
- No AI attribution in commits. After each commit, run `bd comments add claude-9a8 "commit: <hash> — <summary>"` from `~/.claude`.
- Package tests: `go test ./internal/<pkg>/ -run <Name> -count=1`. Before submitting: `make build`, `make lint`, and the affected packages' tests.

---

### Task 1: Resolver in `internal/config` (MR1)

**Files:**
- Create: `internal/config/preset_resolve.go`
- Create: `internal/config/preset_resolve_test.go`
- Modify: `internal/config/loader.go` (`isClaudeAgent`, ~line 1621)
- Modify: `internal/config/agents.go` (the command-match loop in `ResolveProcessNames`, ~line 1008)

**Interfaces:**
- Produces: `ResolveAgentPreset(name, townRoot, rigPath string) (*AgentPresetInfo, bool)`, `HarnessPreset(rc *RuntimeConfig) (*AgentPresetInfo, bool)`, and the unexported `harnessPresetName(command string, args []string, provider string, presets map[string]*AgentPresetInfo) string` and `presetTable() map[string]*AgentPresetInfo`.

- [ ] **Step 1: Write the failing tests** (`preset_resolve_test.go`):

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHarnessPresetName(t *testing.T) {
	t.Parallel()
	presets := map[string]*AgentPresetInfo{
		"claude":        {Name: "claude", Command: "claude"},
		"groq-compound": {Name: "groq-compound", Command: "claude"},
		"codex":         {Name: "codex", Command: "codex"},
		"kiro":          {Name: "kiro", Command: "kiro-cli"},
	}
	tests := []struct {
		name, command, provider string
		args                    []string
		want                    string
	}{
		{"bare claude beats groq-compound", "claude", "", nil, "claude"},
		{"path to claude", "/usr/local/bin/claude", "", nil, "claude"},
		{"gt- wrapper", "gt-claude", "", nil, "claude"},
		{"env wrapper", "env", "", []string{"-u", "X", "claude", "--flag"}, "claude"},
		{"command wins over provider", "claude", "deepseek", nil, "claude"},
		{"command basename maps to preset name", "kiro-cli", "", nil, "kiro"},
		{"empty command uses provider", "", "codex", nil, "codex"},
		{"empty command and provider default to claude", "", "", nil, "claude"},
		{"empty command unknown provider", "", "generic", nil, ""},
		{"unknown command falls back to provider", "my-wrapper.sh", "claude", nil, "claude"},
		{"unknown command no provider", "aider", "", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := harnessPresetName(tt.command, tt.args, tt.provider, presets); got != tt.want {
				t.Errorf("harnessPresetName(%q, %v, %q) = %q, want %q", tt.command, tt.args, tt.provider, got, tt.want)
			}
		})
	}
}

func writeTestSettings(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveAgentPreset(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rig := filepath.Join(town, "myrig")
	writeTestSettings(t, TownSettingsPath(town), `{"type":"town-settings","version":1,"agents":{
		"test-9a8-flash":{"provider":"claude","command":"claude"},
		"test-9a8-dog":{"provider":"deepseek","command":"claude"},
		"test-9a8-over":{"command":"claude"},
		"test-9a8-script":{"command":"/opt/bin/mystery"}}}`)
	writeTestSettings(t, RigSettingsPath(rig), `{"type":"rig-settings","version":1,"agents":{
		"test-9a8-over":{"command":"codex"}}}`)

	tests := []struct {
		name, agent, rigPath, want string
		ok                         bool
	}{
		{"custom claude agent", "test-9a8-flash", "", "claude", true},
		{"provider deepseek command claude", "test-9a8-dog", "", "claude", true},
		{"rig definition wins over town", "test-9a8-over", rig, "codex", true},
		{"town definition without rig", "test-9a8-over", "", "claude", true},
		{"builtin passes through", "codex", "", "codex", true},
		{"unknown name", "test-9a8-nope", "", "", false},
		{"unrecognised command", "test-9a8-script", "", "", false},
		{"empty name", "", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ResolveAgentPreset(tt.agent, town, tt.rigPath)
			if ok != tt.ok {
				t.Fatalf("ResolveAgentPreset(%q) ok = %v, want %v", tt.agent, ok, tt.ok)
			}
			if ok && string(got.Name) != tt.want {
				t.Errorf("ResolveAgentPreset(%q) = %q, want %q", tt.agent, got.Name, tt.want)
			}
		})
	}
	if p, _ := ResolveAgentPreset("test-9a8-flash", town, ""); !p.EscapeCancelsRequest {
		t.Error("custom claude agent must carry the claude preset's EscapeCancelsRequest (gt-cyyg)")
	}
}
```

Also add these rows to `TestIsClaudeAgent` in `loader_test.go`, which record the deliberate changes:

```go
		{"env-wrapped claude → claude", &RuntimeConfig{Command: "env", Args: []string{"-u", "X", "claude"}}, true},
		{"claude provider + gemini command → command wins", &RuntimeConfig{Provider: "claude", Command: "gemini"}, false},
```

- [ ] **Step 2: Run the tests and confirm they fail.** Run `go test ./internal/config/ -run 'TestHarnessPresetName|TestResolveAgentPreset|TestIsClaudeAgent' -count=1`. Expected: a compile failure (`undefined: harnessPresetName`).

- [ ] **Step 3: Implement** `preset_resolve.go`:

```go
package config

import (
	"path/filepath"
	"sort"
	"strings"
)

// ResolveAgentPreset maps an agent name — a GT_AGENT value, role_agents entry,
// or default_agent — to the preset of the harness that actually runs it.
// Custom agents in rig then town settings/config.json come first (the same
// order as lookupAgentConfigIfExists), then the registry. ok=false means the
// name could not be identified: callers must treat that as an unknown
// harness, never as Claude. (claude-9a8)
func ResolveAgentPreset(name, townRoot, rigPath string) (*AgentPresetInfo, bool) {
	if name == "" {
		return nil, false
	}
	if townRoot != "" {
		_ = LoadAgentRegistry(DefaultAgentRegistryPath(townRoot))
	}
	if rigPath != "" {
		_ = LoadRigAgentRegistry(RigAgentRegistryPath(rigPath))
	}
	var town *TownSettings
	if townRoot != "" {
		if ts, err := LoadOrCreateTownSettings(TownSettingsPath(townRoot)); err == nil {
			town = ts
		}
	}
	var rig *RigSettings
	if rigPath != "" {
		if rs, err := LoadRigSettings(RigSettingsPath(rigPath)); err == nil {
			rig = rs
		}
	}
	if rc := customAgentFor(name, town, rig); rc != nil {
		return HarnessPreset(rc)
	}
	if preset := GetAgentPresetByName(name); preset != nil {
		return preset, true
	}
	return nil, false
}

// HarnessPreset returns the preset of the harness a RuntimeConfig launches,
// by the same command-wins rule as isClaudeAgent.
func HarnessPreset(rc *RuntimeConfig) (*AgentPresetInfo, bool) {
	if rc == nil {
		return nil, false
	}
	table := presetTable()
	name := harnessPresetName(rc.Command, rc.Args, rc.Provider, table)
	if name == "" {
		return nil, false
	}
	preset := table[name]
	return preset, preset != nil
}

// customAgentFor finds a custom agent definition, rig before town. It does
// not apply fillRuntimeDefaults, so an unset command stays distinguishable.
func customAgentFor(name string, town *TownSettings, rig *RigSettings) *RuntimeConfig {
	if rig != nil && rig.Agents != nil {
		if rc, ok := rig.Agents[name]; ok && rc != nil {
			return rc
		}
	}
	if town != nil && town.Agents != nil {
		if rc, ok := town.Agents[name]; ok && rc != nil {
			return rc
		}
	}
	return nil
}

// presetTable merges the immutable built-in presets with a snapshot of the
// registry. Built-ins are copied first so a concurrent registry reset in
// tests cannot empty the table (gt-5v82).
func presetTable() map[string]*AgentPresetInfo {
	registryMu.Lock()
	defer registryMu.Unlock()
	initRegistryLocked()
	table := make(map[string]*AgentPresetInfo, len(builtinPresets)+len(globalRegistry.Agents))
	for name, preset := range builtinPresets {
		table[string(name)] = preset
	}
	for name, preset := range globalRegistry.Agents {
		table[name] = preset
	}
	return table
}

// harnessPresetName names the preset for a command line. The command is
// authoritative (basename, gt- prefix and wrappers such as `env -u X claude`
// unwrapped); provider is the fallback. An empty command and provider means
// Claude, the historical default. Returns "" when nothing matches.
func harnessPresetName(command string, args []string, provider string, presets map[string]*AgentPresetInfo) string {
	if command == "" {
		if provider == "" {
			return string(AgentClaude)
		}
		if _, ok := presets[provider]; ok {
			return provider
		}
		return ""
	}
	if name := presetForBinary(commandBinary(command, args), presets); name != "" {
		return name
	}
	if _, ok := presets[provider]; ok && provider != "" {
		return provider
	}
	return ""
}

// commandBinary returns the normalized binary a command line runs.
func commandBinary(command string, args []string) string {
	bin := commandBase(command)
	if wrapperCommands[bin] {
		bin = commandBase(extractWrappedBinary(bin, args))
	}
	return bin
}

func commandBase(command string) string {
	if command == "" {
		return ""
	}
	base := filepath.Base(command)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	return strings.TrimPrefix(base, "gt-")
}

// presetForBinary prefers the preset named after the binary (claude, not
// groq-compound, which also runs claude), then the first preset in sorted
// order whose command matches, so the answer never depends on map order.
func presetForBinary(bin string, presets map[string]*AgentPresetInfo) string {
	if bin == "" {
		return ""
	}
	if p, ok := presets[bin]; ok && p != nil && (p.Command == "" || commandBase(p.Command) == bin) {
		return bin
	}
	for _, name := range sortedPresetNames(presets) {
		if p := presets[name]; p != nil && p.Command != "" && commandBase(p.Command) == bin {
			return name
		}
	}
	return ""
}

func sortedPresetNames(presets map[string]*AgentPresetInfo) []string {
	names := make([]string, 0, len(presets))
	for name := range presets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
```

Replace the body of `isClaudeAgent` in `loader.go`, keeping its doc comment updated:

```go
// isClaudeAgent returns true if the RuntimeConfig launches Claude Code.
// The command is authoritative (see harnessPresetName); provider is the
// fallback; empty command and provider default to Claude.
func isClaudeAgent(rc *RuntimeConfig) bool {
	return harnessPresetName(rc.Command, rc.Args, rc.Provider, presetTable()) == string(AgentClaude)
}
```

Before the edit, run `grep -n "isClaudeAgent\|IsResolvedAgentClaude" internal/config/*.go` and confirm no caller holds `registryMu`, since `presetTable` takes it.

In `ResolveProcessNames`, change the loop `for _, info := range globalRegistry.Agents {` to iterate in canonical-first sorted order:

```go
		for _, name := range canonicalFirst(sortedPresetNames(globalRegistry.Agents), cmdBase, unwrappedCmdBase) {
			info := globalRegistry.Agents[name]
```

and add to `preset_resolve.go`:

```go
// canonicalFirst moves the names equal to any of want to the front,
// keeping the rest in order.
func canonicalFirst(names []string, want ...string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		for _, w := range want {
			if n == w {
				out = append(out, n)
				break
			}
		}
	}
	for _, n := range names {
		keep := true
		for _, w := range want {
			if n == w {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, n)
		}
	}
	return out
}
```

- [ ] **Step 4: Run the package tests.** Run `go test ./internal/config/ -count=1`. Expected: PASS, including every existing `TestIsClaudeAgent` and `ResolveProcessNames` test.

- [ ] **Step 5: Commit.** Run `git add internal/config && git commit -m "config: ResolveAgentPreset maps custom agents to their harness preset (claude-9a8)"`.

### Task 2: One Escape decision in `internal/tmux` (MR1)

**Files:**
- Modify: `internal/tmux/tmux.go`: replace `effectiveSkipEscape` (~1965), and change `NudgeSessionWithOpts` (~2032), `NudgePane` (~2120) and `readyPromptPrefixForSession` (~3825)
- Modify: `internal/tmux/tmux_test.go`: replace `TestEffectiveSkipEscape` (~2679)

**Interfaces:**
- Consumes: `config.ResolveAgentPreset`.
- Produces: `(t *Tmux) SessionAgentPreset(session, townRootHint string) (string, *config.AgentPresetInfo, bool)`, and the unexported `escapeAllowed(agentName string, preset *config.AgentPresetInfo, ok bool) bool` and `(t *Tmux) escapeSafe(target, session, townRootHint string) bool`.

- [ ] **Step 1: Write the failing tests.** Replace `TestEffectiveSkipEscape` with:

```go
// TestEscapeAllowed guards gt-cyyg and claude-9a8: an agent whose harness
// cancels on Escape (Claude Code, Gemini, Copilot), or whose harness cannot
// be identified, never receives the vim-mode Escape, whatever the busy
// scrape says.
func TestEscapeAllowed(t *testing.T) {
	t.Parallel()
	claude := config.GetAgentPresetByName("claude")
	gemini := config.GetAgentPresetByName("gemini")
	codex := config.GetAgentPresetByName("codex")
	tests := []struct {
		name   string
		agent  string
		preset *config.AgentPresetInfo
		ok     bool
		want   bool
	}{
		{"claude", "claude", claude, true, false},
		{"custom agent resolved to claude", "deepseek-flash", claude, true, false},
		{"gemini", "gemini", gemini, true, false},
		{"copilot legacy hardcode", "copilot", nil, false, false},
		{"codex keeps scrape-gated escape", "codex", codex, true, true},
		{"unresolved agent fails safe", "not-a-real-agent", nil, false, false},
		{"no GT_AGENT fails safe", "", nil, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := escapeAllowed(tt.agent, tt.preset, tt.ok); got != tt.want {
				t.Errorf("escapeAllowed(%q) = %v, want %v", tt.agent, got, tt.want)
			}
		})
	}
}

// TestEscapeSafe_CustomClaudeAgentOnIdlePane is the claude-9a8 regression: a
// session whose GT_AGENT is a custom town agent running claude gets no
// Escape even when the pane looks idle, and codex on an idle pane still does.
func TestEscapeSafe_CustomClaudeAgentOnIdlePane(t *testing.T) {
	tm := newTestTmux(t)
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "settings", "config.json"),
		[]byte(`{"type":"town-settings","version":1,"agents":{"test-9a8-flash":{"provider":"claude","command":"claude"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	session := "gt-test-escape-safe-" + t.Name()
	_ = tm.KillSession(session)
	if err := tm.NewSession(session, ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = tm.KillSession(session) }()
	_ = tm.SetEnvironment(session, "GT_ROOT", town)

	_ = tm.SetEnvironment(session, "GT_AGENT", "test-9a8-flash")
	if tm.escapeSafe(session, session, "") {
		t.Fatal("escapeSafe for custom claude agent on idle pane = true, want false")
	}
	_ = tm.SetEnvironment(session, "GT_AGENT", "codex")
	if !tm.escapeSafe(session, session, "") {
		t.Fatal("escapeSafe for codex on idle pane = false, want true")
	}
}
```

Add `os` and `path/filepath` to the test file's imports if they are missing.

- [ ] **Step 2: Run the tests and confirm they fail.** Run `go test ./internal/tmux/ -run 'TestEscapeAllowed|TestEscapeSafe' -count=1`. Expected: a compile failure (`undefined: escapeAllowed`).

- [ ] **Step 3: Implement.** Replace `effectiveSkipEscape` and its doc comment with:

```go
// SessionAgentPreset resolves the harness preset of the agent running in
// session from the session's own GT_AGENT, GT_ROOT and GT_RIG. The town root
// falls back to townRootHint, then the process GT_ROOT. It returns the
// GT_AGENT value; ok=false means the harness is unknown. (claude-9a8)
func (t *Tmux) SessionAgentPreset(session, townRootHint string) (string, *config.AgentPresetInfo, bool) {
	if session == "" {
		return "", nil, false
	}
	agent, _ := t.GetEnvironment(session, "GT_AGENT")
	if agent == "" {
		return "", nil, false
	}
	townRoot, _ := t.GetEnvironment(session, "GT_ROOT")
	if townRoot == "" {
		townRoot = townRootHint
	}
	if townRoot == "" {
		townRoot = os.Getenv("GT_ROOT")
	}
	rigPath := ""
	if rig, _ := t.GetEnvironment(session, "GT_RIG"); rig != "" && townRoot != "" {
		rigPath = filepath.Join(townRoot, rig)
	}
	preset, ok := config.ResolveAgentPreset(agent, townRoot, rigPath)
	return agent, preset, ok
}

// escapeAllowed reports whether agent identity permits the vim-mode Escape
// (nudge delivery step 5). Copilot and Gemini cancel generation on Escape
// (hq-isz, GH#gt-wasn); Claude Code reads a mid-tool-call Escape as an
// operator interrupt (gt-cyyg). An unidentified harness fails safe: custom
// town agents went unidentified and were interrupted for weeks (claude-9a8).
func escapeAllowed(agentName string, preset *config.AgentPresetInfo, ok bool) bool {
	if agentName == "copilot" {
		return false
	}
	if !ok || preset == nil {
		return false
	}
	return !preset.EscapeCancelsRequest
}

// escapeSafe is the single Escape gate for nudge delivery: identity first
// (no pane capture for Claude sessions), then the busy scrape.
func (t *Tmux) escapeSafe(target, session, townRootHint string) bool {
	agent, preset, ok := t.SessionAgentPreset(session, townRootHint)
	return escapeAllowed(agent, preset, ok) && t.shouldSendEscape(target)
}
```

In `NudgeSessionWithOpts`, replace:

```go
	if !opts.SkipEscape {
		agentType, _ := t.GetEnvironment(session, "GT_AGENT")
		opts.SkipEscape = effectiveSkipEscape(agentType)
	}
	// Snapshot before typing the nudge so the message text itself cannot look
	// like the agent's busy indicator.
	sendEscape := !opts.SkipEscape && t.shouldSendEscape(target)
```

with:

```go
	// Decide before typing the nudge so the message text itself cannot look
	// like the agent's busy indicator.
	sendEscape := !opts.SkipEscape && t.escapeSafe(target, session, opts.TownRoot)
```

In `NudgePane`, replace `sendEscape := t.shouldSendEscape(pane)` with:

```go
	sendEscape := t.escapeSafe(pane, t.sessionForPane(pane), "")
```

and add:

```go
// sessionForPane returns the session owning pane, or "" if tmux cannot say
// (which leaves the agent unidentified, so no Escape is sent).
func (t *Tmux) sessionForPane(pane string) string {
	out, err := t.run("display-message", "-p", "-t", pane, "#{session_name}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}
```

Replace the body of `readyPromptPrefixForSession`:

```go
func readyPromptPrefixForSession(t *Tmux, session string) string {
	_, preset, ok := t.SessionAgentPreset(session, "")
	if !ok || preset.ReadyPromptPrefix == "" {
		return DefaultReadyPromptPrefix
	}
	return preset.ReadyPromptPrefix
}
```

Add the `os` and `path/filepath` imports to `tmux.go` if they are missing.

- [ ] **Step 4: Run the tests.** Run `go test ./internal/tmux/ -count=1`. Expected: PASS. If an existing nudge integration test asserted an Escape into a plain shell session, change it to set `GT_AGENT=codex` rather than weakening the fail-safe.

- [ ] **Step 5: Commit.** Run `git commit -am "tmux: one identity-first Escape gate for both nudge paths (claude-9a8)"`.

### Task 3: Nudge commands use the session resolver (MR1)

**Files:**
- Modify: `internal/cmd/nudge_poller.go` (~84-92)
- Modify: `internal/cmd/nudge.go` (~313-316)

**Interfaces:**
- Consumes: `(t *tmux.Tmux) SessionAgentPreset`.

- [ ] **Step 1: Replace the poller's identity block:**

```go
	nudgeOpts := tmux.NudgeOpts{TownRoot: townRoot}
	agentName := ""
	hasPromptDetection := false
	if name, preset, ok := t.SessionAgentPreset(sessionName, townRoot); name != "" {
		agentName = name
		if ok {
			hasPromptDetection = preset.ReadyPromptPrefix != ""
			nudgeOpts.SkipEscape = preset.EscapeCancelsRequest
		} else {
			nudgeOpts.SkipEscape = true // unidentified harness: fail safe (claude-9a8)
		}
	}
```

- [ ] **Step 2: In `nudge.go` wait-idle, replace:**

```go
	if agentName, err := t.GetEnvironment(sessionName, "GT_AGENT"); err == nil && agentName != "" {
		preset := config.GetAgentPresetByName(agentName)
		if preset != nil && preset.ReadyPromptPrefix == "" {
```

with:

```go
	if agentName, preset, ok := t.SessionAgentPreset(sessionName, townRoot); agentName != "" {
		if !ok || preset.ReadyPromptPrefix == "" {
```

Drop the `config` import if it is no longer used.

- [ ] **Step 3: Build and test.** Run `go build ./... && go test ./internal/cmd/ -run 'Nudge' -count=1`. Expected: PASS.

- [ ] **Step 4: Run the MR1 gate.** Run `make build && make lint && go test ./internal/config/ ./internal/tmux/ -count=1`. Expected: PASS.

- [ ] **Step 5: Commit.** Run `git commit -am "nudge: poller and wait-idle resolve custom agents (claude-9a8)"`.

### Task 4: Remaining callers (MR2)

Branch first: `git switch -c crew/sloan/claude-9a8-callers`.

**Files:**
- Modify: `internal/cmd/sling_helpers.go` (~777, ~1643)
- Modify: `internal/crew/manager.go` (~770-778, ~880-890)
- Modify: `internal/runtime/runtime.go` (~176-180)
- Modify: `internal/cmd/hooks_sync.go` (~179-186)
- Modify: `internal/doctor/hooks_sync_check.go` (~113-119)
- Modify: `internal/rig/manager.go` (~849-857)
- Test: the existing tests of each file, plus a new `TestShouldAcceptPermissionWarning_CustomAgent`

- [ ] **Step 1: Write the failing test** in `internal/cmd/sling_helpers_test.go`:

```go
func TestShouldAcceptPermissionWarning_ResolvedPreset(t *testing.T) {
	t.Parallel()
	claude := config.GetAgentPresetByName("claude")
	if !shouldAcceptPermissionWarning("deepseek-flash", claude, true) {
		t.Error("custom agent resolved to claude must accept the bypass-permissions warning")
	}
	if shouldAcceptPermissionWarning("mystery", nil, false) {
		t.Error("unresolved agent must not accept")
	}
	if !shouldAcceptPermissionWarning("", nil, false) {
		t.Error("session without GT_AGENT is Claude by default")
	}
}
```

Update any existing `shouldAcceptPermissionWarning(name)` test calls to the new three-argument form, passing `config.GetAgentPresetByName(name), <preset != nil>`.

- [ ] **Step 2: Run it and confirm it fails.** Run `go test ./internal/cmd/ -run ShouldAcceptPermissionWarning -count=1`. Expected: a compile failure (too many arguments).

- [ ] **Step 3: Implement each caller.**

`sling_helpers.go`:

```go
	agentName, preset, ok := t.SessionAgentPreset(sessionName, "")
	if shouldAcceptPermissionWarning(agentName, preset, ok) {
```

```go
// shouldAcceptPermissionWarning checks if the agent's harness emits a
// bypass-permissions warning on startup that must be acknowledged via tmux.
// A session without GT_AGENT is Claude by default.
func shouldAcceptPermissionWarning(agentName string, preset *config.AgentPresetInfo, ok bool) bool {
	if agentName == "" {
		preset, ok = config.GetAgentPreset(config.AgentClaude), true
	}
	return ok && preset != nil && preset.EmitsPermissionWarning
}
```

`crew/manager.go`: in both blocks, replace the `rc.Provider` fallback. For the resume block (~770):

```go
		agentName := opts.AgentOverride
		if agentName == "" {
			agentName = string(config.AgentClaude)
			if rc := config.ResolveWorkerAgentConfig(name, townRoot, m.rig.Path); rc != nil {
				if preset, ok := config.HarnessPreset(rc); ok {
					agentName = string(preset.Name)
				}
			}
		} else if preset, ok := config.ResolveAgentPreset(agentName, townRoot, m.rig.Path); ok {
			agentName = string(preset.Name)
		}
		resumeArgs, err := buildResumeArgs(agentName, opts.ResumeSessionID)
```

Use the same block in the startup-dialog branch (~880), followed by the unchanged `preset := config.GetAgentPresetByName(agentName)`. The name is now a harness name, which the guard test allows.

`runtime.go` `SessionIDFromEnv`:

```go
	if agentName := os.Getenv("GT_AGENT"); agentName != "" {
		townRoot := os.Getenv("GT_ROOT")
		rigPath := ""
		if rig := os.Getenv("GT_RIG"); rig != "" && townRoot != "" {
			rigPath = filepath.Join(townRoot, rig)
		}
		if preset, ok := config.ResolveAgentPreset(agentName, townRoot, rigPath); ok && preset.SessionIDEnv != "" {
```

`hooks_sync.go` and `doctor/hooks_sync_check.go`:

```go
			preset, ok := config.ResolveAgentPreset(agentName, townRoot, rigPath) // ctx.TownRoot in doctor
			if !ok || preset.HooksDir == "" || preset.HooksSettingsFile == "" {
				continue
			}
			hooksProvider := preset.HooksProvider
			if hooksProvider == "" {
				hooksProvider = string(preset.Name)
			}
```

`rig/manager.go`:

```go
	defaultAgentName := townSettings.DefaultAgent
	if defaultAgentName == "" {
		defaultAgentName = string(config.AgentClaude)
	}
	defaultPreset, ok := config.ResolveAgentPreset(defaultAgentName, m.townRoot, "")
	if ok {
		defaultAgentName = string(defaultPreset.Name) // harness name for ProvisionFor below
	}
	if ok && defaultPreset.HooksProvider != "" {
```

Leave the existing `commands.ProvisionFor(polecatsPath, defaultAgentName)` line as it is; it now receives the harness name.

- [ ] **Step 4: Build and test.** Run `go build ./... && go test ./internal/cmd/ ./internal/crew/ ./internal/runtime/ ./internal/doctor/ ./internal/rig/ -count=1`. Expected: PASS.

- [ ] **Step 5: Commit.** Run `git commit -am "move agent-name preset lookups onto ResolveAgentPreset (claude-9a8)"`.

### Task 5: Guard test (MR2)

**Files:**
- Create: `internal/config/preset_lookup_guard_test.go`

- [ ] **Step 1: Write the guard test:**

```go
package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// allowedPresetLookups names the only call sites outside internal/config that
// may call GetAgentPresetByName: each passes a built-in/registry or
// already-resolved harness name, never a custom agent name. Key: file (relative
// to internal/) + "#" + enclosing function. Anything else must use
// ResolveAgentPreset (claude-9a8).
var allowedPresetLookups = map[string]bool{
	"cmd/seance.go#resolveSeanceCommand":  true,
	"cmd/config.go#runConfigAgentList":    true,
	"cmd/config.go#runConfigAgentGet":     true,
	"cmd/config.go#runConfigAgentSet":     true,
	"runtime/runtime.go#EnsureSettingsForRole": true,
	"crew/manager.go#buildResumeArgs":     true,
	"crew/manager.go#Start":               true,
}

func TestNoAgentNamePresetLookups(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..")
	fset := token.NewFileSet()
	var bad []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if filepath.Base(path) == "config" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "GetAgentPresetByName" {
					return true
				}
				key := filepath.ToSlash(rel) + "#" + fn.Name.Name
				if !allowedPresetLookups[key] {
					bad = append(bad, key+" ("+fset.Position(call.Pos()).String()+")")
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bad {
		t.Errorf("GetAgentPresetByName called with a possibly custom agent name at %s; use ResolveAgentPreset (claude-9a8), or add to allowedPresetLookups if the name is always a harness name", b)
	}
}
```

- [ ] **Step 2: Run it.** Run `go test ./internal/config/ -run TestNoAgentNamePresetLookups -count=1`. Expected: failures list only the legitimate sites. Fix the allow-list keys to the real enclosing function names it prints (`config.go`'s real function names, `runtime.go`'s real function, the `crew/manager.go` function holding the startup-dialog branch). Any listed site that is not a harness-name lookup is a missed caller: fix it the Task 4 way.

- [ ] **Step 3: Run it again.** Expected: PASS.

- [ ] **Step 4: Run the MR2 gate.** Run `make build && make lint && go test ./internal/config/ ./internal/cmd/ ./internal/crew/ ./internal/runtime/ ./internal/doctor/ ./internal/rig/ ./internal/tmux/ -count=1`. Expected: PASS.

- [ ] **Step 5: Commit.** Run `git add internal/config && git commit -m "config: guard test against agent-name preset lookups (claude-9a8)"`.

### Task 6: Role-template interrupt policy (MR3)

Branch first: `git switch -c crew/sloan/claude-9a8-templates origin/main`.

**Files:**
- Create: `internal/templates/roles/partial-interrupts.md.tmpl`
- Modify: `internal/templates/roles/{witness,deacon,refinery,polecat}.md.tmpl`
- Test: `internal/templates/templates_test.go`

- [ ] **Step 1: Write the failing test** in `templates_test.go`:

```go
func TestRoleTemplatesCarryInterruptPolicy(t *testing.T) {
	tmpl, err := New()
	if err != nil {
		t.Fatal(err)
	}
	const marker = "An interrupt is a delivery artifact"
	for role, want := range map[string]bool{
		"witness": true, "deacon": true, "refinery": true, "polecat": true,
		"mayor": false, "crew": false,
	} {
		out, err := tmpl.RenderRole(role, RoleData{RigName: "gastown", Polecat: "p", TownRoot: "/t"})
		if err != nil {
			t.Fatalf("RenderRole(%s): %v", role, err)
		}
		if got := strings.Contains(out, marker); got != want {
			t.Errorf("role %s carries interrupt policy = %v, want %v", role, got, want)
		}
	}
}
```

Before writing it, check the `RoleData` field names with `grep -n "type RoleData" -A20 internal/templates/templates.go`, and copy the field set an existing `RenderRole` test uses.

- [ ] **Step 2: Run it and confirm it fails.** Run `go test ./internal/templates/ -run TestRoleTemplatesCarryInterruptPolicy -count=1`. Expected: FAIL, because the witness does not carry the policy yet.

- [ ] **Step 3: Implement.** Create `partial-interrupts.md.tmpl`:

```
{{ define "interrupt-policy" -}}
## An interrupt is a delivery artifact, not an operator stop

No human types into this pane. A tool call rejected with "The user doesn't
want to proceed with this tool use" or "[Request interrupted by user for tool
use]", with no instruction attached, is a nudge-delivery artifact, not an
order to stop. Re-run the interrupted command if you still need it and
continue your loop.

Real orders arrive as text, from a named sender, and say what to stop. A
"hold" covers only what it names ("no new slings" stops slings), never your
patrol loop. Before replying "holding", cite the order: sender, time, and the
words that name your loop. If you cannot cite one, there is no hold: resume.
{{- end }}
```

In each of the four role templates, insert `{{ template "interrupt-policy" }}` followed by a blank line directly before the `## Gotchas` heading if the template has one. Otherwise put it before the last `##` section. Check first with `grep -n "^## " internal/templates/roles/<role>.md.tmpl`.

- [ ] **Step 4: Run the tests.** Run `go test ./internal/templates/ -count=1`. Expected: PASS. If a golden or size test fails, update its expectation deliberately and say so in the commit.

- [ ] **Step 5: Commit.** Run `git add internal/templates && git commit -m "templates: patrol roles treat bare interrupts as delivery artifacts (claude-9a8)"`.

### Task 7: Submit through the refinery and watch until merged

- [ ] **Step 1: File the gastown beads.** From `~/gt/gastown/crew/sloan`, create one parent bead and three children (MR1, MR2, MR3). MR2 depends on MR1. Link each child to claude-9a8 in its description.
- [ ] **Step 2: Push and submit MR1.** Run `git push origin crew/sloan/claude-9a8`, then `gt mq submit` for the MR1 bead (check `gt mq submit --help` for the flags first).
- [ ] **Step 3: Submit MR3** the same way; it is independent of MR1. **Submit MR2** only after MR1 merges: rebase MR2 onto the new `origin/main`, then push and submit.
- [ ] **Step 4: Watch.** Poll `gt mq list gastown` and the MR wisps. On FIX_NEEDED or an om `request_changes` verdict, read the findings, fix them on the branch, push, and resubmit. Record every gate event in the claude-9a8 notes.
- [ ] **Step 5: Verify each merge on the artifact.** Merged means the MR's commits are in `origin/main` (`git fetch && git cherry origin/main <branch>` shows no `+` lines), not that its bead is closed.
- [ ] **Step 6: Report, then deploy on Sloan's OK.** `make install` restarts the daemon (memory: make-install-restarts-the-daemon), so ask Sloan first. Agent restarts, including a still-holding witness, go through the mayor. The mayor's sling hold is Sloan's to lift.
