package cmd

import (
	"os"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/storypoints/estimate"
	"github.com/steveyegge/gastown/internal/storypoints/estlog"
)

// specPriorRecord is one log line for gt-prior's text; nil is a missing input.
func specPriorRecord(at time.Time, embed, jev *float64) estlog.Record {
	return estlog.Record{BeadID: "gt-prior", DescriptionHash: "hash-a", Minutes: estlog.Minutes{Embed: embed, Jev: jev}, Time: at}
}

// specPriorLog appends records to a fresh log under a temp town root.
func specPriorLog(t *testing.T, records ...estlog.Record) string {
	t.Helper()
	townRoot := t.TempDir()
	for _, r := range records {
		if err := estlog.Append(townRoot, r); err != nil {
			t.Fatalf("appending %+v: %v", r, err)
		}
	}
	return townRoot
}

func TestSpecEstimatePriorNewest(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	oldEmbed, oldJev, newEmbed, newJev := 12.0, 8.0, 9.5, 11.25
	// Records for another hash and bead are newer, and the newer of the pair is appended first, so only the newest matching Time may come back.
	otherHash := specPriorRecord(base.Add(2*time.Hour), &oldEmbed, &oldJev)
	otherHash.DescriptionHash = "hash-b"
	otherBead := specPriorRecord(base.Add(2*time.Hour), &oldEmbed, &oldJev)
	otherBead.BeadID = "gt-other"
	townRoot := specPriorLog(t,
		specPriorRecord(base.Add(time.Hour), &newEmbed, &newJev),
		specPriorRecord(base, &oldEmbed, &oldJev),
		otherHash, otherBead,
	)
	got, ok := specEstimatePrior(townRoot)("gt-prior", "hash-a")
	if want := (estimate.PriorMinutes{Embed: newEmbed, Jev: newJev}); !ok || got != want {
		t.Fatalf("prior = %+v, ok = %v, want %+v: the newest matching record wins", got, ok, want)
	}
}

func TestSpecEstimatePriorAbsent(t *testing.T) {
	t.Parallel()
	embed, jev := 12.0, 8.0
	unreadable := t.TempDir() // a directory at the log file's path cannot be read
	if err := os.MkdirAll(estlog.Path(unreadable), 0o755); err != nil {
		t.Fatalf("building the unreadable log: %v", err)
	}
	cases := map[string]string{
		"no record":      specPriorLog(t),
		"nil jev":        specPriorLog(t, specPriorRecord(time.Time{}, &embed, nil)),
		"nil embed":      specPriorLog(t, specPriorRecord(time.Time{}, nil, &jev)),
		"unreadable log": unreadable,
	}
	for name, townRoot := range cases {
		if got, ok := specEstimatePrior(townRoot)("gt-prior", "hash-a"); ok {
			t.Fatalf("%s: prior = %+v, want ok false", name, got)
		}
	}
}
