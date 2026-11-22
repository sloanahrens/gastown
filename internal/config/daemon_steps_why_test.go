package config

import (
	"strings"
	"testing"
)

// TestDaemonStepsHostOnlyRowsSayWhy: the audit's host-side rows the pod
// replaced keep their class and name their replacement or its condition; the
// hm-sync plugin's Why cites the pod run that observed it (gt-lb2fnf).
func TestDaemonStepsHostOnlyRowsSayWhy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, why string
		kind      DaemonStepKind
		class     DaemonStepClass
	}{
		{"hm_sync", "hm-sync plugin runs this in the container", DaemonStepPatrol, DaemonStepHost},
		{"cloud-check", "cloud_check patrol", DaemonStepPlugin, DaemonStepHost},
		{"polecat-image", "a CI job", DaemonStepPlugin, DaemonStepHost},
		{"hm-sync", "hq-wisp-8c6yl, 2026-11-19 23:41, 'exit 0 after 5m44s'", DaemonStepPlugin, DaemonStepRuns},
	}
	for _, tt := range tests {
		var row DaemonStep
		for _, s := range DaemonSteps() {
			if s.Name == tt.name && s.Kind == tt.kind {
				row = s
			}
		}
		if row.Name == "" {
			t.Fatalf("daemonSteps has no %s %s row", tt.name, tt.kind)
		}
		if row.Class != tt.class {
			t.Errorf("%s class = %s, want %s", tt.name, row.Class, tt.class)
		}
		if !strings.Contains(row.Why, tt.why) {
			t.Errorf("%s why = %q, want %q", tt.name, row.Why, tt.why)
		}
	}
}
