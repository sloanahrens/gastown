package doltsquash

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// designThresholds returns the starting limits, so a change to them starts in
// the design: read docs/plans/2026-11-15-dolt-squash-design.md first. The
// values are 5 GiB, 5 minutes and 200 ms.
func designThresholds() Thresholds {
	return Thresholds{
		BackupBytes:    5 << 30,
		BackupDuration: 5 * time.Minute,
		QueryP95:       200 * time.Millisecond,
	}
}

func TestTriggerEvaluation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   Inputs
		want []Trigger
	}{
		{
			name: "no reading trips nothing",
			in:   Inputs{},
		},
		{
			name: "each reading at its limit does not trip",
			in: Inputs{
				BackupBytes:    Reading[int64]{Value: 5 << 30},
				BackupDuration: Reading[time.Duration]{Value: 5 * time.Minute},
				QueryP95:       Reading[time.Duration]{Value: 200 * time.Millisecond},
			},
		},
		{
			name: "backup size over its limit trips it alone",
			in: Inputs{
				BackupBytes:    Reading[int64]{Value: 5<<30 + 1},
				BackupDuration: Reading[time.Duration]{Value: 5 * time.Minute},
				QueryP95:       Reading[time.Duration]{Value: 200 * time.Millisecond},
			},
			want: []Trigger{TriggerBackupBytes},
		},
		{
			name: "backup duration over its limit trips it alone",
			in: Inputs{
				BackupBytes:    Reading[int64]{Value: 5 << 30},
				BackupDuration: Reading[time.Duration]{Value: 5*time.Minute + time.Second},
				QueryP95:       Reading[time.Duration]{Value: 200 * time.Millisecond},
			},
			want: []Trigger{TriggerBackupDuration},
		},
		{
			name: "query p95 over its limit trips it alone",
			in: Inputs{
				BackupBytes:    Reading[int64]{Value: 5 << 30},
				BackupDuration: Reading[time.Duration]{Value: 5 * time.Minute},
				QueryP95:       Reading[time.Duration]{Value: 200*time.Millisecond + time.Microsecond},
			},
			want: []Trigger{TriggerQueryP95},
		},
		{
			name: "every reading over its limit names all three in check order",
			in: Inputs{
				BackupBytes:    Reading[int64]{Value: 6 << 30},
				BackupDuration: Reading[time.Duration]{Value: 6 * time.Minute},
				QueryP95:       Reading[time.Duration]{Value: 300 * time.Millisecond},
			},
			want: []Trigger{TriggerBackupBytes, TriggerBackupDuration, TriggerQueryP95},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Evaluate(tc.in, designThresholds())
			if !slices.Equal(got.TrippedBy, tc.want) {
				t.Errorf("TrippedBy = %v, want %v", got.TrippedBy, tc.want)
			}
			if got.Tripped() != (len(tc.want) > 0) {
				t.Errorf("Tripped() = %v, want %v", got.Tripped(), len(tc.want) > 0)
			}
			if len(got.Unreadable) != 0 {
				t.Errorf("Unreadable = %v, want none", got.Unreadable)
			}
		})
	}
}

func TestTriggerUnreadable(t *testing.T) {
	t.Parallel()
	errRead := errors.New("read failed")
	tests := []struct {
		name       string
		in         Inputs
		wantTrips  []Trigger
		wantUnread []Trigger
	}{
		{
			name: "an unreadable backup size trips nothing even with a value over its limit",
			in: Inputs{
				BackupBytes:    Reading[int64]{Value: 9 << 30, Err: errRead},
				BackupDuration: Reading[time.Duration]{Value: 5 * time.Minute},
				QueryP95:       Reading[time.Duration]{Value: 200 * time.Millisecond},
			},
			wantUnread: []Trigger{TriggerBackupBytes},
		},
		{
			name: "an unreadable backup duration trips nothing even with a value over its limit",
			in: Inputs{
				BackupBytes:    Reading[int64]{Value: 5 << 30},
				BackupDuration: Reading[time.Duration]{Value: 9 * time.Minute, Err: errRead},
				QueryP95:       Reading[time.Duration]{Value: 200 * time.Millisecond},
			},
			wantUnread: []Trigger{TriggerBackupDuration},
		},
		{
			name: "an unreadable query p95 trips nothing even with a value over its limit",
			in: Inputs{
				BackupBytes:    Reading[int64]{Value: 5 << 30},
				BackupDuration: Reading[time.Duration]{Value: 5 * time.Minute},
				QueryP95:       Reading[time.Duration]{Value: 900 * time.Millisecond, Err: errRead},
			},
			wantUnread: []Trigger{TriggerQueryP95},
		},
		{
			name: "every input unreadable trips nothing and names all three",
			in: Inputs{
				BackupBytes:    Reading[int64]{Value: 9 << 30, Err: errRead},
				BackupDuration: Reading[time.Duration]{Value: 9 * time.Minute, Err: errRead},
				QueryP95:       Reading[time.Duration]{Value: 900 * time.Millisecond, Err: errRead},
			},
			wantUnread: []Trigger{TriggerBackupBytes, TriggerBackupDuration, TriggerQueryP95},
		},
		{
			name: "a trip beside an unreadable input is still reported",
			in: Inputs{
				BackupBytes:    Reading[int64]{Value: 5 << 30},
				BackupDuration: Reading[time.Duration]{Value: 6 * time.Minute},
				QueryP95:       Reading[time.Duration]{Value: 900 * time.Millisecond, Err: errRead},
			},
			wantTrips:  []Trigger{TriggerBackupDuration},
			wantUnread: []Trigger{TriggerQueryP95},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Evaluate(tc.in, designThresholds())
			if !slices.Equal(got.TrippedBy, tc.wantTrips) {
				t.Errorf("TrippedBy = %v, want %v", got.TrippedBy, tc.wantTrips)
			}
			if !slices.Equal(got.Unreadable, tc.wantUnread) {
				t.Errorf("Unreadable = %v, want %v", got.Unreadable, tc.wantUnread)
			}
			if got.Tripped() != (len(tc.wantTrips) > 0) {
				t.Errorf("Tripped() = %v, want %v", got.Tripped(), len(tc.wantTrips) > 0)
			}
		})
	}
}
