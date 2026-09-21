package doctor

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// startTestListener opens a TCP listener on a free localhost port and returns
// the port number. Used to simulate a reachable Dolt server.
func startTestListener(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.Addr().(*net.TCPAddr).Port
}

// writeDaemonConfig writes a mayor/daemon.json with the given patrols JSON body.
func writeDaemonConfig(t *testing.T, townRoot, patrolsJSON string) {
	t.Helper()
	dir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	body := `{"type":"daemon","version":1,"patrols":` + patrolsJSON + `}`
	if err := os.WriteFile(filepath.Join(dir, "daemon.json"), []byte(body), 0644); err != nil {
		t.Fatalf("write daemon.json: %v", err)
	}
}

func TestDoltServerPatrolCheck_DoltUp(t *testing.T) {
	townRoot := t.TempDir()
	port := startTestListener(t)
	t.Setenv("GT_DOLT_PORT", strconv.Itoa(port))

	check := NewDoltServerPatrolCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusOK {
		t.Fatalf("expected OK when Dolt reachable, got %s (%s)", result.Status, result.Message)
	}
}

func TestDoltServerPatrolCheck_DownPatrolDisabled(t *testing.T) {
	townRoot := t.TempDir()
	// Point at a port with no listener → Dolt unreachable.
	t.Setenv("GT_DOLT_PORT", "1")
	// No dolt_server key in daemon.json → patrol not enabled.
	writeDaemonConfig(t, townRoot, `{"refinery":{"enabled":true}}`)

	check := NewDoltServerPatrolCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusWarning {
		t.Fatalf("expected WARNING for the config gap, got %s (%s)", result.Status, result.Message)
	}
	if result.FixHint == "" {
		t.Error("expected a FixHint for the config gap")
	}
}

func TestDoltServerPatrolCheck_DownPatrolEnabled(t *testing.T) {
	townRoot := t.TempDir()
	t.Setenv("GT_DOLT_PORT", "1")
	// dolt_server patrol present and enabled → daemon will detect/recover.
	writeDaemonConfig(t, townRoot, `{"dolt_server":{"enabled":true}}`)

	check := NewDoltServerPatrolCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusOK {
		t.Fatalf("expected OK when dolt_server patrol enabled, got %s (%s)", result.Status, result.Message)
	}
}

func TestDoltServerPatrolCheck_DownNoConfig(t *testing.T) {
	townRoot := t.TempDir()
	t.Setenv("GT_DOLT_PORT", "1")
	// No mayor/daemon.json at all → patrol not enabled.

	check := NewDoltServerPatrolCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusWarning {
		t.Fatalf("expected WARNING when no daemon config, got %s (%s)", result.Status, result.Message)
	}
}
