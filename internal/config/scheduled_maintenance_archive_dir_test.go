package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestScheduledMaintenanceArchiveDir: archive_dir survives strict decoding into
// the field the daemon reads, parses to a clean absolute path, and is empty when
// unset. A relative path is refused rather than resolved against the daemon's
// working directory, which is not the operator's (gt-7nxj4).
func TestScheduledMaintenanceArchiveDir(t *testing.T) {
	t.Parallel()
	if MaintenanceArchiveDirKey != "patrols.scheduled_maintenance.archive_dir" {
		t.Fatalf("setting key = %q, want the daemon.json path the field decodes from", MaintenanceArchiveDirKey)
	}
	body := `{"patrols":{"scheduled_maintenance":{"enabled":true,"window":"03:00","archive_dir":"/home/op/gt-archive"}}}`
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Patrols.ScheduledMaintenance == nil {
		t.Fatal("scheduled_maintenance decoded as absent")
	}
	got, err := cfg.Patrols.ScheduledMaintenance.ParseArchiveDir()
	if err != nil {
		t.Fatalf("ParseArchiveDir: %v", err)
	}
	if want := "/home/op/gt-archive"; got != want {
		t.Errorf("archive_dir = %q, want %q", got, want)
	}

	// Unset means archiving is off, and an absent section reads the same way.
	if got, err := (&ScheduledMaintenanceConfig{}).ParseArchiveDir(); got != "" || err != nil {
		t.Errorf("unset archive_dir = %q, %v; want empty, nil", got, err)
	}
	var absent *ScheduledMaintenanceConfig
	if got, err := absent.ParseArchiveDir(); got != "" || err != nil {
		t.Errorf("absent scheduled_maintenance section = %q, %v; want empty, nil", got, err)
	}

	tests := []struct {
		name    string
		dir     string
		want    string
		wantErr bool
	}{
		{name: "absolute", dir: "/srv/gt-archive", want: "/srv/gt-archive"},
		{name: "cleaned", dir: "/home/op/../op/gt-archive", want: "/home/op/gt-archive"},
		{name: "trimmed", dir: "  /srv/gt-archive  ", want: "/srv/gt-archive"},
		{name: "relative", dir: "archive/dolt", wantErr: true},
		{name: "tilde is not absolute", dir: "~/gt-archive", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &ScheduledMaintenanceConfig{ArchiveDir: tt.dir}
			got, err := c.ParseArchiveDir()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseArchiveDir(%q) = %q, want an error", tt.dir, got)
				}
				if want := fmt.Sprintf("%s = %q: want an absolute path", MaintenanceArchiveDirKey, tt.dir); err.Error() != want {
					t.Errorf("error = %q, want %q", err, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseArchiveDir(%q): %v", tt.dir, err)
			}
			if got != tt.want {
				t.Errorf("ParseArchiveDir(%q) = %q, want %q", tt.dir, got, tt.want)
			}
		})
	}
}

// TestScheduledMaintenanceArchiveLimitsDecodeAndDefault: both batch bounds
// survive strict decoding into the fields the daemon reads, and an absent
// section or zero setting reads the 5000-row default (gt-s6srv).
func TestScheduledMaintenanceArchiveLimitsDecodeAndDefault(t *testing.T) {
	t.Parallel()
	if DefaultArchiveCommitLimit != 5000 || DefaultArchiveEventsLimit != 5000 {
		t.Fatalf("defaults = %d/%d, want 5000/5000 (an unset config must behave as before the settings existed)",
			DefaultArchiveCommitLimit, DefaultArchiveEventsLimit)
	}
	body := `{"patrols":{"scheduled_maintenance":{"enabled":true,"archive_commit_limit":3,"archive_events_limit":4}}}`
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sm := cfg.Patrols.ScheduledMaintenance
	if sm == nil {
		t.Fatal("scheduled_maintenance decoded as absent")
	}
	if got, err := sm.ParseArchiveCommitLimit(); err != nil || got != 3 {
		t.Errorf("ParseArchiveCommitLimit = %d, %v; want 3, nil (the decoded setting)", got, err)
	}
	if got, err := sm.ParseArchiveEventsLimit(); err != nil || got != 4 {
		t.Errorf("ParseArchiveEventsLimit = %d, %v; want 4, nil (the decoded setting)", got, err)
	}

	zero := 0
	for _, tt := range []struct {
		name string
		cfg  *ScheduledMaintenanceConfig
	}{
		{name: "absent section", cfg: nil},
		{name: "neither setting", cfg: &ScheduledMaintenanceConfig{}},
		{name: "zero", cfg: &ScheduledMaintenanceConfig{ArchiveCommitLimit: &zero, ArchiveEventsLimit: &zero}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got, err := tt.cfg.ParseArchiveCommitLimit(); err != nil || got != DefaultArchiveCommitLimit {
				t.Errorf("commit limit = %d, %v; want the %d default", got, err, DefaultArchiveCommitLimit)
			}
			if got, err := tt.cfg.ParseArchiveEventsLimit(); err != nil || got != DefaultArchiveEventsLimit {
				t.Errorf("events limit = %d, %v; want the %d default", got, err, DefaultArchiveEventsLimit)
			}
		})
	}
}

// TestScheduledMaintenanceArchiveBudget: archive_budget survives strict
// decoding into the field the daemon reads; an empty setting or an absent
// section reads the 10-minute default; and a value below a minute, above the
// step's 30-minute timeout, or one that does not parse is refused with the key
// named (gt-o4sk8.1).
func TestScheduledMaintenanceArchiveBudget(t *testing.T) {
	t.Parallel()
	if MaintenanceArchiveBudgetKey != "patrols.scheduled_maintenance.archive_budget" {
		t.Fatalf("setting key = %q, want the daemon.json path the field decodes from", MaintenanceArchiveBudgetKey)
	}
	if DefaultArchiveBudget != 10*time.Minute {
		t.Fatalf("default budget = %v, want 10m", DefaultArchiveBudget)
	}
	body := `{"patrols":{"scheduled_maintenance":{"enabled":true,"archive_budget":"12m"}}}`
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Patrols.ScheduledMaintenance == nil {
		t.Fatal("scheduled_maintenance decoded as absent")
	}
	if got, err := cfg.Patrols.ScheduledMaintenance.ParseArchiveBudget(); err != nil || got != 12*time.Minute {
		t.Errorf("decoded archive_budget = %v, %v; want 12m", got, err)
	}

	for _, tt := range []struct {
		name string
		cfg  *ScheduledMaintenanceConfig
	}{
		{name: "absent section", cfg: nil},
		{name: "empty setting", cfg: &ScheduledMaintenanceConfig{}},
	} {
		if got, err := tt.cfg.ParseArchiveBudget(); err != nil || got != DefaultArchiveBudget {
			t.Errorf("%s budget = %v, %v; want the %v default", tt.name, got, err, DefaultArchiveBudget)
		}
	}

	for _, setting := range []string{"30s", "45m", "abc"} {
		got, err := (&ScheduledMaintenanceConfig{ArchiveBudget: setting}).ParseArchiveBudget()
		if err == nil {
			t.Fatalf("ParseArchiveBudget(%q) = %v, want an error", setting, got)
		}
		if !strings.Contains(err.Error(), MaintenanceArchiveBudgetKey) {
			t.Errorf("error %q does not name %s", err, MaintenanceArchiveBudgetKey)
		}
	}
}

// TestArchiveLimitSettingsRejectBadValues: a negative bound and one above the
// maximum are refused for both settings, and the error names the daemon.json
// key to fix (gt-s6srv).
func TestArchiveLimitSettingsRejectBadValues(t *testing.T) {
	t.Parallel()
	for _, n := range []int{-1, MaxArchiveLimit + 1} {
		n := n
		t.Run(fmt.Sprintf("commit=%d", n), func(t *testing.T) {
			t.Parallel()
			c := &ScheduledMaintenanceConfig{ArchiveCommitLimit: &n}
			got, err := c.ParseArchiveCommitLimit()
			if err == nil {
				t.Fatalf("ParseArchiveCommitLimit(%d) = %d, want an error", n, got)
			}
			if !strings.Contains(err.Error(), MaintenanceArchiveCommitLimitKey) {
				t.Errorf("error %q does not name %s", err, MaintenanceArchiveCommitLimitKey)
			}
		})
		t.Run(fmt.Sprintf("events=%d", n), func(t *testing.T) {
			t.Parallel()
			c := &ScheduledMaintenanceConfig{ArchiveEventsLimit: &n}
			got, err := c.ParseArchiveEventsLimit()
			if err == nil {
				t.Fatalf("ParseArchiveEventsLimit(%d) = %d, want an error", n, got)
			}
			if !strings.Contains(err.Error(), MaintenanceArchiveEventsLimitKey) {
				t.Errorf("error %q does not name %s", err, MaintenanceArchiveEventsLimitKey)
			}
		})
	}
}
