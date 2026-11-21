package cmd

import (
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// dashFollowupLandings joins every store's landings since the window's start, in
// store order (gt-yy29ej). A failing store fails the whole read, naming it. read
// is one store's landings, injectable for tests; production reads the file.
func dashFollowupLandings(townRoot string, since time.Time, read ...func(store string, since time.Time) ([]land.LandingRecord, error)) ([]land.LandingRecord, error) {
	stores, err := dashFollowupStores(townRoot)
	if err != nil {
		return nil, err
	}
	one := func(store string, since time.Time) ([]land.LandingRecord, error) {
		file, err := land.RigLandingsFile(townRoot, store)
		if err != nil {
			return nil, err
		}
		return file.Since(since)
	}
	if len(read) > 0 && read[0] != nil {
		one = read[0]
	}
	return followupAcrossStores(stores, func(store string) ([]land.LandingRecord, error) {
		recs, err := one(store, since)
		if err != nil {
			return nil, fmt.Errorf("reading the landings of store %s: %w", store, err)
		}
		return recs, nil
	})
}
