package config

import (
	"testing"
	"time"
)

// TestReconcilerConfigDefaults: an absent block and an empty one read the
// design's defaults — shadow on, a five-minute pass, a two-hour grace and the
// four freshness limits the duties shipped with (gt-709ssw).
func TestReconcilerConfigDefaults(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		cfg  *ReconcilerConfig
	}{
		{"nil block", nil},
		{"empty block", &ReconcilerConfig{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !tt.cfg.ParseShadow() {
				t.Error("ParseShadow() = false, want true: the reconciler ships in shadow")
			}
			if got, err := tt.cfg.ParseInterval(); err != nil || got != 5*time.Minute {
				t.Errorf("ParseInterval() = %v, %v; want 5m", got, err)
			}
			if got, err := tt.cfg.ParseGrace(); err != nil || got != 2*time.Hour {
				t.Errorf("ParseGrace() = %v, %v; want 2h", got, err)
			}
			for duty, want := range map[string]time.Duration{
				ReconcilerDutyBackup:    24 * time.Hour,
				ReconcilerDutyArchive:   24 * time.Hour,
				ReconcilerDutyGC:        164 * time.Hour,
				ReconcilerDutyTownState: 24 * time.Hour,
			} {
				if got, err := tt.cfg.ParseLimit(duty); err != nil || got != want {
					t.Errorf("ParseLimit(%q) = %v, %v; want %v", duty, got, err, want)
				}
			}
		})
	}
}
