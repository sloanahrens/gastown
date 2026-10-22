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

// TestDoltServerPatrolCheck_TwoFileTownNamesTheDaemonSection: on a town that has
// run gt config migrate mayor/daemon.json is gone, so the hint must send the
// operator to the "daemon" section of settings/config.json.
func TestDoltServerPatrolCheck_TwoFileTownNamesTheDaemonSection(t *testing.T) {
	t.Parallel()
	townRoot := endpointTown(t)
	writeDoctorTownFile(t, townRoot, "mayor/town.json", `{"registry":{}}`)
	writeDoctorTownFile(t, townRoot, "settings/config.json",
		`{"daemon":{"type":"daemon-patrol-config","version":1,"patrols":{"doctor_dog":{"enabled":true}}}}`)

	result := patrolCheck(t, false).Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusWarning {
		t.Fatalf("expected WARNING for the config gap, got %s (%s)", result.Status, result.Message)
	}
	text := result.FixHint + " " + strings.Join(result.Details, " ")
	if !strings.Contains(text, `settings/config.json (section "daemon")`) {
		t.Errorf("hint and details %q must name the daemon section of settings/config.json", text)
	}
	if strings.Contains(text, "daemon.json") {
		t.Errorf("hint and details %q name the retired daemon.json", text)
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

// TestDoltServerPatrolCheck_NoEndpointPatrolEnabledIsNotOK: a town with no Dolt
// endpoint gets no daemon-managed Dolt, so an enabled patrol is no cover.
func TestDoltServerPatrolCheck_NoEndpointPatrolEnabledIsNotOK(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeDaemonConfig(t, townRoot, `{"dolt_server":{"enabled":true}}`)

	check := NewDoltServerPatrolCheck()
	check.dial = func(addr string) error {
		t.Errorf("dialed %q, want no dial for a town with no endpoint", addr)
		return errors.New("connection refused")
	}
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusWarning {
		t.Fatalf("expected WARNING for a town with no Dolt endpoint, got %s (%s)", result.Status, result.Message)
	}
	if strings.Contains(result.Message, "daemon will detect and recover") {
		t.Errorf("message %q reports the patrol as cover", result.Message)
	}
	if !strings.Contains(result.FixHint, "gt config set dolt.port") {
		t.Errorf("FixHint %q must name gt config set dolt.port", result.FixHint)
	}
}

// TestDoltServerPatrolCheck_NoEndpointPatrolDisabledIsNotOK: the same warning
// holds when the patrol is also disabled, and the check never dials.
func TestDoltServerPatrolCheck_NoEndpointPatrolDisabledIsNotOK(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeDaemonConfig(t, townRoot, `{"doctor_dog":{"enabled":true}}`)

	check := NewDoltServerPatrolCheck()
	check.dial = func(addr string) error {
		t.Errorf("dialed %q, want no dial for a town with no endpoint", addr)
		return errors.New("connection refused")
	}
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusWarning {
		t.Fatalf("expected WARNING for a town with no Dolt endpoint, got %s (%s)", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "no Dolt endpoint") {
		t.Errorf("message %q must name the missing endpoint", result.Message)
	}
}

// TestDoltServerPatrolCheck_UnloadableTownConfigIsNotNoEndpoint: a mayor/town.json
// that will not load (here dolt.port 0) is a broken config, not an absent
// endpoint, so the warning names the load error rather than "no Dolt endpoint".
func TestDoltServerPatrolCheck_UnloadableTownConfigIsNotNoEndpoint(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeDoctorTownFile(t, townRoot, "mayor/town.json", `{"type":"town","version":1,"name":"test","dolt":{"host":"127.0.0.1"}}`)

	check := NewDoltServerPatrolCheck()
	check.dial = func(addr string) error {
		t.Errorf("dialed %q, want no dial for an unloadable town config", addr)
		return errors.New("connection refused")
	}
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusWarning {
		t.Fatalf("expected WARNING for an unloadable town config, got %s (%s)", result.Status, result.Message)
	}
	if strings.Contains(result.Message, "no Dolt endpoint") {
		t.Errorf("message %q conflates an unloadable town config with no endpoint", result.Message)
	}
	if !strings.Contains(strings.Join(result.Details, " "), "dolt.port") {
		t.Errorf("details %q must name the load error for dolt.port", result.Details)
	}
	if !strings.Contains(result.FixHint, "mayor/town.json") {
		t.Errorf("FixHint %q must name mayor/town.json", result.FixHint)
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
