package crewfresh

import (
	"slices"
	"time"
)

// Clone is one crew checkout as a report lists it: where it is, what its last
// fetch left it on, and why it could not be read when Err is set.
type Clone struct {
	// Path is the checkout's directory, and the identity a report lists it by.
	Path string
	// Rig is the rig the checkout belongs to.
	Rig string
	// Dir is the crew member's directory name under the rig.
	Dir string
	// Branch is the checked-out branch, "" for a detached HEAD.
	Branch string
	// LastFetch is when the checkout last fetched, the zero time for never.
	LastFetch time.Time
	// Behind counts the commits origin/main has that HEAD lacks.
	Behind int
	// Err names why the checkout could not be read; a clone that could not be
	// read is still listed.
	Err string
}

// Stale returns the clones not fetched within maxAge before now, oldest fetch
// first and ties ordered by Path, counting a zero LastFetch as never fetched; a
// clone with Err set is listed whatever its LastFetch, since an unreadable
// clone cannot be shown to be fresh.
func Stale(cs []Clone, now time.Time, maxAge time.Duration) []Clone {
	var stale []Clone
	for _, c := range cs {
		if c.Err == "" && now.Sub(c.LastFetch) < maxAge {
			continue
		}
		stale = append(stale, c)
	}
	slices.SortStableFunc(stale, func(a, b Clone) int {
		// A zero time is the earliest, so it sorts first, as never fetched
		// should.
		switch {
		case a.LastFetch.Before(b.LastFetch):
			return -1
		case b.LastFetch.Before(a.LastFetch):
			return 1
		case a.Path < b.Path:
			return -1
		case b.Path < a.Path:
			return 1
		}
		return 0
	})
	return stale
}
