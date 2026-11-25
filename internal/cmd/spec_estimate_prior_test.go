package cmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
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
	got, ok := specEstimatePrior(townRoot, io.Discard)("gt-prior", "hash-a")
	if want := (estimate.PriorMinutes{Embed: newEmbed, Jev: newJev}); !ok || got != want {
		t.Fatalf("prior = %+v, ok = %v, want %+v: the newest matching record wins", got, ok, want)
	}
}

// TestSpecEstimatePriorWithoutJev: a record with embed minutes and no jev, as
// every record written after the estimate stopped asking Jev is, is a prior
// with the logged embed minutes and Jev 0.
func TestSpecEstimatePriorWithoutJev(t *testing.T) {
	t.Parallel()
	embed := 12.0
	townRoot := specPriorLog(t, specPriorRecord(time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC), &embed, nil))
	got, ok := specEstimatePrior(townRoot, io.Discard)("gt-prior", "hash-a")
	if want := (estimate.PriorMinutes{Embed: 12}); !ok || got != want {
		t.Fatalf("prior = %+v, ok = %v, want %+v: embed without jev is a prior", got, ok, want)
	}
}

func TestSpecEstimatePriorAbsent(t *testing.T) {
	t.Parallel()
	jev := 8.0
	unreadable := t.TempDir() // a directory at the log file's path cannot be read
	if err := os.MkdirAll(estlog.Path(unreadable), 0o755); err != nil {
		t.Fatalf("building the unreadable log: %v", err)
	}
	cases := map[string]string{
		"no record":      specPriorLog(t),
		"nil embed":      specPriorLog(t, specPriorRecord(time.Time{}, nil, &jev)),
		"unreadable log": unreadable,
	}
	for name, townRoot := range cases {
		if got, ok := specEstimatePrior(townRoot, io.Discard)("gt-prior", "hash-a"); ok {
			t.Fatalf("%s: prior = %+v, want ok false", name, got)
		}
	}
}

// specPriorReaderStub asks deps.Prior once, as estimate.Estimate does for an
// unchanged bead text, and records whether the run wired one and what it
// answered.
type specPriorReaderStub struct {
	calls   int
	wired   bool
	prior   estimate.PriorMinutes
	priorOK bool
}

func (s *specPriorReaderStub) run(_ context.Context, deps estimate.Deps, issue *beads.Issue) (estimate.Result, error) {
	s.calls++
	s.wired = deps.Prior != nil
	if s.wired {
		s.prior, s.priorOK = deps.Prior(issue.ID, estimate.SectionHash(issue))
	}
	return estimate.Result{Points: 8, Minutes: 20, Inputs: []string{"shape"}, Rung: "shape", Hash: estimate.SectionHash(issue)}, nil
}

// TestSpecEstimatePriorWiring: the deps the command builds carry the prior
// reader, so a forced re-estimate of text estimated before reads the log where
// the run appends. An unreadable log is one line on the run's error writer,
// while a log with no record of the text and a bead not estimated before (which
// reads no log at all) are quiet.
func TestSpecEstimatePriorWiring(t *testing.T) {
	t.Parallel()
	embed, jev := 4.0, 2.3
	cases := []struct {
		name            string
		unreadableLog   bool
		estimatedBefore bool
		wantPrior       bool
		wantLogLine     bool
	}{
		{"unreadable log", true, true, false, true},
		{"no record of the text", false, true, false, false},
		{"logged record", false, true, true, false},
		{"new text", true, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.unreadableLog {
				// A directory at the log file's path cannot be read.
				if err := os.MkdirAll(estlog.Path(town), 0o755); err != nil {
					t.Fatalf("building the unreadable log: %v", err)
				}
			}
			db := specEstimateSeed()
			hash := specEstimateHashOf(t, db)
			if tc.estimatedBefore {
				if err := db.AddComment("gt-est", "SP ESTIMATE v1 sp:5 rung:all hash:"+hash); err != nil {
					t.Fatal(err)
				}
			}
			if tc.wantPrior {
				if err := estlog.Append(town, estlog.Record{BeadID: "gt-est", DescriptionHash: hash, Minutes: estlog.Minutes{Embed: &embed, Jev: &jev}}); err != nil {
					t.Fatalf("appending the logged estimate: %v", err)
				}
			}
			var errw bytes.Buffer
			stub := &specPriorReaderStub{}
			run := specEstimateRunner{
				Estimate: stub.run,
				Deps:     func() (estimate.Deps, error) { return specEstimateDeps(town, "gastown", &errw) },
			}
			if err := runSpecEstimateOn(io.Discard, "gt-est", db, specEstimateOpts{Force: true}, run); err != nil {
				t.Fatalf("runSpecEstimateOn: %v", err)
			}
			if stub.calls != 1 {
				t.Fatalf("estimate calls = %d, want 1", stub.calls)
			}
			if stub.wired != tc.estimatedBefore {
				t.Errorf("prior wired = %v, want %v: only text estimated before reads the log", stub.wired, tc.estimatedBefore)
			}
			if stub.priorOK != tc.wantPrior {
				t.Errorf("prior = %+v, ok = %v, want ok %v", stub.prior, stub.priorOK, tc.wantPrior)
			}
			if want := (estimate.PriorMinutes{Embed: embed, Jev: jev}); tc.wantPrior && stub.prior != want {
				t.Errorf("prior = %+v, want the logged %+v", stub.prior, want)
			}
			got := errw.String()
			if tc.wantLogLine {
				if !strings.Contains(got, "estimate log is unreadable") {
					t.Errorf("error output = %q, want the unreadable log named", got)
				}
			} else if got != "" {
				t.Errorf("error output = %q, want nothing said", got)
			}
		})
	}
}
