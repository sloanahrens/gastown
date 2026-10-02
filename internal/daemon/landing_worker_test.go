package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

func TestLandingWorkerConfigDefaults(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "landing_worker") {
		t.Fatal("landing_worker enabled with no config; it must be opt-in")
	}
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}
	if IsPatrolEnabled(cfg, "landing_worker") {
		t.Fatal("landing_worker enabled with no entry")
	}
	if landingWorkerInterval(cfg) != defaultLandingWorkerInterval || landingWorkerLandTimeout(cfg) != landworker.DefaultLandTimeout {
		t.Fatal("defaults not applied")
	}
	cfg.Patrols.LandingWorker = &LandingWorkerConfig{Enabled: true, IntervalStr: "2m", LandTimeoutStr: "45m", Rigs: []string{"gastown"}}
	if !IsPatrolEnabled(cfg, "landing_worker") || landingWorkerInterval(cfg) != 2*time.Minute || landingWorkerLandTimeout(cfg) != 45*time.Minute {
		t.Fatal("configured values not applied")
	}
	if got := landingWorkerRigs(cfg, []string{"beads", "gastown", "hm"}); len(got) != 1 || got[0] != "gastown" {
		t.Fatalf("rigs = %v", got)
	}
}

func TestPruneLandingLogs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	old, fresh := filepath.Join(root, "land-old"), filepath.Join(root, "land-new")
	for _, d := range []string{old, fresh} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-landingLogRetention - time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	pruneLandingLogs(root, time.Now())
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old log dir kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh log dir removed")
	}
}

func TestLandingReviewOnByDefault(t *testing.T) {
	t.Parallel()
	if !landingReviewEnabled(nil) || !landingReviewEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{LandingWorker: &LandingWorkerConfig{Enabled: true}}}) {
		t.Fatal("om review must be on unless explicitly disabled")
	}
	off := false
	if landingReviewEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{LandingWorker: &LandingWorkerConfig{Review: &off}}}) {
		t.Fatal("review:false did not disable the review")
	}
}

func TestResolveOMPath(t *testing.T) {
	t.Parallel()
	missing := func(string) (string, error) { return "", os.ErrNotExist }
	onPath := func(string) (string, error) { return "/usr/local/bin/om", nil }
	if got := resolveOMPath("/opt/om", onPath, "/Users/x"); got != "/opt/om" {
		t.Errorf("configured: %s", got)
	}
	if got := resolveOMPath("", onPath, "/Users/x"); got != "/usr/local/bin/om" {
		t.Errorf("on PATH: %s", got)
	}
	if got := resolveOMPath("", missing, "/Users/x"); got != "/Users/x/go/bin/om" {
		t.Errorf("fallback: %s", got)
	}
}

func TestRigPostLandCommandReadsRigSettingsOnly(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	if got := rigPostLandCommand(rigPath); got != "" {
		t.Fatalf("no settings: %q", got)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"rig-settings","version":1,"merge_queue":{"gate":"make gate","post_land_command":"make test-slow"}}`
	if err := os.WriteFile(filepath.Join(rigPath, "settings", "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := rigPostLandCommand(rigPath); got != "make test-slow" {
		t.Fatalf("post_land_command = %q", got)
	}
}

// The landing work root must never be under the town root: internal/git
// refuses worktree targets there, which failed every landing on 2026-09-30.
func TestLandingWorkRootIsOutsideTheTownRoot(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	root, err := landingWorkRoot("", town, "gastown")
	if err != nil {
		t.Fatalf("default work root: %v", err)
	}
	if landingPathWithin(root, town) {
		t.Fatalf("default work root %s is under the town root %s", root, town)
	}
	if filepath.Base(root) != "gastown" || !strings.Contains(root, fmt.Sprintf("gt-landing-%d", os.Getuid())) {
		t.Fatalf("default work root %s is not uid- and rig-scoped", root)
	}

	for _, bad := range []string{filepath.Join(town, ".runtime", "landing-work"), town} {
		if _, err := landingWorkRoot(bad, town, "gastown"); err == nil || !strings.Contains(err.Error(), "under the town root") {
			t.Errorf("work_root %s: err = %v; want a refusal naming the town root", bad, err)
		}
	}
	if _, err := landingWorkRoot("relative/dir", town, "gastown"); err == nil {
		t.Error("a relative work_root was accepted")
	}
	outside := t.TempDir()
	if got, err := landingWorkRoot(outside, town, "gastown"); err != nil || got != filepath.Join(outside, "gastown") {
		t.Errorf("work_root outside the town: %s, %v", got, err)
	}
}

// A work root spelled through a symlink into the town root is refused too.
func TestLandingWorkRootRefusesASymlinkIntoTheTown(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(town, link); err != nil {
		t.Fatal(err)
	}
	if _, err := landingWorkRoot(filepath.Join(link, "work"), town, "gastown"); err == nil {
		t.Fatal("a work_root reaching the town root through a symlink was accepted")
	}
}

// gitRemote reads the landing target on origin: the branch tip there, and
// whether a commit is on it after fetching the target.
func TestGitRemoteReadsTheTargetOnOrigin(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	root := t.TempDir()
	origin, bare := filepath.Join(root, "origin.git"), filepath.Join(root, ".repo.git")
	f.InitBare(t, origin)
	seed := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	if err := f.Open(root).CloneBareWithBranch(origin, bare, "main"); err != nil {
		t.Fatal(err)
	}
	landed := f.Commit(t, origin, "main", "landed", map[string]string{"b.txt": "b\n"})
	r := gitRemote{g: f.Open(bare), remote: "origin"}

	if tip, err := r.BranchTip("main"); err != nil || tip != landed {
		t.Fatalf("BranchTip(main) = %q, %v; want origin's tip %s", tip, err, landed)
	}
	for _, c := range []string{seed, landed} {
		if on, err := r.Contains("main", c); err != nil || !on {
			t.Errorf("Contains(main, %s) = %v, %v; want true once the target is fetched", c, on, err)
		}
	}
	if on, err := r.Contains("main", strings.Repeat("e", 40)); err != nil || on {
		t.Errorf("Contains(main, unknown) = %v, %v; want false and no error", on, err)
	}
	if on, err := r.Contains("no-such-target", seed); err == nil || on {
		t.Errorf("Contains(missing target) = %v, %v; want an error, never a no", on, err)
	}
}

// A direct push is seen on origin only: the post-land run fetches the target
// so the worktree can be added at the pushed commit (gt-p2rs0).
func TestPostLandFetchBringsADirectPushIntoTheRigRepo(t *testing.T) {
	t.Parallel()
	f := gitfake.New()
	root := t.TempDir()
	origin, bare := filepath.Join(root, "origin.git"), filepath.Join(root, ".repo.git")
	f.InitBare(t, origin)
	seed := f.Commit(t, origin, "main", "seed", map[string]string{"a.txt": "a\n"})
	if err := f.Open(root).CloneBareWithBranch(origin, bare, "main"); err != nil {
		t.Fatal(err)
	}
	pushed := f.Commit(t, origin, "main", "direct push", map[string]string{"b.txt": "b\n"})
	g := f.Open(bare)
	if have, _ := g.RefExists(pushed + "^{commit}"); have {
		t.Fatal("fixture: the rig repo already has the pushed commit")
	}

	if err := postLandFetch(g, "origin", landworker.PostLand{Commit: pushed, Target: "main", Direct: true, From: seed}); err != nil {
		t.Fatalf("postLandFetch: %v", err)
	}
	if have, err := g.RefExists(pushed + "^{commit}"); err != nil || !have {
		t.Fatalf("pushed commit in the rig repo after the fetch = %v, %v", have, err)
	}
	if err := g.WorktreeAddDetached(filepath.Join(root, "wt"), pushed); err != nil {
		t.Fatalf("worktree at the pushed commit: %v", err)
	}
	if err := postLandFetch(g, "origin", landworker.PostLand{Commit: seed, Target: "no-such-branch"}); err != nil {
		t.Errorf("a commit already present fetched anyway: %v", err)
	}
	if err := postLandFetch(g, "origin", landworker.PostLand{Commit: strings.Repeat("e", 40), Target: "main"}); err == nil {
		t.Error("a commit not on origin after the fetch was accepted")
	}
}

func TestPostLandRerunCommandMatchesTheTier(t *testing.T) {
	t.Parallel()
	const pkg = "github.com/steveyegge/gastown/internal/cmd"
	for cmd, want := range map[string]string{
		"make test-slow":                        "GT_TEST_DOCKER=0 go test -count=1 -timeout 20m " + pkg,
		"make test-slow; make test-integration": "GT_TEST_DOCKER=1 go test -count=1 -tags integration -timeout 20m " + pkg,
	} {
		if got, err := postLandRerunCommand(cmd, pkg); err != nil || got != want {
			t.Errorf("%q: %q, %v; want %q", cmd, got, err, want)
		}
	}
	for _, bad := range []string{"pkg;rm -rf x", "$(id)", "-exec=sh"} {
		if got, err := postLandRerunCommand("make test-slow", bad); err == nil {
			t.Errorf("package %q accepted as %q", bad, got)
		}
	}
}

func TestRerunCommandRerunsOnlyThePackagesInTheTier(t *testing.T) {
	t.Parallel()
	const a, b = "github.com/x/a", "github.com/x/b"
	if got, err := rerunCommand(false, a, b); err != nil || got != "GT_TEST_DOCKER=0 go test -count=1 -timeout 20m "+a+" "+b {
		t.Errorf("unit tier: %q, %v", got, err)
	}
	if got, err := rerunCommand(true, a); err != nil || got != "GT_TEST_DOCKER=1 go test -count=1 -tags integration -timeout 20m "+a {
		t.Errorf("full tier: %q, %v", got, err)
	}
	if got, err := rerunCommand(false, a, "$(id)"); err == nil {
		t.Errorf("a bad package among good ones accepted as %q", got)
	}
	if got, err := rerunCommand(false); err == nil {
		t.Errorf("no package accepted as %q", got)
	}
}

func TestWriteRedMainStatusReplacesTheLine(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for _, line := range []string{"main RED at c1 (landed by gt-1): pkg [gt-2]", "main GREEN at c2 (landed by gt-3)"} {
		if err := writeRedMainStatus(town, "gastown", line, now); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(RedMainStatusPath(town, "gastown"))
	if err != nil || string(got) != "2026-09-30T12:00:00Z main GREEN at c2 (landed by gt-3)\n" {
		t.Fatalf("status file %q, %v", got, err)
	}
}

func TestFileMainStateRoundTrips(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f := fileMainState{path: RedMainStatePath(town, "gastown")}
	if st, err := f.Load(); err != nil || st != (landworker.MainState{}) {
		t.Fatalf("missing file: %+v %v; want the zero state", st, err)
	}
	want := landworker.MainState{LastGreen: "g1", LastRun: "r1"}
	if err := f.Save(want); err != nil {
		t.Fatal(err)
	}
	if st, err := f.Load(); err != nil || st != want {
		t.Fatalf("loaded %+v %v; want %+v", st, err, want)
	}
}

// The state file is an interface between the landing worker that writes the
// revert in flight and the spec dispatcher that reads it to hold a rig's
// red-main beads (gt-zkdwt). This pins the two halves together, so a rename
// on either side breaks here rather than silently dispatching the fix
// forward a revert supersedes.
func TestFileMainStateCarriesTheRevertTheDispatcherReads(t *testing.T) {
	t.Parallel()
	if specdispatch.LabelRedMain != landworker.LabelRedMain {
		t.Fatalf("dispatcher label %q, red-main owner's %q", specdispatch.LabelRedMain, landworker.LabelRedMain)
	}
	town := t.TempDir()
	f := fileMainState{path: RedMainStatePath(town, "gastown")}
	started := time.Date(2026, 9, 30, 11, 55, 0, 0, time.UTC)
	if err := f.Save(landworker.MainState{LastGreen: "g1", Revert: &landworker.PendingRevert{Culprit: "gt-cul", Bead: "gt-rv", StartedAt: started}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	rv := specdispatch.ParseRevert(raw)
	if rv == nil || rv.Culprit != "gt-cul" || rv.Bead != "gt-rv" {
		t.Fatalf("dispatcher read %+v from %s; want the revert of gt-cul as gt-rv", rv, raw)
	}
	// The build's start time crosses the wire too: it is what lets the
	// dispatcher expire a record a crash left with no bead (gt-wgyca).
	if !rv.StartedAt.Equal(started) {
		t.Fatalf("dispatcher read started_at %v, want %v", rv.StartedAt, started)
	}
	// And the beads this hold is about are the ones the owner labels.
	if specdispatch.RedMainHold(specdispatch.Spec{Labels: []string{landworker.LabelRedMain}}, rv) == "" {
		t.Fatal("a bead the red-main owner filed is not held while its revert is in flight")
	}
}

// TestLandingLogDirIsPerLanding is gt-2ycne.2's log half: every landing
// checks out at the same <work root>/wt, so a log directory named from the
// worktree path would put every landing's gate.log in one place. It is named
// by the landing's ID; a context without one keeps the old path-derived name.
func TestLandingLogDirIsPerLanding(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("/work", "gastown", "wt")
	a := landingLogDir(land.WithLandingID(context.Background(), "land-111"), "/logs", dir)
	b := landingLogDir(land.WithLandingID(context.Background(), "land-222"), "/logs", dir)
	if a != filepath.Join("/logs", "land-111") || b != filepath.Join("/logs", "land-222") {
		t.Fatalf("log dirs = %q, %q; want /logs/land-111 and /logs/land-222", a, b)
	}
	if got := landingLogDir(context.Background(), "/logs", filepath.Join("/work", "gastown", "land-9", "wt")); got != filepath.Join("/logs", "land-9") {
		t.Fatalf("log dir without a LandingID = %q, want /logs/land-9", got)
	}
}

// landingWorkerDaemon is a daemon whose landing worker runs without a rig on
// disk: the identity-bead read is answered in process for the fixture's
// "testrig" (rig_status_test.go).
func landingWorkerDaemon(t *testing.T) *Daemon {
	t.Helper()
	f := newRigStatusFakeFixture(t, rigShowOperational)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.daemon.ctx = ctx
	f.daemon.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		LandingWorker: &LandingWorkerConfig{Enabled: true},
	}}
	if ok, why := f.daemon.isRigOperational(f.rigName); !ok {
		t.Fatalf("fixture rig is not operational (%s); the worker would skip its pass", why)
	}
	return f.daemon
}

// gt-fzwcd: the worker passes at once instead of waiting out an interval, so
// a daemon restarted for an upgrade resumes landing the moment it is up.
func TestLandingWorkerPassesAtStart(t *testing.T) {
	t.Parallel()
	d := landingWorkerDaemon(t)
	passed := make(chan struct{})
	pass := func(context.Context) landworker.Report { close(passed); return landworker.Report{} }
	go d.landingWorkerLoop("testrig", time.Hour, pass)
	// The interval is an hour: a worker that waited it out would never pass,
	// and the test binary's timeout reports that with every stack.
	<-passed
}

// gt-fzwcd: the pass that was in flight when the restart went pending is the
// last thing it waits for, so its end wakes the run loop to restart now
// rather than at the next heartbeat (up to 3 min).
func TestLandingWorkerWakesTheRunLoopWhenADrainedPassEnds(t *testing.T) {
	t.Parallel()
	d := landingWorkerDaemon(t)
	started, release := make(chan struct{}), make(chan struct{})
	pass := func(context.Context) landworker.Report {
		close(started)
		<-release
		return landworker.Report{Landed: 1}
	}
	go d.landingWorkerLoop("testrig", time.Hour, pass)
	<-started
	// The heartbeat drains the workers while this pass is in flight.
	d.upgradeRestartPending.Store(true)
	close(release)
	// Never woken = the run loop restarts only at the next heartbeat; the
	// test binary's timeout reports that.
	<-d.landingDrained()
}

// A pass ending with no restart pending must stay silent: a wake per pass
// would run the run loop's restart check over and over.
func TestLandingWorkerDoesNotWakeTheRunLoopWithoutAPendingRestart(t *testing.T) {
	t.Parallel()
	d := landingWorkerDaemon(t)
	// The loop handles one pass's end before it starts the next, so once the
	// second pass has started, a wake for the first would already be sent.
	second, hold := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(hold) })
	var calls atomic.Int32
	pass := func(context.Context) landworker.Report {
		if calls.Add(1) == 2 {
			close(second)
			<-hold
		}
		return landworker.Report{}
	}
	go d.landingWorkerLoop("testrig", time.Nanosecond, pass)
	<-second
	select {
	case <-d.landingDrained():
		t.Fatal("woke the run loop with no restart pending")
	default:
	}
}

// A pass that landed something is followed at once by another, so a bead
// submitted while it ran is not idled for a whole interval.
func TestLandingWorkerPassesAgainAtOnceAfterABusyPass(t *testing.T) {
	t.Parallel()
	d := landingWorkerDaemon(t)
	second, hold := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(hold) })
	var calls atomic.Int32
	pass := func(context.Context) landworker.Report {
		if calls.Add(1) == 2 {
			close(second)
			<-hold
			return landworker.Report{}
		}
		return landworker.Report{Landed: 1}
	}
	// The interval is an hour: a worker that waited it out after the busy
	// first pass would never start the second, and the test binary's timeout
	// reports that with every stack.
	go d.landingWorkerLoop("testrig", time.Hour, pass)
	<-second
}

// A pass that only skipped or failed waits the interval: re-passing at once
// would spin on a queue that cannot land.
func TestLandingWorkerWaitsAfterAFailedOnlyPass(t *testing.T) {
	t.Parallel()
	d := landingWorkerDaemon(t)
	first := make(chan struct{})
	var calls atomic.Int32
	pass := func(context.Context) landworker.Report {
		if calls.Add(1) == 1 {
			close(first)
		}
		return landworker.Report{Failed: 1, Skipped: 1}
	}
	go d.landingWorkerLoop("testrig", time.Hour, pass)
	<-first
	// The loop is now in its hour-long wait or about to enter it; it can only
	// pass again if it skipped the wait, which it does within one scheduling
	// round. Yield a few rounds, then check nothing ran a second pass.
	for i := 0; i < 1000; i++ {
		runtime.Gosched()
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("passes = %d after a failed-only pass, want 1 (it must wait the interval)", n)
	}
}

// TestLandingSlowAlarmWiring: alarm_after sets the threshold (8m when unset or
// invalid), a slow stage files a low-severity escalation keyed by bead and
// stage, and the evidence goes in the landing's own log directory (gt-lcu5p).
func TestLandingSlowAlarmWiring(t *testing.T) {
	t.Parallel()
	d, rec := daemonWithRecorder(t)

	for in, want := range map[string]time.Duration{"": 8 * time.Minute, "3m": 3 * time.Minute, "soon": 8 * time.Minute} {
		if got := d.landingSlowAlarm("gastown", &LandingWorkerConfig{AlarmAfterStr: in}).After; got != want {
			t.Errorf("alarm_after %q = %s, want %s", in, got, want)
		}
	}

	alarm := d.landingSlowAlarm("gastown", &LandingWorkerConfig{})
	alarm.Escalate("gt-abc", "om", "Landing of gt-abc is slow")
	got := rec.Escalations()
	if len(got) != 1 || got[0].Escalation.Severity != "low" || got[0].Escalation.Fingerprint != "landing-slow:gt-abc:om" {
		t.Fatalf("escalations = %+v, want one low-severity alert keyed landing-slow:gt-abc:om", got)
	}

	ctx := land.WithLandingID(context.Background(), "land-123")
	want := filepath.Join(d.config.TownRoot, ".runtime", "landing-logs", "gastown", "land-123")
	if dir := alarm.EvidenceDir(ctx, "/work/wt"); dir != want {
		t.Errorf("evidence dir = %q, want %q", dir, want)
	}
}
