package land

import (
	"errors"
	"fmt"
	"time"
)

const (
	// DefaultFollowupBudgetPercent is the percent BudgetAllows reads a 0 as: the
	// share of a week's landings follow-up beads may take when none is configured.
	DefaultFollowupBudgetPercent = 25

	// followupBudgetFloor is the follow-up beads any week may file whatever its
	// budget says, so a week with few landings does not starve the feed.
	followupBudgetFloor = 5

	// FollowupBudgetWindow is the span a week's budget counts over: the cutoff
	// FiledSince and LandedSince take is now minus this.
	FollowupBudgetWindow = 7 * 24 * time.Hour
)

// BudgetAllows reports whether the feed may file one more follow-up bead this
// week and keep the week's follow-up beads at or under percent of its landings;
// the week's first followupBudgetFloor beads are always allowed, a percent of 0
// is DefaultFollowupBudgetPercent and a negative percent is no budget (gt-wwpfo7).
func BudgetAllows(filedThisWeek, landedThisWeek, percent int) bool {
	if percent < 0 {
		return true
	}
	if percent == 0 {
		percent = DefaultFollowupBudgetPercent
	}
	if filedThisWeek < followupBudgetFloor {
		return true
	}
	return (filedThisWeek+1)*100 <= percent*landedThisWeek
}

// FiledSince counts the beads in existing created at or after since, so the feed
// knows how many follow-up beads the week has already filed; a zero CreatedAt is
// a bead whose filed stamp did not parse, and is not counted (gt-wwpfo7).
func FiledSince(existing []ExistingFollowup, since time.Time) int {
	n := 0
	for _, e := range existing {
		if e.CreatedAt.IsZero() || e.CreatedAt.Before(since) {
			continue
		}
		n++
	}
	return n
}

// LandedSince counts the landings at or after since, the week's landings the
// budget measures against, and returns an error naming a nil landings file
// rather than a count of none (gt-wwpfo7).
func LandedSince(f *LandingsFile, since time.Time) (int, error) {
	if f == nil {
		return 0, errors.New("landings file is nil, so LandedSince cannot count the week's landings")
	}
	recs, err := f.Since(since)
	if err != nil {
		return 0, fmt.Errorf("counting landings since %s: %w", since.Format(time.RFC3339), err)
	}
	return len(recs), nil
}
