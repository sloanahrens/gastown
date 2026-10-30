package daemon

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/ominstaller"
)

const (
	rebuildOMStaleSHA = "1111111111111111111111111111111111111111"
	rebuildOMHeadSHA  = "2222222222222222222222222222222222222222"
)

// rebuildOMTown is a town with an om clone already in place, the installed om
// one commit behind the clone's origin/main, a quiet gate, and a fake clock.
// Nothing it does starts a process. An install the test did not ask for fails
// the test: a test that expects one replaces the install with fakeOMInstall.
func rebuildOMTown(t *testing.T) (*Daemon, *notifyfake.Recorder, *clockwork.FakeClock) {
	t.Helper()
	d, rec := daemonWithRecorder(t)
	clk := newFixedClock()
	d.clock = clk
	mkdirs(t, filepath.Join(d.config.TownRoot, rebuildOMSrcDir, ".git"))

	d.omVersionFn = func(string) (string, error) { return rebuildOMStaleSHA, nil }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	d.rebuildOMInstallFn = func(context.Context, ominstaller.Config) (ominstaller.Result, ominstaller.Exit) {
		t.Error("an install ran that this test did not expect")
		return ominstaller.Result{}, ominstaller.ExitFailed
	}
	return d, rec, clk
}

// rebuildOMCli answers the git reads a cycle makes with head as origin/main's
// tip. Everything unrecognized succeeds with no output.
func rebuildOMCli(t *testing.T, d *Daemon, head string) *fakeCLI {
	t.Helper()
	return withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "rev-parse"):
			return cliReply{stdout: head + "\n"}
		case gitSub(c, "remote"):
			return cliReply{stdout: "https://example.invalid/om.git\n"}
		}
		return cliReply{}
	})
}

// omInstall is an install the cycle runs in process. It records each config it
// is run with and answers res and exit, so no build, sign or swap runs.
type omInstall struct {
	runs []ominstaller.Config
	res  ominstaller.Result
	exit ominstaller.Exit
}

// fakeOMInstall makes the cycle's install answer res and exit, and returns the
// record of the runs it is given.
func fakeOMInstall(d *Daemon, res ominstaller.Result, exit ominstaller.Exit) *omInstall {
	inst := &omInstall{res: res, exit: exit}
	d.rebuildOMInstallFn = func(_ context.Context, cfg ominstaller.Config) (ominstaller.Result, ominstaller.Exit) {
		inst.runs = append(inst.runs, cfg)
		return inst.res, inst.exit
	}
	return inst
}

// gitCalls returns the git argv of every call whose subcommand is sub.
func gitCalls(cli *fakeCLI, sub string) [][]string {
	var out [][]string
	for _, c := range cli.recorded() {
		if c.name == "git" && slices.Contains(c.args, sub) {
			out = append(out, c.args)
		}
	}
	return out
}

func escalationKeys(rec *notifyfake.Recorder) []string {
	var keys []string
	for _, c := range rec.Escalations() {
		keys = append(keys, c.Escalation.Fingerprint)
	}
	return keys
}

// TestRebuildOMCycle_StaleAndQuietInstallsOnce is the case the job exists for:
// an om behind origin/main, a quiet town, one install in process that builds
// the job's clone and replaces the om the landing worker uses.
func TestRebuildOMCycle_StaleAndQuietInstallsOnce(t *testing.T) {
	t.Parallel()
	d, rec, _ := rebuildOMTown(t)
	inst := fakeOMInstall(d, ominstaller.Result{Event: "installed", Commit: rebuildOMHeadSHA, Prev: rebuildOMStaleSHA}, ominstaller.ExitInstalled)
	cli := rebuildOMCli(t, d, rebuildOMHeadSHA)

	if settled := d.runRebuildOM(); !settled {
		t.Fatal("a completed install must hold the interval, not retry the next heartbeat")
	}

	if len(inst.runs) != 1 {
		t.Fatalf("installs = %d, want exactly one", len(inst.runs))
	}
	cfg := inst.runs[0]
	src := filepath.Join(d.config.TownRoot, rebuildOMSrcDir)
	if cfg.OMPath != d.omSkewPath() {
		t.Errorf("OMPath = %q, want the om the landing worker resolves, %q", cfg.OMPath, d.omSkewPath())
	}
	if cfg.SrcDir != src {
		t.Errorf("SrcDir = %q, want the job's clone %q", cfg.SrcDir, src)
	}
	if cfg.TownRoot != d.config.TownRoot {
		t.Errorf("TownRoot = %q, want %q", cfg.TownRoot, d.config.TownRoot)
	}
	if want := filepath.Join(d.config.TownRoot, "daemon"); cfg.DaemonDir != want {
		t.Errorf("DaemonDir = %q, want %q", cfg.DaemonDir, want)
	}
	if cfg.Source != "rebuild-om" {
		t.Errorf("Source = %q, want rebuild-om", cfg.Source)
	}
	if cfg.LockWait != 60*time.Second {
		t.Errorf("LockWait = %v, want the job's 60s install-lock wait", cfg.LockWait)
	}
	if mb := gitCalls(cli, "merge-base"); len(mb) != 1 {
		t.Errorf("ancestry checks = %v, want one before the install (the installed om is an ancestor of origin/main)", mb)
	}
	if merges := gitCalls(cli, "merge"); len(merges) != 1 || !slices.Contains(merges[0], "--ff-only") {
		t.Errorf("merges = %v, want one fast-forward of the clone before the install", merges)
	}
	if esc := rec.Escalations(); len(esc) != 0 {
		t.Errorf("a clean install escalated: %+v", esc)
	}
}

// TestRebuildOMCycle_InstalledOmNotBehindIsLeftInPlace pins the downgrade
// guard: an installed om the clone does not show as an ancestor of origin/main
// (a hotfix ahead of it, a diverged build, a sha the clone never fetched) is
// not replaced, however many cycles run, and the skip is not a starvation.
func TestRebuildOMCycle_InstalledOmNotBehindIsLeftInPlace(t *testing.T) {
	t.Parallel()
	cases := map[string]cliReply{
		"installed is a descendant of origin/main": {code: 1},
		"installed is unknown to the clone":        {stderr: "fatal: Not a valid commit name", code: 128},
	}
	for name, ancestry := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, rec, clk := rebuildOMTown(t)
			d.landingPasses.Store(1) // a busy town must not age this into a starved alert
			inst := fakeOMInstall(d, ominstaller.Result{}, ominstaller.ExitFailed)
			cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
				switch {
				case gitSub(c, "merge-base"):
					return ancestry
				case gitSub(c, "rev-parse"):
					return cliReply{stdout: rebuildOMHeadSHA + "\n"}
				}
				return cliReply{}
			})

			clk.Advance(rebuildOMStarveAfter + time.Minute)
			for range 2 {
				if settled := d.runRebuildOM(); !settled {
					t.Fatal("leaving an om that is not behind in place is a verdict and holds the interval")
				}
			}

			if len(inst.runs) != 0 {
				t.Errorf("an om that is not behind origin/main was replaced: %d installs", len(inst.runs))
			}
			if merges := gitCalls(cli, "merge"); len(merges) != 0 {
				t.Errorf("a skipped cycle moved the clone's checkout: %v", merges)
			}
			mb := gitCalls(cli, "merge-base")
			if len(mb) == 0 {
				t.Fatal("no ancestry check ran")
			}
			if got := strings.Join(mb[0][len(mb[0])-3:], " "); got != "--is-ancestor "+rebuildOMStaleSHA+" "+rebuildOMHeadSHA {
				t.Errorf("ancestry asked %q, want installed then origin/main", got)
			}
			if esc := rec.Escalations(); len(esc) != 0 {
				t.Errorf("a deliberate newer om escalated: %+v", esc)
			}
		})
	}
}

// TestRebuildOMCycle_AncestryCheckFailureDefers pins that a merge-base that
// failed to run says nothing about ancestry: the cycle defers, leaves the
// starvation block open and the starved alert uncleared, and logs git's error.
func TestRebuildOMCycle_AncestryCheckFailureDefers(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		reply cliReply
		logs  string
	}{
		"exit 128 on a broken clone": {cliReply{stderr: "fatal: not a git repository", code: 128}, "fatal: not a git repository"},
		"killed by the timeout":      {cliReply{code: -1}, "exit status -1"},
		"any other exit":             {cliReply{stderr: "error: object file is empty", code: 2}, "error: object file is empty"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, rec, clk := rebuildOMTown(t)
			var logs strings.Builder
			d.logger = log.New(&logs, "", 0)
			inst := fakeOMInstall(d, ominstaller.Result{}, ominstaller.ExitFailed)
			cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
				switch {
				case gitSub(c, "merge-base"):
					return tc.reply
				case gitSub(c, "rev-parse"):
					return cliReply{stdout: rebuildOMHeadSHA + "\n"}
				}
				return cliReply{}
			})
			d.rebuildOMBlock.open(clk.Now().Add(-time.Hour))

			if settled := d.runRebuildOM(); settled {
				t.Fatal("a failed ancestry check is no verdict and must retry on the next heartbeat")
			}

			if len(gitCalls(cli, "merge-base")) == 0 {
				t.Fatal("no ancestry check ran")
			}
			if len(inst.runs) != 0 {
				t.Errorf("an om not shown to be behind was replaced: %d installs", len(inst.runs))
			}
			if age, open := d.rebuildOMBlock.openFor(clk.Now()); !open || age != time.Hour {
				t.Errorf("block = (%v, open=%v), want it left open at its hour", age, open)
			}
			if clears := rec.Clears(); len(clears) != 0 {
				t.Errorf("a failed ancestry check cleared alerts: %+v", clears)
			}
			if !strings.Contains(logs.String(), tc.logs) {
				t.Errorf("log does not name the git error %q:\n%s", tc.logs, logs.String())
			}
		})
	}
}

// TestRebuildOMCycle_FreshOmClearsEveryAlert pins the close: an om at origin's
// tip installs nothing, ends the starvation episode, and clears both alerts.
func TestRebuildOMCycle_FreshOmClearsEveryAlert(t *testing.T) {
	t.Parallel()
	d, rec, clk := rebuildOMTown(t)
	d.omVersionFn = func(string) (string, error) { return rebuildOMHeadSHA, nil }
	inst := fakeOMInstall(d, ominstaller.Result{}, ominstaller.ExitFailed)
	rebuildOMCli(t, d, rebuildOMHeadSHA)
	d.rebuildOMBlock.open(clk.Now().Add(-time.Hour))

	if settled := d.runRebuildOM(); !settled {
		t.Fatal("a fresh om is a verdict and holds the interval")
	}

	if len(inst.runs) != 0 {
		t.Errorf("a fresh om ran the installer: %d runs", len(inst.runs))
	}
	if _, open := d.rebuildOMBlock.openFor(clk.Now()); open {
		t.Error("the starvation episode stayed open over a fresh om")
	}
	clears := rec.Clears()
	if len(clears) != 1 {
		t.Fatalf("clears = %+v, want one", clears)
	}
	got := clears[0].Fingerprints
	for _, key := range []string{alertKeyRebuildOMStarved, alertKeyRebuildOMInstallFail} {
		if !slices.Contains(got, key) {
			t.Errorf("clear %v does not cover %s", got, key)
		}
	}
}

// TestRebuildOMCycle_BusyTownDefersThenEscalatesStarved pins the quiet-town
// gate: a landing or a gate suite defers the install and keeps the cycle
// retrying; past the starve window the block raises rebuild-om:starved once.
// The drain wait is configured past the starve window here, since a block the
// drain clears past it is the case the drain tests cover (gt-0hsf6).
func TestRebuildOMCycle_BusyTownDefersThenEscalatesStarved(t *testing.T) {
	t.Parallel()
	cases := map[string]func(d *Daemon){
		"a landing in flight": func(d *Daemon) { d.landingPasses.Store(1) },
		"a gate suite holds a slot": func(d *Daemon) {
			d.rebuildGTGateFn = func() (string, bool) { return "a gate suite holds a slot (gastown/landing)", true }
		},
	}
	for name, makeBusy := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, rec, clk := rebuildOMTown(t)
			d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
				RebuildOM: &PatrolConfig{Enabled: true, DrainAfter: (rebuildOMStarveAfter + time.Hour).String()},
			}}
			makeBusy(d)
			inst := fakeOMInstall(d, ominstaller.Result{}, ominstaller.ExitFailed)
			cli := rebuildOMCli(t, d, rebuildOMHeadSHA)

			if settled := d.runRebuildOM(); settled {
				t.Fatal("a deferral accomplished nothing and must retry next heartbeat")
			}
			if len(inst.runs) != 0 {
				t.Fatalf("a busy town ran the installer: %d runs", len(inst.runs))
			}
			if merges := gitCalls(cli, "merge"); len(merges) != 0 {
				t.Errorf("a deferred cycle moved the clone's checkout: %v", merges)
			}
			if esc := rec.Escalations(); len(esc) != 0 {
				t.Fatalf("a young block escalated: %+v", esc)
			}

			clk.Advance(rebuildOMStarveAfter + time.Minute)
			d.runRebuildOM()
			d.runRebuildOM()

			if keys := escalationKeys(rec); len(keys) != 1 || keys[0] != alertKeyRebuildOMStarved {
				t.Fatalf("escalations = %v, want exactly one %s", keys, alertKeyRebuildOMStarved)
			}
		})
	}
}

// rebuildOMStaleBlock makes the current block older than the drain wait: the om
// has been stale and uninstalled long enough that the job holds landings off
// for it.
func rebuildOMStaleBlock(t *testing.T, d *Daemon, clk clockwork.Clock) {
	t.Helper()
	d.rebuildOMBlock.open(clk.Now().Add(-d.rebuildOMDrainAfter() - time.Minute))
}

// rebuildOMCycleInBackground runs one cycle on its own goroutine, with the
// drain hook telling the test the landing hold is on.
func rebuildOMCycleInBackground(t *testing.T, d *Daemon) (done <-chan bool, held <-chan struct{}) {
	t.Helper()
	heldCh := make(chan struct{})
	d.rebuildOMDrainFn = func() { close(heldCh) }
	doneCh := make(chan bool, 1)
	go func() { doneCh <- d.runRebuildOM() }()
	return doneCh, heldCh
}

// TestRebuildOMCycle_DrainInstallsOnceThePassesFinish is the starvation fix: a
// stale om blocked by a landing pass takes the same hold the upgrade restart
// takes, installs as soon as that pass ends, and releases the hold, so steady
// landings no longer starve the install (gt-0hsf6).
func TestRebuildOMCycle_DrainInstallsOnceThePassesFinish(t *testing.T) {
	t.Parallel()
	d, _, clk := rebuildOMTown(t)
	d.landingPasses.Store(1)
	rebuildOMStaleBlock(t, d, clk)
	inst := fakeOMInstall(d, ominstaller.Result{Event: "installed", Commit: rebuildOMHeadSHA, Prev: rebuildOMStaleSHA}, ominstaller.ExitInstalled)
	rebuildOMCli(t, d, rebuildOMHeadSHA)
	done, held := rebuildOMCycleInBackground(t, d)

	<-held
	if !d.landingHolds.heldBy(landingHoldRebuildOM) {
		t.Fatal("the cycle waits for a landing pass without holding new ones off")
	}
	// The pass in flight ends; the hold keeps another from taking its place.
	d.landingPassEnded()

	if settled := <-done; !settled {
		t.Fatal("the install is a verdict and holds the interval")
	}
	if len(inst.runs) != 1 {
		t.Fatalf("installs = %d, want exactly one", len(inst.runs))
	}
	if d.landingHolds.held() {
		t.Error("the hold outlived the cycle, so no landing pass starts again")
	}
}

// TestRebuildOMCycle_DrainReleasedAfterAFailedInstall pins the release on the
// failure path: the om stays as it was, and a landing pass starts again.
func TestRebuildOMCycle_DrainReleasedAfterAFailedInstall(t *testing.T) {
	t.Parallel()
	d, rec, clk := rebuildOMTown(t)
	d.landingPasses.Store(1)
	rebuildOMStaleBlock(t, d, clk)
	fakeOMInstall(d, ominstaller.Result{Event: "failed", Reason: "build-failed"}, ominstaller.ExitFailed)
	rebuildOMCli(t, d, rebuildOMHeadSHA)
	done, held := rebuildOMCycleInBackground(t, d)

	<-held
	d.landingPassEnded()

	if settled := <-done; !settled {
		t.Fatal("a failed install is a verdict and holds the interval")
	}
	if keys := escalationKeys(rec); !slices.Contains(keys, alertKeyRebuildOMInstallFail) {
		t.Errorf("escalations = %v, want %s", keys, alertKeyRebuildOMInstallFail)
	}
	if d.landingHolds.held() {
		t.Error("a failed install left landings held off")
	}
}

// TestRebuildOMCycle_DrainTimedOutReleasesTheHold pins the bound: past
// rebuildOMDrainBound with the pass still in flight the cycle installs nothing
// (a pass is reviewed by the om being replaced), releases the hold and retries
// on the next heartbeat.
func TestRebuildOMCycle_DrainTimedOutReleasesTheHold(t *testing.T) {
	t.Parallel()
	d, _, clk := rebuildOMTown(t)
	d.landingPasses.Store(1)
	rebuildOMStaleBlock(t, d, clk)
	inst := fakeOMInstall(d, ominstaller.Result{Event: "installed", Commit: rebuildOMHeadSHA}, ominstaller.ExitInstalled)
	rebuildOMCli(t, d, rebuildOMHeadSHA)
	done, held := rebuildOMCycleInBackground(t, d)

	<-held
	// The pass never ends: the drain stops waiting at its bound.
	if settled := driveClock(t, clk, rebuildOMDrainBound, done); settled {
		t.Fatal("a drain that timed out installed beside the pass it waited for")
	}
	if len(inst.runs) != 0 {
		t.Errorf("installs = %d, want none while a pass is in flight", len(inst.runs))
	}
	if d.landingHolds.held() {
		t.Error("the drain timed out and left landings held off")
	}
}

// TestRebuildOMCycle_DrainCutOffReleasesTheHold pins the budget path: a cycle
// whose context ends inside the drain wait installs nothing, holds no verdict,
// and gives the landings back.
func TestRebuildOMCycle_DrainCutOffReleasesTheHold(t *testing.T) {
	t.Parallel()
	d, _, clk := rebuildOMTown(t)
	d.landingPasses.Store(1)
	rebuildOMStaleBlock(t, d, clk)
	inst := fakeOMInstall(d, ominstaller.Result{Event: "installed", Commit: rebuildOMHeadSHA}, ominstaller.ExitInstalled)
	rebuildOMCli(t, d, rebuildOMHeadSHA)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.rebuildOMDrainFn = cancel // the budget ends while the drain waits
	cycle := d.startDogCycle("rebuild_om")
	defer cycle.close()

	if settled := d.rebuildOMCycle(ctx, cycle); settled {
		t.Fatal("a drain cut off by the budget must retry on the next heartbeat")
	}
	if len(inst.runs) != 0 {
		t.Errorf("installs = %d, want none after a cut-off drain", len(inst.runs))
	}
	if d.landingHolds.held() {
		t.Error("a cut-off drain left landings held off")
	}
}

// TestRebuildOMCycle_NoDrainWithoutAStaleBlockOrALandingPass pins the two
// conditions the drain needs: a block younger than the wait is ordinary churn,
// and a gate suite is not the rebuild's to stop, so neither holds landings off.
func TestRebuildOMCycle_NoDrainWithoutAStaleBlockOrALandingPass(t *testing.T) {
	t.Parallel()
	cases := map[string]func(d *Daemon, clk clockwork.Clock){
		"a block younger than the drain wait": func(d *Daemon, clk clockwork.Clock) {
			d.landingPasses.Store(1)
			d.rebuildOMBlock.open(clk.Now())
		},
		"a gate suite with no landing pass": func(d *Daemon, clk clockwork.Clock) {
			d.rebuildGTGateFn = func() (string, bool) { return "a gate suite holds a slot (gastown/landing)", true }
			rebuildOMStaleBlock(t, d, clk)
		},
	}
	for name, setUp := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, _, clk := rebuildOMTown(t)
			setUp(d, clk)
			inst := fakeOMInstall(d, ominstaller.Result{}, ominstaller.ExitFailed)
			rebuildOMCli(t, d, rebuildOMHeadSHA)
			d.rebuildOMDrainFn = func() {
				t.Error("the cycle held landing passes off for a blocker the drain cannot clear")
			}

			if settled := d.runRebuildOM(); settled {
				t.Fatal("a deferral accomplished nothing and must retry next heartbeat")
			}
			if len(inst.runs) != 0 {
				t.Errorf("installs = %d, want none in a busy town", len(inst.runs))
			}
			if d.landingHolds.held() {
				t.Error("a hold was left behind")
			}
		})
	}
}

// TestRebuildOMDrainAfter pins the configured wait: the default when the key is
// absent, the configured value when it parses, and the default with a line
// naming the bad value when it does not.
func TestRebuildOMDrainAfter(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		config *DaemonPatrolConfig
		want   time.Duration
		warn   string
	}{
		{"no config", nil, defaultRebuildOMDrainAfter, ""},
		{"no patrols", &DaemonPatrolConfig{}, defaultRebuildOMDrainAfter, ""},
		{"no rebuild_om", &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, defaultRebuildOMDrainAfter, ""},
		{"no drain_after", &DaemonPatrolConfig{Patrols: &PatrolsConfig{RebuildOM: &PatrolConfig{Enabled: true}}}, defaultRebuildOMDrainAfter, ""},
		{"configured", &DaemonPatrolConfig{Patrols: &PatrolsConfig{RebuildOM: &PatrolConfig{DrainAfter: "30m"}}}, 30 * time.Minute, ""},
		{"not a duration", &DaemonPatrolConfig{Patrols: &PatrolsConfig{RebuildOM: &PatrolConfig{DrainAfter: "soon"}}}, defaultRebuildOMDrainAfter, `"soon"`},
		{"not positive", &DaemonPatrolConfig{Patrols: &PatrolsConfig{RebuildOM: &PatrolConfig{DrainAfter: "-5m"}}}, defaultRebuildOMDrainAfter, `"-5m"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logs strings.Builder
			d := &Daemon{logger: log.New(&logs, "", 0), patrolConfig: tc.config}

			if got := d.rebuildOMDrainAfter(); got != tc.want {
				t.Errorf("drain wait = %v, want %v", got, tc.want)
			}
			if got := strings.Contains(logs.String(), tc.warn); tc.warn != "" && !got {
				t.Errorf("a bad drain_after was not named in the log: %q", logs.String())
			} else if tc.warn == "" && logs.Len() != 0 {
				t.Errorf("a usable drain_after logged: %q", logs.String())
			}
		})
	}
}

// TestRebuildOMCycle_InstallOutcomes maps each install exit to what the job
// does: a verdict holds the interval, a busy lock retries, and only a failure
// raises the keyed alert.
func TestRebuildOMCycle_InstallOutcomes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		res         ominstaller.Result
		exit        ominstaller.Exit
		wantSettled bool
		wantAlerts  []string
	}{
		{"installed holds the interval",
			ominstaller.Result{Event: "installed", Commit: rebuildOMHeadSHA, Prev: rebuildOMStaleSHA}, ominstaller.ExitInstalled, true, nil},
		{"noop is in force",
			ominstaller.Result{Event: "noop", Commit: rebuildOMHeadSHA, Prev: rebuildOMHeadSHA, Reason: "already-installed"}, ominstaller.ExitInstalled, true, nil},
		{"refused holds the interval",
			ominstaller.Result{Event: "refused", Reason: "dirty"}, ominstaller.ExitRefused, true, nil},
		{"busy retries",
			ominstaller.Result{Event: "refused", Reason: "lock-busy"}, ominstaller.ExitBusy, false, nil},
		{"build failed keeps the old om and alerts once",
			ominstaller.Result{Event: "failed", Commit: rebuildOMHeadSHA, Prev: rebuildOMStaleSHA, Reason: "build-failed"}, ominstaller.ExitFailed, true, []string{alertKeyRebuildOMInstallFail}},
		{"rolled back alerts once",
			ominstaller.Result{Event: "rolled_back", Commit: rebuildOMHeadSHA, Prev: rebuildOMStaleSHA, Reason: "smoke-failed"}, ominstaller.ExitFailed, true, []string{alertKeyRebuildOMInstallFail}},
		{"a failure with no reason alerts once",
			ominstaller.Result{}, ominstaller.ExitFailed, true, []string{alertKeyRebuildOMInstallFail}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, rec, _ := rebuildOMTown(t)
			rebuildOMCli(t, d, rebuildOMHeadSHA)
			fakeOMInstall(d, tc.res, tc.exit)

			if settled := d.runRebuildOM(); settled != tc.wantSettled {
				t.Errorf("settled = %v, want %v", settled, tc.wantSettled)
			}
			if keys := escalationKeys(rec); !slices.Equal(keys, tc.wantAlerts) {
				t.Errorf("escalations = %v, want %v", keys, tc.wantAlerts)
			}
		})
	}
}

// TestRebuildOMCycle_InstallCutOffByBudgetIsNoVerdict pins that an install the
// cycle's budget cut off holds no verdict, even when the install reports a
// failure: nothing alerts, the interval is not held, and the next heartbeat asks
// again.
func TestRebuildOMCycle_InstallCutOffByBudgetIsNoVerdict(t *testing.T) {
	t.Parallel()
	d, rec, _ := rebuildOMTown(t)
	rebuildOMCli(t, d, rebuildOMHeadSHA)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.rebuildOMInstallFn = func(context.Context, ominstaller.Config) (ominstaller.Result, ominstaller.Exit) {
		cancel() // the budget runs out while the install is running
		return ominstaller.Result{Event: "failed", Reason: "build-failed"}, ominstaller.ExitFailed
	}
	cycle := d.startDogCycle("rebuild_om")
	defer cycle.close()

	if settled := d.rebuildOMCycle(ctx, cycle); settled {
		t.Fatal("an install cut off by the budget must retry on the next heartbeat")
	}
	if keys := escalationKeys(rec); len(keys) != 0 {
		t.Errorf("a cut-off install alerted: %v", keys)
	}
}

// TestRebuildOMRunInstall_UnresolvableSettingsFailWithoutRunning pins that a
// config that does not resolve fails the install before any command runs.
func TestRebuildOMRunInstall_UnresolvableSettingsFailWithoutRunning(t *testing.T) {
	t.Parallel()
	d, _ := daemonWithRecorder(t)
	cli := withRebuildGTCli(t, d, func(cliCall) cliReply { return cliReply{} })
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)

	// Every path is set, so resolution reads nothing from the environment and
	// stops at the negative wait.
	res, exit := d.runOMInstall(context.Background(), ominstaller.Config{
		OMPath:       filepath.Join(d.config.TownRoot, "om"),
		TownRoot:     d.config.TownRoot,
		SrcDir:       filepath.Join(d.config.TownRoot, rebuildOMSrcDir),
		SignDir:      filepath.Join(d.config.TownRoot, "signing"),
		SignIdentity: "gastown-test",
		DaemonDir:    filepath.Join(d.config.TownRoot, "daemon"),
		LockWait:     -time.Second,
	})

	if exit != ominstaller.ExitFailed || res.Reason != "unexpected" {
		t.Errorf("result = %+v exit %d, want an unexpected failure", res, exit)
	}
	if calls := cli.recorded(); len(calls) != 0 {
		t.Errorf("an install whose settings did not resolve ran commands: %v", calls)
	}
	if !strings.Contains(logs.String(), "do not resolve") {
		t.Errorf("log does not say the settings did not resolve:\n%s", logs.String())
	}
}

// TestRebuildOMInstallExec_RunsThroughTheDaemonSeam pins the command runner the
// install uses: each command runs in the directory the install names, through
// the daemon's command seam, and its output comes back.
func TestRebuildOMInstallExec_RunsThroughTheDaemonSeam(t *testing.T) {
	t.Parallel()
	d, _ := daemonWithRecorder(t)
	cli := withRebuildGTCli(t, d, func(cliCall) cliReply {
		return cliReply{stdout: "built\n", stderr: "warn\n"}
	})

	stdout, stderr, err := d.omInstallExec(context.Background(), ominstaller.Command{
		Name: "go",
		Args: []string{"build", "./cmd/om"},
		Dir:  "/src/om",
	})
	if err != nil {
		t.Fatalf("omInstallExec: %v", err)
	}
	calls := cli.recorded()
	if len(calls) != 1 || calls[0].name != "go" || calls[0].dir != "/src/om" || !slices.Equal(calls[0].args, []string{"build", "./cmd/om"}) {
		t.Errorf("calls = %+v, want one go build in /src/om", calls)
	}
	if stdout != "built\n" || stderr != "warn\n" {
		t.Errorf("stdout, stderr = %q, %q, want the command's output", stdout, stderr)
	}
}

// TestRebuildOMInstallExec_FeedsStdinToTheCommand: the keychain unlock sends the
// password on stdin, so the child reads it there; a command with no stdin keeps
// /dev/null.
func TestRebuildOMInstallExec_FeedsStdinToTheCommand(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		stdin     string
		wantInput string
		wantNil   bool
	}{
		{name: "password on stdin", stdin: "pw-sentinel\n", wantInput: "pw-sentinel\n"},
		{name: "no stdin", stdin: "", wantNil: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _ := daemonWithRecorder(t)
			var input string
			var stdinNil bool
			d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
				stdinNil = cmd.Stdin == nil
				if cmd.Stdin != nil {
					b, err := io.ReadAll(cmd.Stdin)
					if err != nil {
						t.Errorf("read stdin: %v", err)
					}
					input = string(b)
				}
				return nil, nil, nil
			}

			if _, _, err := d.omInstallExec(context.Background(), ominstaller.Command{
				Name:  "security",
				Args:  []string{"unlock-keychain", "/keys/gastown-signing.keychain-db"},
				Stdin: tc.stdin,
			}); err != nil {
				t.Fatalf("omInstallExec: %v", err)
			}
			if stdinNil != tc.wantNil {
				t.Errorf("stdin nil = %v, want %v", stdinNil, tc.wantNil)
			}
			if input != tc.wantInput {
				t.Errorf("stdin = %q, want %q", input, tc.wantInput)
			}
		})
	}
}

// TestRebuildOMCycle_FailedInstallThenFreshOmClearsTheAlert: the alert the
// failure raised closes on the cycle that finds om current.
func TestRebuildOMCycle_FailedInstallThenFreshOmClearsTheAlert(t *testing.T) {
	t.Parallel()
	d, rec, _ := rebuildOMTown(t)
	rebuildOMCli(t, d, rebuildOMHeadSHA)
	inst := fakeOMInstall(d, ominstaller.Result{Event: "failed", Commit: rebuildOMHeadSHA, Prev: rebuildOMStaleSHA, Reason: "build-failed"}, ominstaller.ExitFailed)
	d.runRebuildOM()
	if keys := escalationKeys(rec); len(keys) != 1 {
		t.Fatalf("escalations = %v, want one", keys)
	}

	// The operator installs by hand; the next cycle sees om at origin's tip.
	d.omVersionFn = func(string) (string, error) { return rebuildOMHeadSHA, nil }
	d.runRebuildOM()
	if len(inst.runs) != 1 {
		t.Errorf("installs = %d, want only the first cycle's", len(inst.runs))
	}

	clears := rec.Clears()
	if len(clears) != 1 || !slices.Contains(clears[0].Fingerprints, alertKeyRebuildOMInstallFail) {
		t.Fatalf("clears = %+v, want the install-failed alert cleared", clears)
	}
}

// TestRebuildOMCycle_DetectionFailuresDeferSoftly pins the fail-soft rule: a
// reading the job cannot take defers to the next heartbeat without installing,
// escalating or touching the alert state.
func TestRebuildOMCycle_DetectionFailuresDeferSoftly(t *testing.T) {
	t.Parallel()
	cases := map[string]func(d *Daemon, answer *func(c cliCall) cliReply){
		"installed om unreadable": func(d *Daemon, _ *func(cliCall) cliReply) {
			d.omVersionFn = func(string) (string, error) { return "", errors.New("exec format error") }
		},
		"fetch fails": func(_ *Daemon, answer *func(cliCall) cliReply) {
			*answer = func(c cliCall) cliReply {
				if gitSub(c, "fetch") {
					return cliReply{stderr: "could not resolve host", code: 128}
				}
				return cliReply{stdout: rebuildOMHeadSHA + "\n"}
			}
		},
		"origin/main is not a commit": func(_ *Daemon, answer *func(cliCall) cliReply) {
			*answer = func(c cliCall) cliReply { return cliReply{stdout: "fatal garbage\n"} }
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, rec, _ := rebuildOMTown(t)
			answer := func(c cliCall) cliReply { return cliReply{stdout: rebuildOMHeadSHA + "\n"} }
			breakIt(d, &answer)
			withRebuildGTCli(t, d, answer)
			inst := fakeOMInstall(d, ominstaller.Result{}, ominstaller.ExitFailed)

			if settled := d.runRebuildOM(); settled {
				t.Error("a detection failure accomplished nothing and must retry next heartbeat")
			}
			if len(inst.runs) != 0 {
				t.Errorf("a detection failure ran the installer: %d runs", len(inst.runs))
			}
			if calls := rec.Calls(); len(calls) != 0 {
				t.Errorf("a detection failure touched the alert state: %+v", calls)
			}
		})
	}
}

// TestRebuildOMCycle_ClonesAndFetchesOnlyUnderOMSrc pins the isolation: on
// first use the clone is made from the rig's origin url, every write goes to
// <town>/.runtime/om-src, and the rig's worktree is asked for that url and
// nothing else.
func TestRebuildOMCycle_ClonesAndFetchesOnlyUnderOMSrc(t *testing.T) {
	t.Parallel()
	d, _, _ := rebuildOMTown(t)
	src := filepath.Join(d.config.TownRoot, rebuildOMSrcDir)
	rig := filepath.Join(d.config.TownRoot, omSkewRigWorktree)
	// First use: no clone yet.
	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}
	cli := rebuildOMCli(t, d, rebuildOMHeadSHA)
	fakeOMInstall(d, ominstaller.Result{Event: "installed", Commit: rebuildOMHeadSHA, Prev: rebuildOMStaleSHA}, ominstaller.ExitInstalled)

	d.runRebuildOM()

	clones := gitCalls(cli, "clone")
	if len(clones) != 1 || !slices.Equal(clones[0], []string{"clone", "--quiet", "--", "https://example.invalid/om.git", src}) {
		t.Fatalf("clones = %v, want one from the rig's origin url into %s", clones, src)
	}
	for _, c := range cli.recorded() {
		if c.name != "git" {
			continue
		}
		argv := strings.Join(c.args, " ")
		if strings.Contains(argv, rig) && argv != "-C "+rig+" remote get-url origin" {
			t.Errorf("git call %q touches the om rig beyond reading its origin url", argv)
		}
		for _, write := range []string{"fetch", "merge", "clone"} {
			if slices.Contains(c.args, write) && !strings.Contains(argv, src) {
				t.Errorf("git %s %q does not run under %s", write, argv, src)
			}
		}
	}
}

// TestRebuildOMPatrol_DisabledByDefaultAndDoesNothing pins the opt-in: with no
// config, or enabled:false, the heartbeat step starts no cycle; enabled:true
// runs one.
func TestRebuildOMPatrol_DisabledByDefaultAndDoesNothing(t *testing.T) {
	t.Parallel()
	patrols := func(c *PatrolConfig) *DaemonPatrolConfig {
		return &DaemonPatrolConfig{Patrols: &PatrolsConfig{RebuildOM: c}}
	}
	for name, cfg := range map[string]*DaemonPatrolConfig{
		"no config":        nil,
		"no patrols":       {},
		"key absent":       patrols(nil),
		"explicitly off":   patrols(&PatrolConfig{Enabled: false, Interval: "1m"}),
		"interval, not on": patrols(&PatrolConfig{Interval: "1m"}),
	} {
		if IsPatrolEnabled(cfg, "rebuild_om") {
			t.Errorf("%s: rebuild_om is enabled, want opt-in", name)
		}
	}

	d, rec, _ := rebuildOMTown(t)
	cli := rebuildOMCli(t, d, rebuildOMHeadSHA)
	inst := fakeOMInstall(d, ominstaller.Result{Event: "installed", Commit: rebuildOMHeadSHA, Prev: rebuildOMStaleSHA}, ominstaller.ExitInstalled)
	d.patrolConfig = patrols(&PatrolConfig{Enabled: false})
	d.triggerRebuildOM()
	if calls := cli.recorded(); len(calls) != 0 || d.rebuildOMRunning.Load() {
		t.Fatalf("a disabled patrol ran: calls=%v", calls)
	}

	d.patrolConfig = patrols(&PatrolConfig{Enabled: true})
	cycleEnd := make(chan struct{}, 1)
	d.rebuildOMCycleDoneFn = func() {
		select {
		case cycleEnd <- struct{}{}:
		default:
		}
	}
	d.triggerRebuildOM()
	for d.rebuildOMRunning.Load() {
		<-cycleEnd
	}
	if len(inst.runs) != 1 {
		t.Fatalf("an enabled patrol ran %d installs, want 1", len(inst.runs))
	}
	// The verdict held the interval: the next heartbeat does nothing.
	before := len(cli.recorded())
	d.triggerRebuildOM()
	if after := len(cli.recorded()); after != before || d.rebuildOMRunning.Load() {
		t.Errorf("a patrol inside its interval ran again (%d -> %d calls)", before, after)
	}
	if esc := rec.Escalations(); len(esc) != 0 {
		t.Errorf("escalations = %+v", esc)
	}
}

func TestRebuildOMInterval(t *testing.T) {
	t.Parallel()
	cfg := func(s string) *DaemonPatrolConfig {
		return &DaemonPatrolConfig{Patrols: &PatrolsConfig{RebuildOM: &PatrolConfig{Enabled: true, Interval: s}}}
	}
	for _, tc := range []struct {
		name string
		cfg  *DaemonPatrolConfig
		want time.Duration
	}{
		{"nil config", nil, time.Hour},
		{"unset", cfg(""), time.Hour},
		{"garbage", cfg("soon"), time.Hour},
		{"non-positive", cfg("-5m"), time.Hour},
		{"set", cfg("15m"), 15 * time.Minute},
	} {
		if got := rebuildOMInterval(tc.cfg); got != tc.want {
			t.Errorf("%s: interval = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRebuildOMInForce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		installed, head string
		want            bool
	}{
		{rebuildOMHeadSHA, rebuildOMHeadSHA, true},
		{rebuildOMHeadSHA[:8], rebuildOMHeadSHA, true},
		{rebuildOMStaleSHA, rebuildOMHeadSHA, false},
	} {
		if got := rebuildOMInForce(tc.installed, tc.head); got != tc.want {
			t.Errorf("rebuildOMInForce(%q, %q) = %v, want %v", tc.installed, tc.head, got, tc.want)
		}
	}
}

// TestRebuildOMDrainBoundFitsTheCycleBudget pins that the drain's timeout
// branch can fire in production: the bound, with the longest waits that
// surround it (one git step, the sync's lock wait, the install's lock wait),
// stays inside the cycle budget, and the bound is no longer than the upgrade
// restart's (gt-0hsf6).
func TestRebuildOMDrainBoundFitsTheCycleBudget(t *testing.T) {
	t.Parallel()
	around := rebuildOMGitTimeout + rebuildOMLockWait + rebuildOMInstallLockWait
	if rebuildOMDrainBound+around >= rebuildOMRunBudget {
		t.Errorf("drain bound %s plus %s of surrounding waits does not fit the %s cycle budget, so the timeout branch cannot fire",
			rebuildOMDrainBound, around, rebuildOMRunBudget)
	}
	if rebuildOMDrainBound > postLandRestartCap {
		t.Errorf("drain bound %s is longer than the upgrade restart's %s", rebuildOMDrainBound, postLandRestartCap)
	}
}
