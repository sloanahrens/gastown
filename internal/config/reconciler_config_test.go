package config

import (
	"strings"
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

// TestReconcilerConfigRefusesBadValues: a value the block cannot use is refused
// rather than clamped, and each error names the entry to fix. These rows belong
// to gt-a4jff0 (part b); they land here because the presubmit refuses an added
// error branch that no test drives (gt-709ssw). A Value such as 100d does not
// parse as a Go duration, so the ceiling row uses 1000h to reach the range
// branch.
func TestReconcilerConfigRefusesBadValues(t *testing.T) {
	t.Parallel()
	cfg := &ReconcilerConfig{Interval: "30s", BackupLimit: "10m", ArchiveLimit: "1000h"}
	tests := []struct {
		name string
		want string
		call func() (time.Duration, error)
	}{
		{"non-duration interval", ReconcilerIntervalKey, (&ReconcilerConfig{Interval: "soon"}).ParseInterval},
		{"interval under the floor", ReconcilerIntervalKey, cfg.ParseInterval},
		{"limit under the floor", ReconcilerBackupLimitKey, func() (time.Duration, error) {
			return cfg.ParseLimit(ReconcilerDutyBackup)
		}},
		{"limit over the ceiling", ReconcilerArchiveLimitKey, func() (time.Duration, error) {
			return cfg.ParseLimit(ReconcilerDutyArchive)
		}},
		{"unknown duty", "bogus", func() (time.Duration, error) {
			return cfg.ParseLimit("bogus")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tt.call(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want one naming %s", err, tt.want)
			}
		})
	}
}
