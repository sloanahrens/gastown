package doctor

import (
	"strings"
	"testing"
)

func TestTownConfigSecretsCheck(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeDoctorTownFile(t, town, "mayor/town.json", `{"type": "town", "version": 2, "name": "t", "created_at": "2026-01-01T00:00:00Z"}`)
	settings := `{"type": "town-settings", "version": 1, "agents": {"ds": {"command": "claude", "env": {"ANTHROPIC_AUTH_TOKEN": "%s"}}}}`
	writeDoctorTownFile(t, town, "settings/config.json", strings.Replace(settings, "%s", "sk-fake0000000000000000000000000000", 1))

	r := NewTownConfigSecretsCheck().Run(&CheckContext{TownRoot: town})
	if r.Status != StatusWarning || strings.Join(r.Details, ",") != "agents.ds.env.ANTHROPIC_AUTH_TOKEN" {
		t.Fatalf("literal token: %s %s %v", r.Status, r.Message, r.Details)
	}
	if strings.Contains(r.Message+strings.Join(r.Details, "")+r.FixHint, "sk-fake") {
		t.Error("the check prints the token")
	}

	writeDoctorTownFile(t, town, "settings/config.json", strings.Replace(settings, "%s", "${DS_TOKEN}", 1))
	if r := NewTownConfigSecretsCheck().Run(&CheckContext{TownRoot: town}); r.Status != StatusOK {
		t.Errorf("reference: %s %s", r.Status, r.Message)
	}
}
