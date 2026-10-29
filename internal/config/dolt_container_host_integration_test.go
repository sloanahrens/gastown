//go:build integration

package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// TestIntegrationDoltContainerHostFromEnvironment drives the one environment
// read on the Dolt endpoint path end to end: the real GT_DAEMON_SUPERVISOR
// decides the mode through config.InContainerMode, and the exported entry
// points resolve dolt.container_host for a daemon the compose file started and
// dolt.host for every other process (gt-qpeei.17). The unit tier cannot set the
// environment (docs/testing.md, `no-env`), so the read itself is tested here;
// the branch table for the same substitution is TestDoltEndpointContainerHost.
func TestIntegrationDoltContainerHostFromEnvironment(t *testing.T) {
	const (
		localHost     = "127.0.0.1"
		containerHost = "host.docker.internal"
	)
	townRoot := t.TempDir()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	townJSON := `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z",` +
		`"dolt":{"host":"` + localHost + `","container_host":"` + containerHost + `","port":3307}}`
	if err := os.WriteFile(filepath.Join(mayorDir, "town.json"), []byte(townJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		supervisor string
		wantHost   string
	}{
		{"the flag the compose file exports reaches container_host", config.SupervisorContainer, containerHost},
		{"a launchd daemon keeps dolt.host", config.SupervisorLaunchd, localHost},
		{"an unset flag keeps dolt.host", "", localHost},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(config.ContainerModeEnv, tt.supervisor)
			ep, ok := config.ResolveDoltEndpoint(townRoot)
			if !ok || ep.Host != tt.wantHost {
				t.Fatalf("ResolveDoltEndpoint = %+v, %v; want host %q", ep, ok, tt.wantHost)
			}
			if got := config.ConfiguredDoltEnv(townRoot)["BEADS_DOLT_SERVER_HOST"]; got != tt.wantHost {
				t.Errorf("ConfiguredDoltEnv BEADS_DOLT_SERVER_HOST = %q, want %q", got, tt.wantHost)
			}
			normalized := config.NormalizeConfiguredDoltEnv([]string{"BEADS_DOLT_SERVER_HOST=stale"}, townRoot)
			if got := envEntryValue(normalized, "BEADS_DOLT_SERVER_HOST"); got != tt.wantHost {
				t.Errorf("NormalizeConfiguredDoltEnv BEADS_DOLT_SERVER_HOST = %q, want %q", got, tt.wantHost)
			}
		})
	}
}

// envEntryValue is the value of key in an env slice, or "" when absent.
func envEntryValue(env []string, key string) string {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, key+"="); ok {
			return value
		}
	}
	return ""
}
