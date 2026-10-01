package cmd

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// The token is fake.
const fakeCmdToken = "sk-fake0000000000000000000000000000"

func secretsCmdTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	s := config.NewTownSettings()
	s.Agents["ds"] = &config.RuntimeConfig{Command: "claude", Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": fakeCmdToken}}
	if err := config.SaveTownSettings(config.TownSettingsPath(town), s); err != nil {
		t.Fatal(err)
	}
	return town
}

func TestConfigSecretsMigrateDryRunPrintsNamesOnly(t *testing.T) {
	t.Parallel()
	town := secretsCmdTown(t)
	var out bytes.Buffer
	if err := configSecretsMigrate(townConfigCmdEnv(town, &out), true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "agents.ds.env.ANTHROPIC_AUTH_TOKEN -> ${DS_ANTHROPIC_AUTH_TOKEN}") || strings.Contains(out.String(), "sk-fake") {
		t.Errorf("dry-run output:\n%s", out.String())
	}
	if _, err := os.Stat(config.DaemonEnvPath(town)); !os.IsNotExist(err) {
		t.Error("dry-run wrote daemon.env")
	}
}

func TestConfigSetRefuseLiteralsWaitsForMigrate(t *testing.T) {
	t.Parallel()
	town := secretsCmdTown(t)
	var out bytes.Buffer
	e := townConfigCmdEnv(town, &out)
	err := configSet(e, []string{"secrets.refuse_literals", "true"})
	if err == nil || !strings.Contains(err.Error(), "agents.ds.env.ANTHROPIC_AUTH_TOKEN") || strings.Contains(err.Error(), "sk-fake") {
		t.Fatalf("set over a literal = %v", err)
	}
	if err := configSecretsMigrate(e, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "sk-fake") {
		t.Errorf("migrate output prints the token:\n%s", out.String())
	}
	if err := configSet(e, []string{"secrets.refuse_literals", "true"}); err != nil {
		t.Fatalf("set after migrate = %v", err)
	}
	out.Reset()
	if err := configGet(e, []string{"secrets.refuse_literals"}); err != nil || strings.TrimSpace(out.String()) != "true" {
		t.Errorf("get = %q, %v", out.String(), err)
	}
}
