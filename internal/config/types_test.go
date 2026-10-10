package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// --- ParseDurationOrDefault ---

func TestParseDurationOrDefault(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		fallback time.Duration
		want     time.Duration
	}{
		{"valid seconds", "15s", 0, 15 * time.Second},
		{"valid minutes", "5m", 0, 5 * time.Minute},
		{"valid hours", "2h", 0, 2 * time.Hour},
		{"valid milliseconds", "500ms", 0, 500 * time.Millisecond},
		{"valid composite", "1m30s", 0, 90 * time.Second},
		{"empty string returns fallback", "", 42 * time.Second, 42 * time.Second},
		{"invalid string returns fallback", "not-a-duration", 7 * time.Second, 7 * time.Second},
		{"negative duration parses", "-5s", 10 * time.Second, -5 * time.Second},
		{"zero duration parses", "0s", 10 * time.Second, 0},
		{"bare number returns fallback", "15", 3 * time.Second, 3 * time.Second},
		{"whitespace returns fallback", "  ", 1 * time.Second, 1 * time.Second},
		{"zero fallback with empty", "", 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ParseDurationOrDefault(tt.input, tt.fallback)
			if got != tt.want {
				t.Errorf("ParseDurationOrDefault(%q, %v) = %v, want %v",
					tt.input, tt.fallback, got, tt.want)
			}
		})
	}
}

// --- Gemini provider defaults ---

func TestTownSettings_WithoutNewFields_LoadsDefaults(t *testing.T) {
	t.Parallel()
	// Simulate a pre-existing settings/config.json that has NO new config fields.
	// This verifies backward compatibility: existing deployments continue to work.
	settingsJSON := `{
		"type": "town-settings",
		"version": 1,
		"default_agent": "claude"
	}`

	tmpDir := t.TempDir()
	settingsDir := filepath.Join(tmpDir, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	settingsPath := filepath.Join(settingsDir, "config.json")
	if err := os.WriteFile(settingsPath, []byte(settingsJSON), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ts, err := LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		t.Fatalf("LoadOrCreateTownSettings: %v", err)
	}

	// Existing fields should still load correctly
	if ts.DefaultAgent != "claude" {
		t.Errorf("DefaultAgent = %q, want %q", ts.DefaultAgent, "claude")
	}
}

// TestTownSettings_RemovedFeedCuratorKeyStillLoads: towns written before the
// feed curator was deleted (gt-3vdcx) carry a feed_curator section; strict
// decoding still accepts it.
func TestTownSettings_RemovedFeedCuratorKeyStillLoads(t *testing.T) {
	t.Parallel()
	settingsJSON := `{
		"type": "town-settings",
		"version": 1,
		"default_agent": "claude",
		"feed_curator": {
			"done_dedupe_window": "25s"
		}
	}`

	tmpDir := t.TempDir()
	settingsPath := filepath.Join(tmpDir, "config.json")
	if err := os.WriteFile(settingsPath, []byte(settingsJSON), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ts, err := LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		t.Fatalf("LoadOrCreateTownSettings: %v", err)
	}
	if ts.DefaultAgent != "claude" {
		t.Errorf("DefaultAgent = %q, want %q", ts.DefaultAgent, "claude")
	}
}

func TestTownSettings_MissingFile_ReturnsDefaults(t *testing.T) {
	t.Parallel()
	// LoadOrCreateTownSettings on a missing file should return defaults.
	tmpDir := t.TempDir()
	settingsPath := filepath.Join(tmpDir, "does-not-exist.json")

	ts, err := LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		t.Fatalf("LoadOrCreateTownSettings: %v", err)
	}

	if ts.Type != "town-settings" {
		t.Errorf("Type = %q, want %q", ts.Type, "town-settings")
	}
	if ts.Version != CurrentTownSettingsVersion {
		t.Errorf("Version = %d, want %d", ts.Version, CurrentTownSettingsVersion)
	}
}

func TestTownSettings_DisabledPatrols_RoundTrip(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	settingsPath := filepath.Join(tmpDir, "config.json")

	original := NewTownSettings()
	original.DisabledPatrols = []string{"doctor_dog", "compactor_dog", "witness"}

	if err := SaveTownSettings(settingsPath, original); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}

	loaded, err := LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		t.Fatalf("LoadOrCreateTownSettings: %v", err)
	}

	if len(loaded.DisabledPatrols) != 3 {
		t.Fatalf("expected 3 disabled patrols, got %d", len(loaded.DisabledPatrols))
	}

	expected := map[string]bool{"doctor_dog": true, "compactor_dog": true, "witness": true}
	for _, p := range loaded.DisabledPatrols {
		if !expected[p] {
			t.Errorf("unexpected disabled patrol: %q", p)
		}
	}
}

func TestTownSettings_DisabledPatrols_OmitemptyWhenNil(t *testing.T) {
	t.Parallel()
	ts := NewTownSettings()
	data, err := json.MarshalIndent(ts, "", "  ")
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "disabled_patrols") {
		t.Error("JSON should not contain disabled_patrols when nil")
	}
}

// --- PolecatPool knobs ---

// TestTownSettings_RetiredLocalPoolKeysStillLoad: the live town config still
// carries the retired local-model seat (local_agent, max_local, idle_fill,
// D4) beside the seat pool's retired key spellings (overflow_agent,
// max_overflow). All of it must decode under strict decoding; the retired seat
// keys must load the pool's agent and max_seats, and the local-seat keys must
// be written back verbatim.
func TestTownSettings_RetiredLocalPoolKeysStillLoad(t *testing.T) {
	t.Parallel()
	settingsJSON := `{
		"type": "town-settings",
		"version": 1,
		"default_agent": "claude",
		"polecat_pool": {
			"local_agent": "local-coder-polecat",
			"max_local": 0,
			"idle_fill": true,
			"min_spawn_gap": "4m",
			"overflow_agent": "deepseek-flash",
			"max_overflow": 3
		}
	}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(settingsJSON), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ts, err := LoadOrCreateTownSettings(path)
	if err != nil {
		t.Fatalf("LoadOrCreateTownSettings: %v", err)
	}
	pool := ts.PolecatPool
	if pool == nil || pool.Agent != "deepseek-flash" || pool.MaxSeats != 3 || pool.MinSpawnGapD() != 4*time.Minute {
		t.Fatalf("live pool keys not loaded: %+v", pool)
	}
	if !pool.SeatsCapped() {
		t.Error("pool with max_seats 3 must be capped")
	}

	if err := SaveTownSettings(path, ts); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"local_agent": "local-coder-polecat"`, `"max_local": 0`, `"idle_fill": true`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("saved settings dropped retired key %s:\n%s", want, raw)
		}
	}
	// The rewrite is the writer's merge against the file, and the writer keeps
	// text it did not author: the retired keys stay exactly as the operator
	// wrote them, the same way the local seat's keys above do. gt reads them as
	// the pool's agent and max_seats from then on. Moving a live file onto the
	// current names is a migration, not a side effect of a save.
	ts.PolecatPool.ShapeGate = "refuse"
	if err := SaveTownSettings(path, ts); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"shape_gate": "refuse"`) {
		t.Errorf("the edit did not reach the file:\n%s", raw)
	}
	again, err := LoadOrCreateTownSettings(path)
	if err != nil {
		t.Fatalf("reload after the save: %v", err)
	}
	if p := again.PolecatPool; p == nil || p.Agent != "deepseek-flash" || p.MaxSeats != 3 {
		t.Errorf("the retired keys stopped loading after a save: %+v", p)
	}
}

// TestPolecatPool_WritesOnlyTheCurrentKeys: gt never authors the retired seat
// spellings. A settings file written from a pool carries agent and max_seats,
// which is what makes the rename a rename rather than a second name for the
// same seat.
func TestPolecatPool_WritesOnlyTheCurrentKeys(t *testing.T) {
	t.Parallel()
	ts := NewTownSettings()
	ts.PolecatPool = &PolecatPool{Agent: "deepseek-flash", MaxSeats: 2}
	data, err := json.Marshal(ts)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{`"agent":"deepseek-flash"`, `"max_seats":2`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("marshalled settings missing %s:\n%s", want, data)
		}
	}
	for _, gone := range []string{"overflow_agent", "max_overflow"} {
		if strings.Contains(string(data), gone) {
			t.Errorf("marshalled settings carry the retired key %s:\n%s", gone, data)
		}
	}
}

// TestPolecatPool_SeatKeyRename: the seat pool's keys are polecat_pool.agent
// and polecat_pool.max_seats. A file carrying the retired spellings
// (overflow_agent, max_overflow) loads the same values, and the current key wins
// when a file carries both.
func TestPolecatPool_SeatKeyRename(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		poolJSON string
		want     PolecatPool
	}{
		{
			name:     "the current keys",
			poolJSON: `{"agent":"deepseek-flash","max_seats":2}`,
			want:     PolecatPool{Agent: "deepseek-flash", MaxSeats: 2},
		},
		{
			name:     "the retired keys still load",
			poolJSON: `{"overflow_agent":"deepseek-flash","max_overflow":2}`,
			want:     PolecatPool{Agent: "deepseek-flash", MaxSeats: 2},
		},
		{
			name:     "the current key wins when both are present",
			poolJSON: `{"agent":"claude-sonnet","max_seats":5,"overflow_agent":"deepseek-flash","max_overflow":2}`,
			want:     PolecatPool{Agent: "claude-sonnet", MaxSeats: 5},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ts TownSettings
			if err := DecodeJSONFile("settings/config.json",
				[]byte(`{"type":"town-settings","version":1,"polecat_pool":`+tc.poolJSON+`}`), &ts); err != nil {
				t.Fatalf("DecodeJSONFile: %v", err)
			}
			pool := ts.PolecatPool
			if pool == nil {
				t.Fatal("polecat_pool did not load")
			}
			if pool.Agent != tc.want.Agent || pool.MaxSeats != tc.want.MaxSeats {
				t.Errorf("pool = {agent: %q, max_seats: %d}, want {agent: %q, max_seats: %d}",
					pool.Agent, pool.MaxSeats, tc.want.Agent, tc.want.MaxSeats)
			}
			if pool.DeprecatedOverflowAgent != nil || pool.DeprecatedMaxOverflow != nil {
				t.Errorf("retired fields survived the load: %+v", pool)
			}
		})
	}
}

// TestPolecatPool_SeatKeyRenameReportsTheRetiredKey: the fold reports the
// retired key it read and the one that replaced it, which is what the load
// warns about — the retired keys and nothing else, so a file naming both keys
// stays quiet about a spelling it did not need.
func TestPolecatPool_SeatKeyRenameReportsTheRetiredKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		poolJSON string
		want     []retiredSeatKey
	}{
		{
			name:     "the retired keys report both",
			poolJSON: `{"overflow_agent":"deepseek-flash","max_overflow":2}`,
			want:     []retiredSeatKey{{old: "overflow_agent", current: "agent"}, {old: "max_overflow", current: "max_seats"}},
		},
		{
			name:     "the current keys report nothing",
			poolJSON: `{"agent":"deepseek-flash","max_seats":2}`,
		},
		{
			name:     "a shadowed retired key reports nothing",
			poolJSON: `{"agent":"claude-sonnet","max_seats":5,"overflow_agent":"deepseek-flash","max_overflow":2}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var pool PolecatPool
			if err := json.Unmarshal([]byte(tc.poolJSON), &pool); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got := pool.applyDeprecatedKeys(); !slices.Equal(got, tc.want) {
				t.Errorf("applyDeprecatedKeys() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestRetiredKeyWarnings_OnePerProcess: a daemon reads the settings file on
// every pass, so a retired key must not repeat its line. The first use warns,
// every use after it stays quiet, and each retired key has its own line.
func TestRetiredKeyWarnings_OnePerProcess(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	w := &retiredKeyWarnings{out: &out}
	w.warn("overflow_agent", "agent")
	w.warn("overflow_agent", "agent")
	if got := strings.Count(out.String(), "\n"); got != 1 {
		t.Errorf("%d lines for one retired key read twice, want 1:\n%s", got, out.String())
	}
	w.warn("max_overflow", "max_seats")
	if got := strings.Count(out.String(), "\n"); got != 2 {
		t.Errorf("%d lines after a second retired key, want 2:\n%s", got, out.String())
	}
}

// TestNonClaudeProviderGetsClaudeRuntime: the Claude CLI is the only runtime
// (D4). A live town agent carries provider "deepseek" (a backend, not a
// harness) and a backend wrapper's command is not literally "claude"; both
// once took the no-hooks path and started without --settings or guards
// (gt-be0z). Both must now resolve to Claude's defaults and settings.
func TestNonClaudeProviderGetsClaudeRuntime(t *testing.T) {
	t.Parallel()
	rc := normalizeRuntimeConfig(&RuntimeConfig{Provider: "deepseek", Command: "claude"})
	if rc.Provider != string(AgentClaude) {
		t.Errorf("Provider = %q, want claude", rc.Provider)
	}
	if rc.Session.SessionIDEnv != "CLAUDE_SESSION_ID" || rc.Tmux.ReadyPromptPrefix != "❯ " || rc.Instructions.File != "CLAUDE.md" {
		t.Errorf("provider deepseek did not take Claude defaults: session=%+v tmux=%+v instructions=%+v", rc.Session, rc.Tmux, rc.Instructions)
	}

	rigPath := filepath.Join("town", "rig")
	wrapped := withRoleSettingsFlag(&RuntimeConfig{Command: "claude-deepseek-flash"}, "polecat", rigPath)
	want := []string{"--settings", filepath.Join(rigPath, "polecats", ".claude", "settings.json")}
	if len(wrapped.Args) != 2 || wrapped.Args[0] != want[0] || wrapped.Args[1] != want[1] {
		t.Errorf("backend wrapper Args = %v, want %v", wrapped.Args, want)
	}
}

// TestForgejoConfigAccessors pins the nil-safe readers on the
// merge_queue.forgejo block (gt-fn9e6.3): a rig that never configured
// Forgejo must get the default workflow name and no bots rather than a panic,
// because ResolveForgejoConfig returns a nil block for exactly that rig.
func TestForgejoConfigAccessors(t *testing.T) {
	t.Parallel()

	var absent *ForgejoConfig
	if got := absent.GateWorkflowName(); got != DefaultGateWorkflow {
		t.Errorf("nil GateWorkflowName() = %q, want %q", got, DefaultGateWorkflow)
	}
	if got := absent.BotLogin(ForgejoRoleLanding); got != "" {
		t.Errorf("nil BotLogin() = %q, want empty", got)
	}

	unset := &ForgejoConfig{}
	if got := unset.GateWorkflowName(); got != DefaultGateWorkflow {
		t.Errorf("unset GateWorkflowName() = %q, want %q", got, DefaultGateWorkflow)
	}

	full := &ForgejoConfig{
		GateWorkflow: "heavy",
		Bots:         map[string]string{ForgejoRoleRegistry: "gt-registry"},
	}
	if got := full.GateWorkflowName(); got != "heavy" {
		t.Errorf("GateWorkflowName() = %q, want heavy", got)
	}
	if got := full.BotLogin(ForgejoRoleRegistry); got != "gt-registry" {
		t.Errorf("BotLogin(registry) = %q, want gt-registry", got)
	}
	if got := full.BotLogin(ForgejoRolePolecat); got != "" {
		t.Errorf("BotLogin(polecat) = %q, want empty (unset role)", got)
	}
}

// TestForgejoConfigDecodesItsKeys pins the JSON keys the three config files
// spell, so a rename cannot silently stop a committed .gastown/settings.json
// from resolving.
func TestForgejoConfigDecodesItsKeys(t *testing.T) {
	t.Parallel()
	const body = `{
		"remote_url": "https://forgejo.example/gastown/gastown",
		"gate_workflow": "gate",
		"bots": {"polecat": "gt-polecat", "landing": "gt-landing", "registry": "gt-registry"},
		"mirror_target": "git@github.com:sloanahrens/gastown.git",
		"promote_target": "git@github.com:sloanahrens/gastown.git",
		"promote_key_file": "/home/gt/.config/gt/promote-gastown.key"
	}`
	var mq MergeQueueConfig
	if err := json.Unmarshal([]byte(`{"forgejo":`+body+`}`), &mq); err != nil {
		t.Fatalf("decode merge_queue.forgejo: %v", err)
	}
	if mq.Forgejo == nil {
		t.Fatal("Forgejo = nil, want the decoded block")
	}
	if mq.Forgejo.RemoteURL != "https://forgejo.example/gastown/gastown" {
		t.Errorf("RemoteURL = %q", mq.Forgejo.RemoteURL)
	}
	if mq.Forgejo.GateWorkflowName() != "gate" {
		t.Errorf("GateWorkflow = %q, want gate", mq.Forgejo.GateWorkflow)
	}
	if mq.Forgejo.BotLogin(ForgejoRoleLanding) != "gt-landing" {
		t.Errorf("landing bot = %q, want gt-landing", mq.Forgejo.BotLogin(ForgejoRoleLanding))
	}
	if mq.Forgejo.MirrorTarget != "git@github.com:sloanahrens/gastown.git" {
		t.Errorf("MirrorTarget = %q", mq.Forgejo.MirrorTarget)
	}
	if mq.Forgejo.PromoteTarget != "git@github.com:sloanahrens/gastown.git" {
		t.Errorf("PromoteTarget = %q", mq.Forgejo.PromoteTarget)
	}
	if mq.Forgejo.PromoteKeyFile != "/home/gt/.config/gt/promote-gastown.key" {
		t.Errorf("PromoteKeyFile = %q", mq.Forgejo.PromoteKeyFile)
	}
}

// TestForgejoConfigIgnoresAnUndeclaredKey: the operator-owned forgejo block
// decodes with a key this binary does not declare, so a rig config written for
// another gt version still loads — and the keys beside it keep their values.
// Every other block stays strict, which the last case pins.
func TestForgejoConfigIgnoresAnUndeclaredKey(t *testing.T) {
	t.Parallel()
	body := `{"type":"rig","version":1,"name":"gastown","merge_queue":{"forgejo":` +
		`{"remote_url":"https://forgejo.example/gastown/gastown.git","retired_key":true}}}`
	var cfg RigConfig
	if err := DecodeJSONFile("config.json", []byte(body), &cfg); err != nil {
		t.Fatalf("decode: %v; an undeclared key in the forgejo block must not fail the load", err)
	}
	if cfg.MergeQueue == nil || cfg.MergeQueue.Forgejo == nil {
		t.Fatal("the forgejo block did not decode")
	}
	if got := cfg.MergeQueue.Forgejo.RemoteURL; got != "https://forgejo.example/gastown/gastown.git" {
		t.Errorf("RemoteURL = %q, want the declared key beside the undeclared one", got)
	}

	strict := `{"type":"rig","version":1,"name":"gastown","merge_queue":{"retired_key":true}}`
	if err := DecodeJSONFile("config.json", []byte(strict), &cfg); err == nil {
		t.Error("an undeclared key outside the forgejo block decoded; want a parse error")
	}
}
