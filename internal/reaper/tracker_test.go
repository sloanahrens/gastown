package reaper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
)

// fakeTrackerWisps answers the wisp reads from an in-memory list, applying the
// filter fields the reaper sends, and records what it was asked. The rig is not
// on api.Issue, so the fake checks it through the recorded filters instead.
type fakeTrackerWisps struct {
	issues   []api.Issue
	counted  []api.ListFilter
	listed   []api.ListFilter
	purged   [][]string
	keep     map[string]bool
	countErr error
	// countFails, when set, fails the counts whose filter it matches.
	countFails func(api.ListFilter) bool
	listErr    error
	purgeErr   error
}

var errTrackerRead = errors.New("tracker unreachable")

func (f *fakeTrackerWisps) match(flt api.ListFilter) []api.Issue {
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

func (f *fakeTrackerWisps) Count(_ context.Context, flt api.ListFilter) (int, error) {
	f.counted = append(f.counted, flt)
	if f.countErr != nil {
		return 0, f.countErr
	}
	if f.countFails != nil && f.countFails(flt) {
		return 0, errTrackerRead
	}
	return len(f.match(flt)), nil
}

func (f *fakeTrackerWisps) ListAll(_ context.Context, flt api.ListFilter) ([]api.Issue, error) {
	f.listed = append(f.listed, flt)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.match(flt), nil
}

func (f *fakeTrackerWisps) PurgeWisps(_ context.Context, ids []string) (api.PurgeWispsResult, error) {
	f.purged = append(f.purged, slices.Clone(ids))
	if f.purgeErr != nil {
		return api.PurgeWispsResult{}, f.purgeErr
	}
	res := api.PurgeWispsResult{Purged: []string{}, Kept: []api.KeptWisp{}}
	for _, id := range ids {
		if f.keep[id] {
			res.Kept = append(res.Kept, api.KeptWisp{ID: id, Reason: "referenced by a live agent"})
			continue
		}
		res.Purged = append(res.Purged, id)
	}
	return res, nil
}

var trackerNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// trackerRigFixture is a rig on prefix gt with one wisp of each kind the
// reaper's counts tell apart, plus a durable closed issue and another rig's
// wisp that no count may reach.
func trackerRigFixture() []api.Issue {
	ago := func(d time.Duration) time.Time { return trackerNow.Add(-d) }
	closedAt := func(d time.Duration) *time.Time { t := ago(d); return &t }
	return []api.Issue{
		{ID: "gt-old-open", Status: "open", Ephemeral: true, CreatedAt: ago(72 * time.Hour)},
		{ID: "gt-old-hooked", Status: "hooked", Ephemeral: true, CreatedAt: ago(48 * time.Hour)},
		{ID: "gt-new-open", Status: "open", Ephemeral: true, CreatedAt: ago(time.Hour)},
		{ID: "gt-old-closed", Status: "closed", Ephemeral: true, CreatedAt: ago(240 * time.Hour), ClosedAt: closedAt(216 * time.Hour)},
		{ID: "gt-new-closed", Status: "closed", Ephemeral: true, CreatedAt: ago(48 * time.Hour), ClosedAt: closedAt(24 * time.Hour)},
		{ID: "gt-durable-closed", Status: "closed", Ephemeral: false, CreatedAt: ago(240 * time.Hour), ClosedAt: closedAt(216 * time.Hour)},
	}
}

func TestScanTrackerReportsTheExpiredWisps(t *testing.T) {
	t.Parallel()
	fake := &fakeTrackerWisps{issues: trackerRigFixture()}
	rig := TrackerRig{Name: "gastown", Prefix: "gt"}

	got, err := ScanTracker(context.Background(), fake, rig, 24*time.Hour, 7*24*time.Hour, trackerNow)
	if err != nil {
		t.Fatalf("ScanTracker: %v", err)
	}
	if got.Database != "gastown" {
		t.Errorf("Database = %q, want the rig name gastown", got.Database)
	}
	if got.OpenWisps != 3 {
		t.Errorf("OpenWisps = %d, want 3 (old-open, old-hooked, new-open)", got.OpenWisps)
	}
	if got.ReapCandidates != 2 {
		t.Errorf("ReapCandidates = %d, want 2: the open and hooked wisps past max-age", got.ReapCandidates)
	}
	if got.PurgeCandidates != 1 {
		t.Errorf("PurgeCandidates = %d, want 1: the closed wisp past purge-age, not the durable issue", got.PurgeCandidates)
	}
	if len(got.Anomalies) != 1 || got.Anomalies[0].Type != "tracker-scope" {
		t.Errorf("Anomalies = %+v, want the tracker-scope note that mail and stale are not counted", got.Anomalies)
	}
	for _, flt := range fake.counted {
		if flt.Rig != "gt" || !flt.Ephemeral || !flt.IncludeInfra {
			t.Errorf("count filter %+v must name the rig, ephemeral wisps and infra", flt)
		}
	}
}

func TestReapTrackerDryRunCountsAndPurgesNothing(t *testing.T) {
	t.Parallel()
	fake := &fakeTrackerWisps{issues: trackerRigFixture()}
	rig := TrackerRig{Name: "gastown", Prefix: "gt"}

	got, err := ReapTracker(context.Background(), fake, rig, 7*24*time.Hour, true, trackerNow)
	if err != nil {
		t.Fatalf("ReapTracker dry run: %v", err)
	}
	if len(fake.purged) != 0 {
		t.Errorf("dry run purged %v, want no purge call", fake.purged)
	}
	if !got.DryRun || got.Reaped != 1 || got.OpenRemain != 3 {
		t.Errorf("dry-run result = %+v, want DryRun, Reaped 1, OpenRemain 3", got)
	}
}

func TestReapTrackerPurgesTheExpiredWispsThroughTheTracker(t *testing.T) {
	t.Parallel()
	fake := &fakeTrackerWisps{issues: trackerRigFixture()}
	rig := TrackerRig{Name: "gastown", Prefix: "gt"}

	got, err := ReapTracker(context.Background(), fake, rig, 7*24*time.Hour, false, trackerNow)
	if err != nil {
		t.Fatalf("ReapTracker: %v", err)
	}
	want := [][]string{{"gt-old-closed"}}
	if !slices.EqualFunc(fake.purged, want, slices.Equal) {
		t.Errorf("purge calls = %v, want %v: only the expired closed wisp goes to the purge", fake.purged, want)
	}
	if got.DryRun || got.Reaped != 1 || got.OpenRemain != 3 {
		t.Errorf("result = %+v, want Reaped 1 and OpenRemain 3", got)
	}
	if len(got.Anomalies) != 0 {
		t.Errorf("Anomalies = %+v, want none when the purge kept nothing", got.Anomalies)
	}
}

func TestReapTrackerPurgesInBatchesOfTheTrackerLimit(t *testing.T) {
	t.Parallel()
	const expired = 2*api.MaxPurgeWisps + 1
	var issues []api.Issue
	for i := range expired {
		closed := trackerNow.Add(-30 * 24 * time.Hour)
		issues = append(issues, api.Issue{
			ID: fmt.Sprintf("gt-%04d", i), Status: "closed",
			Ephemeral: true, CreatedAt: closed, ClosedAt: &closed,
		})
	}
	fake := &fakeTrackerWisps{issues: issues}
	rig := TrackerRig{Name: "gastown", Prefix: "gt"}

	got, err := ReapTracker(context.Background(), fake, rig, 7*24*time.Hour, false, trackerNow)
	if err != nil {
		t.Fatalf("ReapTracker: %v", err)
	}
	var sizes []int
	for _, batch := range fake.purged {
		sizes = append(sizes, len(batch))
	}
	if want := []int{api.MaxPurgeWisps, api.MaxPurgeWisps, 1}; !slices.Equal(sizes, want) {
		t.Errorf("purge batch sizes = %v, want %v", sizes, want)
	}
	if got.Reaped != expired {
		t.Errorf("Reaped = %d, want all %d purged across batches", got.Reaped, expired)
	}
}

func TestReapTrackerReportsWhatThePurgeKept(t *testing.T) {
	t.Parallel()
	fake := &fakeTrackerWisps{
		issues: trackerRigFixture(),
		keep:   map[string]bool{"gt-old-closed": true},
	}
	rig := TrackerRig{Name: "gastown", Prefix: "gt"}

	got, err := ReapTracker(context.Background(), fake, rig, 7*24*time.Hour, false, trackerNow)
	if err != nil {
		t.Fatalf("ReapTracker: %v", err)
	}
	if got.Reaped != 0 {
		t.Errorf("Reaped = %d, want 0: the purge kept the only candidate", got.Reaped)
	}
	if len(got.Anomalies) != 1 || got.Anomalies[0].Type != "purge-kept" || got.Anomalies[0].Count != 1 {
		t.Errorf("Anomalies = %+v, want one purge-kept anomaly counting 1", got.Anomalies)
	}
}

func TestReapTrackerRefusesWhenThePurgeFails(t *testing.T) {
	t.Parallel()
	boom := errors.New("tracker unreachable")
	fake := &fakeTrackerWisps{issues: trackerRigFixture(), purgeErr: boom}
	rig := TrackerRig{Name: "gastown", Prefix: "gt"}

	got, err := ReapTracker(context.Background(), fake, rig, 7*24*time.Hour, false, trackerNow)
	if !errors.Is(err, boom) {
		t.Fatalf("ReapTracker error = %v, want it to wrap the purge failure", err)
	}
	if got != nil {
		t.Errorf("ReapTracker result = %+v, want none when the purge failed", got)
	}
}

func TestReapTrackerRefusesWhenAReadFails(t *testing.T) {
	t.Parallel()
	boom := errors.New("tracker unreachable")
	rig := TrackerRig{Name: "gastown", Prefix: "gt"}
	tests := []struct {
		name   string
		fake   *fakeTrackerWisps
		dryRun bool
	}{
		{"dry run count", &fakeTrackerWisps{issues: trackerRigFixture(), countErr: boom}, true},
		{"live list", &fakeTrackerWisps{issues: trackerRigFixture(), listErr: boom}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ReapTracker(context.Background(), tt.fake, rig, 7*24*time.Hour, tt.dryRun, trackerNow)
			if !errors.Is(err, boom) {
				t.Fatalf("ReapTracker error = %v, want it to wrap the read failure", err)
			}
			if got != nil {
				t.Errorf("ReapTracker result = %+v, want none when a read failed", got)
			}
			if len(tt.fake.purged) != 0 {
				t.Errorf("purge calls = %v, want none when the read failed", tt.fake.purged)
			}
		})
	}
}

// TestTrackerCountFailuresRefuse fails one count at a time, so each read the
// reaper makes is shown to be checked: a refused count yields no result.
func TestTrackerCountFailuresRefuse(t *testing.T) {
	t.Parallel()
	rig := TrackerRig{Name: "gastown", Prefix: "gt"}
	tests := []struct {
		name  string
		fails func(api.ListFilter) bool
		run   func(*fakeTrackerWisps) (any, error)
	}{
		{
			name:  "scan stale count",
			fails: func(f api.ListFilter) bool { return !f.CreatedBefore.IsZero() },
			run: func(f *fakeTrackerWisps) (any, error) {
				return ScanTracker(context.Background(), f, rig, 24*time.Hour, 7*24*time.Hour, trackerNow)
			},
		},
		{
			name:  "scan expired count",
			fails: func(f api.ListFilter) bool { return !f.ClosedBefore.IsZero() },
			run: func(f *fakeTrackerWisps) (any, error) {
				return ScanTracker(context.Background(), f, rig, 24*time.Hour, 7*24*time.Hour, trackerNow)
			},
		},
		{
			name:  "live reap open count after the purge",
			fails: func(f api.ListFilter) bool { return len(f.Statuses) > 0 && f.Statuses[0] == "open" },
			run: func(f *fakeTrackerWisps) (any, error) {
				return ReapTracker(context.Background(), f, rig, 7*24*time.Hour, false, trackerNow)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeTrackerWisps{issues: trackerRigFixture(), countFails: tt.fails}
			got, err := tt.run(fake)
			if !errors.Is(err, errTrackerRead) {
				t.Fatalf("error = %v, want it to wrap the refused count", err)
			}
			if got != nil && !isNilResult(got) {
				t.Errorf("result = %+v, want none when a count was refused", got)
			}
		})
	}
}

// isNilResult reports whether got is a typed nil pointer, which a refused call
// returns as an interface holding no value.
func isNilResult(got any) bool {
	switch r := got.(type) {
	case *ScanResult:
		return r == nil
	case *ReapResult:
		return r == nil
	}
	return false
}

func TestScanTrackerRefusesWhenACountFails(t *testing.T) {
	t.Parallel()
	boom := errors.New("tracker unreachable")
	fake := &fakeTrackerWisps{issues: trackerRigFixture(), countErr: boom}
	rig := TrackerRig{Name: "gastown", Prefix: "gt"}

	got, err := ScanTracker(context.Background(), fake, rig, 24*time.Hour, 7*24*time.Hour, trackerNow)
	if !errors.Is(err, boom) {
		t.Fatalf("ScanTracker error = %v, want it to wrap the count failure", err)
	}
	if got != nil {
		t.Errorf("ScanTracker result = %+v, want none when a count failed", got)
	}
}

// writeTownRoutes plants a town whose route table is body, and returns its root.
func writeTownRoutes(t *testing.T, body string) string {
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

func TestTrackerRigsReadsTheRoutesOnTheTracker(t *testing.T) {
	t.Parallel()
	townRoot := writeTownRoutes(t, `{"path":".","prefix":"hq-"}
{"path":"gastown/mayor/rig","prefix":"gt-","backend":"tracker"}
{"path":"beads/mayor/rig","prefix":"be-"}
`)
	rigDir := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, "metadata.json"), []byte(`{"dolt_database":"gastown"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := TrackerRigs(townRoot)
	if err != nil {
		t.Fatalf("TrackerRigs: %v", err)
	}
	want := []TrackerRig{{Name: "gastown", Prefix: "gt", Database: "gastown"}}
	if !slices.Equal(got, want) {
		t.Errorf("TrackerRigs = %+v, want %+v: only the tracker route, with its database from its metadata", got, want)
	}
}

func TestTrackerRigsRefusesAnUnreadableRouteTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{"line that does not parse", "{not json\n"},
		{"tracker route without a path", `{"prefix":"gt-","backend":"tracker"}` + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := writeTownRoutes(t, tt.body)
			if rigs, err := TrackerRigs(townRoot); err == nil {
				t.Fatalf("TrackerRigs = %+v, want an error: a tracker rig the table cannot name must not reach the bd sweep", rigs)
			}
		})
	}
}

func TestTrackerRigsRefusesARouteFileItCannotRead(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	// A directory where the route file belongs fails the read, not the parse.
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads", "routes.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if rigs, err := TrackerRigs(townRoot); err == nil {
		t.Fatalf("TrackerRigs = %+v, want an error for a route file that cannot be read", rigs)
	}
}

func TestTrackerRigsIsEmptyWithoutARouteFile(t *testing.T) {
	t.Parallel()
	rigs, err := TrackerRigs(t.TempDir())
	if err != nil {
		t.Fatalf("TrackerRigs with no route file: %v", err)
	}
	if len(rigs) != 0 {
		t.Errorf("TrackerRigs = %+v, want none: a town with no route file has no tracker rigs", rigs)
	}
}

func TestWithoutTrackerDatabasesDropsOnlyTrackerOwnedDatabases(t *testing.T) {
	t.Parallel()
	rigs := []TrackerRig{
		{Name: "gastown", Prefix: "gt", Database: "gastown_dolt"},
		{Name: "orphan", Prefix: "or", Database: ""},
	}
	// gastown owns gastown_dolt (its metadata names it) and gastown (its name);
	// orphan has no metadata, so the database named after it is the one it owns.
	got := WithoutTrackerDatabases([]string{"gastown_dolt", "orphan", "gastown", "beads", "hq"}, rigs)
	if want := []string{"beads", "hq"}; !slices.Equal(got, want) {
		t.Errorf("WithoutTrackerDatabases = %v, want %v: the bd sweep keeps every database a tracker rig does not own", got, want)
	}
}
