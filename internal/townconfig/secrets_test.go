package townconfig

import (
	"strings"
	"testing"
)

// The token below is fake.
const fakeSecretSettings = `{"type": "town-settings", "version": 1%s,
  "agents": {"ds": {"command": "claude", "env": {"ANTHROPIC_AUTH_TOKEN": "sk-fake0000000000000000000000000000"}}}}`

func literalSecretTown(t *testing.T, extra string) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, FileTown, `{"type": "town", "version": 2, "name": "t", "created_at": "2026-01-01T00:00:00Z"}`)
	write(t, root, FileSettings, strings.Replace(fakeSecretSettings, "%s", extra, 1))
	return root
}

func TestLoadWarnsOnLiteralTokenByDefault(t *testing.T) {
	t.Parallel()
	town, err := Load(literalSecretTown(t, ""))
	if err != nil {
		t.Fatalf("Load = %v; literals must only warn by default", err)
	}
	msg := town.LiteralSecretsWarning()
	if !strings.Contains(msg, "agents.ds.env.ANTHROPIC_AUTH_TOKEN") || !strings.Contains(msg, "gt config secrets migrate") {
		t.Errorf("warning = %q", msg)
	}
	if strings.Contains(msg, "sk-fake") {
		t.Error("warning prints the token")
	}
}

func TestLoadRefusesLiteralTokenWhenSwitchedOn(t *testing.T) {
	t.Parallel()
	_, err := Load(literalSecretTown(t, `, "secrets": {"refuse_literals": true}`))
	if err == nil {
		t.Fatal("Load accepted a literal token with secrets.refuse_literals set")
	}
	if !strings.Contains(err.Error(), "agents.ds.env.ANTHROPIC_AUTH_TOKEN") || strings.Contains(err.Error(), "sk-fake") {
		t.Errorf("error must name the path and not the value: %v", err)
	}
}
