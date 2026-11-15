package config

import (
	"strings"
	"testing"
	"time"
)

// TestSquashKeys: every squash setting is a key under the scheduled_maintenance
// section the archive's keys sit in, so a refusal names the daemon.json path an
// operator edits (gt-f1or4z).
func TestSquashKeys(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, got, want string
	}{
		{"enabled", SquashEnabledKey, "patrols.scheduled_maintenance.squash.enabled"},
		{"databases", SquashDatabasesKey, "patrols.scheduled_maintenance.squash.databases"},
		{"keep_days", SquashKeepDaysKey, "patrols.scheduled_maintenance.squash.keep_days"},
		{"trigger_backup_bytes", SquashTriggerBackupBytesKey, "patrols.scheduled_maintenance.squash.trigger_backup_bytes"},
		{"trigger_backup_duration", SquashTriggerBackupDurationKey, "patrols.scheduled_maintenance.squash.trigger_backup_duration"},
		{"trigger_query_p95", SquashTriggerQueryP95Key, "patrols.scheduled_maintenance.squash.trigger_query_p95"},
		{"retain", SquashRetainKey, "patrols.scheduled_maintenance.squash.retain"},
		{"max_runtime", SquashMaxRuntimeKey, "patrols.scheduled_maintenance.squash.max_runtime"},
	} {
		if tt.got != tt.want {
			t.Errorf("%s key = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

// TestSquashDefaultsWhenKeysAreAbsent: a block that names only its databases
// reads the design's defaults, and a zero or empty entry reads the same default
// as an absent one (gt-f1or4z).
func TestSquashDefaultsWhenKeysAreAbsent(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		block string
	}{
		{"absent keys", `{"enabled":true,"databases":["gt"]}`},
		{"zero and empty keys", `{"enabled":true,"databases":["gt"],"keep_days":0,"trigger_backup_bytes":0,"trigger_backup_duration":"","trigger_query_p95":"","retain":"","max_runtime":""}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := parseSquashBlock(t, tt.block)
			if s.KeepDays != 7 {
				t.Errorf("keep_days = %d, want 7", s.KeepDays)
			}
			if s.TriggerBackupBytes != 5<<30 {
				t.Errorf("trigger_backup_bytes = %d, want %d (5 GiB)", s.TriggerBackupBytes, int64(5)<<30)
			}
			if s.TriggerBackupDuration != 5*time.Minute {
				t.Errorf("trigger_backup_duration = %v, want 5m", s.TriggerBackupDuration)
			}
			if s.TriggerQueryP95 != 200*time.Millisecond {
				t.Errorf("trigger_query_p95 = %v, want 200ms", s.TriggerQueryP95)
			}
			if s.Retain != 48*time.Hour {
				t.Errorf("retain = %v, want 48h", s.Retain)
			}
			if s.MaxRuntime != 30*time.Minute {
				t.Errorf("max_runtime = %v, want 30m", s.MaxRuntime)
			}
			if len(s.Databases) != 1 || s.Databases[0] != "gt" {
				t.Errorf("databases = %v, want [gt]", s.Databases)
			}
		})
	}
}

// TestSquashSettingsFromDaemonJSON: each setting decodes from the daemon.json
// name the operator writes into the value ParseSquash returns (gt-f1or4z).
func TestSquashSettingsFromDaemonJSON(t *testing.T) {
	t.Parallel()
	s := parseSquashBlock(t, `{"enabled":true,"databases":["gt","be"],"keep_days":3,`+
		`"trigger_backup_bytes":1073741824,"trigger_backup_duration":"2m",`+
		`"trigger_query_p95":"50ms","retain":"72h","max_runtime":"1h"}`)
	if got, want := s.Databases, []string{"gt", "be"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("databases = %v, want %v", got, want)
	}
	if s.KeepDays != 3 {
		t.Errorf("keep_days = %d, want 3", s.KeepDays)
	}
	if s.TriggerBackupBytes != 1<<30 {
		t.Errorf("trigger_backup_bytes = %d, want %d", s.TriggerBackupBytes, int64(1)<<30)
	}
	if s.TriggerBackupDuration != 2*time.Minute {
		t.Errorf("trigger_backup_duration = %v, want 2m", s.TriggerBackupDuration)
	}
	if s.TriggerQueryP95 != 50*time.Millisecond {
		t.Errorf("trigger_query_p95 = %v, want 50ms", s.TriggerQueryP95)
	}
	if s.Retain != 72*time.Hour {
		t.Errorf("retain = %v, want 72h", s.Retain)
	}
	if s.MaxRuntime != time.Hour {
		t.Errorf("max_runtime = %v, want 1h", s.MaxRuntime)
	}
}

// TestSquashAbsentOrDisabledBuildsNothing: an absent section, an absent squash
// block, and enabled false all read as no settings and no error, which is how
// the daemon knows to build no squash step (gt-f1or4z).
func TestSquashAbsentOrDisabledBuildsNothing(t *testing.T) {
	t.Parallel()
	var absentSection *ScheduledMaintenanceConfig
	for _, tt := range []struct {
		name string
		cfg  *ScheduledMaintenanceConfig
	}{
		{"absent section", absentSection},
		{"absent squash block", decodeMaintenance(t, `{"enabled":true}`)},
		{"enabled false", decodeMaintenance(t, `{"squash":{"enabled":false,"databases":["gt"]}}`)},
		{"enabled false with a value out of bounds", decodeMaintenance(t, `{"squash":{"enabled":false,"databases":["gt"],"keep_days":-1}}`)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := tt.cfg.ParseSquash()
			if err != nil {
				t.Fatalf("ParseSquash: %v, want no error", err)
			}
			if s != nil {
				t.Fatalf("ParseSquash = %+v, want nil (no step constructed)", s)
			}
		})
	}
}

// TestSquashRefusesAValueOutOfBounds: every setting refuses a value outside its
// bounds, and the error names the daemon.json key to fix (gt-f1or4z).
func TestSquashRefusesAValueOutOfBounds(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		block string
		key   string
	}{
		{"no database", `{"enabled":true,"databases":[]}`, SquashDatabasesKey},
		{"absent databases", `{"enabled":true}`, SquashDatabasesKey},
		{"blank database", `{"enabled":true,"databases":["gt"," "]}`, SquashDatabasesKey},
		{"duplicate database", `{"enabled":true,"databases":["gt","gt"]}`, SquashDatabasesKey},
		{"keep_days below", `{"enabled":true,"databases":["gt"],"keep_days":-1}`, SquashKeepDaysKey},
		{"keep_days above", `{"enabled":true,"databases":["gt"],"keep_days":366}`, SquashKeepDaysKey},
		{"trigger_backup_bytes negative", `{"enabled":true,"databases":["gt"],"trigger_backup_bytes":-1}`, SquashTriggerBackupBytesKey},
		{"trigger_backup_bytes above", `{"enabled":true,"databases":["gt"],"trigger_backup_bytes":1099511627777}`, SquashTriggerBackupBytesKey},
		{"trigger_backup_duration zero", `{"enabled":true,"databases":["gt"],"trigger_backup_duration":"0s"}`, SquashTriggerBackupDurationKey},
		{"trigger_backup_duration above", `{"enabled":true,"databases":["gt"],"trigger_backup_duration":"25h"}`, SquashTriggerBackupDurationKey},
		{"trigger_backup_duration unparseable", `{"enabled":true,"databases":["gt"],"trigger_backup_duration":"soon"}`, SquashTriggerBackupDurationKey},
		{"trigger_query_p95 zero", `{"enabled":true,"databases":["gt"],"trigger_query_p95":"0s"}`, SquashTriggerQueryP95Key},
		{"trigger_query_p95 above", `{"enabled":true,"databases":["gt"],"trigger_query_p95":"25h"}`, SquashTriggerQueryP95Key},
		{"trigger_query_p95 unparseable", `{"enabled":true,"databases":["gt"],"trigger_query_p95":"fast"}`, SquashTriggerQueryP95Key},
		{"retain below", `{"enabled":true,"databases":["gt"],"retain":"30m"}`, SquashRetainKey},
		{"retain above", `{"enabled":true,"databases":["gt"],"retain":"169h"}`, SquashRetainKey},
		{"retain unparseable", `{"enabled":true,"databases":["gt"],"retain":"two days"}`, SquashRetainKey},
		{"max_runtime below", `{"enabled":true,"databases":["gt"],"max_runtime":"1m"}`, SquashMaxRuntimeKey},
		{"max_runtime above", `{"enabled":true,"databases":["gt"],"max_runtime":"3h"}`, SquashMaxRuntimeKey},
		{"max_runtime unparseable", `{"enabled":true,"databases":["gt"],"max_runtime":"forever"}`, SquashMaxRuntimeKey},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := decodeMaintenance(t, `{"squash":`+tt.block+`}`).ParseSquash()
			if err == nil {
				t.Fatalf("ParseSquash = %+v, want an error naming %s", s, tt.key)
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Errorf("error %q does not name %s", err, tt.key)
			}
			if s != nil {
				t.Errorf("ParseSquash returned %+v beside the error, want nil", s)
			}
		})
	}
}

// TestSquashAcceptsItsBounds: the floor and ceiling of each setting are allowed,
// so a run is refused only outside them and not on them (gt-f1or4z).
func TestSquashAcceptsItsBounds(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		block string
		want  func(*SquashSettings) bool
	}{
		{"keep_days floor", `"keep_days":1`, func(s *SquashSettings) bool { return s.KeepDays == 1 }},
		{"keep_days ceiling", `"keep_days":365`, func(s *SquashSettings) bool { return s.KeepDays == 365 }},
		{"trigger_backup_bytes floor", `"trigger_backup_bytes":1`, func(s *SquashSettings) bool { return s.TriggerBackupBytes == 1 }},
		{"trigger_backup_bytes ceiling", `"trigger_backup_bytes":1099511627776`, func(s *SquashSettings) bool {
			return s.TriggerBackupBytes == 1<<40
		}},
		{"trigger_backup_duration floor", `"trigger_backup_duration":"1ms"`, func(s *SquashSettings) bool {
			return s.TriggerBackupDuration == time.Millisecond
		}},
		{"trigger_backup_duration ceiling", `"trigger_backup_duration":"24h"`, func(s *SquashSettings) bool {
			return s.TriggerBackupDuration == 24*time.Hour
		}},
		{"trigger_query_p95 floor", `"trigger_query_p95":"1ms"`, func(s *SquashSettings) bool {
			return s.TriggerQueryP95 == time.Millisecond
		}},
		{"trigger_query_p95 ceiling", `"trigger_query_p95":"24h"`, func(s *SquashSettings) bool {
			return s.TriggerQueryP95 == 24*time.Hour
		}},
		{"retain floor", `"retain":"1h"`, func(s *SquashSettings) bool { return s.Retain == time.Hour }},
		{"retain ceiling", `"retain":"168h"`, func(s *SquashSettings) bool { return s.Retain == 168*time.Hour }},
		{"max_runtime floor", `"max_runtime":"5m"`, func(s *SquashSettings) bool { return s.MaxRuntime == 5*time.Minute }},
		{"max_runtime ceiling", `"max_runtime":"2h"`, func(s *SquashSettings) bool { return s.MaxRuntime == 2*time.Hour }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := decodeMaintenance(t, `{"squash":{"enabled":true,"databases":["gt"],`+tt.block+`}}`)
			got, err := s.ParseSquash()
			if err != nil {
				t.Fatalf("ParseSquash: %v; the bound itself is allowed", err)
			}
			if !tt.want(got) {
				t.Errorf("ParseSquash = %+v, want the value the bound allows", got)
			}
		})
	}
}

// decodeMaintenance decodes one scheduled_maintenance object through the strict
// decoder the daemon loads daemon.json with, so each test row proves the key
// names an operator writes and not only the Go field names.
func decodeMaintenance(t *testing.T, block string) *ScheduledMaintenanceConfig {
	t.Helper()
	body := `{"patrols":{"scheduled_maintenance":` + block + `}}`
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if cfg.Patrols.ScheduledMaintenance == nil {
		t.Fatalf("scheduled_maintenance decoded as absent from %s", body)
	}
	return cfg.Patrols.ScheduledMaintenance
}

// parseSquashBlock decodes one squash block and returns its settings.
func parseSquashBlock(t *testing.T, block string) *SquashSettings {
	t.Helper()
	s, err := decodeMaintenance(t, `{"squash":`+block+`}`).ParseSquash()
	if err != nil {
		t.Fatalf("ParseSquash(%s): %v", block, err)
	}
	if s == nil {
		t.Fatalf("ParseSquash(%s) = nil, want settings", block)
	}
	return s
}
