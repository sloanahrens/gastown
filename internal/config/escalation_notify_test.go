package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeEscalationFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings", "escalation.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecActionName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		action   string
		wantName string
		wantOK   bool
	}{
		{"exec:ntfy", "ntfy", true},
		{"exec:", "", true},
		{"exec:a:b", "a:b", true},
		{"bead", "", false},
		{"mail:gastown/witness", "", false},
		{"slack", "", false},
		{"EXEC:ntfy", "", false},
	}
	for _, tc := range tests {
		name, ok := ExecActionName(tc.action)
		if name != tc.wantName || ok != tc.wantOK {
			t.Errorf("ExecActionName(%q) = %q, %v; want %q, %v", tc.action, name, ok, tc.wantName, tc.wantOK)
		}
	}
}

func TestEscalationConfigNotifyLoads(t *testing.T) {
	t.Parallel()
	path := writeEscalationFile(t, `{
		"type": "escalation",
		"version": 1,
		"routes": {"high": ["bead", "exec:ntfy"], "critical": ["exec:ntfy", "exec:pager"]},
		"notify": {"commands": {
			"ntfy": ["curl", "-fsS", "--data-binary", "@-", "https://ntfy.sh/REPLACE_WITH_PRIVATE_TOPIC"],
			"pager": ["/usr/local/bin/page"]
		}}
	}`)

	cfg, err := LoadEscalationConfig(path)
	if err != nil {
		t.Fatalf("LoadEscalationConfig: %v", err)
	}
	want := []string{"curl", "-fsS", "--data-binary", "@-", "https://ntfy.sh/REPLACE_WITH_PRIVATE_TOPIC"}
	if got := cfg.Notify.Commands["ntfy"]; !reflect.DeepEqual(got, want) {
		t.Errorf("notify.commands.ntfy = %v, want %v", got, want)
	}
	if got := cfg.GetRouteForSeverity("high"); !reflect.DeepEqual(got, []string{"bead", "exec:ntfy"}) {
		t.Errorf("high route = %v", got)
	}
}

func TestEscalationConfigNotifyRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		wantErr []string
	}{
		{
			name:    "route names an unconfigured command",
			body:    `{"routes": {"high": ["bead", "exec:ntfy"]}}`,
			wantErr: []string{"exec:ntfy", "notify.commands.ntfy"},
		},
		{
			name:    "route names a command that is not among the configured ones",
			body:    `{"routes": {"low": ["exec:other"]}, "notify": {"commands": {"ntfy": ["curl"]}}}`,
			wantErr: []string{"exec:other", "other"},
		},
		{
			name:    "route with no name",
			body:    `{"routes": {"low": ["exec:"]}, "notify": {"commands": {"ntfy": ["curl"]}}}`,
			wantErr: []string{"exec:"},
		},
		{
			name:    "command with an empty argv",
			body:    `{"notify": {"commands": {"ntfy": []}}}`,
			wantErr: []string{"notify.commands.ntfy", "argv"},
		},
		{
			name:    "command with no program",
			body:    `{"notify": {"commands": {"ntfy": [""]}}}`,
			wantErr: []string{"notify.commands.ntfy", "argv"},
		},
		{
			name:    "command given as a string instead of an argv array",
			body:    `{"notify": {"commands": {"ntfy": "curl -fsS"}}}`,
			wantErr: []string{"ntfy"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadEscalationConfig(writeEscalationFile(t, tc.body))
			if err == nil {
				t.Fatal("LoadEscalationConfig succeeded, want an error")
			}
			for _, part := range tc.wantErr {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not mention %q", err, part)
				}
			}
		})
	}
}

// TestEscalationConfigWithoutNotifyIsUnchanged: a town that configures no
// notify command loads, defaults and saves as it did before the key existed.
func TestEscalationConfigWithoutNotifyIsUnchanged(t *testing.T) {
	t.Parallel()
	cfg := NewEscalationConfig()
	if cfg.Notify.Commands != nil {
		t.Errorf("default Notify.Commands = %v, want nil", cfg.Notify.Commands)
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"notify"`) {
		t.Errorf("default config marshals a notify key: %s", data)
	}

	path := filepath.Join(t.TempDir(), "settings", "escalation.json")
	if err := SaveEscalationConfig(path, cfg); err != nil {
		t.Fatalf("SaveEscalationConfig: %v", err)
	}
	loaded, err := LoadEscalationConfig(path)
	if err != nil {
		t.Fatalf("LoadEscalationConfig: %v", err)
	}
	if !reflect.DeepEqual(loaded.Routes, cfg.Routes) {
		t.Errorf("routes = %v, want %v", loaded.Routes, cfg.Routes)
	}
}

func TestEscalationConfigSaveRejectsUnconfiguredExecRoute(t *testing.T) {
	t.Parallel()
	cfg := NewEscalationConfig()
	cfg.Routes[SeverityHigh] = []string{"bead", "exec:ntfy"}

	err := SaveEscalationConfig(filepath.Join(t.TempDir(), "escalation.json"), cfg)
	if err == nil || !strings.Contains(err.Error(), "ntfy") {
		t.Fatalf("SaveEscalationConfig = %v, want an error naming ntfy", err)
	}
}
