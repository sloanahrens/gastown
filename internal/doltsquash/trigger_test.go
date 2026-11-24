package doltsquash

import (
	"errors"
	"fmt"
	"slices"
	"strings"
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
			name: "readings read as zero trip nothing",
			in: Inputs{
				BackupBytes:    Read[int64](0),
				BackupDuration: Read(time.Duration(0)),
				QueryP95:       Read(time.Duration(0)),
			},
		},
		{
			name: "each reading at its limit does not trip",
			in: Inputs{
				BackupBytes:    Read[int64](5 << 30),
				BackupDuration: Read(5 * time.Minute),
				QueryP95:       Read(200 * time.Millisecond),
			},
		},
		{
			name: "each reading below its limit does not trip",
			in: Inputs{
				BackupBytes:    Read[int64](4 << 30),
				BackupDuration: Read(4 * time.Minute),
				QueryP95:       Read(150 * time.Millisecond),
			},
		},
		{
			name: "backup size over its limit trips it alone",
			in: Inputs{
				BackupBytes:    Read[int64](5<<30 + 1),
				BackupDuration: Read(5 * time.Minute),
				QueryP95:       Read(200 * time.Millisecond),
			},
			want: []Trigger{TriggerBackupBytes},
		},
		{
			name: "backup duration over its limit trips it alone",
			in: Inputs{
				BackupBytes:    Read[int64](5 << 30),
				BackupDuration: Read(5*time.Minute + time.Second),
				QueryP95:       Read(200 * time.Millisecond),
			},
			want: []Trigger{TriggerBackupDuration},
		},
		{
			name: "query p95 over its limit trips it alone",
			in: Inputs{
				BackupBytes:    Read[int64](5 << 30),
				BackupDuration: Read(5 * time.Minute),
				QueryP95:       Read(200*time.Millisecond + time.Microsecond),
			},
			want: []Trigger{TriggerQueryP95},
		},
		{
			name: "every reading over its limit names all three in check order",
			in: Inputs{
				BackupBytes:    Read[int64](6 << 30),
				BackupDuration: Read(6 * time.Minute),
				QueryP95:       Read(300 * time.Millisecond),
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
	// readWithErr returns a Reading Read built that the caller then marked
	// failed.
	readWithErr := func(v int64) Reading[int64] {
		r := Read(v)
		r.Err = errRead
		return r
	}
	tests := []struct {
		name       string
		in         Inputs
		wantTrips  []Trigger
		wantUnread []Trigger
	}{
		{
			name:       "no input read names all three and trips nothing",
			in:         Inputs{},
			wantUnread: []Trigger{TriggerBackupBytes, TriggerBackupDuration, TriggerQueryP95},
		},
		{
			name: "an unread backup size trips nothing even with a value over its limit",
			in: Inputs{
				BackupBytes:    Reading[int64]{Value: 9 << 30},
				BackupDuration: Read(5 * time.Minute),
				QueryP95:       Read(200 * time.Millisecond),
			},
			wantUnread: []Trigger{TriggerBackupBytes},
		},
		{
			name: "an unread backup duration trips nothing even with a value over its limit",
			in: Inputs{
				BackupBytes:    Read[int64](5 << 30),
				BackupDuration: Reading[time.Duration]{Value: 9 * time.Minute},
				QueryP95:       Read(200 * time.Millisecond),
			},
			wantUnread: []Trigger{TriggerBackupDuration},
		},
		{
			name: "an unread query p95 trips nothing even with a value over its limit",
			in: Inputs{
				BackupBytes:    Read[int64](5 << 30),
				BackupDuration: Read(5 * time.Minute),
				QueryP95:       Reading[time.Duration]{Value: 900 * time.Millisecond},
			},
			wantUnread: []Trigger{TriggerQueryP95},
		},
		{
			name: "every input unread with a value over its limit names all three",
			in: Inputs{
				BackupBytes:    Reading[int64]{Value: 9 << 30},
				BackupDuration: Reading[time.Duration]{Value: 9 * time.Minute},
				QueryP95:       Reading[time.Duration]{Value: 900 * time.Millisecond},
			},
			wantUnread: []Trigger{TriggerBackupBytes, TriggerBackupDuration, TriggerQueryP95},
		},
		{
			name: "a failed read is unreadable",
			in: Inputs{
				BackupBytes:    Failed[int64](errRead),
				BackupDuration: Read(5 * time.Minute),
				QueryP95:       Read(200 * time.Millisecond),
			},
			wantUnread: []Trigger{TriggerBackupBytes},
		},
		{
			name: "a read value marked failed is unreadable even over its limit",
			in: Inputs{
				BackupBytes:    readWithErr(9 << 30),
				BackupDuration: Read(5 * time.Minute),
				QueryP95:       Read(200 * time.Millisecond),
			},
			wantUnread: []Trigger{TriggerBackupBytes},
		},
		{
			name: "a failed read with a stale value over its limit trips nothing",
			in: Inputs{
				BackupBytes:    Reading[int64]{Value: 9 << 30, Err: errRead},
				BackupDuration: Read(5 * time.Minute),
				QueryP95:       Read(200 * time.Millisecond),
			},
			wantUnread: []Trigger{TriggerBackupBytes},
		},
		{
			name: "a trip beside an unreadable input is still reported",
			in: Inputs{
				BackupBytes:    Read[int64](5 << 30),
				BackupDuration: Read(6 * time.Minute),
				QueryP95:       Failed[time.Duration](errRead),
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

// backupBytesTrips returns readings in which the backup-size threshold alone is
// over its limit.
func backupBytesTrips() Inputs {
	return Inputs{
		BackupBytes:    Read[int64](6 << 30),
		BackupDuration: Read(5 * time.Minute),
		QueryP95:       Read(200 * time.Millisecond),
	}
}

// designSettings returns the plan's starting settings: the design thresholds
// and the seven-day keep window of docs/plans/2026-11-15-dolt-squash-design.md.
func designSettings() Settings {
	return Settings{Thresholds: designThresholds(), KeepDays: 7}
}

func TestCutInstant(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		now      time.Time
		keepDays int
		want     time.Time
	}{
		{
			name:     "a seven-day window reaches back seven days",
			now:      time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC),
			keepDays: 7,
			want:     time.Date(2026, 11, 8, 20, 0, 0, 0, time.UTC),
		},
		{
			name:     "a one-day window reaches back one day",
			now:      time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC),
			keepDays: 1,
			want:     time.Date(2026, 11, 14, 20, 0, 0, 0, time.UTC),
		},
		{
			name:     "the window crosses a month boundary",
			now:      time.Date(2026, 3, 3, 6, 30, 0, 0, time.UTC),
			keepDays: 7,
			want:     time.Date(2026, 2, 24, 6, 30, 0, 0, time.UTC),
		},
		{
			name:     "the cut keeps the check clock's own zone",
			now:      time.Date(2026, 11, 15, 20, 0, 0, 0, time.FixedZone("town", -6*60*60)),
			keepDays: 7,
			want:     time.Date(2026, 11, 8, 20, 0, 0, 0, time.FixedZone("town", -6*60*60)),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := designSettings()
			cfg.KeepDays = tc.keepDays
			got, err := Decide(tc.now, time.Time{}, backupBytesTrips(), cfg)
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if !got.Cut.Equal(tc.want) {
				t.Errorf("Cut = %s, want %s", got.Cut, tc.want)
			}
			// Equal compares instants only, so a cut moved into another zone
			// of the same instant still passes it.
			gotZone, gotOffset := got.Cut.Zone()
			wantZone, wantOffset := tc.want.Zone()
			if gotZone != wantZone || gotOffset != wantOffset {
				t.Errorf("Cut zone = %s (%ds), want %s (%ds)", gotZone, gotOffset, wantZone, wantOffset)
			}
		})
	}
}

func TestCutNotDueWithin24Hours(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC)
	errRead := errors.New("read failed")
	trippedAndUnreadable := backupBytesTrips()
	trippedAndUnreadable.QueryP95 = Failed[time.Duration](errRead)
	tests := []struct {
		name       string
		in         Inputs
		lastRun    time.Time
		wantDue    bool
		wantReason string
	}{
		{
			name:       "never run and a threshold tripped is due",
			in:         backupBytesTrips(),
			wantDue:    true,
			wantReason: "due: backup bytes over the limit",
		},
		{
			name:       "a run 25 hours ago does not hold a tripped check back",
			in:         backupBytesTrips(),
			lastRun:    now.Add(-25 * time.Hour),
			wantDue:    true,
			wantReason: "due: backup bytes over the limit",
		},
		{
			name:       "a run exactly 24 hours ago is outside the gate",
			in:         backupBytesTrips(),
			lastRun:    now.Add(-RunInterval),
			wantDue:    true,
			wantReason: "due: backup bytes over the limit",
		},
		{
			name:       "a run one hour ago holds a tripped check back",
			in:         backupBytesTrips(),
			lastRun:    now.Add(-time.Hour),
			wantDue:    false,
			wantReason: "tripped (backup bytes), but the last run started 1h0m0s ago, inside the 24h gate",
		},
		{
			name:       "a run 23h59m ago is still inside the gate",
			in:         backupBytesTrips(),
			lastRun:    now.Add(-(23*time.Hour + 59*time.Minute)),
			wantDue:    false,
			wantReason: "tripped (backup bytes), but the last run started 23h59m0s ago, inside the 24h gate",
		},
		{
			name:       "a run 30 seconds ahead counts as now, naming no negative age",
			in:         backupBytesTrips(),
			lastRun:    now.Add(30 * time.Second),
			wantDue:    false,
			wantReason: "tripped (backup bytes), but the last run started 0s ago, inside the 24h gate",
		},
		{
			// The largest lead the skew accepts; without the clamp its age
			// truncates to a negative -1m0s in the reason.
			name:       "a run exactly a minute ahead counts as now, naming no negative age",
			in:         backupBytesTrips(),
			lastRun:    now.Add(time.Minute),
			wantDue:    false,
			wantReason: "tripped (backup bytes), but the last run started 0s ago, inside the 24h gate",
		},
		{
			name: "nothing tripped and never run is not due",
			in: Inputs{
				BackupBytes:    Read[int64](5 << 30),
				BackupDuration: Read(5 * time.Minute),
				QueryP95:       Read(200 * time.Millisecond),
			},
			wantDue:    false,
			wantReason: "no threshold tripped",
		},
		{
			name: "nothing tripped with a run an hour ago names no threshold",
			in: Inputs{
				BackupBytes:    Read[int64](5 << 30),
				BackupDuration: Read(5 * time.Minute),
				QueryP95:       Read(200 * time.Millisecond),
			},
			lastRun:    now.Add(-time.Hour),
			wantDue:    false,
			wantReason: "no threshold tripped",
		},
		{
			name: "an unreadable input is reported with no trip",
			in: Inputs{
				BackupBytes:    Read[int64](5 << 30),
				BackupDuration: Read(5 * time.Minute),
				QueryP95:       Failed[time.Duration](errRead),
			},
			wantDue:    false,
			wantReason: "no threshold tripped; unreadable: query p95",
		},
		{
			name:       "an unreadable input is reported on a due check",
			in:         trippedAndUnreadable,
			wantDue:    true,
			wantReason: "due: backup bytes over the limit; unreadable: query p95",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Decide(now, tc.lastRun, tc.in, designSettings())
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if got.Due != tc.wantDue {
				t.Errorf("Due = %v, want %v (reason %q)", got.Due, tc.wantDue, got.Reason)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
		})
	}
}

func TestCutRefusesNonPositiveKeepDays(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC)
	for _, keepDays := range []int{0, -1} {
		t.Run(fmt.Sprintf("keep_days %d", keepDays), func(t *testing.T) {
			t.Parallel()
			cfg := designSettings()
			cfg.KeepDays = keepDays
			got, err := Decide(now, time.Time{}, backupBytesTrips(), cfg)
			if err == nil {
				t.Fatal("Decide accepted keep_days below 1")
			}
			if !strings.Contains(err.Error(), "keep_days") {
				t.Errorf("error %q does not name keep_days", err)
			}
			if got.Due {
				t.Errorf("Due = true beside an error, want false")
			}
		})
	}
}

func TestCutRefusesKeepDaysPastTheDurationRange(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC)
	cfg := designSettings()
	cfg.KeepDays = maxKeepDays + 1
	got, err := Decide(now, time.Time{}, backupBytesTrips(), cfg)
	if err == nil {
		t.Fatalf("Decide accepted keep_days %d, want an error", cfg.KeepDays)
	}
	if !strings.Contains(err.Error(), "keep_days") {
		t.Errorf("error %q does not name keep_days", err)
	}
	if got.Due {
		t.Errorf("Due = true beside an error, want false")
	}
}

func TestCutAcceptsTheLongestKeepWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC)
	cfg := designSettings()
	cfg.KeepDays = maxKeepDays
	got, err := Decide(now, time.Time{}, backupBytesTrips(), cfg)
	if err != nil {
		t.Fatalf("Decide refused keep_days %d: %v", cfg.KeepDays, err)
	}
	if !got.Cut.Before(now) {
		t.Errorf("Cut = %s, want before now %s", got.Cut, now)
	}
}

// TestDecideLastRunInTheFuture: a last run after the check's clock is no run
// that holds the gate, and no run that opens it: more than a minute ahead is an
// error and nothing runs, up to a minute ahead counts as now (the gate holds),
// and the past cases are unchanged (gt-5eicg2).
func TestDecideLastRunInTheFuture(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		lastRun time.Time
		wantDue bool
		wantErr bool
	}{
		{"an hour ahead is refused", now.Add(time.Hour), false, true},
		{"one minute and a second ahead is refused", now.Add(time.Minute + time.Second), false, true},
		{"exactly one minute ahead holds the gate", now.Add(time.Minute), false, false},
		{"59 seconds ahead holds the gate", now.Add(59 * time.Second), false, false},
		{"an hour ago holds the gate", now.Add(-time.Hour), false, false},
		{"past the interval is due", now.Add(-RunInterval - time.Minute), true, false},
		{"no run recorded is due", time.Time{}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Decide(now, tc.lastRun, backupBytesTrips(), designSettings())
			if (err != nil) != tc.wantErr {
				t.Fatalf("Decide error = %v, want error %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrLastRunInFuture) {
				t.Errorf("Decide error %v does not match ErrLastRunInFuture", err)
			}
			if got.Due != tc.wantDue {
				t.Errorf("Due = %v, want %v (reason %q)", got.Due, tc.wantDue, got.Reason)
			}
		})
	}
}

// TestDecideOtherErrorsAreNotTheFutureRefusal: only the future-last-run
// refusal matches ErrLastRunInFuture. The KeepDays refusals are decisions
// about the settings, so a caller's errors.Is cannot mistake one for a stamp
// that needs an alert (gt-wzuwn7).
func TestDecideOtherErrorsAreNotTheFutureRefusal(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		keepDays int
	}{
		{"keep_days below 1", 0},
		{"keep_days past the duration range", maxKeepDays + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := designSettings()
			cfg.KeepDays = tc.keepDays
			_, err := Decide(now, time.Time{}, backupBytesTrips(), cfg)
			if err == nil {
				t.Fatalf("Decide accepted keep_days %d", tc.keepDays)
			}
			if errors.Is(err, ErrLastRunInFuture) {
				t.Errorf("Decide error %v matches ErrLastRunInFuture", err)
			}
		})
	}
}
