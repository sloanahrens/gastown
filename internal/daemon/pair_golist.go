package daemon

import (
	"context"
	"strings"

	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/forgejo"
)

// goListDegradedBuilds is how many consecutive images builds that ran the
// pin-drift guard without go list make the guard degraded. One failure is a
// hiccup (a toolchain download, a timeout) and two can be a short outage; the
// tier sweep publishes at most one green commit an hour, so three builds in a row
// is at least a few hours on the static list alone: long enough to be a cause,
// short enough to be read the same day. It is a constant, not a setting.
const goListDegradedBuilds = 3

// goListNoteStreak counts, from the newest of runs, the images builds whose first
// job's log holds the guard's go list note, and stops at the first that does not.
// read is false when a log or a run's jobs could not be read: the count so far is
// returned with it, so the caller neither raises nor clears on an unread log.
func goListNoteStreak(ctx context.Context, r pairReadyRig, runs []forgejo.ActionRun) (streak int, read bool) {
	for _, run := range runs {
		jobs, err := r.API.ListRunJobs(ctx, r.Owner, r.Repo, run.ID)
		if err != nil || len(jobs) == 0 {
			return streak, false
		}
		log, err := r.API.JobLogs(ctx, r.Owner, r.Repo, jobs[0].ID)
		if err != nil {
			return streak, false
		}
		if !strings.Contains(log, daemonimage.GoListNoteMarker) {
			return streak, true
		}
		streak++
	}
	return streak, true
}
