package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
)

// Every token in these tests is fake.
const (
	fakeTokenA = "sk-fake0000000000000000000000000000"
	fakeTokenB = "sk-fake1111111111111111111111111111"
)

func TestLooksLikeSecret(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		key, value string
		want       bool
	}{
		{"ANTHROPIC_AUTH_TOKEN", fakeTokenA, true},
		{"WHATEVER", fakeTokenA, true}, // known prefix, any key
		{"GITHUB_TOKEN", "ghp_fakefakefakefakefakefake", true},
		{"MY_API_KEY", "abcdefabcdefabcdefabcdef", true}, // credential key, long value
		{"ANTHROPIC_AUTH_TOKEN", "ollama", false},        // local placeholder
		{"ANTHROPIC_AUTH_TOKEN", "${DEEPSEEK_API_KEY}", false},
		{"ANTHROPIC_CUSTOM_HEADERS", "Bearer ${X_TOKEN}", false},
		{"ANTHROPIC_API_KEY", "", false},
		{"ANTHROPIC_BASE_URL", "https://api.example.invalid/anthropic", false},
		{"CLAUDE_CODE_MAX_OUTPUT_TOKENS", "32000", false},
		{"CLAUDE_CODE_TOTAL_TOKENS_REMINDER", "1234567890123456789", false}, // a number
		{"GOOGLE_APPLICATION_CREDENTIALS_SECRET", "/home/x/creds/service.json", false},
		{"ANTHROPIC_MODEL", "deepseek-v4-flash-20260901", false},
	} {
		if got := LooksLikeSecret(tt.key, tt.value); got != tt.want {
			t.Errorf("LooksLikeSecret(%s, %q) = %v, want %v", tt.key, tt.value, got, tt.want)
		}
	}
}

func secretsTownSettings() *TownSettings {
	s := NewTownSettings()
	s.Agents["ds-flash"] = &RuntimeConfig{Command: "claude", Env: map[string]string{
		"ANTHROPIC_AUTH_TOKEN": fakeTokenA, "ANTHROPIC_MODEL": "deepseek-v4-flash",
	}}
	s.Agents["ds-pro"] = &RuntimeConfig{Command: "claude", Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": fakeTokenA}}
	s.Agents["other"] = &RuntimeConfig{Command: "claude", Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": fakeTokenB}}
	s.Agents["local"] = &RuntimeConfig{Command: "claude", Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "ollama"}}
	return s
}

func TestFindLiteralSecretsNamesPathsNotValues(t *testing.T) {
	t.Parallel()
	got := strings.Join(LiteralSecretPaths(FindLiteralSecrets(secretsTownSettings())), ",")
	want := "agents.ds-flash.env.ANTHROPIC_AUTH_TOKEN,agents.ds-pro.env.ANTHROPIC_AUTH_TOKEN,agents.other.env.ANTHROPIC_AUTH_TOKEN"
	if got != want {
		t.Errorf("paths = %s, want %s", got, want)
	}
	if FindLiteralSecrets(nil) != nil {
		t.Error("nil settings must have no literals")
	}
}

func TestPlanSecretMovesSharesEqualValuesAndReusesEntries(t *testing.T) {
	t.Parallel()
	moves := PlanSecretMoves(secretsTownSettings(), map[string]string{
		"OTHER_ANTHROPIC_AUTH_TOKEN": "not-the-same", // name taken by a different value
		"KEPT_B":                     fakeTokenB,     // value already stored
	})
	var got []string
	for _, m := range moves {
		got = append(got, m.Path()+"="+m.Var+map[bool]string{true: "+", false: ""}[m.Added])
	}
	want := []string{
		"agents.ds-flash.env.ANTHROPIC_AUTH_TOKEN=DS_FLASH_ANTHROPIC_AUTH_TOKEN+",
		"agents.ds-pro.env.ANTHROPIC_AUTH_TOKEN=DS_FLASH_ANTHROPIC_AUTH_TOKEN",
		"agents.other.env.ANTHROPIC_AUTH_TOKEN=KEPT_B",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("moves:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if n := secretVarName(LiteralSecret{Agent: "other", Key: "ANTHROPIC_AUTH_TOKEN"}, map[string]bool{"OTHER_ANTHROPIC_AUTH_TOKEN": true}); n != "OTHER_ANTHROPIC_AUTH_TOKEN_2" {
		t.Errorf("taken name not suffixed: %s", n)
	}
}

func TestMigrateSecretsMovesTokensToDaemonEnv(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := SaveTownSettings(TownSettingsPath(town), secretsTownSettings()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DaemonEnvPath(town), []byte("SDKROOT=/fake/sdk"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := PlanSecretMigration(town)
	if err != nil || len(plan) != 3 {
		t.Fatalf("plan = %v, %v; want 3 moves", plan, err)
	}
	moves, err := MigrateSecrets(town)
	if err != nil || len(moves) != 3 {
		t.Fatalf("MigrateSecrets = %v, %v", moves, err)
	}

	env, err := LoadDaemonEnv(town)
	if err != nil {
		t.Fatal(err)
	}
	if env["SDKROOT"] != "/fake/sdk" || env["DS_FLASH_ANTHROPIC_AUTH_TOKEN"] != fakeTokenA || env["OTHER_ANTHROPIC_AUTH_TOKEN"] != fakeTokenB {
		t.Errorf("daemon.env lost or misplaced entries: keys %v", sortedNames(env))
	}
	if fi, err := os.Stat(DaemonEnvPath(town)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("daemon.env mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}

	data, err := os.ReadFile(TownSettingsPath(town))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sk-fake") {
		t.Error("settings/config.json still holds a token")
	}
	s, err := LoadOrCreateTownSettings(TownSettingsPath(town))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Agents["ds-pro"].Env["ANTHROPIC_AUTH_TOKEN"]; got != "${DS_FLASH_ANTHROPIC_AUTH_TOKEN}" {
		t.Errorf("ds-pro token = %q, want the reference", got)
	}
	if s.Agents["local"].Env["ANTHROPIC_AUTH_TOKEN"] != "ollama" || s.Agents["ds-flash"].Env["ANTHROPIC_MODEL"] != "deepseek-v4-flash" {
		t.Error("migrate changed a value that is not a token")
	}

	again, err := MigrateSecrets(town)
	if err != nil || len(again) != 0 {
		t.Errorf("second migrate = %v, %v; want nothing to do", again, err)
	}
}

// A reference to a daemon.env entry reaches the agent through a read the
// command does when it runs: the token is in no argv and no pane start
// command (G3-18), and the exec env assignments cannot override it.
func TestBuildStartupCommand_ReadsDaemonEnvSecretAtRunTime(t *testing.T) {
	t.Parallel()
	fh := agentHost(map[string]string{"ANTHROPIC_AUTH_TOKEN": fakeTokenB})
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "testrig")
	s := NewTownSettings()
	s.RoleAgents = map[string]string{constants.RoleMayor: "proxied"}
	s.Agents["proxied"] = &RuntimeConfig{Command: "claude", Env: map[string]string{
		"ANTHROPIC_AUTH_TOKEN":     "${DS_TOKEN}",
		"ANTHROPIC_CUSTOM_HEADERS": "x-key: ${DS_TOKEN}",
	}}
	if err := SaveTownSettings(TownSettingsPath(townRoot), s); err != nil {
		t.Fatal(err)
	}
	if err := SaveRigSettings(RigSettingsPath(rigPath), NewRigSettings()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DaemonEnvPath(townRoot), []byte("DS_TOKEN="+fakeTokenA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd, err := buildStartupCommandFromConfig(fh, AgentEnvConfig{
		Role: constants.RoleMayor, TownRoot: townRoot, Getenv: fh.getenv,
	}, rigPath, "", "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if strings.Contains(cmd, "sk-fake") {
		t.Fatalf("startup command carries a token: %s", cmd)
	}
	read := daemonEnvRead(DaemonEnvPath(townRoot), "DS_TOKEN")
	for _, want := range []string{
		`ANTHROPIC_AUTH_TOKEN="` + read + `"; export ANTHROPIC_AUTH_TOKEN; `,
		`ANTHROPIC_CUSTOM_HEADERS="x-key: ` + read + `"; export ANTHROPIC_CUSTOM_HEADERS; `,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command lacks %q:\n%s", want, cmd)
		}
	}
	execPart := cmd[strings.Index(cmd, "exec env "):]
	if strings.Contains(execPart, "ANTHROPIC_AUTH_TOKEN=") {
		t.Errorf("exec env assigns ANTHROPIC_AUTH_TOKEN after the run-time read: %s", execPart)
	}
}

// The Dolt password follows the same rule as an agent env value: a literal is
// reported by FindLiteralSecrets and moved by MigrateSecrets, a reference is
// not (gt-y3pgh.2.4).
func TestLiteralSecretsCoverDoltPassword(t *testing.T) {
	t.Parallel()
	ref := "${GT_DOLT_PASSWORD}"
	// A value no agent holds, so the plan mints a fresh DOLT_PASSWORD entry
	// rather than reusing the agent's.
	literal := "sk-fake2222222222222222222222222222"

	s := secretsTownSettings()
	s.Operational = &OperationalConfig{Dolt: &DoltThresholds{Password: &ref}}
	if got := LiteralSecretPaths(FindLiteralSecrets(s)); strings.Contains(strings.Join(got, ","), doltPasswordPath) {
		t.Errorf("a reference was reported as a literal: %v", got)
	}

	s.Operational.Dolt.Password = &literal
	got := LiteralSecretPaths(FindLiteralSecrets(s))
	if !slices.Contains(got, doltPasswordPath) {
		t.Fatalf("paths = %v, want %s among them", got, doltPasswordPath)
	}

	moves := PlanSecretMoves(s, nil)
	i := slices.IndexFunc(moves, func(m SecretMove) bool { return m.KeyPath == doltPasswordPath })
	if i < 0 {
		t.Fatalf("no move for the Dolt password: %v", moves)
	}
	if moves[i].Var != "DOLT_PASSWORD" || !moves[i].Added {
		t.Errorf("move = %+v, want a new DOLT_PASSWORD entry", moves[i])
	}
}

func TestMigrateSecretsMovesDoltPassword(t *testing.T) {
	t.Parallel()
	literal := fakeTokenA
	town := t.TempDir()
	s := NewTownSettings()
	s.Operational = &OperationalConfig{Dolt: &DoltThresholds{Password: &literal}}
	if err := SaveTownSettings(TownSettingsPath(town), s); err != nil {
		t.Fatal(err)
	}

	moves, err := MigrateSecrets(town)
	if err != nil || len(moves) != 1 {
		t.Fatalf("MigrateSecrets = %v, %v; want one move", moves, err)
	}
	env, err := LoadDaemonEnv(town)
	if err != nil || env["DOLT_PASSWORD"] != fakeTokenA {
		t.Errorf("daemon.env = %v, %v; want DOLT_PASSWORD in it", env, err)
	}
	back, err := LoadOrCreateTownSettings(TownSettingsPath(town))
	if err != nil {
		t.Fatal(err)
	}
	if got := *back.Operational.Dolt.Password; got != "${DOLT_PASSWORD}" {
		t.Errorf("password = %q, want the reference", got)
	}
	data, err := os.ReadFile(TownSettingsPath(town))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sk-fake") {
		t.Error("settings/config.json still holds the token")
	}
}

func TestSpawnEnvShellWordEscapesLiteralText(t *testing.T) {
	t.Parallel()
	got := daemonEnvShellWord("/t/settings/daemon.env", `a"$b${T}${P}`, map[string]string{"T": "x"},
		func(n string) string { return map[string]string{"P": "p`q"}[n] })
	want := `"a\"\$b` + daemonEnvRead("/t/settings/daemon.env", "T") + "p\\`q\""
	if got != want {
		t.Errorf("word = %s\nwant   %s", got, want)
	}
}
