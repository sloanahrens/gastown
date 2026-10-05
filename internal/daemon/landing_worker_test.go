package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// writeDaemonRigConfigFile writes body to <rigPath>/config.json, so a caller
// reads a real rig config file through the same strict loader production uses.
func writeDaemonRigConfigFile(t *testing.T, rigPath, body string) {
	t.Helper()
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rigPath, err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
}

// TestRigDefaultBranch_ValidConfigUnchanged is the positive half of gt-8xk9k:
// a config.json that decodes still supplies default_branch, and nothing is
// reported.
func TestRigDefaultBranch_ValidConfigUnchanged(t *testing.T) {
	t.Parallel()
	rigPath := filepath.Join(t.TempDir(), "testrig")
	writeDaemonRigConfigFile(t, rigPath, `{"type":"rig","version":1,"name":"testrig","default_branch":"develop"}`)

	got, err := rigDefaultBranch(rigPath)
	if err != nil {
		t.Fatalf("rigDefaultBranch() error = %v; want nil", err)
	}
	if got != "develop" {
		t.Errorf("rigDefaultBranch() = %q, want %q", got, "develop")
	}
	if rig.RigConfigWarned(rigPath) {
		t.Errorf("RigConfigWarned(%s) = true for a valid config", rigPath)
	}
}

// TestRigDefaultBranch_AbsentConfigFallsBackToMain pins the case that keeps
// today's behavior through the new error return: a rig with no config.json at
// all has no branch to be wrong about, so main stands (gt-v4r0x).
func TestRigDefaultBranch_AbsentConfigFallsBackToMain(t *testing.T) {
	t.Parallel()
	rigPath := filepath.Join(t.TempDir(), "testrig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rigPath, err)
	}

	got, err := rigDefaultBranch(rigPath)
	if err != nil {
		t.Fatalf("rigDefaultBranch(no config.json) error = %v; want nil", err)
	}
	if got != "main" {
		t.Errorf("rigDefaultBranch(no config.json) = %q, want the main fallback", got)
	}
}

// TestRigDefaultBranch_UnparseableConfigFailsClosed is the fail-closed half of
// gt-v4r0x: a typo'd key yields an error rather than a silent "main" with the
// operator's file looking authoritative.
func TestRigDefaultBranch_UnparseableConfigFailsClosed(t *testing.T) {
	t.Parallel()
	rigPath := filepath.Join(t.TempDir(), "testrig")
	writeDaemonRigConfigFile(t, rigPath, `{"type":"rig","version":1,"name":"testrig","default_branchh":"develop"}`)

	got, err := rigDefaultBranch(rigPath)
	if err == nil {
		t.Fatalf("rigDefaultBranch() = %q, nil; want an error for a config.json that does not decode", got)
	}
	if got != "" {
		t.Errorf("rigDefaultBranch() = %q with an error; want no branch, never the main fallback", got)
	}

	// The same seam on the next pass after the operator fixes the file: the
	// rig is readable again and names its branch (gt-v4r0x).
	writeDaemonRigConfigFile(t, rigPath, `{"type":"rig","version":1,"name":"testrig","default_branch":"develop"}`)
	got, err = rigDefaultBranch(rigPath)
	if err != nil || got != "develop" {
		t.Errorf("rigDefaultBranch() = %q, %v after the file was fixed; want %q, nil", got, err, "develop")
	}
}

// TestNewRigLandingWorker_RefusesARigWhoseConfigDoesNotDecode is the landing
// worker's half of gt-v4r0x: a rig whose config.json exists but does not
// decode gets no worker at all, so the pass never runs and nothing lands for
// it, and the operator gets one escalation naming the file. The rig resumes
// landing on the manager's next attempt once the file is fixed.
func TestNewRigLandingWorker_RefusesARigWhoseConfigDoesNotDecode(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	const rigName = "testrig"
	rigPath := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(filepath.Join(rigPath, ".repo.git"), 0o755); err != nil {
		t.Fatalf("mkdir .repo.git: %v", err)
	}
	writeDaemonRigConfigFile(t, rigPath, `{"type":"rig","version":1,"name":"testrig","default_branchh":"develop"}`)

	rec := notifyfake.New()
	d := &Daemon{logger: discardLogger, config: &Config{TownRoot: townRoot}, notifier: rec}

	if _, err := d.newRigLandingWorker(rigName); err == nil {
		t.Fatal("newRigLandingWorker() = nil error for a config.json that does not decode; want the rig refused")
	} else if configPath := filepath.Join(rigPath, "config.json"); !strings.Contains(err.Error(), configPath) {
		t.Errorf("error %q does not name %s", err, configPath)
	}

	esc := rec.Escalations()
	if len(esc) != 1 {
		t.Fatalf("escalations = %+v, want exactly one", esc)
	}
	if key := "landing-rig-config:" + rigName; esc[0].Escalation.Fingerprint != key {
		t.Errorf("escalation fingerprint = %q, want %q", esc[0].Escalation.Fingerprint, key)
	}
	if !strings.Contains(esc[0].Escalation.Reason, filepath.Join(rigPath, "config.json")) {
		t.Errorf("escalation reason %q does not name the config file", esc[0].Escalation.Reason)
	}
}

func TestLandingWorkerConfigDefaults(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "landing_worker") {
		t.Fatal("landing_worker enabled with no config; it must be opt-in")
	}
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}
	if IsPatrolEnabled(cfg, "landing_worker") {
		t.Fatal("landing_worker enabled with no entry")
	}
	if landingWorkerInterval(cfg) != defaultLandingWorkerInterval || landingRigLandTimeout(cfg) != 30*time.Minute {
		t.Fatal("defaults not applied")
	}
	cfg.Patrols.LandingWorker = &LandingWorkerConfig{Enabled: true, IntervalStr: "2m", Rigs: []string{"gastown"}}
	if !IsPatrolEnabled(cfg, "landing_worker") || landingWorkerInterval(cfg) != 2*time.Minute {
		t.Fatal("configured values not applied")
	}
	if got := landingWorkerRigs(cfg, []string{"beads", "gastown", "hm"}); len(got) != 1 || got[0] != "gastown" {
		t.Fatalf("rigs = %v", got)
	}
}

// TestLandingRigLandTimeout: every rig lands through Forgejo CI, so its
// deadline is the whole pipeline — the CI wait, the om review and the merge —
// and follows the rig's om_timeout (gt-fn9e6.26, gt-fn9e6.32).
func TestLandingRigLandTimeout(t *testing.T) {
	t.Parallel()
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		LandingWorker: &LandingWorkerConfig{Enabled: true},
	}}
	if got, want := landingRigLandTimeout(cfg), 30*time.Minute; got != want {
		t.Fatalf("deadline = %s, want %s (CI 20m + om 5m + merge slack 5m)", got, want)
	}
	cfg.Patrols.LandingWorker.OMTimeoutStr = "12m"
	if got, want := landingRigLandTimeout(cfg), 37*time.Minute; got != want {
		t.Fatalf("with om_timeout 12m: deadline = %s, want %s", got, want)
	}
	// The candidate gate's stage budget follows the CI wait.
	if got, want := landingCIBudget(), 20*time.Minute+land.DefaultCandidateCallTimeout; got != want {
		t.Fatalf("landingCIBudget() = %s, want %s", got, want)
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

// forgejoRigConfig is a rig root config.json naming the rig's Forgejo landing
// block: the operator tier ResolveForgejoConfig reads.
func forgejoRigConfig(t *testing.T, townRoot, rigName string) string {
	t.Helper()
	rigPath := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(filepath.Join(rigPath, ".repo.git"), 0o755); err != nil {
		t.Fatalf("mkdir .repo.git: %v", err)
	}
	writeDaemonRigConfigFile(t, rigPath, `{"type":"rig","version":1,"name":"`+rigName+`","default_branch":"main",
		"merge_queue":{"forgejo":{"remote_url":"https://forgejo.example/gastown/gastown.git","gate_workflow":"gate","bots":{"landing":"gt-landing"}}}}`)
	return rigPath
}

// forgejoLandingRemote gives the fake world's rig repository the remote the
// rig's Forgejo block names, so worker construction resolves it rather than
// failing closed. A named remote carries it: origin is still the pre-cutover
// GitHub remote.
func forgejoLandingRemote(t *testing.T, f *gitfake.Fake, townRoot, rigName string) {
	t.Helper()
	repo := filepath.Join(townRoot, rigName, ".repo.git")
	f.InitBare(t, repo)
	f.AddRemote(t, repo, "origin", "https://github.com/acme/gastown.git")
	f.AddRemote(t, repo, "forgejo", "https://forgejo.example/gastown/gastown.git")
}

// TestNewRigLandingWorker_WiresTheForgejoLanding: a rig with a
// merge_queue.forgejo block lands through CI, so its worker carries the
// candidate gate (slice 5) and the PR merger (slice 6) built from that block.
func TestNewRigLandingWorker_WiresTheForgejoLanding(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	const rigName = "testrig"
	forgejoRigConfig(t, townRoot, rigName)

	tokenDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tokenDir, "forgejo-landing.env"), []byte("FORGEJO_TOKEN=secret\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	d := &Daemon{logger: discardLogger, config: &Config{TownRoot: townRoot}, notifier: notifyfake.New(),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{LandingWorker: &LandingWorkerConfig{
			Forgejo: &config.ForgejoWorkerConfig{TokenDir: tokenDir}}}}}
	// The worker resolves the rig's landing remote through the daemon's git
	// seam, so the unit tier reads a fake world rather than starting git.
	f := useGitfake(t, d)
	forgejoLandingRemote(t, f, townRoot, rigName)

	w, err := d.newRigLandingWorker(rigName)
	if err != nil {
		t.Fatalf("newRigLandingWorker: %v", err)
	}
	lander, ok := w.Lander.(*land.Lander)
	if !ok {
		t.Fatalf("Lander = %T, want *land.Lander", w.Lander)
	}
	gate, ok := lander.Candidate.(*land.CandidateGate)
	if !ok || gate == nil {
		t.Fatalf("Candidate = %T; want the rig's Forgejo gate", lander.Candidate)
	}
	if gate.Owner != "gastown" || gate.RepoName != "gastown" || gate.Workflow != "gate" {
		t.Fatalf("gate %+v; want the owner/repo and workflow from merge_queue.forgejo", gate)
	}
	// The candidate has to reach the Forgejo instance whose CI gates it, so
	// both the gate and the worker push to the remote carrying the configured
	// URL, not an assumed origin (gt-fn9e6.9).
	if gate.Remote != "forgejo" {
		t.Fatalf("gate.Remote = %q; want forgejo, the remote carrying the configured URL", gate.Remote)
	}
	merger, ok := lander.Merger.(*land.ForgejoMerger)
	if !ok || merger == nil {
		t.Fatalf("Merger = %T; want the rig's Forgejo PR merger", lander.Merger)
	}
	if merger.Owner != "gastown" || merger.RepoName != "gastown" {
		t.Fatalf("merger %+v; want the owner/repo from merge_queue.forgejo", merger)
	}
	// The merge's creator check trusts om / review only from this login, so it
	// comes from the same merge_queue.forgejo block the rest of the merger does.
	if merger.BotLogin != "gt-landing" {
		t.Fatalf("merger BotLogin = %q, want the landing bot from merge_queue.forgejo.bots", merger.BotLogin)
	}
}

// TestNewRigLandingWorker_ForgejoRemoteUnmatchedFailsClosed: a rig whose
// Forgejo block names a URL no remote carries must not build a worker at all.
// Building one against origin would push candidates to GitHub, and every
// landing would wait on a Forgejo verdict that never comes (gt-fn9e6.18).
func TestNewRigLandingWorker_ForgejoRemoteUnmatchedFailsClosed(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	const rigName = "testrig"
	forgejoRigConfig(t, townRoot, rigName)

	d := &Daemon{logger: discardLogger, config: &Config{TownRoot: townRoot}, notifier: notifyfake.New()}
	f := useGitfake(t, d)
	repo := filepath.Join(townRoot, rigName, ".repo.git")
	f.InitBare(t, repo)
	f.AddRemote(t, repo, "origin", "https://github.com/acme/gastown.git")

	_, err := d.newRigLandingWorker(rigName)
	if err == nil {
		t.Fatal("newRigLandingWorker() = nil error for a Forgejo URL no remote carries; want the rig refused")
	}
	for _, want := range []string{rigName, "https://forgejo.example/gastown/gastown.git", "origin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

// TestNewRigLandingWorker_NoForgejoConfigFailsClosed: merge_queue.forgejo is
// mandatory for any rig that lands. The local gate and the force-push are gone
// (gt-fn9e6.32), so a rig without the block has no landing path at all: it
// gets no worker and one escalation naming the rig and the file to fix, rather
// than silently falling back to a path that no longer exists.
func TestNewRigLandingWorker_NoForgejoConfigFailsClosed(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	const rigName = "testrig"
	rigPath := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(filepath.Join(rigPath, ".repo.git"), 0o755); err != nil {
		t.Fatalf("mkdir .repo.git: %v", err)
	}
	writeDaemonRigConfigFile(t, rigPath, `{"type":"rig","version":1,"name":"testrig","default_branch":"main"}`)

	rec := notifyfake.New()
	d := &Daemon{logger: discardLogger, config: &Config{TownRoot: townRoot}, notifier: rec}
	_, err := d.newRigLandingWorker(rigName)
	if err == nil {
		t.Fatal("newRigLandingWorker() = nil error for a rig with no merge_queue.forgejo block; want the rig refused")
	}
	if !strings.Contains(err.Error(), rigName) || !strings.Contains(err.Error(), "merge_queue.forgejo") {
		t.Errorf("error %q does not name the rig and the missing block", err)
	}

	esc := rec.Escalations()
	if len(esc) != 1 {
		t.Fatalf("escalations = %+v, want exactly one", esc)
	}
	if key := "landing-rig-config:" + rigName; esc[0].Escalation.Fingerprint != key {
		t.Errorf("escalation fingerprint = %q, want %q", esc[0].Escalation.Fingerprint, key)
	}
	if want := filepath.Join(rigPath, "settings", "config.json"); !strings.Contains(esc[0].Escalation.Reason, want) {
		t.Errorf("escalation reason %q does not name the file to add the block to (%s)", esc[0].Escalation.Reason, want)
	}
}

// TestNewRigLandingWorker_ForgejoWithoutATokenFailsClosed: the rig's only
// landing path is CI, so an unusable landing bot token is a rig that must not
// land at all.
func TestNewRigLandingWorker_ForgejoWithoutATokenFailsClosed(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	const rigName = "testrig"
	forgejoRigConfig(t, townRoot, rigName)

	d := &Daemon{logger: discardLogger, config: &Config{TownRoot: townRoot}, notifier: notifyfake.New(),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{LandingWorker: &LandingWorkerConfig{
			Forgejo: &config.ForgejoWorkerConfig{TokenDir: t.TempDir()}}}}}
	// The worker resolves the rig's landing remote through the daemon's git
	// seam, so the unit tier reads a fake world rather than starting git.
	f := useGitfake(t, d)
	forgejoLandingRemote(t, f, townRoot, rigName)

	if _, err := d.newRigLandingWorker(rigName); err == nil {
		t.Fatal("newRigLandingWorker() = nil error without a landing bot token; want the rig refused")
	} else if !strings.Contains(err.Error(), "token") {
		t.Fatalf("error %q does not name the token", err)
	}
}

// candidateGateResult and localGateResult are the two shapes a landing
// record's gate_result takes: the Forgejo path records one step named ci, the
// local gate names lint and gate (gt-fn9e6.24).
const (
	candidateGateResult = "pass (ci exit 0 1m30s)"
	localGateResult     = "pass (lint exit 0 16s, gate exit 0 1m44s)"
)

// TestLastLandedCandidateReadsTheWorkersOwnLandings: the startup context check
// reads commits the worker itself landed whose candidates CI tested.
func TestLastLandedCandidateReadsTheWorkersOwnLandings(t *testing.T) {
	t.Parallel()
	landings, err := land.RigLandingsFile(t.TempDir(), "testrig")
	if err != nil {
		t.Fatalf("RigLandingsFile: %v", err)
	}
	if got := lastLandedCandidate(landings, t.Logf, "testrig"); got != "" {
		t.Fatalf("lastLandedCandidate on an empty file = %q, want empty", got)
	}
	for _, rec := range []land.LandingRecord{
		{BeadID: "gt-a", Route: "daemon", GateResult: candidateGateResult, LandedCommit: "aaaa"},
		{BeadID: "gt-b", Route: "crew", GateResult: candidateGateResult, LandedCommit: "bbbb"},
		{BeadID: "gt-c", Route: "daemon", GateResult: candidateGateResult, LandedCommit: "cccc"},
	} {
		if err := landings.Append(rec); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if got := lastLandedCandidate(landings, t.Logf, "testrig"); got != "cccc" {
		t.Fatalf("lastLandedCandidate = %q, want the newest daemon landing whose candidate the gate tested", got)
	}
}

// TestLastLandedCandidateIgnoresLocalGateLandings: a rig that has just cut
// over holds only pre-cutover records, which never went up as candidates, so
// the check has no evidence commit and must stay quiet.
func TestLastLandedCandidateIgnoresLocalGateLandings(t *testing.T) {
	t.Parallel()
	landings, err := land.RigLandingsFile(t.TempDir(), "testrig")
	if err != nil {
		t.Fatalf("RigLandingsFile: %v", err)
	}
	for _, rec := range []land.LandingRecord{
		{BeadID: "gt-a", Route: "daemon", GateResult: localGateResult, LandedCommit: "aaaa"},
		{BeadID: "gt-b", Route: "daemon", GateResult: localGateResult, LandedCommit: "bbbb"},
	} {
		if err := landings.Append(rec); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if got := lastLandedCandidate(landings, t.Logf, "testrig"); got != "" {
		t.Fatalf("lastLandedCandidate = %q, want empty when only local-gate landings exist", got)
	}
}

// TestLastLandedCandidateSkipsALaterLocalGateLanding: a mixed history reads
// the newest candidate-gate landing, not a later local-gate one: the local
// landing's commit never went up as a candidate, so it is not evidence.
func TestLastLandedCandidateSkipsALaterLocalGateLanding(t *testing.T) {
	t.Parallel()
	landings, err := land.RigLandingsFile(t.TempDir(), "testrig")
	if err != nil {
		t.Fatalf("RigLandingsFile: %v", err)
	}
	for _, rec := range []land.LandingRecord{
		{BeadID: "gt-a", Route: "daemon", GateResult: localGateResult, LandedCommit: "aaaa"},
		{BeadID: "gt-b", Route: "daemon", GateResult: candidateGateResult, LandedCommit: "bbbb"},
		{BeadID: "gt-c", Route: "daemon", GateResult: localGateResult, LandedCommit: "cccc"},
	} {
		if err := landings.Append(rec); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if got := lastLandedCandidate(landings, t.Logf, "testrig"); got != "bbbb" {
		t.Fatalf("lastLandedCandidate = %q, want the candidate-gate landing bbbb, not the later local-gate cccc", got)
	}
}

// TestVerifyForgejoGateStaysQuietWithoutACandidateLanding: a rig with only
// local-gate records logs the existing "no landed candidate yet" line and
// raises nothing, rather than flagging the workflow as missing from a
// pre-cutover landing.
func TestVerifyForgejoGateStaysQuietWithoutACandidateLanding(t *testing.T) {
	t.Parallel()
	landings, err := land.RigLandingsFile(t.TempDir(), "testrig")
	if err != nil {
		t.Fatalf("RigLandingsFile: %v", err)
	}
	for _, rec := range []land.LandingRecord{
		{BeadID: "gt-a", Route: "daemon", GateResult: localGateResult, LandedCommit: "aaaa"},
		{BeadID: "gt-b", Route: "daemon", GateResult: localGateResult, LandedCommit: "bbbb"},
	} {
		if err := landings.Append(rec); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	rec := notifyfake.New()
	var logged bytes.Buffer
	d := &Daemon{logger: log.New(&logged, "", 0), notifier: rec}
	// The no-candidate path returns before the gate is used, so a nil gate
	// proves the check did not read the landing's tree.
	d.verifyForgejoGate("testrig", nil, "", "gate", landings)

	if !strings.Contains(logged.String(), "no landed candidate yet") {
		t.Errorf("log %q does not say there is no landed candidate yet", logged.String())
	}
	if esc := rec.Escalations(); len(esc) != 0 {
		t.Errorf("escalations = %+v, want none for a rig with no candidate-gate landing", esc)
	}
}

// signalingNotifier is a notifyfake.Recorder that also hands each escalation
// to the test as it lands. The stuck-landing watch raises from its own
// goroutine, so a test awaits the alert on a channel rather than sleeping.
type signalingNotifier struct {
	*notifyfake.Recorder
	escalated chan notify.Escalation
}

func newSignalingNotifier() *signalingNotifier {
	return &signalingNotifier{Recorder: notifyfake.New(), escalated: make(chan notify.Escalation, 4)}
}

func (n *signalingNotifier) Escalate(ctx context.Context, e notify.Escalation) error {
	err := n.Recorder.Escalate(ctx, e)
	n.escalated <- e
	return err
}

// forgejoLandingWorker builds one Forgejo rig's landing worker on the fake git
// world, with its alerts recorded and its clock the caller advances.
func forgejoLandingWorker(t *testing.T, rigName string, clk clockwork.Clock, rec notify.Notifier) *landworker.Worker {
	t.Helper()
	townRoot := t.TempDir()
	forgejoRigConfig(t, townRoot, rigName)
	tokenDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tokenDir, "forgejo-landing.env"), []byte("FORGEJO_TOKEN=secret\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	d := &Daemon{logger: discardLogger, config: &Config{TownRoot: townRoot}, notifier: rec, clock: clk,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{LandingWorker: &LandingWorkerConfig{
			Forgejo: &config.ForgejoWorkerConfig{TokenDir: tokenDir}}}}}
	f := useGitfake(t, d)
	forgejoLandingRemote(t, f, townRoot, rigName)
	w, err := d.newRigLandingWorker(rigName)
	if err != nil {
		t.Fatalf("newRigLandingWorker: %v", err)
	}
	return w
}

// TestLandingStuckAlertRaisesAndClears: a landing on a Forgejo rig that is
// still in flight past the CI wait raises one landing-stuck:<rig> alert
// naming the bead, the rig and the candidate branch the gate pushed, and the
// alert clears when the landing leaves flight (gt-fn9e6.27).
func TestLandingStuckAlertRaisesAndClears(t *testing.T) {
	t.Parallel()
	const rigName = "testrig"
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC))
	rec := newSignalingNotifier()
	w := forgejoLandingWorker(t, rigName, clk, rec)

	w.Active("gt-a")
	clk.BlockUntil(1) // the watch's timer is armed before the clock moves
	clk.Advance(landingStuckAfter() + time.Minute)

	esc := <-rec.escalated
	if want := "landing-stuck:" + rigName; esc.Fingerprint != want {
		t.Errorf("alert fingerprint = %q, want %q", esc.Fingerprint, want)
	}
	for _, want := range []string{"gt-a", rigName, "land/gt-a", landingStuckAfter().String()} {
		if !strings.Contains(esc.Reason, want) {
			t.Errorf("alert reason %q does not name %q", esc.Reason, want)
		}
	}
	if got := rec.Escalations(); len(got) != 1 {
		t.Fatalf("escalations = %d, want one alert for one stuck landing", len(got))
	}

	w.Active("")
	clears := rec.Clears()
	wantKey := "landing-stuck:" + rigName
	if len(clears) != 1 || len(clears[0].Fingerprints) != 1 || clears[0].Fingerprints[0] != wantKey {
		t.Errorf("clears = %+v, want one under %s when the landing ends", clears, wantKey)
	}
}

// TestLandingStuckAlertStaysQuietUnderTheCIWait: a landing still inside the
// wait is healthy work, not an alert, and leaving flight clears nothing
// because nothing was raised (gt-fn9e6.27).
func TestLandingStuckAlertStaysQuietUnderTheCIWait(t *testing.T) {
	t.Parallel()
	const rigName = "testrig"
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC))
	rec := newSignalingNotifier()
	w := forgejoLandingWorker(t, rigName, clk, rec)

	w.Active("gt-a")
	clk.BlockUntil(1)
	clk.Advance(landingStuckAfter() - time.Minute)
	w.Active("")

	if esc := rec.Escalations(); len(esc) != 0 {
		t.Errorf("escalations = %+v, want none before the CI wait is out", esc)
	}
	if clears := rec.Clears(); len(clears) != 0 {
		t.Errorf("clears = %+v, want none for an alert that never fired", clears)
	}
}

// TestLandingStuckWatchSurvivesAPanickingAlert: the alert path runs on the
// watch's own goroutine, so a panic there must neither take the daemon down
// nor wedge the end that follows it (gt-fn9e6.27). end() blocking forever is
// the failure this guards, so a hang here is the test failing.
func TestLandingStuckWatchSurvivesAPanickingAlert(t *testing.T) {
	t.Parallel()
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC))
	w := &landingStuckWatch{
		rig:   "testrig",
		clk:   clk,
		after: landingStuckAfter(),
		raise: func(string) { panic("the alert path is broken") },
		clear: func() {},
		logf:  func(string, ...any) {},
	}
	end := w.begin("gt-a")
	clk.BlockUntil(1)
	clk.Advance(landingStuckAfter() + time.Minute)
	end()
}
