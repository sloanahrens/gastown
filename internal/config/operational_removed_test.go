package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// removedOperationalKeys are the operational keys gt-y3pgh.2.13.1 deleted, as a
// settings file spelled them. gt-y3pgh.2.13 warned about each for a release so
// an operator could delete the key first; now the declaration is gone and
// strict decoding must refuse the key outright.
var removedOperationalKeys = []string{
	"operational.deacon",
	"operational.polecat",
	"operational.web",

	"operational.dolt.health_check_interval",
	"operational.dolt.cmd_timeout",
	"operational.dolt.max_connections",
	"operational.dolt.slow_query_threshold",

	"operational.session.claude_start_timeout",
	"operational.session.shell_ready_timeout",
	"operational.session.graceful_shutdown_timeout",
	"operational.session.gupp_violation_timeout",
	"operational.session.hung_session_threshold",

	"operational.nudge.ready_timeout",
	"operational.nudge.retry_interval",
	"operational.nudge.lock_timeout",
	"operational.nudge.normal_ttl",
	"operational.nudge.urgent_ttl",

	"operational.daemon.dog_idle_session_timeout",
	"operational.daemon.dog_idle_remove_timeout",
	"operational.daemon.stale_working_timeout",
	"operational.daemon.max_dog_pool_size",
	"operational.daemon.boot_spawn_cooldown",
	"operational.daemon.boot_turn_budget",
	"operational.daemon.boot_idle_suppression",
	"operational.daemon.boot_mode",
	"operational.daemon.deacon_grace_period",
	"operational.daemon.mass_death_window",
	"operational.daemon.mass_death_threshold",
	"operational.daemon.max_lifecycle_message_age",
	"operational.daemon.sync_failure_escalation_threshold",
	"operational.daemon.doctor_mol_cooldown",

	"operational.mail.idle_notify_timeout",
	"operational.mail.bd_read_timeout",
	"operational.mail.bd_write_timeout",
	"operational.mail.max_concurrent_ack_ops",

	"operational.witness.startup_stall_threshold",
	"operational.witness.startup_activity_grace",
	"operational.witness.done_intent_stuck_timeout",
	"operational.witness.done_intent_recent_grace",
	"operational.witness.done_intent_max_age",
	"operational.witness.composer_stall_frozen_for",
}

// TestRemovedOperationalKeysAreRejected: every key the follow-up deleted is
// named by strict decoding, so a settings file that still sets one is refused
// with the key rather than silently ignored.
func TestRemovedOperationalKeysAreRejected(t *testing.T) {
	t.Parallel()
	if len(removedOperationalKeys) != 41 {
		t.Fatalf("removedOperationalKeys has %d keys, want the 41 gt-y3pgh.2.13.1 deleted", len(removedOperationalKeys))
	}
	for _, key := range removedOperationalKeys {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			data := settingsWithKey(t, key)
			err := DecodeJSONFile("settings/config.json", data, &TownSettings{})
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("DecodeJSONFile accepted the removed key %s: %v", key, err)
			}
			if len(pe.Keys) != 1 || pe.Keys[0] != key {
				t.Fatalf("ParseError names %v, want [%s]", pe.Keys, key)
			}
		})
	}
}

// TestContainerGateStillLoads: the live town's only operational key survives the
// deletion, so its settings file still loads strictly.
func TestContainerGateStillLoads(t *testing.T) {
	t.Parallel()
	path := writeSettings(t, `{
  "type": "town-settings",
  "version": 1,
  "operational": {"container_gate": {"slots": 4, "reserved_for_gate": 2}}
}`)
	settings, err := LoadOrCreateTownSettings(path)
	if err != nil {
		t.Fatalf("LoadOrCreateTownSettings: %v", err)
	}
	gate := settings.Operational.GetContainerGateConfig()
	if gate.SlotsV() != 4 || gate.ReservedForGateV() != 2 {
		t.Errorf("container_gate = slots %d reserved %d, want 4 and 2", gate.SlotsV(), gate.ReservedForGateV())
	}
}

// settingsWithKey nests key under a set value. The walker reports an undeclared
// key without descending, so the value's type does not matter except that a
// section's own value (operational.deacon) must be an object for the file to
// read as the settings it always was.
func settingsWithKey(t *testing.T, key string) []byte {
	t.Helper()
	parts := strings.Split(key, ".")
	value := any("1")
	if len(parts) == 2 {
		value = map[string]any{}
	}
	var body any = value
	for i := len(parts) - 1; i >= 0; i-- {
		body = map[string]any{parts[i]: body}
	}
	data, err := json.Marshal(map[string]any{
		"type":        "town-settings",
		"version":     1,
		"operational": body.(map[string]any)["operational"],
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeSettings(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
