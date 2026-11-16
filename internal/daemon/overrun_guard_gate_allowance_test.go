package daemon

import (
	"testing"
	"time"
)

// gateAllowanceConfig is a daemon patrol config whose overrun guard sets
// gate_allowance to s.
func gateAllowanceConfig(s string) *DaemonPatrolConfig {
	return &DaemonPatrolConfig{Patrols: &PatrolsConfig{OverrunGuard: &OverrunGuardConfig{GateAllowance: s}}}
}

// An unset, empty or unusable gate_allowance gives twice the stop threshold —
// and follows the threshold it was given, not a fixed 40m. A set one gives
// itself; "0" gives zero.
func TestOverrunGuardGateAllowanceDefaultAndConfigured(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		cfg       *DaemonPatrolConfig
		stopAfter time.Duration
		want      time.Duration
	}{
		{"nil config", nil, 20 * time.Minute, 40 * time.Minute},
		{"nil patrols", &DaemonPatrolConfig{}, 20 * time.Minute, 40 * time.Minute},
		{"overrun guard absent", &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, 20 * time.Minute, 40 * time.Minute},
		{"unset", gateAllowanceConfig(""), 20 * time.Minute, 40 * time.Minute},
		{"unset follows a raised stop threshold", gateAllowanceConfig(""), 30 * time.Minute, 60 * time.Minute},
		{"unset follows a lowered stop threshold", gateAllowanceConfig(""), 10 * time.Minute, 20 * time.Minute},
		{"unparseable falls back", gateAllowanceConfig("five minutes"), 20 * time.Minute, 40 * time.Minute},
		{"a number with no unit falls back", gateAllowanceConfig("5"), 20 * time.Minute, 40 * time.Minute},
		{"negative falls back", gateAllowanceConfig("-5m"), 20 * time.Minute, 40 * time.Minute},
		{"five minutes", gateAllowanceConfig("5m"), 20 * time.Minute, 5 * time.Minute},
		{"beyond the default ceiling", gateAllowanceConfig("45m"), 20 * time.Minute, 45 * time.Minute},
		{"zero banks no gate time", gateAllowanceConfig("0"), 20 * time.Minute, 0},
		{"zero seconds banks no gate time", gateAllowanceConfig("0s"), 20 * time.Minute, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := overrunGateAllowance(tt.cfg, tt.stopAfter); got != tt.want {
				t.Errorf("overrunGateAllowance = %v, want %v", got, tt.want)
			}
		})
	}
}
