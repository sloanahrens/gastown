package townconfig

import (
	"fmt"
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

// The loader, not only the classifier, must refuse what the sandbox skeleton
// treats as a credential, and must accept a region under the same switch.
func TestLoadRefuseLiteralsClassifiesRegionAndPasswd(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		key, value string
		refused    bool
	}{
		{"AWS_REGION", "ASIA-PACIFIC-EAST-1", false},
		{"DB_PASSWD", "synthetic-passwd-value-0123", true},
	} {
		root := t.TempDir()
		write(t, root, FileTown, `{"type": "town", "version": 2, "name": "t", "created_at": "2026-01-01T00:00:00Z"}`)
		write(t, root, FileSettings, fmt.Sprintf(
			`{"type": "town-settings", "version": 1, "secrets": {"refuse_literals": true}, "agents": {"ds": {"command": "claude", "env": {%q: %q}}}}`,
			tt.key, tt.value))
		_, err := Load(root)
		if got := err != nil; got != tt.refused {
			t.Errorf("Load with %s set: refused = %v, want %v (err %v)", tt.key, got, tt.refused, err)
		}
		if err != nil && strings.Contains(err.Error(), tt.value) {
			t.Errorf("error prints the value of %s: %v", tt.key, err)
		}
	}
}
