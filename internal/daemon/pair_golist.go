package daemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/attention"
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
		// A built run always prints output, so an empty log carries no evidence
		// either way: it is an unread log, never a healthy guard (gt-mm7otm).
		if strings.TrimSpace(log) == "" {
			return streak, false
		}
		if !strings.Contains(log, daemonimage.GoListNoteMarker) {
			return streak, true
		}
		streak++
	}
	return streak, true
}

// goListDegradedKey is the rig's degraded-guard item for one pin: the bd pin is
// what the guard checks, so a moved pin is a new fact and a held item for the old
// one is dropped.
func goListDegradedKey(rig, pin string) string {
	return "deploy-pair-golist-degraded:" + rig + ":" + greenRefShortSHA(pin)
}

// goListHeld is the degraded item the rig held on the last pass, whatever its pin.
func goListHeld(prev []attention.Item, rig string) []attention.Item {
	for _, it := range prev {
		if it.Kind == attention.KindDeployPairGoListDegraded && it.Rig == rig {
			return []attention.Item{it}
		}
	}
	return nil
}

// goListDegradedItems is the rig's degraded-guard item: raised when the newest
// goListDegradedBuilds successful images builds all ran the pin-drift guard
// without go list, held while they do, and gone when the newest build's log lacks
// the note. A listing, a log or a pin that cannot be read says nothing about the
// guard, so the held item stays; it is never silent either: the read that failed
// raises the check's unreadable item for the rig, so an unreadable rig does not
// look like a healthy one. pinAt reads the bd pin a commit's image pins name.
func goListDegradedItems(ctx context.Context, r pairReadyRig, pinAt func(sha string) (string, error), prev []attention.Item) []attention.Item {
	unread := func(err error) []attention.Item {
		return append(goListHeld(prev, r.Rig), pairReadyUnreadableItem(r.Rig, err))
	}
	list, err := r.API.ListRuns(ctx, r.Owner, r.Repo, forgejo.RunFilter{
		WorkflowID: pairReadyImagesWorkflow, Status: []string{deployRunSucceeded}, Limit: goListDegradedBuilds,
	})
	if err != nil {
		return unread(fmt.Errorf("listing %s's images builds for the go list streak: %w", r.Rig, err))
	}
	if list == nil {
		return unread(fmt.Errorf("listing %s's images builds for the go list streak: no answer", r.Rig))
	}
	var runs []forgejo.ActionRun
	for _, run := range list.Runs {
		if run.Status == deployRunSucceeded && len(runs) < goListDegradedBuilds {
			runs = append(runs, run)
		}
	}
	streak, read := goListNoteStreak(ctx, r, runs)
	switch {
	case !read:
		return unread(fmt.Errorf("reading %s's images build logs for the go list streak", r.Rig))
	case streak < goListDegradedBuilds:
		return nil
	}
	pin, err := pinAt(runs[0].CommitSHA)
	if err != nil {
		return unread(err)
	}
	return []attention.Item{{
		Key: goListDegradedKey(r.Rig, pin), Kind: attention.KindDeployPairGoListDegraded, Severity: attention.SeverityLow, Rig: r.Rig, SHA: runs[0].CommitSHA,
		Summary: fmt.Sprintf("the pin-drift guard ran without go list in the last %d images builds of %s (newest %s, bd pin %s): it checked the pin against the static input list and the closure walk only", streak, r.Rig, greenRefShortSHA(runs[0].CommitSHA), greenRefShortSHA(pin)),
	}}
}
