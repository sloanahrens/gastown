package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// --- ship definitions: gastown ships on the daemon restart, app rigs on their
// staging deploy, and a rig with neither shows nothing at all (gt-lqqjj) ---

// shipRigOf is the routes table the tracker is given in a test, as a map from
// bead to rig.
func shipRigOf(beads map[string]string) func(bead string) string {
	return func(bead string) string { return beads[bead] }
}

// fakeStaging answers the staging lookup from a table keyed "<rig>\x00<commit>";
// a key that is not in it is a rig with no staging ship definition.
func fakeStaging(ships map[string]dashboard.StagingShip) tailShipStaging {
	return func(rig, commit string, _ time.Time) (dashboard.StagingShip, bool) {
		s, ok := ships[rig+"\x00"+commit]
		return s, ok
	}
}

// appShipTracker is a tracker with the town's ship definitions installed: the
// restart rig is gastown, and beads resolve through table. ancestor answers the
// restart path's install questions.
func appShipTracker(ancestor tailAncestry, table map[string]string, ships map[string]dashboard.StagingShip) *tailDeploys {
	track := newTailDeploys(fixedNow, ancestor, at("2026-09-30T13:00:00Z"))
	track.setShipDefinitions("gastown", shipRigOf(table), fakeStaging(ships))
	return track
}

// TestTailDeploys_AppLandingShipsAtItsStagingDeploy: a fractals landing is
// dated by the staging run that covered it, not by a daemon restart that never
// installs its commit.
func TestTailDeploys_AppLandingShipsAtItsStagingDeploy(t *testing.T) {
	t.Parallel()
	track := appShipTracker(
		fakeTailAncestry(),
		map[string]string{"fr-1": "fractals"},
		map[string]dashboard.StagingShip{"fractals\x00aaaaaaaa": {State: dashboard.StagingDeployed, At: at("2026-09-30T13:20:00Z")}},
	)
	track.observe([]tailLine{
		daemonAt("2026-09-30T13:00:00Z", "spec_dispatch: dispatched: fr-1: slung to fractals/opal on x (seat 1/3)"),
		daemonAt("2026-09-30T13:05:00Z", "landing_worker: [land] fr-1: landed aaaaaaaa on origin/main (patch-id 1)"),
		// A restart installing other code cannot ship it.
		daemonAt("2026-09-30T13:30:00Z", "upgrade-restart: running 2222bbbb covers marker 2222bbbb; cleared"),
	})
	s := track.shipStatus("fractals", "fr-1")
	if s.Secs == nil || *s.Secs != 1200 || s.Pending || s.Via != tailShipViaStaging {
		t.Fatalf("ship = %+v; want 1200s via staging", s)
	}
}

// TestTailDeploys_AppLandingPendingUntilStagingCovers: a landed app bead whose
// staging deploy has not covered it reads as pending, with the app rig's title.
func TestTailDeploys_AppLandingPendingUntilStagingCovers(t *testing.T) {
	t.Parallel()
	track := appShipTracker(
		fakeTailAncestry(),
		map[string]string{"fr-1": "fractals"},
		map[string]dashboard.StagingShip{"fractals\x00aaaaaaaa": {State: dashboard.StagingPending}},
	)
	track.observe([]tailLine{
		daemonAt("2026-09-30T13:00:00Z", "spec_dispatch: dispatched: fr-1: slung to fractals/opal on x (seat 1/3)"),
		daemonAt("2026-09-30T13:05:00Z", "landing_worker: [land] fr-1: landed aaaaaaaa on origin/main (patch-id 1)"),
	})
	s := track.shipStatus("fractals", "fr-1")
	if s.Secs != nil || !s.Pending || s.Failed || s.Via != tailShipViaStaging {
		t.Fatalf("ship = %+v; want pending via staging", s)
	}
}

// TestTailDeploys_StagingFailureKeepsPendingAndNamesTheRunState: the landing is
// still pending, and the failed run's own state rides with it for the cell.
func TestTailDeploys_StagingFailureKeepsPendingAndNamesTheRunState(t *testing.T) {
	t.Parallel()
	track := appShipTracker(
		fakeTailAncestry(),
		map[string]string{"bv-1": "beaver"},
		map[string]dashboard.StagingShip{"beaver\x00bbbbbbbb": {State: dashboard.StagingFailed, RunState: "failure"}},
	)
	track.observe([]tailLine{
		daemonAt("2026-09-30T13:00:00Z", "spec_dispatch: dispatched: bv-1: slung to beaver/opal on x (seat 1/3)"),
		daemonAt("2026-09-30T13:05:00Z", "landing_worker: [land] bv-1: landed bbbbbbbb on origin/main (patch-id 1)"),
	})
	s := track.shipStatus("beaver", "bv-1")
	if s.Secs != nil || !s.Pending || !s.Failed || s.RunState != "failure" {
		t.Fatalf("ship = %+v; want pending, failed, state failure", s)
	}
}

// TestTailDeploys_RigWithoutAShipDefinitionReportsNothing: devops has no
// staging runs, so its landings show neither a time nor a pending mark — the
// cell is a dash rather than a landing that looks about to ship.
func TestTailDeploys_RigWithoutAShipDefinitionReportsNothing(t *testing.T) {
	t.Parallel()
	track := appShipTracker(fakeTailAncestry(), map[string]string{"dv-1": "devops"}, nil)
	track.observe([]tailLine{
		daemonAt("2026-09-30T13:00:00Z", "spec_dispatch: dispatched: dv-1: slung to devops/opal on x (seat 1/3)"),
		daemonAt("2026-09-30T13:05:00Z", "landing_worker: [land] dv-1: landed dddddddd on origin/main (patch-id 1)"),
	})
	if s := track.shipStatus("devops", "dv-1"); s.Secs != nil || s.Pending || s.Failed || s.Via != "" {
		t.Fatalf("ship = %+v; want a dash", s)
	}
}

// TestTailDeploys_AppLandingsStayOutOfTheSummary: the Ship time tile and its
// waiting count are the restart rig's, so an app rig's landings — deployed or
// pending — do not move them.
func TestTailDeploys_AppLandingsStayOutOfTheSummary(t *testing.T) {
	t.Parallel()
	track := appShipTracker(
		fakeTailAncestry("aaaaaaaa aaaaaaaa"),
		map[string]string{"gt-a": "gastown", "gt-b": "gastown", "fr-1": "fractals", "dv-1": "devops"},
		map[string]dashboard.StagingShip{
			"fractals\x00cccccccc": {State: dashboard.StagingDeployed, At: at("2026-09-30T13:20:00Z")},
		},
	)
	track.observe([]tailLine{
		daemonAt("2026-09-30T13:00:00Z", "spec_dispatch: dispatched: gt-a: slung to gastown/opal on x (seat 1/3)"),
		daemonAt("2026-09-30T13:10:00Z", "landing_worker: [land] gt-a: landed aaaaaaaa on origin/main (patch-id 1)"),
		daemonAt("2026-09-30T13:12:00Z", "spec_dispatch: dispatched: gt-b: slung to gastown/opal on x (seat 1/3)"),
		daemonAt("2026-09-30T13:15:00Z", "landing_worker: [land] gt-b: landed bbbbbbbb on origin/main (patch-id 2)"),
		daemonAt("2026-09-30T13:01:00Z", "spec_dispatch: dispatched: fr-1: slung to fractals/opal on x (seat 1/3)"),
		daemonAt("2026-09-30T13:05:00Z", "landing_worker: [land] fr-1: landed cccccccc on origin/main (patch-id 3)"),
		daemonAt("2026-09-30T13:02:00Z", "spec_dispatch: dispatched: dv-1: slung to devops/opal on x (seat 1/3)"),
		daemonAt("2026-09-30T13:06:00Z", "landing_worker: [land] dv-1: landed dddddddd on origin/main (patch-id 4)"),
		// The restart ships gt-a and leaves gt-b waiting.
		daemonAt("2026-09-30T13:40:00Z", "upgrade-restart: running aaaaaaaa covers marker aaaaaaaa; cleared"),
	})
	s := track.snapshot()
	if s.Landed != 2 || s.Waiting != 1 {
		t.Fatalf("snapshot = %+v; want 2 gastown landings with 1 waiting", s)
	}
	// gt-a: 13:00 -> 13:40 is 40m. The app landings are not in the median.
	if !s.HasMedian || s.MedianBeads != 1 || s.MedianMin != 40 {
		t.Fatalf("median = %+v; want 40m over 1 bead", s)
	}
	// The app rows still read for the table, which is what they are for.
	if ship := track.shipStatus("fractals", "fr-1"); ship.Secs == nil || ship.Via != tailShipViaStaging {
		t.Fatalf("fractals ship = %+v; want its staging time", ship)
	}
	if ship := track.shipStatus("devops", "dv-1"); ship.Pending || ship.Secs != nil {
		t.Fatalf("devops ship = %+v; want a dash", ship)
	}
}

// TestTailDeploys_UnplacedBeadKeepsItsRestartReading: a bead the routes table
// cannot place is not known to be an app rig's, so it keeps the restart
// reading rather than losing its ship time.
func TestTailDeploys_UnplacedBeadKeepsItsRestartReading(t *testing.T) {
	t.Parallel()
	track := appShipTracker(fakeTailAncestry(), nil, nil)
	track.observe([]tailLine{
		daemonAt("2026-09-30T13:00:00Z", "spec_dispatch: dispatched: hq-1: slung to gastown/opal on x (seat 1/3)"),
		daemonAt("2026-09-30T13:10:00Z", "landing_worker: [land] hq-1: landed aaaaaaaa on origin/main (patch-id 1)"),
	})
	s := track.shipStatus("", "hq-1")
	if s.Secs != nil || !s.Pending || s.Via != tailShipViaDeploy {
		t.Fatalf("ship = %+v; want the restart reading", s)
	}
}

// TestTailDeploys_RestartDeploysOnlyTheRestartRig: an app rig's bead is not
// marked deployed by a restart, however the ancestry answers, because a restart
// installs the restart rig's code and not the app's.
func TestTailDeploys_RestartDeploysOnlyTheRestartRig(t *testing.T) {
	t.Parallel()
	track := newTailDeploys(fixedNow, fakeTailAncestry("aaaa 2222bbbb"), at("2026-09-30T13:00:00Z"))
	track.setShipDefinitions("gastown", shipRigOf(map[string]string{"fr-1": "fractals"}), nil)
	track.observe([]tailLine{
		daemonAt("2026-09-30T13:00:00Z", "spec_dispatch: dispatched: fr-1: slung to fractals/opal on x (seat 1/3)"),
		daemonAt("2026-09-30T13:05:00Z", "landing_worker: [land] fr-1: landed aaaaaaaa on origin/main (patch-id 1)"),
		daemonAt("2026-09-30T13:10:00Z", "upgrade-restart: running 2222bbbb covers marker 2222bbbb; cleared"),
	})
	if s := track.shipStatus("fractals", "fr-1"); s.Secs != nil {
		t.Fatalf("ship = %+v; an app landing was marked deployed by a restart", s)
	}
}

// TestTailDeploys_GastownShipTimeIsUnchanged: with ship definitions installed,
// a gastown landing still ships on the restart that installs its commit, with
// the same time as before.
func TestTailDeploys_GastownShipTimeIsUnchanged(t *testing.T) {
	t.Parallel()
	track := newTailDeploys(fixedNow, fakeTailAncestry("1111aaaa 2222bbbb"), at("2026-09-30T13:00:00Z"))
	track.setShipDefinitions("gastown", shipRigOf(map[string]string{"gt-1": "gastown"}), fakeStaging(nil))
	got := track.observe([]tailLine{
		daemonAt("2026-09-30T13:01:00Z", "spec_dispatch: dispatched: gt-1: slung to gastown/opal on x (seat 1/3)"),
		daemonAt("2026-09-30T13:03:00Z", "landing_worker: [land] gt-1: landed 1111aaaa on origin/main (patch-id 1)"),
		daemonAt("2026-09-30T13:04:00Z", "upgrade-restart: running 2222bbbb covers marker 2222bbbb; cleared"),
	})
	if want := []string{"town daemon gt-1 deployed in 3.0m (deploy 1.0m)"}; len(got) != 1 || texts(got)[0] != want[0] {
		t.Fatalf("deployed = %q, want %q", texts(got), want)
	}
	s := track.shipStatus("gastown", "gt-1")
	if s.Secs == nil || *s.Secs != 180 || s.Via != tailShipViaDeploy {
		t.Fatalf("ship = %+v; want 180s via deploy", s)
	}
	if snap := track.snapshot(); snap.Landed != 1 || snap.Waiting != 0 || !snap.HasMedian || snap.MedianMin != 3 {
		t.Fatalf("snapshot = %+v; want 1 landed, 3m median", snap)
	}
}

// TestTailRestartRig_ReadsTheTownsCheckout: the restart rig is the rig holding
// the town's gt source, and a town without one names no rig.
func TestTailRestartRig_ReadsTheTownsCheckout(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if rig := tailRestartRig(town); rig != "" {
		t.Fatalf("restart rig = %q; want none without a checkout", rig)
	}
	dir := filepath.Join(town, "gastown", "cmd", "gt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rig := tailRestartRig(town); rig != "gastown" {
		t.Fatalf("restart rig = %q; want gastown", rig)
	}
}

// TestRigShipReposReadsEachRigsOwnConfig: the rig-to-repository map comes from
// each rig's own config.json git_url and the registry, so a rig added to the
// town is read without a code change. A rig with no config or no git_url is
// left out rather than guessed at.
func TestRigShipReposReadsEachRigsOwnConfig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownFile(t, filepath.Join(town, "mayor", "rigs.json"),
		`{"version":1,"rigs":{"fractals":{},"devops":{},"half":{}}}`)
	writeTownFile(t, filepath.Join(town, "fractals", "config.json"),
		`{"type":"rig","version":1,"name":"fractals","git_url":"http://forgejo:3000/sloan/fractals-nextjs.git"}`)
	writeTownFile(t, filepath.Join(town, "devops", "config.json"),
		`{"type":"rig","version":1,"name":"devops","git_url":"http://forgejo:3000/sloan/devops.git"}`)
	// "half" is registered but has no config, so it names no repository.

	got := rigShipRepos(town)
	want := map[string]string{"fractals": "sloan/fractals-nextjs", "devops": "sloan/devops"}
	if len(got) != len(want) {
		t.Fatalf("repos = %v, want %v", got, want)
	}
	for rig, repo := range want {
		if got[rig] != repo {
			t.Errorf("repos[%s] = %q, want %q", rig, got[rig], repo)
		}
	}
}

// writeTownFile writes a fixture file under a directory created for it.
func writeTownFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, data)
}
