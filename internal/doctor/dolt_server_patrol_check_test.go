package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// patrolCheck is a DoltServerPatrolCheck whose dial reaches the Dolt server
// only when up is true.
func patrolCheck(t *testing.T, up bool) *DoltServerPatrolCheck {
	t.Helper()
	c := NewDoltServerPatrolCheck()
	c.dial = func(addr string) error {
		if !strings.HasSuffix(addr, ":3307") {
			t.Errorf("dialed %q, want the town's port 3307", addr)
		}
		if up {
			return nil
		}
		return errors.New("connection refused")
	}
	return c
}

// endpointTown is a town whose Dolt endpoint is port 3307.
func endpointTown(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	dir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("listener:\n  port: 3307\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

// writeDaemonConfig writes a mayor/daemon.json with the given patrols JSON body.
func writeDaemonConfig(t *testing.T, townRoot, patrolsJSON string) {
	t.Helper()
	dir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":` + patrolsJSON + `}`
	if err := os.WriteFile(filepath.Join(dir, "daemon.json"), []byte(body), 0644); err != nil {
		t.Fatalf("write daemon.json: %v", err)
	}
}

func TestDoltServerPatrolCheck_DoltUp(t *testing.T) {
	t.Parallel()
	townRoot := endpointTown(t)
	check := patrolCheck(t, true)
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusOK {
		t.Fatalf("expected OK when Dolt reachable, got %s (%s)", result.Status, result.Message)
	}
}

func TestDoltServerPatrolCheck_DownPatrolDisabled(t *testing.T) {
	t.Parallel()
	townRoot := endpointTown(t)
	// No dolt_server key in daemon.json → patrol not enabled.
	writeDaemonConfig(t, townRoot, `{"doctor_dog":{"enabled":true}}`)

	check := patrolCheck(t, false) // Dolt unreachable
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusWarning {
		t.Fatalf("expected WARNING for the config gap, got %s (%s)", result.Status, result.Message)
	}
	if result.FixHint == "" {
		t.Error("expected a FixHint for the config gap")
	}
}

func TestDoltServerPatrolCheck_DownPatrolEnabled(t *testing.T) {
	t.Parallel()
	townRoot := endpointTown(t)
	// dolt_server patrol present and enabled → daemon will detect/recover.
	writeDaemonConfig(t, townRoot, `{"dolt_server":{"enabled":true}}`)

	check := patrolCheck(t, false) // Dolt unreachable
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusOK {
		t.Fatalf("expected OK when dolt_server patrol enabled, got %s (%s)", result.Status, result.Message)
	}
}

func TestDoltServerPatrolCheck_DownNoConfig(t *testing.T) {
	t.Parallel()
	townRoot := endpointTown(t)
	// No mayor/daemon.json at all → patrol not enabled.

	check := patrolCheck(t, false) // Dolt unreachable
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusWarning {
		t.Fatalf("expected WARNING when no daemon config, got %s (%s)", result.Status, result.Message)
	}
}
