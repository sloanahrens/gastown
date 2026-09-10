package daemon

import (
	"encoding/json"
	"testing"
	"time"
)

func TestQuotaDogInterval(t *testing.T) {
	// Default interval
	if got := quotaDogInterval(nil); got != defaultQuotaDogInterval {
		t.Errorf("expected default interval %v, got %v", defaultQuotaDogInterval, got)
	}

	// Custom interval
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			QuotaDog: &QuotaDogConfig{
				Enabled:     true,
				IntervalStr: "2m",
			},
		},
	}
	if got := quotaDogInterval(config); got != 2*time.Minute {
		t.Errorf("expected 2m interval, got %v", got)
	}

	// Invalid interval falls back to default
	config.Patrols.QuotaDog.IntervalStr = "invalid"
	if got := quotaDogInterval(config); got != defaultQuotaDogInterval {
		t.Errorf("expected default interval for invalid config, got %v", got)
	}
}

func TestIsPatrolEnabled_QuotaDog(t *testing.T) {
	// Nil config: disabled (opt-in patrol)
	if IsPatrolEnabled(nil, "quota_dog") {
		t.Error("expected quota_dog to be disabled with nil config")
	}

	// Empty patrols: disabled
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{},
	}
	if IsPatrolEnabled(config, "quota_dog") {
		t.Error("expected quota_dog to be disabled by default")
	}

	// Explicitly enabled
	config.Patrols.QuotaDog = &QuotaDogConfig{Enabled: true}
	if !IsPatrolEnabled(config, "quota_dog") {
		t.Error("expected quota_dog to be enabled when configured")
	}

	// Explicitly disabled
	config.Patrols.QuotaDog = &QuotaDogConfig{Enabled: false}
	if IsPatrolEnabled(config, "quota_dog") {
		t.Error("expected quota_dog to be disabled when explicitly disabled")
	}
}

func TestQuotaDogConfigJSON(t *testing.T) {
	jsonData := `{"enabled": true, "interval": "3m"}`

	var config QuotaDogConfig
	if err := json.Unmarshal([]byte(jsonData), &config); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if !config.Enabled {
		t.Error("expected enabled=true")
	}
	if config.IntervalStr != "3m" {
		t.Errorf("expected interval=3m, got %s", config.IntervalStr)
	}
}

func TestQuotaDogDefaultConstants(t *testing.T) {
	if defaultQuotaDogInterval != 5*time.Minute {
		t.Errorf("expected default interval 5m, got %v", defaultQuotaDogInterval)
	}
	if quotaDogTimeout != 2*time.Minute {
		t.Errorf("expected timeout 2m, got %v", quotaDogTimeout)
	}
}

func TestQuotaResumeInterval(t *testing.T) {
	// Default interval
	if got := quotaResumeInterval(nil); got != defaultQuotaResumeInterval {
		t.Errorf("expected default interval %v, got %v", defaultQuotaResumeInterval, got)
	}

	// Custom interval
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			QuotaResume: &QuotaDogConfig{
				Enabled:     true,
				IntervalStr: "2m",
			},
		},
	}
	if got := quotaResumeInterval(config); got != 2*time.Minute {
		t.Errorf("expected 2m interval, got %v", got)
	}

	// Invalid interval falls back to default
	config.Patrols.QuotaResume.IntervalStr = "invalid"
	if got := quotaResumeInterval(config); got != defaultQuotaResumeInterval {
		t.Errorf("expected default interval for invalid config, got %v", got)
	}
}

// TestIsPatrolEnabled_QuotaResume guards gt-749e's daemon fix: the resume
// nudge must default ON even with zero daemon config, unlike quota_dog
// (which is opt-in and needs an account pool). An explicit config entry
// can still turn it off.
func TestIsPatrolEnabled_QuotaResume(t *testing.T) {
	// Nil config: enabled by default — must work with no daemon.json at all.
	if !IsPatrolEnabled(nil, "quota_resume") {
		t.Error("expected quota_resume to be enabled with nil config")
	}

	// Empty patrols: still enabled by default.
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{},
	}
	if !IsPatrolEnabled(config, "quota_resume") {
		t.Error("expected quota_resume to be enabled by default")
	}

	// Explicitly enabled
	config.Patrols.QuotaResume = &QuotaDogConfig{Enabled: true}
	if !IsPatrolEnabled(config, "quota_resume") {
		t.Error("expected quota_resume to be enabled when configured")
	}

	// Explicitly disabled — the escape hatch still works.
	config.Patrols.QuotaResume = &QuotaDogConfig{Enabled: false}
	if IsPatrolEnabled(config, "quota_resume") {
		t.Error("expected quota_resume to be disabled when explicitly disabled")
	}
}
