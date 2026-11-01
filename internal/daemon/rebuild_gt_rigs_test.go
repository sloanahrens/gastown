package daemon

import (
	"context"
	"errors"
	"log"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/version"
)

// rigCheckout creates the canonical checkout the sweep sees for rig, and
// returns its path.
func rigCheckout(t *testing.T, d *Daemon, rig string) string {
	t.Helper()
	checkout := filepath.Join(d.config.TownRoot, rig, "mayor", "rig")
	mkdirs(t, filepath.Join(checkout, ".git"))
	return checkout
}

// gitDir returns the checkout a `git -C <dir>` call ran in.
func gitDir(c cliCall) string {
	if c.name == "git" && len(c.args) >= 2 && c.args[0] == "-C" {
		return c.args[1]
	}
	return ""
}

// mergedDirs returns the rig checkouts a fast-forward was run in, in call
// order. The gt source checkout the cycle syncs itself is not a rig checkout.
func mergedDirs(cli *fakeCLI) []string {
	var out []string
	for _, c := range cli.recorded() {
		if gitSub(c, "merge") && strings.HasSuffix(gitDir(c), filepath.Join("mayor", "rig")) {
			out = append(out, gitDir(c))
		}
	}
	return out
}

// setupRigs points the daemon's rig registry at the named rigs.
func setupRigs(d *Daemon, rigs ...string) {
	d.knownRigsCache, d.knownRigsCacheValid = rigs, true
}

// TestRebuildGTSyncsOtherRigCheckouts is the case the sweep exists for: a clean
// checkout behind origin/main is fast-forwarded, and a dirty one and one with
// commits of its own are logged with their reason and left alone (gt-f91o8).
func TestRebuildGTSyncsOtherRigCheckouts(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	// A fresh binary: the cycle still syncs, since a rig's directive layer is
	// stale whatever the installed gt is.
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo {
		return &version.StaleBinaryInfo{}
	}
	clean := rigCheckout(t, d, "clean")
	dirty := rigCheckout(t, d, "dirty")
	ahead := rigCheckout(t, d, "ahead")
	setupRigs(d, "clean", "dirty", "ahead")

	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)

	cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case gitSub(c, "status"):
			if gitDir(c) == dirty {
				return cliReply{stdout: " M internal/thing.go\n"}
			}
			return cliReply{}
		case gitSub(c, "rev-list"):
			if gitDir(c) == ahead {
				return cliReply{stdout: "2\t0\n"}
			}
			return cliReply{stdout: "0\t3\n"}
		}
		return cliReply{}
	})

	if settled := d.runRebuildGT(); !settled {
		t.Fatal("a fresh binary is a verdict, not a deferral")
	}

	if merged := mergedDirs(cli); !slices.Equal(merged, []string{clean}) {
		t.Fatalf("fast-forwarded checkouts = %v, want only %s", merged, clean)
	}
	for _, want := range []string{
		"rebuild_gt: rig dirty: the work tree has uncommitted changes; leaving its checkout alone",
		"rebuild_gt: rig ahead: 2 commits on HEAD are not on origin/main; leaving its checkout alone",
		"rebuild_gt: rig checkouts: 1 moved, 2 skipped, 0 failed",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log does not contain %q:\n%s", want, logs.String())
		}
	}
}

// TestRebuildGTRigCheckoutSyncFailureNeverFailsTheInstall pins the bound: a rig whose
// git fails and one that outlives its timeout are both reported, and neither
// changes the install's outcome nor stops the rigs after them. The timeout is
// driven, not faked: the slow rig's runner returns only once the sweep's own
// per-rig context is done.
func TestRebuildGTRigCheckoutSyncFailureNeverFailsTheInstall(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	d.rebuildGTRigSyncTimeoutFn = func() time.Duration { return 50 * time.Millisecond }
	d.rebuildGTRigSyncBudgetFn = func() time.Duration { return 10 * time.Minute }
	broken := rigCheckout(t, d, "broken")
	slow := rigCheckout(t, d, "slow")
	clean := rigCheckout(t, d, "clean")
	// The slow rig sits between the other two, so the clean one running after it
	// is what shows a hung rig does not stop the sweep.
	setupRigs(d, "broken", "slow", "clean")

	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)

	cli := newFakeCLIFor(func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case gitSub(c, "rev-parse"):
			return cliReply{stdout: "abc1234567890\n"}
		case gitSub(c, "log"):
			return cliReply{stdout: "abc1234 the commit that was inert\n"}
		case gitSub(c, "fetch") && gitDir(c) == broken:
			return cliReply{stderr: "fatal: could not read from remote repository", code: 1}
		case gitSub(c, "rev-list"):
			return cliReply{stdout: "0\t3\n"}
		}
		if c.name == "gt-installer" {
			return cliReply{stdout: "install-gt: RESULT installed abc1234567890 beefbeef -\n"}
		}
		return cliReply{}
	})
	// The slow rig's git never answers: it waits on the context the sweep
	// bounds with rebuildGTRigSyncTimeoutFn, so the real 50ms per-rig bound is
	// what cuts it off rather than an error the test pretends it returned.
	d.rebuildGTRigGitFn = func(ctx context.Context, checkout string, args ...string) (string, error) {
		if checkout != slow {
			return d.rebuildGTGitCtx(ctx, checkout, args...)
		}
		// The sweep's budget is ten minutes here, so a context that lives longer
		// than a second is not the per-rig one: fail at once instead of waiting
		// for the budget, and the log below names what the rig got.
		if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > time.Second {
			return "", errors.New("the rig's context carries no per-rig bound")
		}
		<-ctx.Done()
		return "", ctx.Err()
	}
	d.execCmd = cli.run

	if settled := d.runRebuildGT(); !settled {
		t.Fatal("a failing rig turned a reached verdict into a deferral")
	}
	if installs := gtInstallerCalls(cli); len(installs) != 1 {
		t.Fatalf("install calls = %v, want exactly one", installs)
	}
	if merged := mergedDirs(cli); !slices.Equal(merged, []string{clean}) {
		t.Errorf("fast-forwarded checkouts = %v, want %s: one rig must not stop the others", merged, clean)
	}
	for _, want := range []string{
		"rebuild_gt: rig broken: cannot fetch origin: fatal: could not read from remote repository",
		"rebuild_gt: rig slow: cannot read the branch: context deadline exceeded",
		"rebuild_gt: rig checkouts: 1 moved, 0 skipped, 2 failed",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log does not contain %q:\n%s", want, logs.String())
		}
	}
}

// TestRebuildGTSyncRigCheckoutLeavesOtherStatesAlone pins every state the sweep
// refuses to move, and the readings it reports as failures: a checkout is
// fast-forwarded only when doing so cannot lose anything.
func TestRebuildGTSyncRigCheckoutLeavesOtherStatesAlone(t *testing.T) {
	t.Parallel()
	// A clean checkout on main that is three commits behind origin/main.
	behind := func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case gitSub(c, "rev-list"):
			return cliReply{stdout: "0\t3\n"}
		}
		return cliReply{}
	}
	over := func(name string, r cliReply) func(c cliCall) cliReply {
		return func(c cliCall) cliReply {
			if gitSub(c, name) {
				return r
			}
			return behind(c)
		}
	}

	cases := []struct {
		name       string
		answer     func(c cliCall) cliReply
		moved      bool
		failed     bool
		wantMerges int
		wantWhy    string
	}{
		{name: "clean and behind", answer: behind, moved: true, wantMerges: 1},
		{name: "detached HEAD", answer: over("branch", cliReply{}), wantWhy: "HEAD is detached"},
		{name: "another branch", answer: over("branch", cliReply{stdout: "feature\n"}), wantWhy: "on branch feature"},
		{name: "dirty work tree", answer: over("status", cliReply{stdout: " M a.go\n"}), wantWhy: "the work tree has uncommitted changes"},
		{name: "already at origin/main", answer: over("rev-list", cliReply{stdout: "0\t0\n"}), wantWhy: "already at origin/main"},
		{name: "commits of its own", answer: over("rev-list", cliReply{stdout: "2\t0\n"}), wantWhy: "2 commits on HEAD are not on origin/main"},
		{
			name:    "unreadable work tree",
			answer:  over("status", cliReply{stderr: "fatal: not a git repository", code: 1}),
			failed:  true,
			wantWhy: "cannot read the work tree: fatal: not a git repository",
		},
		{
			name:    "unreadable history",
			answer:  over("rev-list", cliReply{stdout: "0 3 7\n"}),
			failed:  true,
			wantWhy: "rev-list did not print two counts",
		},
		{
			name:       "the fast-forward fails",
			answer:     over("merge", cliReply{stderr: "fatal: Not possible to fast-forward, aborting.", code: 1}),
			failed:     true,
			wantMerges: 1,
			wantWhy:    "cannot fast-forward to origin/main: fatal: Not possible to fast-forward",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _ := daemonWithRecorder(t)
			checkout := rigCheckout(t, d, "rig")
			cli := withRebuildGTCli(t, d, tc.answer)

			moved, failed, why := d.rebuildGTSyncRigCheckout(context.Background(), checkout)
			if moved != tc.moved || failed != tc.failed {
				t.Fatalf("moved, failed = %v, %v; want %v, %v (why %q)", moved, failed, tc.moved, tc.failed, why)
			}
			if !strings.Contains(why, tc.wantWhy) {
				t.Errorf("why = %q, want it to name %q", why, tc.wantWhy)
			}
			if got := mergedDirs(cli); len(got) != tc.wantMerges {
				t.Errorf("fast-forwards = %v, want %d", got, tc.wantMerges)
			}
		})
	}
}

// TestRebuildGTRigCheckoutSweepRunsOnlyAfterAVerdictPastTheSync pins when the sweep
// runs: a cycle that deferred syncs nothing, because the next heartbeat runs
// the whole cycle again, and one that never reached the gt sync has not earned
// a rig fetch either.
func TestRebuildGTRigCheckoutSweepRunsOnlyAfterAVerdictPastTheSync(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		prepare func(d *Daemon)
		gate    func() (string, bool)
		settled bool
	}{
		{
			name:    "deferred on a busy gate",
			prepare: func(*Daemon) {},
			gate:    func() (string, bool) { return "a gate suite holds a slot (gastown/landing)", false },
			settled: false,
		},
		{
			name: "refused before the sync",
			prepare: func(d *Daemon) {
				// The gt source checkout is dirty, which the cycle refuses
				// before it fetches anything.
				d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
			},
			gate:    func() (string, bool) { return "", false },
			settled: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _ := rebuildGTTown(t)
			d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
			tc.prepare(d)
			rig := rigCheckout(t, d, "devops")
			setupRigs(d, "devops")
			cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
				if gitSub(c, "status") && gitDir(c) == filepath.Join(d.config.TownRoot, "gastown") {
					if tc.settled {
						return cliReply{stdout: " M internal/daemon/daemon.go\n"}
					}
				}
				if gitSub(c, "branch") {
					return cliReply{stdout: "main\n"}
				}
				return cliReply{}
			})
			d.rebuildGTGateFn = tc.gate

			if settled := d.runRebuildGT(); settled != tc.settled {
				t.Fatalf("settled = %v, want %v", settled, tc.settled)
			}
			for _, c := range cli.recorded() {
				if gitDir(c) == rig {
					t.Fatalf("the sweep ran git in %s on a cycle that synced nothing: %v", rig, c.args)
				}
			}
		})
	}
}

// TestRebuildGTGitCtxNamesItsBound pins what a git call cut off by its caller's
// bound reports: the expiry, not the signal that killed it (gt-f91o8).
func TestRebuildGTGitCtxNamesItsBound(t *testing.T) {
	t.Parallel()
	d, _ := daemonWithRecorder(t)
	d.execCmd = func(*exec.Cmd) ([]byte, []byte, error) { return nil, nil, errors.New("signal: killed") }

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	_, err := d.rebuildGTGitCtx(ctx, t.TempDir(), "fetch", "origin")
	if err == nil || !strings.Contains(err.Error(), "git fetch origin: context deadline exceeded") {
		t.Fatalf("err = %v, want the expired bound named", err)
	}
}

// TestRebuildGTRigCheckoutSweepSkipsGastownAndRigWithoutACheckout pins what the sweep
// looks at: the gt source rig is moved by the cycle itself, and a rig with no
// canonical checkout has nothing to move.
func TestRebuildGTRigCheckoutSweepSkipsGastownAndRigWithoutACheckout(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	gastown := rigCheckout(t, d, "gastown")
	setupRigs(d, "gastown", "nocheckout")
	cli := withRebuildGTCli(t, d, nil)

	if counts := d.rebuildGTSyncRigCheckouts(); counts != (rebuildGTRigCheckoutCounts{}) {
		t.Fatalf("counts = %+v, want none", counts)
	}
	for _, c := range cli.recorded() {
		if gitDir(c) == gastown {
			t.Errorf("the sweep ran git in the gt source checkout: %v", c.args)
		}
	}
}

// TestRebuildGTRigCheckoutSweepHoldsNoHeartbeatAndKeepsTheRequestArmed pins what the
// sweep costs the daemon: it runs on the cycle's own goroutine, so the
// heartbeat entry returns while the sweep is blocked, and a landing's install
// request armed while it runs is still armed when the cycle ends (gt-f91o8).
func TestRebuildGTRigCheckoutSweepHoldsNoHeartbeatAndKeepsTheRequestArmed(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return staleInfo(3) }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	rig := rigCheckout(t, d, "devops")
	setupRigs(d, "devops")

	inSweep := make(chan struct{}, 1)
	release := make(chan struct{})
	d.rebuildGTRigGitFn = func(_ context.Context, checkout string, args ...string) (string, error) {
		if checkout != rig {
			return "", nil
		}
		select {
		case inSweep <- struct{}{}:
		default:
		}
		<-release
		switch args[0] {
		case "branch":
			return "main\n", nil
		case "rev-list":
			return "0\t3\n", nil
		}
		return "", nil
	}
	withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case gitSub(c, "rev-parse"):
			return cliReply{stdout: "abc1234567890\n"}
		case gitSub(c, "log"):
			return cliReply{stdout: "abc1234 the commit that was inert\n"}
		case c.name == "gt-installer":
			return cliReply{stdout: "install-gt: RESULT installed abc1234567890 beefbeef -\n"}
		}
		return cliReply{}
	})
	cycleEnd := watchRebuildGTCycle(d)
	var releaseOnce sync.Once
	releaseSweep := func() { releaseOnce.Do(func() { close(release) }) }
	// A failed assertion below must not leave the cycle parked on release.
	t.Cleanup(func() {
		releaseSweep()
		awaitRebuildGTIdle(t, d, cycleEnd)
	})

	// A heartbeat entry must never wait on the sweep: the sweep runs on the
	// cycle's goroutine, and the single-flight guard is set before that
	// goroutine starts. The bound is a deadlock backstop only; it does not
	// stand in for any timing the sweep has.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	heartbeatReturns := func(when string) {
		t.Helper()
		returned := make(chan struct{})
		go func() {
			d.triggerRebuildGT()
			close(returned)
		}()
		select {
		case <-returned:
		case <-ctx.Done():
			t.Fatalf("%s: the heartbeat blocked on the rig sweep", when)
		}
	}

	heartbeatReturns("starting the cycle")
	select {
	case <-inSweep:
	case <-ctx.Done():
		t.Fatal("the cycle never reached the rig sweep")
	}
	// With the cycle's goroutine parked in the sweep, a second entry has
	// nothing to do and must return. A sweep the heartbeat ran inline, before
	// setting the guard, would start again here and block.
	heartbeatReturns("while the sweep is blocked")

	// A request a landing arms while the sweep runs survives the cycle's end.
	d.requestRebuildGTInstall()
	releaseSweep()
	awaitRebuildGTIdle(t, d, cycleEnd)

	if !d.rebuildGTRequested.Load() {
		t.Fatal("the cycle's end cleared a request armed while the sweep ran")
	}
}

// TestRebuildGTRigCheckoutSweepCountsTheRigsTheBudgetNeverReached pins the counts: a rig
// the spent budget stopped the sweep from visiting is skipped with that reason,
// so moved+skipped+failed is every checkout the town has (gt-f91o8).
func TestRebuildGTRigCheckoutSweepCountsTheRigsTheBudgetNeverReached(t *testing.T) {
	t.Parallel()
	d, _ := daemonWithRecorder(t)
	for _, rig := range []string{"alpha", "beta", "gamma"} {
		rigCheckout(t, d, rig)
	}
	setupRigs(d, "alpha", "beta", "gamma")
	// A budget that is spent before the sweep starts, so no rig is visited.
	d.rebuildGTRigSyncBudgetFn = func() time.Duration { return time.Nanosecond }
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	cli := withRebuildGTCli(t, d, nil)

	counts := d.rebuildGTSyncRigCheckouts()
	if counts != (rebuildGTRigCheckoutCounts{Skipped: 3}) {
		t.Fatalf("counts = %+v, want three skipped and nothing else", counts)
	}
	if merged := mergedDirs(cli); len(merged) != 0 {
		t.Errorf("a spent budget still fast-forwarded %v", merged)
	}
	for _, rig := range []string{"alpha", "beta", "gamma"} {
		want := "rebuild_gt: rig " + rig + ": the 1ns sweep budget is spent; leaving its checkout alone"
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log does not contain %q:\n%s", want, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "rebuild_gt: rig checkouts: 0 moved, 3 skipped, 0 failed") {
		t.Errorf("the cycle's line does not add up:\n%s", logs.String())
	}
}

// TestRebuildGTRigCheckoutSweepRefusesAPathRigName pins the guard on the name the sweep
// joins into a path: an empty name, "." and ".." name the town root, and a name
// holding a separator names a directory that is not the rig's, so the sweep
// skips it without reading git there (gt-f91o8).
func TestRebuildGTRigCheckoutSweepRefusesAPathRigName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", ".", "..", "a/b", `a\b`} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, _ := daemonWithRecorder(t)
			// A checkout sits at the path the name would join to, so a guard
			// that did not refuse the name would find one and run git in it.
			rigCheckout(t, d, name)
			setupRigs(d, name)
			var logs strings.Builder
			d.logger = log.New(&logs, "", 0)
			cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
				if gitSub(c, "branch") {
					return cliReply{stdout: "main\n"}
				}
				return cliReply{}
			})

			counts := d.rebuildGTSyncRigCheckouts()
			if counts != (rebuildGTRigCheckoutCounts{Skipped: 1}) {
				t.Fatalf("counts = %+v, want one skipped", counts)
			}
			for _, c := range cli.recorded() {
				t.Errorf("the sweep ran git for an invalid rig name: %v", c.args)
			}
			if !strings.Contains(logs.String(), "invalid rig name") {
				t.Errorf("the log does not name the refusal:\n%s", logs.String())
			}
		})
	}
}

// TestRebuildGTRigCheckoutSweepStopsWithTheDaemon pins that the sweep's budget
// hangs off the daemon's context: with the daemon stopping, no git runs and
// every checkout is skipped with that reason (gt-f91o8).
func TestRebuildGTRigCheckoutSweepStopsWithTheDaemon(t *testing.T) {
	t.Parallel()
	d, _ := daemonWithRecorder(t)
	for _, rig := range []string{"alpha", "beta"} {
		rigCheckout(t, d, rig)
	}
	setupRigs(d, "alpha", "beta")
	stopped, stop := context.WithCancel(t.Context())
	stop()
	d.ctx = stopped
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	cli := withRebuildGTCli(t, d, nil)

	counts := d.rebuildGTSyncRigCheckouts()
	if counts != (rebuildGTRigCheckoutCounts{Skipped: 2}) {
		t.Fatalf("counts = %+v, want both rigs skipped", counts)
	}
	if merged := mergedDirs(cli); len(merged) != 0 {
		t.Errorf("a stopping daemon still fast-forwarded %v", merged)
	}
	for _, rig := range []string{"alpha", "beta"} {
		want := "rebuild_gt: rig " + rig + ": the daemon is stopping; leaving its checkout alone"
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log does not contain %q:\n%s", want, logs.String())
		}
	}
}
