package update

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeMaintSeams is one cycle under test control: Running is true while polls
// remains (negative forever), Wait advances the clock a poll so the bound needs
// no real wait, and cancelAfterWaits cancels at that wait.
type fakeMaintSeams struct {
	live, due        bool
	dueErr, startErr error
	polls            int
	cancelAfterWaits int
	cancel           context.CancelFunc
	now              time.Time
	starts, waits    int
}

func (f *fakeMaintSeams) Live() bool                        { return f.live }
func (f *fakeMaintSeams) Due(context.Context) (bool, error) { return f.due, f.dueErr }
func (f *fakeMaintSeams) Start(context.Context) error       { f.starts++; return f.startErr }
func (f *fakeMaintSeams) Now() time.Time                    { return f.now }

func (f *fakeMaintSeams) Running() bool {
	if f.polls == 0 {
		return false
	}
	f.polls--
	return true
}

func (f *fakeMaintSeams) Wait(context.Context) {
	f.waits++
	f.now = f.now.Add(time.Second)
	if f.waits == f.cancelAfterWaits {
		f.cancel()
	}
}

func TestMaintenanceBeforeRoll(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		live, due        bool
		dueErr, startErr error
		polls            int
		cancelAfterWaits int
		want, wantDetail string
		wantStarts       int
		wantWaits        int
	}{
		{name: "not live starts nothing even when due", due: true, want: MaintenanceDone},
		{name: "nothing due starts nothing", live: true, want: MaintenanceDone},
		{name: "due cycle runs to done", live: true, due: true, polls: 2, want: MaintenanceDone, wantStarts: 1, wantWaits: 2},
		{name: "still running at the bound times out", live: true, due: true, polls: -1, want: MaintenanceTimedOut, wantStarts: 1, wantWaits: 5},
		{name: "declined start is deferred", live: true, due: true, startErr: ErrMaintenanceDeferred, want: MaintenanceDeferred, wantStarts: 1},
		{name: "unreadable due is failed", live: true, dueErr: errors.New("due read broke"), want: MaintenanceFailed, wantDetail: "due read broke"},
		{name: "failed start is failed", live: true, due: true, startErr: errors.New("start broke"), want: MaintenanceFailed, wantDetail: "start broke", wantStarts: 1},
		{name: "canceled wait is failed", live: true, due: true, polls: -1, cancelAfterWaits: 1, want: MaintenanceFailed, wantDetail: "context canceled", wantStarts: 1, wantWaits: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := &fakeMaintSeams{live: tt.live, due: tt.due, dueErr: tt.dueErr, startErr: tt.startErr,
				polls: tt.polls, cancelAfterWaits: tt.cancelAfterWaits, cancel: cancel}
			got := MaintenanceBeforeRoll(ctx, f, 5*time.Second)
			if got.Outcome != tt.want {
				t.Errorf("Outcome = %q, want %q (detail %q)", got.Outcome, tt.want, got.Detail)
			}
			if tt.wantDetail != "" && !strings.Contains(got.Detail, tt.wantDetail) {
				t.Errorf("Detail = %q, want it to hold %q", got.Detail, tt.wantDetail)
			}
			if f.starts != tt.wantStarts || f.waits != tt.wantWaits {
				t.Errorf("starts, waits = %d, %d, want %d, %d", f.starts, f.waits, tt.wantStarts, tt.wantWaits)
			}
		})
	}
}
