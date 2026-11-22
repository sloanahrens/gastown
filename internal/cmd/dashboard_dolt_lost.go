package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/hostprobe"
)

// The Dolt panel's lost-commits section: the Dolt server's serialization
// failures (error 1213) per ten minutes for a day, read from the host probe
// file gt-mac-watch writes (operator request 2026-11-18, row (1), gt-5zv50m).

// lostHour is how many ten-minute buckets make an hour.
const lostHour = 6

// errNoDoltLostTownRoot is returned when the read's context carries no town root.
var errNoDoltLostTownRoot = errors.New("the read's context carries no town root")

// errDoltLostNotPublished is the section's answer while the host has not written
// the dolt_conflicts field: an absent key is unknown, never zero lost commits
// (dv-w4ehm is the publisher).
var errDoltLostNotPublished = errors.New("the host has not published lost commits yet (dv-w4ehm)")

// doltLostProbe reads the host probe file under townRoot as of now. The clock and
// the reader are injected; production reads hostprobe.Path with its maximum age.
type doltLostProbe func(townRoot string, now time.Time) (hostprobe.Probes, error)

func readHostProbe(townRoot string, now time.Time) (hostprobe.Probes, error) {
	return hostprobe.Read(hostprobe.Path(townRoot), now, hostprobe.ProbeFileMaxAge)
}

func init() {
	registerDoltSection("lost", newDoltLostSection(readHostProbe, time.Now).read)
}

type doltLostSection struct {
	probe doltLostProbe
	now   func() time.Time
}

func newDoltLostSection(probe doltLostProbe, now func() time.Time) *doltLostSection {
	return &doltLostSection{probe: probe, now: now}
}

// read draws the totals of the last ten minutes, hour and 24 hours, the peak
// bucket with its time, and a row per database with its 24 hour total. A probe
// that cannot be read, a field the host has not written (nil) and a field whose
// own read failed are the section's error; a field with every bucket zero is the
// reading "0 lost commits", not an error.
func (s *doltLostSection) read(ctx context.Context, _ time.Time) (dashboard.DoltSection, error) {
	root, ok := doltTownRoot(ctx)
	if !ok {
		return dashboard.DoltSection{}, errNoDoltLostTownRoot
	}
	probes, err := s.probe(root, s.now())
	if err != nil {
		return dashboard.DoltSection{}, err
	}
	if probes.DoltConflicts == nil {
		return dashboard.DoltSection{}, errDoltLostNotPublished
	}
	if probes.DoltConflicts.Err != "" {
		return dashboard.DoltSection{}, fmt.Errorf("the host's Dolt log read failed: %s", probes.DoltConflicts.Err)
	}
	buckets := probes.DoltConflicts.Value.Buckets
	var last, hour, day, peak int
	var peakAt time.Time
	byDB := map[string]int{}
	for i, b := range buckets {
		day += b.Total
		if i >= len(buckets)-lostHour {
			hour += b.Total
		}
		if i == len(buckets)-1 {
			last = b.Total
		}
		if b.Total > 0 && b.Total >= peak {
			peak, peakAt = b.Total, b.At
		}
		for db, n := range b.ByDB {
			byDB[db] += n
		}
	}
	out := dashboard.DoltSection{Title: "Dolt lost commits (error 1213)", Head: []string{"database", "24 hours"}}
	if day == 0 {
		out.Note = "0 lost commits in the last 24 hours"
		return out, nil
	}
	out.Note = fmt.Sprintf("last 10 minutes: %d; last hour: %d; last 24 hours: %d; peak bucket: %d at %s",
		last, hour, day, peak, peakAt.UTC().Format("2006-01-02 15:04 UTC"))
	// The newest six buckets, oldest first: the series ends with the bucket now.
	newest := buckets[max(0, len(buckets)-lostHour):]
	parts := make([]string, len(newest))
	for i, b := range newest {
		parts[i] = fmt.Sprintf("%s=%d", b.At.UTC().Format("15:04"), b.Total)
	}
	out.Note += "; newest six buckets, oldest first: " + strings.Join(parts, " ")
	dbs := make([]string, 0, len(byDB))
	for db := range byDB {
		dbs = append(dbs, db)
	}
	slices.SortFunc(dbs, func(a, b string) int { return cmp.Or(cmp.Compare(byDB[b], byDB[a]), cmp.Compare(a, b)) })
	for _, db := range dbs {
		out.Rows = append(out.Rows, []string{db, strconv.Itoa(byDB[db])})
	}
	return out, nil
}
