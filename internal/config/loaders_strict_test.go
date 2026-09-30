package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryTownConfigLoaderIsStrict: each loader under internal/config
// decodes through the one parser, so an unknown key or a syntax error is a
// *ParseError naming the file (gt-y3pgh.1), never a silently ignored key.
func TestEveryTownConfigLoaderIsStrict(t *testing.T) {
	t.Parallel()
	loaders := map[string]struct {
		valid string
		load  func(path string) error
	}{
		"LoadTownConfig":           {`{"type":"town","version":1,"name":"t"}`, func(p string) error { _, err := LoadTownConfig(p); return err }},
		"LoadRigsConfig":           {`{"version":1,"rigs":{}}`, func(p string) error { _, err := LoadRigsConfig(p); return err }},
		"LoadRigConfig":            {`{"type":"rig","version":1,"name":"r"}`, func(p string) error { _, err := LoadRigConfig(p); return err }},
		"LoadRigSettings":          {`{"type":"rig-settings","version":1}`, func(p string) error { _, err := LoadRigSettings(p); return err }},
		"LoadMayorConfig":          {`{"type":"mayor-config","version":1}`, func(p string) error { _, err := LoadMayorConfig(p); return err }},
		"LoadDaemonPatrolConfig":   {`{"type":"daemon-patrol-config","version":1}`, func(p string) error { _, err := LoadDaemonPatrolConfig(p); return err }},
		"LoadAccountsConfig":       {`{"version":1,"accounts":{}}`, func(p string) error { _, err := LoadAccountsConfig(p); return err }},
		"LoadMessagingConfig":      {`{"type":"messaging","version":1}`, func(p string) error { _, err := LoadMessagingConfig(p); return err }},
		"LoadEscalationConfig":     {`{"type":"escalation","version":1}`, func(p string) error { _, err := LoadEscalationConfig(p); return err }},
		"LoadOverseerConfig":       {`{"type":"overseer","version":1,"name":"n"}`, func(p string) error { _, err := LoadOverseerConfig(p); return err }},
		"LoadOrCreateTownSettings": {`{"type":"town-settings","version":1}`, func(p string) error { _, err := LoadOrCreateTownSettings(p); return err }},
	}
	for name, l := range loaders {
		dir := t.TempDir()
		good := filepath.Join(dir, "good.json")
		writeFile(t, good, l.valid)
		if err := l.load(good); err != nil {
			t.Errorf("%s(valid) = %v", name, err)
			continue
		}
		unknown := filepath.Join(dir, "unknown.json")
		writeFile(t, unknown, strings.TrimSuffix(l.valid, "}")+`,"zz_unknown":1}`)
		var pe *ParseError
		if err := l.load(unknown); !errors.As(err, &pe) || pe.Path != unknown || len(pe.Keys) != 1 || pe.Keys[0] != "zz_unknown" {
			t.Errorf("%s(unknown key) = %v, want *ParseError naming zz_unknown in %s", name, err, unknown)
		}
		broken := filepath.Join(dir, "broken.json")
		writeFile(t, broken, strings.TrimSuffix(l.valid, "}")+",}")
		if err := l.load(broken); !errors.As(err, &pe) || pe.Path != broken || pe.Line != 1 {
			t.Errorf("%s(broken) = %v, want *ParseError at line 1 of %s", name, err, broken)
		}
	}
}

func TestAgentRegistryOverlayIsStrict(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeFile(t, DefaultAgentRegistryPath(town), `{"version":1,"agents":{"x":{"command":"x","bogus":1}}}`)
	_, err := LoadAgentRegistryFor(town, "")
	if !errors.Is(err, ErrUnparseable) || !strings.Contains(err.Error(), "agents.x.bogus") {
		t.Fatalf("LoadAgentRegistryFor = %v, want a ParseError naming agents.x.bogus", err)
	}
}

func TestLoadDaemonEnvMalformedLineIsAParseError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeFile(t, DaemonEnvPath(town), "A=1\n\nno-equals-here\n")
	_, err := LoadDaemonEnv(town)
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Line != 3 || pe.Path != DaemonEnvPath(town) {
		t.Fatalf("LoadDaemonEnv = %v, want *ParseError at line 3", err)
	}
}
