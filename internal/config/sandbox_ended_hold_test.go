package config

import (
	"strings"
	"testing"
)

// patrolWithInterval is a daemon patrol config whose patrol_scan tick runs at
// interval; "" leaves the interval unset.
func patrolWithInterval(interval string) *DaemonPatrolConfig {
	return &DaemonPatrolConfig{Patrols: &PatrolsConfig{PatrolScan: &PatrolScanConfig{Enabled: true, IntervalStr: interval}}}
}

// holdOf is a pointer to an explicit ended_hold_seconds value.
func holdOf(seconds int) *int { return &seconds }

// The hold must be at least three patrol_scan intervals. A set value below that
// is refused and the error names the value and the minimum; the default is
// checked the same way when the value is unset.
func TestSandboxEndedHoldValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		hold    *int
		daemon  *DaemonPatrolConfig
		wantErr []string // substrings the error must carry; nil means valid
	}{
		{
			name:    "a set hold below three intervals is refused and names both numbers",
			hold:    holdOf(300),
			daemon:  patrolWithInterval("10m"),
			wantErr: []string{"ended_hold_seconds 300", "want at least 1800", "3 times patrols.patrol_scan.interval 10m0s"},
		},
		{
			name:   "a hold equal to three intervals passes",
			hold:   holdOf(1800),
			daemon: patrolWithInterval("10m"),
		},
		{
			name:   "a hold above three intervals passes",
			hold:   holdOf(7200),
			daemon: patrolWithInterval("10m"),
		},
		{
			name:   "an unset hold takes the default, which passes at the town's 10m tick",
			hold:   nil,
			daemon: patrolWithInterval("10m"),
		},
		{
			name:   "an unset hold passes with no patrol_scan block, which runs at 2m",
			hold:   nil,
			daemon: &DaemonPatrolConfig{},
		},
		{
			name:    "an unset hold is refused when the default is below three intervals, naming both",
			hold:    nil,
			daemon:  patrolWithInterval("11m"),
			wantErr: []string{"ended_hold_seconds 1800", "want at least 1980", "3 times patrols.patrol_scan.interval 11m0s"},
		},
		{
			name:    "an explicit zero is refused, not read as unset",
			hold:    holdOf(0),
			daemon:  patrolWithInterval("10m"),
			wantErr: []string{"ended_hold_seconds 0", "want at least 1800"},
		},
		{
			name:    "a negative hold is refused",
			hold:    holdOf(-60),
			daemon:  patrolWithInterval("10m"),
			wantErr: []string{"ended_hold_seconds -60"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts := &TownSettings{Sandboxes: map[string]SandboxProfile{
				"polecat-default": {Image: "img", Policy: "p.yaml", Gateway: "k8s", EndedHoldSeconds: tt.hold},
			}}
			err := ValidateSandboxEndedHolds(ts, tt.daemon)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateSandboxEndedHolds = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateSandboxEndedHolds = nil, want an error carrying %q", tt.wantErr)
			}
			for _, sub := range tt.wantErr {
				if !strings.Contains(err.Error(), sub) {
					t.Fatalf("error %q does not carry %q", err, sub)
				}
			}
		})
	}
}

// A nil settings file has no sandboxes to check, so the load passes it.
func TestSandboxEndedHoldNoSettingsPasses(t *testing.T) {
	t.Parallel()
	if err := ValidateSandboxEndedHolds(nil, patrolWithInterval("10m")); err != nil {
		t.Fatalf("ValidateSandboxEndedHolds(nil) = %v, want nil", err)
	}
}
