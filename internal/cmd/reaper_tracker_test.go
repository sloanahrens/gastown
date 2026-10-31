package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/reaper"
	"github.com/steveyegge/gastown/tracker/api"
)

// trackerWispFake answers the tracker's wisp reads from an in-memory list with
// the filter fields the reaper sends, and records the purge calls.
type trackerWispFake struct {
	issues   []api.Issue
	purged   [][]string
	purgeErr error
}

func (f *trackerWispFake) match(flt api.ListFilter) []api.Issue {
	var out []api.Issue
	for _, w := range f.issues {
		switch {
		case flt.Ephemeral && !w.Ephemeral:
		case len(flt.Statuses) > 0 && !slices.Contains(flt.Statuses, w.Status):
		case !flt.CreatedBefore.IsZero() && !w.CreatedAt.Before(flt.CreatedBefore):
		case !flt.ClosedBefore.IsZero() && (w.ClosedAt == nil || !w.ClosedAt.Before(flt.ClosedBefore)):
		default:
			out = append(out, w)
		}
	}
	return out
}

func (f *trackerWispFake) Count(_ context.Context, flt api.ListFilter) (int, error) {
	return len(f.match(flt)), nil
}

func (f *trackerWispFake) ListAll(_ context.Context, flt api.ListFilter) ([]api.Issue, error) {
	return f.match(flt), nil
}

func (f *trackerWispFake) PurgeWisps(_ context.Context, ids []string) (api.PurgeWispsResult, error) {
	f.purged = append(f.purged, slices.Clone(ids))
	if f.purgeErr != nil {
		return api.PurgeWispsResult{}, f.purgeErr
	}
	return api.PurgeWispsResult{Purged: slices.Clone(ids), Kept: []api.KeptWisp{}}, nil
}

var trackerCmdNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// trackerCmdFixture is one open wisp past max-age, one closed past purge-age,
// and one open wisp inside max-age.
func trackerCmdFixture() []api.Issue {
	closedAt := trackerCmdNow.Add(-9 * 24 * time.Hour)
	return []api.Issue{
		{ID: "gt-stale", Status: "open", Ephemeral: true, CreatedAt: trackerCmdNow.Add(-72 * time.Hour)},
		{ID: "gt-done", Status: "closed", Ephemeral: true, CreatedAt: trackerCmdNow.Add(-240 * time.Hour), ClosedAt: &closedAt},
		{ID: "gt-fresh", Status: "open", Ephemeral: true, CreatedAt: trackerCmdNow.Add(-time.Hour)},
	}
}

func TestScanTrackerRigsReportsTheExpiredWisps(t *testing.T) {
	t.Parallel()
	fake := &trackerWispFake{issues: trackerCmdFixture()}
	rigs := []reaper.TrackerRig{{Name: "gastown", Prefix: "gt"}}

	got := scanTrackerRigs(context.Background(), fake, rigs, 24*time.Hour, 7*24*time.Hour, trackerCmdNow)
	if len(got) != 1 {
		t.Fatalf("scanTrackerRigs = %d results, want 1", len(got))
	}
	r := got[0]
	if r.Database != "gastown" || r.OpenWisps != 2 || r.ReapCandidates != 1 || r.PurgeCandidates != 1 {
		t.Errorf("scan = %+v, want gastown with 2 open, 1 reap candidate and 1 purge candidate", r)
	}
}

func TestScanTrackerRigsLeavesOutARigThatFails(t *testing.T) {
	t.Parallel()
	failing := &failingCountFake{}
	rigs := []reaper.TrackerRig{{Name: "gastown", Prefix: "gt"}}

	if got := scanTrackerRigs(context.Background(), failing, rigs, 24*time.Hour, 7*24*time.Hour, trackerCmdNow); len(got) != 0 {
		t.Errorf("scanTrackerRigs = %+v, want no result for a rig whose counts fail", got)
	}
}

// failingCountFake refuses every read, so a scan of it fails.
type failingCountFake struct{ trackerWispFake }

func (failingCountFake) Count(context.Context, api.ListFilter) (int, error) {
	return 0, errors.New("tracker unreachable")
}

func TestReapTrackerRigsPurgesThroughTheTrackerAndReportsTheLine(t *testing.T) {
	t.Parallel()
	fake := &trackerWispFake{issues: trackerCmdFixture()}
	rigs := []reaper.TrackerRig{{Name: "gastown", Prefix: "gt"}}

	results, failed := reapTrackerRigs(context.Background(), fake, rigs, 7*24*time.Hour, false, trackerCmdNow)
	if failed != 0 || len(results) != 1 {
		t.Fatalf("reapTrackerRigs = %d results, %d failed; want 1 and 0", len(results), failed)
	}
	if want := [][]string{{"gt-done"}}; !slices.EqualFunc(fake.purged, want, slices.Equal) {
		t.Errorf("purge calls = %v, want %v", fake.purged, want)
	}
	if got, want := reapResultLine(results[0]), "gastown: reaped 1 wisps, 2 open remain"; got != want {
		t.Errorf("summary line = %q, want %q", got, want)
	}
}

func TestReapTrackerRigsCountsARigThatFails(t *testing.T) {
	t.Parallel()
	fake := &trackerWispFake{issues: trackerCmdFixture(), purgeErr: errors.New("tracker unreachable")}
	rigs := []reaper.TrackerRig{{Name: "gastown", Prefix: "gt"}}

	results, failed := reapTrackerRigs(context.Background(), fake, rigs, 7*24*time.Hour, false, trackerCmdNow)
	if failed != 1 || len(results) != 0 {
		t.Errorf("reapTrackerRigs = %d results, %d failed; want none and 1 failed", len(results), failed)
	}
}

func TestReapResultLineIsTheSameForEveryStore(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		result reaper.ReapResult
		want   string
	}{
		{"live", reaper.ReapResult{Database: "gastown", Reaped: 2, OpenRemain: 5}, "gastown: reaped 2 wisps, 5 open remain"},
		{"dry run", reaper.ReapResult{Database: "gastown", Reaped: 2, OpenRemain: 5, DryRun: true}, "gastown: [DRY RUN] would reaped 2 wisps, 5 open remain"},
		{"molecule steps", reaper.ReapResult{Database: "beads", Reaped: 1, MoleculeStepsClosed: 3, OpenRemain: 0}, "beads: reaped 1 wisps (+3 closed-molecule steps), 0 open remain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := reapResultLine(&tt.result); got != tt.want {
				t.Errorf("reapResultLine = %q, want %q", got, tt.want)
			}
		})
	}
}

// writeCmdTownRoutes plants a town whose route table is body and returns its root.
func writeCmdTownRoutes(t *testing.T, body string) string {
	t.Helper()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

func TestReaperTrackerTargetsSplitsTheRigsFromTheSweep(t *testing.T) {
	t.Parallel()
	townRoot := writeCmdTownRoutes(t, `{"path":"gastown/mayor/rig","prefix":"gt-","backend":"tracker"}
{"path":"beads/mayor/rig","prefix":"be-"}
`)
	gastown := reaper.TrackerRig{Name: "gastown", Prefix: "gt"}

	tests := []struct {
		name         string
		townRoot     string
		wholeTown    bool
		databases    []string
		wantRigs     []reaper.TrackerRig
		wantDatabase []string
	}{
		{
			name:         "whole town covers every tracker rig and keeps the bd databases",
			townRoot:     townRoot,
			wholeTown:    true,
			databases:    []string{"gastown", "beads"},
			wantRigs:     []reaper.TrackerRig{gastown},
			wantDatabase: []string{"beads"},
		},
		{
			name:         "a --db naming the tracker rig covers it and keeps it out of the sweep",
			townRoot:     townRoot,
			databases:    []string{"gastown"},
			wantRigs:     []reaper.TrackerRig{gastown},
			wantDatabase: []string{},
		},
		{
			name:         "a --db naming only a bd database covers no tracker rig",
			townRoot:     townRoot,
			databases:    []string{"beads"},
			wantRigs:     nil,
			wantDatabase: []string{"beads"},
		},
		{
			name:         "outside a town there are no tracker rigs",
			townRoot:     "",
			wholeTown:    true,
			databases:    []string{"beads"},
			wantRigs:     nil,
			wantDatabase: []string{"beads"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rigs, dbs, err := reaperTrackerTargets(tt.townRoot, tt.wholeTown, tt.databases)
			if err != nil {
				t.Fatalf("reaperTrackerTargets: %v", err)
			}
			if !slices.Equal(rigs, tt.wantRigs) {
				t.Errorf("tracker rigs = %+v, want %+v", rigs, tt.wantRigs)
			}
			if !slices.Equal(dbs, tt.wantDatabase) {
				t.Errorf("bd databases = %v, want %v", dbs, tt.wantDatabase)
			}
		})
	}
}

func TestScanTrackerTargetsReachesNoTrackerWithoutARig(t *testing.T) {
	t.Parallel()
	reached := 0
	newTracker := func() (reaper.TrackerWisps, error) {
		reached++
		return &trackerWispFake{}, nil
	}
	if got := scanTrackerTargets(context.Background(), nil, newTracker, 24*time.Hour, 7*24*time.Hour, trackerCmdNow); got != nil || reached != 0 {
		t.Errorf("scan with no tracker rig = %+v after %d reaches, want no result and no reach", got, reached)
	}
}

func TestScanTrackerTargetsNamesATrackerItCannotReach(t *testing.T) {
	t.Parallel()
	newTracker := func() (reaper.TrackerWisps, error) { return nil, errors.New("TRACKER_URL is not set") }
	rigs := []reaper.TrackerRig{{Name: "gastown", Prefix: "gt"}}

	if got := scanTrackerTargets(context.Background(), rigs, newTracker, 24*time.Hour, 7*24*time.Hour, trackerCmdNow); got != nil {
		t.Errorf("scan = %+v, want no result when the tracker cannot be reached", got)
	}
}

func TestReapTrackerTargetsCountsEveryRigItCannotReach(t *testing.T) {
	t.Parallel()
	newTracker := func() (reaper.TrackerWisps, error) { return nil, errors.New("TRACKER_URL is not set") }
	rigs := []reaper.TrackerRig{{Name: "gastown", Prefix: "gt"}, {Name: "om", Prefix: "om"}}

	results, failed := reapTrackerTargets(context.Background(), rigs, newTracker, 7*24*time.Hour, false, trackerCmdNow)
	if results != nil || failed != len(rigs) {
		t.Errorf("reap = %+v, %d failed; want no result and every rig (%d) counted as failed", results, failed, len(rigs))
	}
}

func TestReaperTrackerWispsRefusesMissingSettings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"no tracker url", map[string]string{"TRACKER_TOKEN": "t"}},
		{"no token", map[string]string{"TRACKER_URL": "https://tracker.example.com"}},
		{"token file that cannot be read", map[string]string{
			"TRACKER_URL":        "https://tracker.example.com",
			"TRACKER_TOKEN_FILE": filepath.Join(t.TempDir(), "absent-token"),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(key string) string { return tt.env[key] }
			if _, err := reaperTrackerWispsFrom(getenv); err == nil {
				t.Fatal("reaperTrackerWispsFrom = nil error, want a refusal naming the missing setting")
			}
		})
	}
}

func TestReaperTrackerTargetsRefusesAnUnreadableRouteTable(t *testing.T) {
	t.Parallel()
	townRoot := writeCmdTownRoutes(t, "{not json\n")

	if rigs, _, err := reaperTrackerTargets(townRoot, true, []string{"beads"}); err == nil {
		t.Fatalf("reaperTrackerTargets = %+v, want an error: an unreadable route table must not fall through to the bd sweep", rigs)
	}
}

// setReaperFlag sets a flag on a reaper command for one test and restores it.
func setReaperFlag(t *testing.T, cmd *cobra.Command, name, value, restore string) {
	t.Helper()
	if err := cmd.Flags().Set(name, value); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Flags().Set(name, restore) })
}

// townWithRoutes is a town whose cwd the commands read, with the route table
// body; the commands take no directory argument, so the cwd is the input.
func townWithRoutes(t *testing.T, body string) {
	t.Helper()
	town := t.TempDir()
	writeTownFile(t, filepath.Join(town, "mayor", "town.json"), `{"name":"test-town"}`)
	writeTownFile(t, filepath.Join(town, ".beads", "routes.jsonl"), body)
	//testpolicy:allow no-chdir — the town root is the input under test: the commands take no directory argument
	t.Chdir(town)
}

// TestReaperTrackerRunsRefuseAnUnreadableRouteTable drives the scan and reap
// commands in a town whose route table does not parse. They refuse before they
// sweep: a tracker rig's Dolt database could be reached otherwise. --db names a
// bd database so the refusal, not a Dolt discovery, is what ends the run.
//
//testpolicy:allow parallel — the commands read the process working directory (workspace.FindFromCwd), which t.Chdir sets for the whole process
func TestReaperTrackerRunsRefuseAnUnreadableRouteTable(t *testing.T) {
	townWithRoutes(t, "{not json\n")
	setReaperFlag(t, reaperScanCmd, "db", "beads", "")

	for name, run := range map[string]func() error{
		"scan": func() error { return reaperScanCmd.RunE(reaperScanCmd, nil) },
		"reap": func() error { return reaperReapCmd.RunE(reaperReapCmd, nil) },
	} {
		err := run()
		if err == nil || !strings.Contains(err.Error(), "cannot find the tracker rigs") {
			t.Errorf("%s = %v, want the refusal that names the tracker rigs", name, err)
		}
	}
}

// TestReaperReapRefusesABadPurgeAge: reap parses --purge-age before it runs
// anything, so a value that does not parse ends the run with a named error.
//
//testpolicy:allow parallel — the command reads the process working directory (workspace.FindFromCwd), which t.Chdir sets for the whole process
func TestReaperReapRefusesABadPurgeAge(t *testing.T) {
	townWithRoutes(t, "")
	setReaperFlag(t, reaperReapCmd, "purge-age", "bogus", "168h")

	err := reaperReapCmd.RunE(reaperReapCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--purge-age") {
		t.Errorf("reap with --purge-age=bogus = %v, want the refusal that names --purge-age", err)
	}
}
