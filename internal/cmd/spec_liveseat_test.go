package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// liveSandboxRecord is a live sandbox record whose seat was spawned on bead:
// the state gt-712i5k is about, where the record outlives the deferred seat.
func liveSandboxRecord(bead string) session.SandboxRecord {
	return session.SandboxRecord{
		Session: "gt-warboy", Rig: "gastown", Polecat: "warboy",
		Branch: "polecat/warboy/" + bead + "+mwgjywwx",
	}
}

// TestSpecDispatchHoldsABeadALiveSandboxRecordNames is gt-712i5k through the
// production tick: an open, unassigned, shaped bead is on the board while a
// live sandbox record's branch still names it, so the tick holds it with the
// seat named rather than slinging a second seat onto the same bead.
func TestSpecDispatchHoldsABeadALiveSandboxRecordNames(t *testing.T) {
	t.Parallel()
	const bead = "gt-712i5k"
	f := newFakeSpecTown(cleanSpec(bead, 1, "2026-09-30T09:00:00Z"))
	f.roster = specRoster{SandboxRecords: []session.SandboxRecord{liveSandboxRecord(bead)}}
	env := f.env()
	env.ShapeGate = specdispatch.ShapeGateOff
	env.PerTick = 1

	r := runSpecDispatchCycle(env)

	if len(f.slung) != 0 {
		t.Fatalf("slung %v, want nothing: a live sandbox record still names the bead", f.slung)
	}
	line := skipLines(r)
	for _, want := range []string{bead + ": " + specdispatch.SkipLiveSeat, "gastown/polecats/warboy", "still names it"} {
		if !strings.Contains(line, want) {
			t.Errorf("skip line %q should name %q", line, want)
		}
	}
}

// TestSpecDispatchConsidersTheBeadAgainOnceTheRecordIsGone is the other half of
// the hold: nothing is written when the record holds the bead, so the next
// tick, whose read finds the record gone, slings it.
func TestSpecDispatchConsidersTheBeadAgainOnceTheRecordIsGone(t *testing.T) {
	t.Parallel()
	const bead = "gt-712i5k"
	cases := []struct {
		name    string
		records []session.SandboxRecord
		want    []string
	}{
		{
			name:    "a record that is gone holds nothing",
			records: nil,
			want:    []string{bead},
		},
		{
			name:    "a record naming another bead holds nothing",
			records: []session.SandboxRecord{liveSandboxRecord("gt-other")},
			want:    []string{bead},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeSpecTown(cleanSpec(bead, 1, "2026-09-30T09:00:00Z"))
			f.roster = specRoster{SandboxRecords: tc.records}
			env := f.env()
			env.ShapeGate = specdispatch.ShapeGateOff
			env.PerTick = 1

			r := runSpecDispatchCycle(env)

			if got := strings.Join(f.slung, " "); got != strings.Join(tc.want, " ") {
				t.Fatalf("slung %q, want %q (skipped %+v)", got, tc.want, r.Skipped)
			}
		})
	}
}

// TestSpecDispatchRosterReadsTheLiveSandboxRecords drives the production roster
// wiring: the records the candidate check reads come from the town's registry,
// read once with the roster. A record deleted after the listing is skipped, and
// a registry or a record that cannot be read fails the roster instead of
// dispatching on an empty set.
func TestSpecDispatchRosterReadsTheLiveSandboxRecords(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		setup  func(t *testing.T, town string)
		want   string
		wantNo bool
	}{
		{
			name: "a live record reaches the roster",
			setup: func(t *testing.T, town string) {
				t.Helper()
				if err := session.WriteSandboxRecord(town, liveSandboxRecord("gt-712i5k")); err != nil {
					t.Fatalf("writing the record: %v", err)
				}
			},
			want: "polecat/warboy/gt-712i5k+mwgjywwx",
		},
		{
			name: "a record deleted after the listing is skipped",
			setup: func(t *testing.T, town string) {
				t.Helper()
				if err := session.WriteSandboxRecord(town, liveSandboxRecord("gt-712i5k")); err != nil {
					t.Fatalf("writing the record: %v", err)
				}
				// A sandbox deleted between the listing and the by-name read is
				// listed and then not found. A dangling symlink produces that
				// pair hermetically (gt-oz7ops).
				if err := os.Symlink(filepath.Join(town, "gone.json"), filepath.Join(session.SandboxRegistryDir(town), "gt-gone.json")); err != nil {
					t.Fatalf("linking the deleted record: %v", err)
				}
			},
			want: "polecat/warboy/gt-712i5k+mwgjywwx",
		},
		{
			name: "an unreadable registry fails the roster",
			setup: func(t *testing.T, town string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(town, ".runtime"), 0o755); err != nil {
					t.Fatalf("creating the runtime dir: %v", err)
				}
				// The registry path is a file, so listing it is an error rather
				// than the empty set a missing directory is.
				if err := os.WriteFile(filepath.Join(town, ".runtime", "sandboxes"), []byte("x"), 0o644); err != nil {
					t.Fatalf("writing the registry path: %v", err)
				}
			},
			wantNo: true,
		},
		{
			name: "a record that does not parse fails the roster",
			setup: func(t *testing.T, town string) {
				t.Helper()
				dir := session.SandboxRegistryDir(town)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("creating the registry: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, "gt-bad.json"), []byte("{not json"), 0o644); err != nil {
					t.Fatalf("writing the record: %v", err)
				}
			},
			wantNo: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeTestRigsConfig(t, town, "gastown")
			tc.setup(t, town)
			ts := &config.TownSettings{PolecatPool: &config.PolecatPool{}}
			var snap atomic.Pointer[poolSeatSnapshot]
			roster, err := specDispatchRoster(newSpecTickReads(), town, ts,
				func() sessionLister { return &fakeLister{sessions: map[string]map[string]string{}} },
				func(string) beads.Client { return beadsfake.New() }, &snap)()
			if tc.wantNo {
				if err == nil {
					t.Fatal("roster read succeeded, want the unreadable registry to fail it")
				}
				return
			}
			if err != nil {
				t.Fatalf("roster: %v", err)
			}
			if len(roster.SandboxRecords) != 1 || roster.SandboxRecords[0].Branch != tc.want {
				t.Fatalf("roster records = %+v, want the one record whose branch is %q", roster.SandboxRecords, tc.want)
			}
		})
	}
}

// TestSpecDispatchLiveSandboxRecordsRefuseWhatTheyCannotRead pins the read's
// refusals: a record that does not parse is an error — an unreadable record may
// be a live seat's, so it is never the empty set.
func TestSpecDispatchLiveSandboxRecordsRefuseWhatTheyCannotRead(t *testing.T) {
	t.Parallel()
	t.Run("a record that does not parse", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		dir := session.SandboxRegistryDir(town)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating the registry: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "gt-bad.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatalf("writing the record: %v", err)
		}
		if _, err := specLiveSandboxRecords(town); err == nil {
			t.Fatal("read succeeded, want the unparseable record to fail it")
		}
	})
	t.Run("a missing registry is no record", func(t *testing.T) {
		t.Parallel()
		got, err := specLiveSandboxRecords(t.TempDir())
		if err != nil || len(got) != 0 {
			t.Fatalf("records = %+v, err = %v; want none and no error", got, err)
		}
	})
}
