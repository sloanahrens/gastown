package config

import (
	"os"
	"path/filepath"
	"testing"
)

// gate_allowance is the operator-facing key for the overrun guard's ceiling on
// banked gate time: it loads into the field and a read-modify-write keeps it.
func TestOverrunGuardGateAllowanceConfigKey(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "daemon.json")
	raw := `{"type":"daemon-patrol-config","version":1,"patrols":{` +
		`"overrun_guard":{"enabled":true,"gate_allowance":"5m"}}}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadDaemonPatrolConfig(path)
	if err != nil {
		t.Fatalf("LoadDaemonPatrolConfig: %v", err)
	}
	og := cfg.Patrols.OverrunGuard
	if og == nil || !og.Enabled || og.GateAllowance != "5m" {
		t.Fatalf("overrun_guard = %+v, want enabled with gate_allowance 5m", og)
	}

	// The read-modify-write callers (gt config set) must not drop it.
	if err := WriteConfigJSON(path, cfg, 0o644); err != nil {
		t.Fatalf("WriteConfigJSON: %v", err)
	}
	saved, err := LoadDaemonPatrolConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := saved.Patrols.OverrunGuard.GateAllowance; got != "5m" {
		t.Errorf("gate_allowance after the write back = %q, want 5m", got)
	}
}

// A config that does not set gate_allowance leaves the field empty, so the
// guard's default ceiling applies.
func TestOverrunGuardGateAllowanceConfigKeyUnset(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "daemon.json")
	raw := `{"type":"daemon-patrol-config","version":1,"patrols":{` +
		`"overrun_guard":{"enabled":true}}}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadDaemonPatrolConfig(path)
	if err != nil {
		t.Fatalf("LoadDaemonPatrolConfig: %v", err)
	}
	og := cfg.Patrols.OverrunGuard
	if og == nil || og.GateAllowance != "" {
		t.Fatalf("overrun_guard = %+v, want gate_allowance empty", og)
	}
}
