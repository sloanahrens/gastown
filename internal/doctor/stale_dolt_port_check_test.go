package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// TestStaleDoltPortCheck_ConsistentPorts verifies the check passes when all ports are consistent.
func TestStaleDoltPortCheck_ConsistentPorts(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Create .dolt-data/config.yaml with port 3307
	doltDataDir := filepath.Join(tmpDir, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	configYaml := `log_level: warning
listener:
  port: 3307
`
	if err := os.WriteFile(filepath.Join(doltDataDir, "config.yaml"), []byte(configYaml), 0644); err != nil {
		t.Fatal(err)
	}

	// Create town .beads/metadata.json with consistent port
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	metadata := map[string]interface{}{
		"dolt_mode":        "server",
		"dolt_server_host": "127.0.0.1",
		"dolt_server_port": 3307,
		"dolt_database":    "hq",
	}
	metadataBytes, _ := json.MarshalIndent(metadata, "", "  ")
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), metadataBytes, 0644); err != nil {
		t.Fatal(err)
	}

	// Create dolt-server.port file with consistent port
	if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.port"), []byte("3307"), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewStaleDoltPortCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for consistent ports, got %v: %s", result.Status, result.Message)
	}
}

// TestStaleDoltPortCheckIgnoresTheDeprecatedPort verifies the check does not
// report metadata.json for its deprecated dolt_server_port key, and that Fix
// removes the key from a file that carries it while keeping every other field
// and the port file (gt-49f3pn).
func TestStaleDoltPortCheckIgnoresTheDeprecatedPort(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	writeDoctorTownFile(t, tmpDir, ".dolt-data/config.yaml", "listener:\n  port: 3307\n")
	metaFile := writeDoctorTownFile(t, tmpDir, ".beads/metadata.json", `{"dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":3306,"dolt_database":"hq"}`)

	check := NewStaleDoltPortCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK: the key is not a port the check judges, got %v: %s %v", result.Status, result.Message, result.Details)
	}

	// A stale port file makes the check warn; Fix then cleans the key too.
	portFile := writeDoctorTownFile(t, tmpDir, ".beads/dolt-server.port", "3306")
	result = check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning for the stale port file, got %v: %s", result.Status, result.Message)
	}
	for _, detail := range result.Details {
		if strings.Contains(detail, "metadata.json") {
			t.Errorf("metadata.json reported for its deprecated key: %s", detail)
		}
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() failed: %v", err)
	}
	if _, err := os.Stat(portFile); !os.IsNotExist(err) {
		t.Errorf("stale port file survived Fix (stat err: %v)", err)
	}
	data, err := os.ReadFile(metaFile)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]interface{}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if _, ok := meta["dolt_server_port"]; ok {
		t.Errorf("Fix left the deprecated dolt_server_port key: %v", meta["dolt_server_port"])
	}
	if meta["dolt_database"] != "hq" || meta["dolt_server_host"] != "127.0.0.1" {
		t.Errorf("Fix dropped other fields: %v", meta)
	}
}

// TestStaleDoltPortCheck_FixRemovesTheKeyAndStalePort verifies that Fix() removes
// a stale port file and the deprecated key, and that the town is then consistent.
func TestStaleDoltPortCheck_FixRemovesTheKeyAndStalePort(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	writeDoctorTownFile(t, tmpDir, ".dolt-data/config.yaml", "listener:\n  port: 3307\n")
	writeDoctorTownFile(t, tmpDir, ".beads/metadata.json", `{"dolt_mode":"server","dolt_server_port":3306,"dolt_database":"hq"}`)
	writeDoctorTownFile(t, tmpDir, ".beads/dolt-server.port", "3306")

	check := NewStaleDoltPortCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning before fix, got %v", result.Status)
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, ".beads", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "dolt_server_port") {
		t.Errorf("metadata.json still has the deprecated key: %s", data)
	}

	result = check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK after fix, got %v: %s", result.Status, result.Message)
	}
}

// TestStaleDoltPortCheck_TownJSONPortWins verifies the port named in
// mayor/town.json is the correct one when .dolt-data/config.yaml is absent.
func TestStaleDoltPortCheck_TownJSONPortWins(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	writeDoctorTownFile(t, tmpDir, "mayor/town.json", `{"type":"town","version":1,"name":"test","dolt":{"host":"127.0.0.1","port":3310}}`)
	portFile := writeDoctorTownFile(t, tmpDir, ".beads/dolt-server.port", "3310")
	metaFile := writeDoctorTownFile(t, tmpDir, ".beads/metadata.json", `{"dolt_mode":"server","dolt_server_port":3310}`)

	check := NewStaleDoltPortCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK, got %v: %s %v", result.Status, result.Message, result.Details)
	}
	if len(check.stalePorts) != 0 {
		t.Errorf("expected no stale ports, got %+v", check.stalePorts)
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() failed: %v", err)
	}
	if _, err := os.Stat(portFile); err != nil {
		t.Errorf("port file must survive Fix: %v", err)
	}
	data, err := os.ReadFile(metaFile)
	if err != nil || strings.Contains(string(data), "dolt_server_port") {
		t.Errorf("metadata.json must lose the deprecated key, got %q (err %v)", data, err)
	}
}

// TestStaleDoltPortCheck_TownJSONWinsOverConfigYAML verifies a conflicting
// .dolt-data/config.yaml does not displace the port mayor/town.json names.
func TestStaleDoltPortCheck_TownJSONWinsOverConfigYAML(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	writeDoctorTownFile(t, tmpDir, "mayor/town.json", `{"type":"town","version":1,"name":"test","dolt":{"host":"127.0.0.1","port":3310}}`)
	writeDoctorTownFile(t, tmpDir, ".dolt-data/config.yaml", "listener:\n  host: 127.0.0.1\n  port: 3307\n")
	portFile := writeDoctorTownFile(t, tmpDir, ".beads/dolt-server.port", "3310")
	metaFile := writeDoctorTownFile(t, tmpDir, ".beads/metadata.json", `{"dolt_mode":"server","dolt_server_port":3310}`)

	check := NewStaleDoltPortCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	// Ports agreeing with town.json pass even though they disagree with
	// config.yaml: config.yaml's 3307 lost the precedence contest.
	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK judged against town.json's 3310, got %v: %s %v", result.Status, result.Message, result.Details)
	}

	// Point both files at config.yaml's 3307. They are now stale only against
	// town.json's 3310, so this outcome is unreachable unless town.json wins.
	if err := os.WriteFile(portFile, []byte("3307"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaFile, []byte(`{"dolt_mode":"server","dolt_server_port":3307}`), 0o600); err != nil {
		t.Fatal(err)
	}

	result = check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning judged against town.json's 3310, got %v: %s", result.Status, result.Message)
	}
	if len(check.stalePorts) != 1 || check.stalePorts[0].correctPort != 3310 {
		t.Errorf("stalePorts = %+v, want the port file judged against town.json's 3310", check.stalePorts)
	}
}

// TestStaleDoltPortCheck_PortlessTownJSONFallsThroughToConfigYAML verifies a
// mayor/town.json "dolt" block that names no port is not an endpoint: the
// check judges port files against .dolt-data/config.yaml's listener instead of
// skipping.
func TestStaleDoltPortCheck_PortlessTownJSONFallsThroughToConfigYAML(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	writeDoctorTownFile(t, tmpDir, "mayor/town.json", `{"type":"town","version":1,"name":"test","dolt":{"host":"127.0.0.1","port":0}}`)
	writeDoctorTownFile(t, tmpDir, ".dolt-data/config.yaml", "listener:\n  host: 127.0.0.1\n  port: 3307\n")
	writeDoctorTownFile(t, tmpDir, ".beads/dolt-server.port", "3306")
	writeDoctorTownFile(t, tmpDir, ".beads/metadata.json", `{"dolt_mode":"server","dolt_server_port":3307}`)

	// The resolver itself does the fall-through: LoadTownConfig rejects the
	// portless block, so ResolveDoltEndpoint reads config.yaml's listener. A
	// resolver that returned Port 0 with ok would make the check skip below.
	if ep, ok := config.ResolveDoltEndpoint(tmpDir); !ok || ep.Port != 3307 {
		t.Fatalf("ResolveDoltEndpoint = %+v, %v; want config.yaml's 3307", ep, ok)
	}

	check := NewStaleDoltPortCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning for the 3306 port file judged against config.yaml's 3307, got %v: %s", result.Status, result.Message)
	}
	if len(check.stalePorts) != 1 || check.stalePorts[0].correctPort != 3307 {
		t.Errorf("stalePorts = %+v, want the port file judged against 3307", check.stalePorts)
	}
}

// TestStaleDoltPortCheck_UnknownPortSkipsAndFixKeepsPortFile verifies that a
// town naming no Dolt port is reported unknown, and Fix keeps the port file
// (it removes only the deprecated key, which needs no port to judge).
func TestStaleDoltPortCheck_UnknownPortSkipsAndFixKeepsPortFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	portFile := writeDoctorTownFile(t, tmpDir, ".beads/dolt-server.port", "3310")
	metaFile := writeDoctorTownFile(t, tmpDir, ".beads/metadata.json", `{"dolt_mode":"server","dolt_server_port":3310}`)

	check := NewStaleDoltPortCheck()
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)
	if result.Status != StatusSkipped {
		t.Fatalf("expected StatusSkipped, got %v: %s", result.Status, result.Message)
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() failed: %v", err)
	}
	if _, err := os.Stat(portFile); err != nil {
		t.Errorf("port file must survive Fix when the port is unknown: %v", err)
	}
	data, err := os.ReadFile(metaFile)
	if err != nil || strings.Contains(string(data), "dolt_server_port") {
		t.Errorf("metadata.json must lose the deprecated key, got %q (err %v)", data, err)
	}
}
