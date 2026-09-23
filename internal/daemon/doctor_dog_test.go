package daemon

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/slot"
)

func TestDoctorDogInterval(t *testing.T) {
	// Default interval
	if got := doctorDogInterval(nil); got != defaultDoctorDogInterval {
		t.Errorf("expected default interval %v, got %v", defaultDoctorDogInterval, got)
	}

	// Custom interval
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			DoctorDog: &DoctorDogConfig{
				Enabled:     true,
				IntervalStr: "10m",
			},
		},
	}
	if got := doctorDogInterval(config); got != 10*time.Minute {
		t.Errorf("expected 10m interval, got %v", got)
	}

	// Invalid interval falls back to default
	config.Patrols.DoctorDog.IntervalStr = "invalid"
	if got := doctorDogInterval(config); got != defaultDoctorDogInterval {
		t.Errorf("expected default interval for invalid config, got %v", got)
	}
}

func TestDoctorDogDatabases(t *testing.T) {
	// Default databases
	dbs := doctorDogDatabases(nil)
	if len(dbs) != 3 {
		t.Errorf("expected 3 default databases, got %d", len(dbs))
	}

	// Custom databases
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			DoctorDog: &DoctorDogConfig{
				Enabled:   true,
				Databases: []string{"hq", "beads"},
			},
		},
	}
	dbs = doctorDogDatabases(config)
	if len(dbs) != 2 {
		t.Errorf("expected 2 custom databases, got %d", len(dbs))
	}
}

func TestIsPatrolEnabled_DoctorDog(t *testing.T) {
	// Nil config: disabled (opt-in patrol)
	if IsPatrolEnabled(nil, "doctor_dog") {
		t.Error("expected doctor_dog to be disabled with nil config")
	}

	// Empty patrols: disabled
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{},
	}
	if IsPatrolEnabled(config, "doctor_dog") {
		t.Error("expected doctor_dog to be disabled by default")
	}

	// Explicitly enabled
	config.Patrols.DoctorDog = &DoctorDogConfig{Enabled: true}
	if !IsPatrolEnabled(config, "doctor_dog") {
		t.Error("expected doctor_dog to be enabled when configured")
	}

	// Explicitly disabled
	config.Patrols.DoctorDog = &DoctorDogConfig{Enabled: false}
	if IsPatrolEnabled(config, "doctor_dog") {
		t.Error("expected doctor_dog to be disabled when explicitly disabled")
	}
}

func TestDoctorDogDefaultThresholds(t *testing.T) {
	// Verify default thresholds are sane
	if defaultDoctorDogLatencyAlertMs <= 0 {
		t.Error("latency alert threshold must be positive")
	}
	if defaultDoctorDogOrphanAlertCount <= 0 {
		t.Error("orphan alert count must be positive")
	}
	if defaultDoctorDogBackupStaleSeconds <= 0 {
		t.Error("backup stale threshold must be positive")
	}

	// Verify defaults match spec: latency > 5s, orphans > 20, backup > 1hr
	if defaultDoctorDogLatencyAlertMs != 5000.0 {
		t.Errorf("expected latency alert at 5000ms, got %.0f", defaultDoctorDogLatencyAlertMs)
	}
	if defaultDoctorDogOrphanAlertCount != 20 {
		t.Errorf("expected orphan alert at 20, got %d", defaultDoctorDogOrphanAlertCount)
	}
	if defaultDoctorDogBackupStaleSeconds != 3600.0 {
		t.Errorf("expected backup stale at 3600s, got %.0f", defaultDoctorDogBackupStaleSeconds)
	}
}

func TestDoctorDogThresholds(t *testing.T) {
	// Nil config returns defaults
	lat, orphan, backup := doctorDogThresholds(nil)
	if lat != defaultDoctorDogLatencyAlertMs {
		t.Errorf("expected default latency %.0f, got %.0f", defaultDoctorDogLatencyAlertMs, lat)
	}
	if orphan != defaultDoctorDogOrphanAlertCount {
		t.Errorf("expected default orphan %d, got %d", defaultDoctorDogOrphanAlertCount, orphan)
	}
	if backup != defaultDoctorDogBackupStaleSeconds {
		t.Errorf("expected default backup %.0f, got %.0f", defaultDoctorDogBackupStaleSeconds, backup)
	}

	// Custom config overrides
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			DoctorDog: &DoctorDogConfig{
				Enabled:            true,
				LatencyAlertMs:     3000.0,
				OrphanAlertCount:   10,
				BackupStaleSeconds: 1800.0,
			},
		},
	}
	lat, orphan, backup = doctorDogThresholds(config)
	if lat != 3000.0 {
		t.Errorf("expected custom latency 3000, got %.0f", lat)
	}
	if orphan != 10 {
		t.Errorf("expected custom orphan 10, got %d", orphan)
	}
	if backup != 1800.0 {
		t.Errorf("expected custom backup 1800, got %.0f", backup)
	}

	// Partial override: only latency, rest use defaults
	config.Patrols.DoctorDog = &DoctorDogConfig{
		Enabled:        true,
		LatencyAlertMs: 2000.0,
	}
	lat, orphan, backup = doctorDogThresholds(config)
	if lat != 2000.0 {
		t.Errorf("expected custom latency 2000, got %.0f", lat)
	}
	if orphan != defaultDoctorDogOrphanAlertCount {
		t.Errorf("expected default orphan, got %d", orphan)
	}
	if backup != defaultDoctorDogBackupStaleSeconds {
		t.Errorf("expected default backup, got %.0f", backup)
	}
}

func TestDoctorDogConfigBackwardsCompat(t *testing.T) {
	// Verify that configs with the old max_db_count field can still be parsed
	// (JSON decoder ignores unknown fields by default).
	jsonData := `{"enabled": true, "interval": "3m", "max_db_count": 10}`

	var config DoctorDogConfig
	if err := json.Unmarshal([]byte(jsonData), &config); err != nil {
		t.Fatalf("failed to unmarshal config with old max_db_count field: %v", err)
	}

	if !config.Enabled {
		t.Error("expected enabled=true")
	}
	if config.IntervalStr != "3m" {
		t.Errorf("expected interval=3m, got %s", config.IntervalStr)
	}
}

func TestDoctorDogConfigThresholdFields(t *testing.T) {
	// Verify new threshold fields parse from JSON correctly
	jsonData := `{"enabled": true, "latency_alert_ms": 3000, "orphan_alert_count": 15, "backup_stale_seconds": 1800}`

	var config DoctorDogConfig
	if err := json.Unmarshal([]byte(jsonData), &config); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if config.LatencyAlertMs != 3000.0 {
		t.Errorf("expected latency_alert_ms=3000, got %.0f", config.LatencyAlertMs)
	}
	if config.OrphanAlertCount != 15 {
		t.Errorf("expected orphan_alert_count=15, got %d", config.OrphanAlertCount)
	}
	if config.BackupStaleSeconds != 1800.0 {
		t.Errorf("expected backup_stale_seconds=1800, got %.0f", config.BackupStaleSeconds)
	}
}

// healthyDoctorProbes answers every doctor_dog check with a healthy value.
func healthyDoctorProbes() doctorProbes {
	return doctorProbes{
		latency:   func() (time.Duration, error) { return 20 * time.Millisecond, nil },
		conns:     func() (int, int, error) { return 12, 1000, nil },
		databases: func() ([]string, error) { return []string{"hq", "gt"}, nil },
		orphans:   func() (int, error) { return 0, nil },
		backupAge: func(string) (time.Duration, bool) { return 10 * time.Minute, true },
		reap:      func() (slot.ReapReport, error) { return slot.ReapReport{}, nil },
	}
}

func defaultDoctorLimits() doctorLimits {
	return doctorLimits{latency: 5 * time.Second, orphans: 20, backupStale: time.Hour}
}

// The all-clear case is the one the precheck exists for: no finding means no
// molecule and no agent session (claude-l5w).
func TestDoctorDogFindings_AllClear(t *testing.T) {
	r := doctorDogFindings(healthyDoctorProbes(), defaultDoctorLimits())
	if len(r.findings) != 0 {
		t.Fatalf("healthy probes produced findings: %v", r.findings)
	}
	if r.latency != 20*time.Millisecond || r.conns != 12 || r.connMax != 1000 {
		t.Errorf("report values not carried: %+v", r)
	}
}

func TestDoctorDogFindings_EachCheckTrips(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*doctorProbes)
		want string
	}{
		{"unreachable", func(p *doctorProbes) {
			p.latency = func() (time.Duration, error) { return 0, errors.New("connection refused") }
		}, "unreachable"},
		{"slow", func(p *doctorProbes) {
			p.latency = func() (time.Duration, error) { return 6 * time.Second, nil }
		}, "latency"},
		{"connections", func(p *doctorProbes) {
			p.conns = func() (int, int, error) { return 850, 1000, nil }
		}, "connections"},
		{"orphans", func(p *doctorProbes) {
			p.orphans = func() (int, error) { return 21, nil }
		}, "orphan"},
		{"stale backup", func(p *doctorProbes) {
			p.backupAge = func(db string) (time.Duration, bool) {
				if db == "gt" {
					return 3 * time.Hour, true
				}
				return time.Minute, true
			}
		}, "backup gt"},
		{"reap removal failed", func(p *doctorProbes) {
			p.reap = func() (slot.ReapReport, error) {
				return slot.ReapReport{Failed: []string{"gt-gate-1: permission denied"}}, nil
			}
		}, "reap"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := healthyDoctorProbes()
			tc.mut(&p)
			r := doctorDogFindings(p, defaultDoctorLimits())
			if len(r.findings) != 1 || !strings.Contains(r.findings[0], tc.want) {
				t.Fatalf("want one finding containing %q, got %v", tc.want, r.findings)
			}
		})
	}
}

// .dolt-backup keeps directories for databases the server no longer serves
// (on the live town: forkrig, pt0, testrig — untouched for weeks). Judging
// those stale would pour a molecule on every run forever.
func TestDoctorDogFindings_BackupOnlyForServedDatabases(t *testing.T) {
	p := healthyDoctorProbes()
	var asked []string
	p.backupAge = func(db string) (time.Duration, bool) {
		asked = append(asked, db)
		return time.Minute, true
	}
	doctorDogFindings(p, defaultDoctorLimits())
	if strings.Join(asked, ",") != "hq,gt" {
		t.Errorf("backup ages checked for %v, want exactly the served databases [hq gt]", asked)
	}

	// A served database with no backup directory is not a finding: the backup
	// patrol may not cover it, which is configuration, not an outage.
	p.backupAge = func(string) (time.Duration, bool) { return 0, false }
	if r := doctorDogFindings(p, defaultDoctorLimits()); len(r.findings) != 0 {
		t.Errorf("missing backup dirs produced findings: %v", r.findings)
	}
}

// An unreachable server makes every server-side check meaningless; report the
// outage alone rather than a pile of consequential failures.
func TestDoctorDogFindings_UnreachableSkipsServerChecks(t *testing.T) {
	p := healthyDoctorProbes()
	p.latency = func() (time.Duration, error) { return 0, errors.New("dial tcp: connection refused") }
	p.conns = func() (int, int, error) { t.Error("conns probed on an unreachable server"); return 0, 0, nil }
	p.databases = func() ([]string, error) { t.Error("databases probed on an unreachable server"); return nil, nil }
	r := doctorDogFindings(p, defaultDoctorLimits())
	if len(r.findings) != 1 {
		t.Errorf("want only the unreachable finding, got %v", r.findings)
	}
}

// A docker that cannot be listed (not installed, Docker Desktop stopped) is
// nothing an agent can fix from a dog session; it is logged, not poured. A
// removal that failed on a container classified as debris is.
func TestDoctorDogFindings_ReapListingErrorIsANoteNotAFinding(t *testing.T) {
	p := healthyDoctorProbes()
	p.reap = func() (slot.ReapReport, error) {
		return slot.ReapReport{}, errors.New("listing gate containers: exec: \"docker\": executable file not found")
	}
	r := doctorDogFindings(p, defaultDoctorLimits())
	if len(r.findings) != 0 {
		t.Errorf("docker listing error produced findings: %v", r.findings)
	}
	if len(r.notes) != 1 || !strings.Contains(r.notes[0], "docker") {
		t.Errorf("docker listing error not noted: %v", r.notes)
	}

	p.reap = func() (slot.ReapReport, error) {
		return slot.ReapReport{Removed: []string{"gt-gate-7 (3h)"}}, nil
	}
	r = doctorDogFindings(p, defaultDoctorLimits())
	if len(r.findings) != 0 || len(r.notes) != 1 || !strings.Contains(r.notes[0], "gt-gate-7") {
		t.Errorf("successful reap should be a note: findings=%v notes=%v", r.findings, r.notes)
	}
}
