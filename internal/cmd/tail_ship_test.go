package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/buildsrc"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/events"
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
	// The source is the gastown rig's build checkout; its rig is the
	// checkout's own directory name, not the ".runtime" segment above it.
	dir := filepath.Join(town, buildsrc.BaseDirRel(), "gastown", "cmd", "gt")
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
		`{"type":"rig","version":1,"name":"fractals","git_url":"http://forgejo:3000/owner/fractals-nextjs.git"}`)
	writeTownFile(t, filepath.Join(town, "devops", "config.json"),
		`{"type":"rig","version":1,"name":"devops","git_url":"http://forgejo:3000/owner/devops.git"}`)
	// "half" is registered but has no config, so it names no repository.

	got := rigShipRepos(town)
	want := map[string]string{"fractals": "owner/fractals-nextjs", "devops": "owner/devops"}
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

// --- the Ship column reads a whole table of landings, not just a window: the
// tracker is fed by the daemon log's read, so a landing older than the feed's
// --since window has no record, and a record is what the cell reads
// (gt-b5hw2) ---

// The daemon's own lines for one landing, as the log wrote them: gt-4b0i1
// landed as its own merge 720b2c05, and the restart that shipped it installed
// 9820f431, the merge that carries it.
const (
	realShipDispatch = "spec_dispatch: dispatched: gt-4b0i1: slung to gastown/dementus on claude-sonnet (seat claude-sonnet 2/2)"
	realShipMerged   = "landing_worker: [land] gt-4b0i1: merged fce5f9f6 onto origin/main (97a96e99) as 720b2c05; pushing the merge candidate and waiting for its CI verdict, then om review"
	realShipLanded   = "landing_worker: [land] gt-4b0i1: landed 720b2c05 on origin/main (patch-id c3e4e1ec)"
	realShipRestart  = "upgrade-restart: running 9820f431f26472d6ccdab9679e5cfd486ffe5077 covers marker 9820f431f26472d6ccdab9679e5cfd486ffe5077; cleared"
)

// shipDeployTracker is a tracker wired the way the dashboard wires it: the
// town's ship definitions and a horizon that reaches the Landings table's
// whole day, over a run whose own window is the two hours before from.
func shipDeployTracker(ancestor tailAncestry, table map[string]string, ships map[string]dashboard.StagingShip, from time.Time) *tailDeploys {
	track := newTailDeploys(fixedNow, ancestor, from)
	track.setShipDefinitions("gastown", shipRigOf(table), fakeStaging(ships))
	track.setHorizon(trendHours * time.Hour)
	return track
}

// shipSource is the daemon source the dashboard feeds its tracker through: the
// lines it reads are the tracker's, the lines it returns are the stream's.
func shipSource(dir string, track *tailDeploys, from time.Time) *tailDeploySource {
	return &tailDeploySource{
		inner: &daemonSource{dir: dir, cutoff: track.logCutoff(from), loc: tailTestLoc, now: fixedNow},
		track: track, from: from,
	}
}

// TestTailDeploys_ShipsARestartCoveredLandingOlderThanTheRunsWindow: the
// landing the 01:00 restart installed is inside the table but outside the
// feed's window, and its ship time still reads — the stream stays bounded by
// the window, the record does not.
func TestTailDeploys_ShipsARestartCoveredLandingOlderThanTheRunsWindow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	appendFile(t, filepath.Join(dir, "daemon.log"),
		"2026/09/30 03:00:00 "+realShipDispatch+"\n"+
			"2026/09/30 03:03:00 "+realShipMerged+"\n"+
			"2026/09/30 03:04:00 "+realShipLanded+"\n"+
			"2026/09/30 03:10:00 "+realShipRestart+"\n")

	from := at("2026-09-30T12:00:00Z") // the run's --since: two hours back
	track := shipDeployTracker(
		fakeTailAncestry("720b2c05 9820f431f26472d6ccdab9679e5cfd486ffe5077"),
		map[string]string{"gt-4b0i1": "gastown"}, nil, from,
	)
	if got := texts(shipSource(dir, track, from).Poll()); len(got) != 0 {
		t.Fatalf("stream = %q; the landing is older than the run's window", got)
	}
	ship := track.shipStatus("gastown", "gt-4b0i1")
	if ship.Secs == nil || *ship.Secs != 600 || ship.Pending || ship.Via != tailShipViaDeploy {
		t.Fatalf("ship = %+v; want 600s via deploy", ship)
	}
}

// TestTailDeploys_ShipsAnAppLandingOlderThanTheRunsWindow: an app rig's
// landing, staged long before the run's window starts, still reads the staging
// run that shipped it.
func TestTailDeploys_ShipsAnAppLandingOlderThanTheRunsWindow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	appendFile(t, filepath.Join(dir, "daemon.log"),
		"2026/09/30 03:00:00 spec_dispatch: dispatched: fr-1: slung to fractals/opal on x (seat 1/3)\n"+
			"2026/09/30 03:05:00 landing_worker: [land] fr-1: landed aaaaaaaa on origin/main (patch-id 1)\n")

	from := at("2026-09-30T12:00:00Z")
	track := shipDeployTracker(
		fakeTailAncestry(),
		map[string]string{"fr-1": "fractals"},
		map[string]dashboard.StagingShip{"fractals\x00aaaaaaaa": {State: dashboard.StagingDeployed, At: at("2026-09-30T08:20:00Z")}},
		from,
	)
	shipSource(dir, track, from).Poll()
	ship := track.shipStatus("fractals", "fr-1")
	if ship.Secs == nil || *ship.Secs != 1200 || ship.Via != tailShipViaStaging {
		t.Fatalf("ship = %+v; want 1200s via staging", ship)
	}
}

// TestTailDeploys_WaitingLandingOlderThanTheRunsWindowIsPending: a landing with
// a dispatch line and no deploy yet reads as pending however long ago it was
// dispatched, never as a rig with no ship definition.
func TestTailDeploys_WaitingLandingOlderThanTheRunsWindowIsPending(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	appendFile(t, filepath.Join(dir, "daemon.log"),
		"2026/09/30 03:00:00 "+realShipDispatch+"\n"+
			"2026/09/30 03:04:00 "+realShipLanded+"\n")

	from := at("2026-09-30T12:00:00Z")
	track := shipDeployTracker(fakeTailAncestry(), map[string]string{"gt-4b0i1": "gastown"}, nil, from)
	shipSource(dir, track, from).Poll()
	ship := track.shipStatus("gastown", "gt-4b0i1")
	if ship.Secs != nil || !ship.Pending || ship.Failed || ship.Via != tailShipViaDeploy {
		t.Fatalf("ship = %+v; want pending via deploy", ship)
	}
}

// TestTailDeploys_LogCutoffReachesTheHorizon: the tracker reads the daemon log
// back to the table's horizon, and no further than the run's own window when
// nobody gives it one.
func TestTailDeploys_LogCutoffReachesTheHorizon(t *testing.T) {
	t.Parallel()
	from := at("2026-09-30T12:00:00Z") // two hours before fixedNow
	track := newTailDeploys(fixedNow, fakeTailAncestry(), from)
	if got, want := track.logCutoff(from), from; !got.Equal(want) {
		t.Fatalf("logCutoff without a horizon = %v; want the run's window %v", got, want)
	}
	track.setHorizon(trendHours * time.Hour)
	if got, want := track.logCutoff(from), fixedNow().Add(-trendHours*time.Hour); !got.Equal(want) {
		t.Fatalf("logCutoff = %v; want the table's horizon %v", got, want)
	}
}

// --- a hand-slung landing is dated by the events journal, the clock the
// Polecats table reads a seat's sling from: the daemon log holds no dispatch
// line for a bead the operator slung while dispatch was held (gt-04hbp5) ---

// shipSlungFake is the events journal's dispatch clock in a test: the sling
// each bead was given, the error a reader whose log cannot be read answers, and
// the bound every question carried.
type shipSlungFake struct {
	slings map[string]time.Time
	err    error
	asked  map[string]time.Time
}

func (f *shipSlungFake) slung(bead string, before time.Time) (time.Time, bool, error) {
	if f.asked == nil {
		f.asked = map[string]time.Time{}
	}
	f.asked[bead] = before
	if f.err != nil {
		// A failed read hands back no reading the tracker may use, whatever the
		// value beside it says.
		return f.slings[bead], true, f.err
	}
	at, ok := f.slings[bead]
	return at, ok, nil
}

// handSlungTracker is the dashboard's tracker in a test: the town's ship
// definitions, the table's horizon, and journal in place of the dispatch
// reader.
func handSlungTracker(journal tailDispatchSlung, table map[string]string, ancestor tailAncestry) *tailDeploys {
	track := shipDeployTracker(ancestor, table, nil, at("2026-09-30T12:00:00Z"))
	track.setSlungClock(journal, nil)
	return track
}

// TestTailDeploys_HandSlungLandingShipsFromTheJournal: a gastown landing with
// no spec_dispatch line is dated by its journal sling — pending while the
// installed commit lacks its landed sha, and a ship time once a daemon start
// installs a commit that carries it.
func TestTailDeploys_HandSlungLandingShipsFromTheJournal(t *testing.T) {
	t.Parallel()
	journal := &shipSlungFake{slings: map[string]time.Time{"gt-h": at("2026-09-30T13:00:00Z")}}
	track := handSlungTracker(journal.slung, map[string]string{"gt-h": "gastown"}, fakeTailAncestry("1111aaaa 2222bbbb"))
	track.observe([]tailLine{
		daemonAt("2026-09-30T13:03:00Z", "landing_worker: [land] gt-h: landed 1111aaaa on origin/main (patch-id 1)"),
		// This start installs a commit that does not carry the landing.
		daemonAt("2026-09-30T13:04:00Z", "daemon-start: running 3333cccc"),
	})
	if ship := track.shipStatus("gastown", "gt-h"); ship.Secs != nil || !ship.Pending || ship.Via != tailShipViaDeploy {
		t.Fatalf("ship before the landing's commit shipped = %+v; want pending via deploy", ship)
	}
	track.observe([]tailLine{
		daemonAt("2026-09-30T13:09:00Z", "daemon-start: running 2222bbbb"),
	})
	// 13:00 sling -> 13:09 deploy.
	if ship := track.shipStatus("gastown", "gt-h"); ship.Secs == nil || *ship.Secs != 540 || ship.Pending || ship.Via != tailShipViaDeploy {
		t.Fatalf("ship = %+v; want 540s via deploy", ship)
	}
	if before := journal.asked["gt-h"]; !before.Equal(at("2026-09-30T13:03:00Z")) {
		t.Errorf("journal asked for a sling before %v; want the landing time %v", before, at("2026-09-30T13:03:00Z"))
	}
}

// TestTailDeploys_DispatchLineWinsOverTheJournal: a landing with a dispatch
// line keeps that line's time, and the journal is not asked for it.
func TestTailDeploys_DispatchLineWinsOverTheJournal(t *testing.T) {
	t.Parallel()
	// The journal would date the bead five minutes earlier than the log line.
	journal := &shipSlungFake{slings: map[string]time.Time{"gt-1": at("2026-09-30T12:55:00Z")}}
	track := handSlungTracker(journal.slung, map[string]string{"gt-1": "gastown"}, fakeTailAncestry("1111aaaa 2222bbbb"))
	track.observe([]tailLine{
		daemonAt("2026-09-30T13:00:00Z", "spec_dispatch: dispatched: gt-1: slung to gastown/opal on x (seat 1/3)"),
		daemonAt("2026-09-30T13:03:00Z", "landing_worker: [land] gt-1: landed 1111aaaa on origin/main (patch-id 1)"),
		daemonAt("2026-09-30T13:04:00Z", "daemon-start: running 2222bbbb"),
	})
	if ship := track.shipStatus("gastown", "gt-1"); ship.Secs == nil || *ship.Secs != 240 {
		t.Fatalf("ship = %+v; want 240s from the dispatch line", ship)
	}
	if before, asked := journal.asked["gt-1"]; asked {
		t.Errorf("journal was asked (before %v) for a bead the daemon log dispatched", before)
	}
}

// TestTailDeploys_JournalSlingAfterTheLandingIsIgnored: a sling the journal
// dates after the landing is the rework loop, and a bead with neither a sling
// nor a dispatch line has no ship fields — the row is a dash, as before.
func TestTailDeploys_JournalSlingAfterTheLandingIsIgnored(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		journal *shipSlungFake
	}{
		{name: "a rework sling after the landing", journal: &shipSlungFake{slings: map[string]time.Time{"gt-r": at("2026-09-30T13:10:00Z")}}},
		{name: "no sling and no dispatch line", journal: &shipSlungFake{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			track := handSlungTracker(tc.journal.slung, map[string]string{"gt-r": "gastown"}, fakeTailAncestry("1111aaaa 2222bbbb"))
			track.observe([]tailLine{
				daemonAt("2026-09-30T13:03:00Z", "landing_worker: [land] gt-r: landed 1111aaaa on origin/main (patch-id 1)"),
				daemonAt("2026-09-30T13:04:00Z", "daemon-start: running 2222bbbb"),
			})
			if ship := track.shipStatus("gastown", "gt-r"); ship.Secs != nil || ship.Pending || ship.Via != "" {
				t.Fatalf("ship = %+v; want no ship fields", ship)
			}
		})
	}
}

// TestTailDeploys_UnlandedBeadIsNotAskedOfTheJournal: the journal dates a
// landing, so a bead the tracker holds no landed time for is not asked about —
// there is no landing for the answer to be before.
func TestTailDeploys_UnlandedBeadIsNotAskedOfTheJournal(t *testing.T) {
	t.Parallel()
	journal := &shipSlungFake{slings: map[string]time.Time{"gt-n": at("2026-09-30T13:00:00Z")}}
	track := handSlungTracker(journal.slung, map[string]string{"gt-n": "gastown"}, fakeTailAncestry())
	track.observe([]tailLine{
		daemonAt("2026-09-30T13:02:00Z", "landing_worker: [land] gt-n: merged 0f0f onto origin/main (aaaa) as 1111aaaa; gating the merged tree"),
	})
	if ship := track.shipStatus("gastown", "gt-n"); ship.Secs != nil || ship.Pending || ship.Via != "" {
		t.Fatalf("ship = %+v; want no ship fields for a bead with no landing", ship)
	}
	if before, asked := journal.asked["gt-n"]; asked {
		t.Errorf("journal was asked (before %v) for a bead with no landed time", before)
	}
}

// TestTailDeploys_JournalErrorLeavesNoShipFields: a journal that cannot be read
// leaves the row without a ship time and does not fail the table.
func TestTailDeploys_JournalErrorLeavesNoShipFields(t *testing.T) {
	t.Parallel()
	journal := &shipSlungFake{
		slings: map[string]time.Time{"gt-e": at("2026-09-30T13:00:00Z")},
		err:    errors.New("reading the town events log: permission denied"),
	}
	track := handSlungTracker(journal.slung, map[string]string{"gt-e": "gastown"}, fakeTailAncestry("1111aaaa 2222bbbb"))
	track.observe([]tailLine{
		daemonAt("2026-09-30T13:03:00Z", "landing_worker: [land] gt-e: landed 1111aaaa on origin/main (patch-id 1)"),
		daemonAt("2026-09-30T13:04:00Z", "daemon-start: running 2222bbbb"),
	})
	rows := buildRecentLandings(fixedNow(),
		[]omRecord{omTestRec("gt-e", "approve", 0.9, "daemon", at("2026-09-30T13:03:00Z"))},
		nil, nil, nil, track.shipStatus, func(string, string) string { return "" }, landingNames{}, nil, nil, nil, nil, 30)
	if len(rows) != 1 {
		t.Fatalf("rows = %d (%+v); want the table built with the unread journal", len(rows), rows)
	}
	if r := rows[0]; r.ShipSecs != nil || r.ShipPending || r.ShipVia != "" {
		t.Fatalf("row ship = %v via %q pending %v; want a dash", r.ShipSecs, r.ShipVia, r.ShipPending)
	}
}

// TestTailDeploys_JournalErrorIsTracedOnce: a journal that cannot be read is
// traced in the why-log, one line per change rather than one per row read, so
// an undated landing is not a dash with no reason anywhere (gt-oz9qn2).
func TestTailDeploys_JournalErrorIsTracedOnce(t *testing.T) {
	t.Parallel()
	var logged []string
	why := newDashWhyLog(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	journal := &shipSlungFake{err: errors.New("reading the town events log: permission denied")}
	track := handSlungTracker(journal.slung, map[string]string{"gt-e": "gastown"}, fakeTailAncestry("1111aaaa 2222bbbb"))
	track.setSlungClock(journal.slung, why)
	track.observe([]tailLine{
		daemonAt("2026-09-30T13:03:00Z", "landing_worker: [land] gt-e: landed 1111aaaa on origin/main (patch-id 1)"),
	})
	track.shipStatus("gastown", "gt-e")
	track.shipStatus("gastown", "gt-e")
	if len(logged) != 1 {
		t.Fatalf("why-log lines = %q; want one line for a journal that stays unreadable", logged)
	}
	if !strings.Contains(logged[0], dashJournalDispatchSource+" read failed") || !strings.Contains(logged[0], "permission denied") {
		t.Fatalf("why-log line = %q; want the journal's read failure and its reason", logged[0])
	}
}

// TestDashboardDeploys_TracesAnUnreadableJournal: the tracker the dashboard
// builds is given the why-log too, so a journal it cannot read is traced where
// the dashboard's other failed reads are, and the row is still left without a
// ship time (gt-oz9qn2).
func TestDashboardDeploys_TracesAnUnreadableJournal(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A directory where the events file belongs: a log that cannot be read,
	// not one with nothing in it.
	if err := os.Mkdir(filepath.Join(town, events.EventsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	var logged []string
	why := newDashWhyLog(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	track := dashboardDeploys(town, shipRigOf(map[string]string{"gt-u": "gastown"}), at("2026-09-30T12:00:00Z"), nil, why, events.NewDispatchReader(town))
	track.observe([]tailLine{
		daemonAt("2026-09-30T13:03:00Z", "landing_worker: [land] gt-u: landed 1111aaaa on origin/main (patch-id 1)"),
	})
	if ship := track.shipStatus("gastown", "gt-u"); ship.Secs != nil || ship.Pending || ship.Via != "" {
		t.Fatalf("ship = %+v; want no ship fields from an unreadable journal", ship)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], dashJournalDispatchSource+" read failed") {
		t.Fatalf("why-log lines = %q; want the unreadable journal traced", logged)
	}
}

// TestDashboardDeploys_DatesAHandSlungLandingFromTheJournal: the tracker the
// dashboard builds is the one its dispatch reader answers, not just a tracker
// handed the seam in a test.
func TestDashboardDeploys_DatesAHandSlungLandingFromTheJournal(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	appendFile(t, filepath.Join(town, events.EventsFile),
		`{"ts":"2026-09-30T13:00:00Z","source":"gt","type":"sling","actor":"overseer","payload":{"bead":"gt-h","target":"gastown/opal"}}`+"\n"+
			`{"ts":"2026-09-30T13:03:00Z","source":"gt","type":"done","actor":"gastown/polecats/opal","payload":{"bead":"gt-h"}}`+"\n")
	track := dashboardDeploys(town, shipRigOf(map[string]string{"gt-h": "gastown"}), at("2026-09-30T12:00:00Z"), nil, nil, events.NewDispatchReader(town))
	track.observe([]tailLine{
		daemonAt("2026-09-30T13:04:00Z", "landing_worker: [land] gt-h: landed 1111aaaa on origin/main (patch-id 1)"),
	})
	// The journal's sling dates the row, so it reads pending before any restart.
	if ship := track.shipStatus("gastown", "gt-h"); ship.Secs != nil || !ship.Pending || ship.Via != tailShipViaDeploy {
		t.Fatalf("ship = %+v; want pending from the journal's sling", ship)
	}
}

// TestDashboardDeploys_ReadsBackToTheLandingsTable: the tracker the Landings
// pane is served by is the one given the table's horizon — the wiring, not
// just the tracker, is what keeps the Ship column off a dash.
func TestDashboardDeploys_ReadsBackToTheLandingsTable(t *testing.T) {
	t.Parallel()
	cutoff := time.Now().Add(-2 * time.Hour)
	track := dashboardDeploys(t.TempDir(), shipRigOf(nil), cutoff, nil, nil, nil)
	if got := track.horizon; got != trendHours*time.Hour {
		t.Fatalf("horizon = %v; want the Landings table's %v", got, trendHours*time.Hour)
	}
	if age := time.Since(track.logCutoff(cutoff)); age < trendHours*time.Hour-time.Minute || age > trendHours*time.Hour+time.Minute {
		t.Fatalf("logCutoff = %v back; want the table's whole %v", age, trendHours*time.Hour)
	}
}
