package dashboard

import (
	"io/fs"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The series is the status line's file, and in the daemon pod it is the Mac's
// ~/.claude mounted read-only, so a trim the mount refuses is that writer's
// state and not a failure of the poll. Any other trim failure still leaves the
// week drawn and is reported (gt-wndup3).
func TestClaudePlanTrimOnAReadOnlySeries(t *testing.T) {
	t.Parallel()

	cutoff := claudePlanNow.Add(-claudePlanRetentionDays * 24 * time.Hour)
	resets := planDate(2026, time.October, 19).Unix()
	rows := []struct {
		name       string
		trimErr    error
		wantReport bool
	}{
		{
			name:    "a read-only file system belongs to the series' writer",
			trimErr: &fs.PathError{Op: "rename", Path: "samples.jsonl", Err: syscall.EROFS},
		},
		{
			name:       "any other trim failure is reported",
			trimErr:    &fs.PathError{Op: "rename", Path: "samples.jsonl", Err: syscall.ENOSPC},
			wantReport: true,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			path := claudePlanFile(t,
				planLine(cutoff.Add(-(claudePlanTrimSlackDays+1)*24*time.Hour), 5, 10, resets),
				planLine(claudePlanNow, 5, 38, resets),
			)
			r := NewClaudePlanReader(path, 200)
			r.beforeTrimRewrite = func() error { return row.trimErr }

			p := r.Read(claudePlanNow)

			if row.wantReport {
				if !strings.Contains(p.StateError, "series trim") {
					t.Errorf("state_error = %q, want the trim failure reported", p.StateError)
				}
			} else if p.StateError != "" {
				t.Errorf("state_error = %q, want a read-only trim unreported", p.StateError)
			}
			if p.WeeklyPct != 38 {
				t.Errorf("weekly_pct = %v, want the week drawn from the series at 38", p.WeeklyPct)
			}
		})
	}
}
