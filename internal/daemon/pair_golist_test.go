package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/forgejo"
)

// goListLog is an images job log: with the guard's note when degraded.
func goListLog(degraded bool) string {
	if degraded {
		return "pin-drift guard: rig devops pin 123456789abc: " + daemonimage.GoListNoteMarker + " in /src (go: no module); the static input list and the closure walk were used instead\n"
	}
	return "pin-drift guard: rig devops pin 123456789abc is 3 commits behind main, within the bounds\n"
}

// TestPairGoListStreak: the streak counts the newest images builds whose job log
// holds the guard's go list note, from the newest, and stops at the first that
// does not; a log that cannot be read returns the count so far and read false, so
// the caller neither raises nor clears on it.
func TestPairGoListStreak(t *testing.T) {
	t.Parallel()
	runs := []forgejo.ActionRun{{ID: 1}, {ID: 2}, {ID: 3}}
	tests := []struct {
		name       string
		degraded   []bool // the log of run 1, 2, 3
		noJobs     int64  // a run whose jobs the API does not return
		logErr     error
		wantStreak int
		wantRead   bool
	}{
		{name: "three logs with the note", degraded: []bool{true, true, true}, wantStreak: 3, wantRead: true},
		{name: "the newest two of three", degraded: []bool{true, true, false}, wantStreak: 2, wantRead: true},
		{name: "a clean newest log", degraded: []bool{false, true, true}, wantStreak: 0, wantRead: true},
		{name: "a run with no job counts so far and is unread", degraded: []bool{true, true, true}, noJobs: 2, wantStreak: 1, wantRead: false},
		{name: "a log that cannot be read", degraded: []bool{true, true, true}, logErr: errors.New("boom"), wantStreak: 0, wantRead: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			api := &fakePairAPI{jobs: map[int64][]forgejo.ActionRunJob{}, logs: map[int64]string{}, logErr: tt.logErr}
			for i, run := range runs {
				if run.ID == tt.noJobs {
					continue
				}
				api.jobs[run.ID] = []forgejo.ActionRunJob{{ID: run.ID * 10}}
				api.logs[run.ID*10] = goListLog(tt.degraded[i])
			}

			streak, read := goListNoteStreak(context.Background(), pairReadyRig{Rig: "gastown", API: api}, runs)

			if streak != tt.wantStreak || read != tt.wantRead {
				t.Errorf("goListNoteStreak = %d, %v; want %d, %v", streak, read, tt.wantStreak, tt.wantRead)
			}
		})
	}
}
