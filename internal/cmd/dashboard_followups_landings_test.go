package cmd

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// landingsFake is a landings read keyed by store that, like
// land.LandingsFile.Since, keeps the records landed at or after the since it is
// handed: a window not passed on lets the record before it through.
type landingsFake struct {
	records map[string][]land.LandingRecord
	failing string
}

func (f *landingsFake) read(store string, since time.Time) ([]land.LandingRecord, error) {
	if store == f.failing {
		return nil, errors.New("unreadable")
	}
	var kept []land.LandingRecord
	for _, rec := range f.records[store] {
		if !rec.LandedAt.Before(since) {
			kept = append(kept, rec)
		}
	}
	return kept, nil
}

// followupLandings is the shared fixture: three rigs and a week's window.
func followupLandings(t *testing.T) (town string, since time.Time, f *landingsFake) {
	t.Helper()
	now, day := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC), 24*time.Hour
	at := func(bead string, age time.Duration) []land.LandingRecord {
		return []land.LandingRecord{{BeadID: bead, LandedAt: now.Add(-age)}}
	}
	return followupTown(t, "beta", "alpha", "gamma"), now.Add(-7 * day), &landingsFake{records: map[string][]land.LandingRecord{
		"hq": append(at("hq-in", time.Hour), at("hq-out", 30*day)...), "alpha": at("al-in", time.Hour),
		"beta": at("be-in", time.Hour), "gamma": at("ga-in", time.Hour),
	}}
}

func TestDashFollowupLandingsJoin(t *testing.T) {
	t.Parallel()
	town, since, f := followupLandings(t)
	got, err := dashFollowupLandings(town, since, f.read)
	if err != nil {
		t.Fatalf("dashFollowupLandings: %v", err)
	}
	var beads []string
	for _, rec := range got {
		beads = append(beads, rec.BeadID)
	}
	if want := []string{"hq-in", "al-in", "be-in", "ga-in"}; !slices.Equal(beads, want) {
		t.Errorf("joined landings = %v, want %v (hq first, then the rigs by name, nothing outside the window)", beads, want)
	}
}

func TestDashFollowupLandingsFailClosed(t *testing.T) {
	t.Parallel()
	town, since, f := followupLandings(t)
	f.failing = "alpha"
	got, err := dashFollowupLandings(town, since, f.read)
	if err == nil || !strings.Contains(err.Error(), "alpha") {
		t.Fatalf("error = %v, want one naming the store alpha", err)
	}
	if got != nil {
		t.Errorf("landings = %v, want none: a partial answer would shrink the counts", got)
	}
}

// The two refusals before any landing is read: a registry that cannot be read,
// and a store whose name cannot name a landings file (the default read).
func TestDashFollowupLandingsRefusesUnreadableStores(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	if got, err := dashFollowupLandings(t.TempDir(), since); err == nil || got != nil {
		t.Errorf("a town with no rig registry = %v, %v, want an error and no landings", got, err)
	}
	if got, err := dashFollowupLandings(followupTown(t, "bad/rig"), since); err == nil || got != nil {
		t.Errorf("a store named bad/rig = %v, %v, want an error and no landings", got, err)
	}
}
