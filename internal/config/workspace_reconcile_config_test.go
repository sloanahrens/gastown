package config

import (
	"strings"
	"testing"
	"time"
)

// TestWorkspaceReconcileInterval: the reconcile's cadence is the configured
// interval, the five-minute default when none is set, and a value that does not
// name a positive duration is refused under the setting's own name.
func TestWorkspaceReconcileInterval(t *testing.T) {
	t.Parallel()
	withInterval := func(s string) *DaemonPatrolConfig {
		return &DaemonPatrolConfig{Patrols: &PatrolsConfig{WorkspaceReconcile: &WorkspaceReconcileConfig{IntervalStr: s}}}
	}
	tests := []struct {
		name    string
		cfg     *DaemonPatrolConfig
		want    time.Duration
		wantErr string
	}{
		{name: "nil config takes the default", cfg: nil, want: 5 * time.Minute},
		{name: "no patrols block takes the default", cfg: &DaemonPatrolConfig{}, want: 5 * time.Minute},
		{name: "no reconcile block takes the default", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, want: 5 * time.Minute},
		{name: "empty interval takes the default", cfg: withInterval(""), want: 5 * time.Minute},
		{name: "configured interval is used", cfg: withInterval("30s"), want: 30 * time.Second},
		{name: "unparseable interval is refused", cfg: withInterval("soon"), wantErr: "patrols.workspace_reconcile.interval"},
		{name: "zero interval is refused", cfg: withInterval("0s"), wantErr: "patrols.workspace_reconcile.interval"},
		{name: "negative interval is refused", cfg: withInterval("-1m"), wantErr: "patrols.workspace_reconcile.interval"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.cfg.WorkspaceReconcileInterval()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("WorkspaceReconcileInterval() error = %v, want one naming %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("WorkspaceReconcileInterval() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("WorkspaceReconcileInterval() = %v, want %v", got, tt.want)
			}
		})
	}
}
