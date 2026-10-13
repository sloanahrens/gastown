package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

const (
	rebuildOMStaleSHA = "1111111111111111111111111111111111111111"
	rebuildOMHeadSHA  = "2222222222222222222222222222222222222222"
)

// rebuildOMTown is a town with a gt source checkout holding the installer stub,
// an om clone already in place, the installed om one commit behind the clone's
// origin/main, a quiet gate, and a fake clock. Nothing it does starts a process.
func rebuildOMTown(t *testing.T) (*Daemon, *notifyfake.Recorder, *clockwork.FakeClock) {
	t.Helper()
	d, rec := daemonWithRecorder(t)
	clk := newFixedClock()
	d.clock = clk
	repoRoot := filepath.Join(d.config.TownRoot, "gastown")
	mkdirs(t, filepath.Join(repoRoot, "cmd", "gt"), filepath.Join(repoRoot, "scripts"),
		filepath.Join(d.config.TownRoot, rebuildOMSrcDir, ".git"))
	writeFile(t, filepath.Join(repoRoot, "cmd", "gt", "main.go"), "package main\n")
	// Inert text for the same reason as the install-gt stub: the fake CLI answers
	// the call, and a shebang would make this look like an executable.
	writeFile(t, filepath.Join(repoRoot, "scripts", "install-om.sh"), "stub: the fake CLI answers this call\n")

	d.omVersionFn = func(string) (string, error) { return rebuildOMStaleSHA, nil }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	return d, rec, clk
}

// rebuildOMCli answers the git reads a cycle makes with head as origin/main's
// tip, and every bash call with the installer's reply.
func rebuildOMCli(t *testing.T, d *Daemon, head string, installer cliReply) *fakeCLI {
	t.Helper()
	return withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "rev-parse"):
			return cliReply{stdout: head + "\n"}
		case gitSub(c, "remote"):
			return cliReply{stdout: "https://example.invalid/om.git\n"}
		case c.name == "bash":
			return installer
		}
		return cliReply{}
	})
}

func omInstalled(commit string) cliReply {
	return cliReply{stdout: "building om\ninstall-om: RESULT installed " + commit + " " + rebuildOMStaleSHA + " -\n"}
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
// an om behind origin/main, a quiet town, one install through the town's script
// that builds the job's clone and replaces the om the landing worker uses.
func TestRebuildOMCycle_StaleAndQuietInstallsOnce(t *testing.T) {
	t.Parallel()
	d, rec, _ := rebuildOMTown(t)
	cli := rebuildOMCli(t, d, rebuildOMHeadSHA, omInstalled(rebuildOMHeadSHA))

	if settled := d.runRebuildOM(); !settled {
		t.Fatal("a completed install must hold the interval, not retry the next heartbeat")
	}

	installs := installCalls(cli)
	if len(installs) != 1 {
		t.Fatalf("install calls = %v, want exactly one", installs)
	}
	argv := strings.Join(installs[0], " ")
	for _, want := range []string{"install-om.sh", "--source rebuild-om", "--om-path "} {
		if !strings.Contains(argv, want) {
			t.Errorf("install argv %q does not carry %q", argv, want)
		}
	}
	src := filepath.Join(d.config.TownRoot, rebuildOMSrcDir)
	var env cliCall
	for _, c := range cli.recorded() {
		if c.name == "bash" {
			env = c
		}
	}
	if got := env.getenv("INSTALL_OM_SRC_DIR"); got != src {
		t.Errorf("INSTALL_OM_SRC_DIR = %q, want the job's clone %q", got, src)
	}
	if got := env.getenv("GT_TOWN_ROOT"); got != d.config.TownRoot {
		t.Errorf("GT_TOWN_ROOT = %q, want %q", got, d.config.TownRoot)
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
			cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
				switch {
				case gitSub(c, "merge-base"):
					return ancestry
				case gitSub(c, "rev-parse"):
					return cliReply{stdout: rebuildOMHeadSHA + "\n"}
				case c.name == "bash":
					return omInstalled(rebuildOMHeadSHA)
				}
				return cliReply{}
			})

			clk.Advance(rebuildOMStarveAfter + time.Minute)
			for range 2 {
				if settled := d.runRebuildOM(); !settled {
					t.Fatal("leaving an om that is not behind in place is a verdict and holds the interval")
				}
			}

			if installs := installCalls(cli); len(installs) != 0 {
				t.Errorf("an om that is not behind origin/main was replaced: %v", installs)
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

// TestRebuildOMCycle_FreshOmClearsEveryAlert pins the close: an om at origin's
// tip installs nothing, ends the starvation episode, and clears both alerts.
func TestRebuildOMCycle_FreshOmClearsEveryAlert(t *testing.T) {
	t.Parallel()
	d, rec, clk := rebuildOMTown(t)
	d.omVersionFn = func(string) (string, error) { return rebuildOMHeadSHA, nil }
	cli := rebuildOMCli(t, d, rebuildOMHeadSHA, cliReply{})
	d.rebuildOMBlock.open(clk.Now().Add(-time.Hour))

	if settled := d.runRebuildOM(); !settled {
		t.Fatal("a fresh om is a verdict and holds the interval")
	}

	if installs := installCalls(cli); len(installs) != 0 {
		t.Errorf("a fresh om ran the installer: %v", installs)
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
			makeBusy(d)
			cli := rebuildOMCli(t, d, rebuildOMHeadSHA, omInstalled(rebuildOMHeadSHA))

			if settled := d.runRebuildOM(); settled {
				t.Fatal("a deferral accomplished nothing and must retry next heartbeat")
			}
			if installs := installCalls(cli); len(installs) != 0 {
				t.Fatalf("a busy town ran the installer: %v", installs)
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

// TestRebuildOMCycle_InstallOutcomes maps each install-om.sh exit to what the
// job does: a verdict holds the interval, a busy lock retries, and only a
// failure raises the keyed alert.
func TestRebuildOMCycle_InstallOutcomes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		reply       cliReply
		wantSettled bool
		wantAlerts  []string
	}{
		{"noop is in force", cliReply{stdout: "install-om: RESULT noop " + rebuildOMHeadSHA + " " + rebuildOMHeadSHA + " already-installed\n"}, true, nil},
		{"refused holds the interval", cliReply{stdout: "install-om: RESULT refused - - dirty\n", code: 2}, true, nil},
		{"busy retries", cliReply{stdout: "install-om: RESULT refused - - lock-busy\n", code: 3}, false, nil},
		{"build failed keeps the old om and alerts once", cliReply{stdout: "install-om: RESULT failed " + rebuildOMHeadSHA + " " + rebuildOMStaleSHA + " build-failed\n", code: 1},
			true, []string{alertKeyRebuildOMInstallFail}},
		{"rolled back alerts once", cliReply{stdout: "install-om: RESULT rolled_back " + rebuildOMHeadSHA + " " + rebuildOMStaleSHA + " smoke-failed\n", code: 1},
			true, []string{alertKeyRebuildOMInstallFail}},
		{"no result line alerts once", cliReply{stdout: "boom\n", code: 1}, true, []string{alertKeyRebuildOMInstallFail}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, rec, _ := rebuildOMTown(t)
			rebuildOMCli(t, d, rebuildOMHeadSHA, tc.reply)

			if settled := d.runRebuildOM(); settled != tc.wantSettled {
				t.Errorf("settled = %v, want %v", settled, tc.wantSettled)
			}
			if keys := escalationKeys(rec); !slices.Equal(keys, tc.wantAlerts) {
				t.Errorf("escalations = %v, want %v", keys, tc.wantAlerts)
			}
		})
	}
}

// TestRebuildOMCycle_FailedInstallThenFreshOmClearsTheAlert: the alert the
// failure raised closes on the cycle that finds om current.
func TestRebuildOMCycle_FailedInstallThenFreshOmClearsTheAlert(t *testing.T) {
	t.Parallel()
	d, rec, _ := rebuildOMTown(t)
	rebuildOMCli(t, d, rebuildOMHeadSHA, cliReply{stdout: "install-om: RESULT failed - - build-failed\n", code: 1})
	d.runRebuildOM()
	if keys := escalationKeys(rec); len(keys) != 1 {
		t.Fatalf("escalations = %v, want one", keys)
	}

	// The operator installs by hand; the next cycle sees om at origin's tip.
	d.omVersionFn = func(string) (string, error) { return rebuildOMHeadSHA, nil }
	d.runRebuildOM()

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
			cli := withRebuildGTCli(t, d, answer)

			if settled := d.runRebuildOM(); settled {
				t.Error("a detection failure accomplished nothing and must retry next heartbeat")
			}
			if installs := installCalls(cli); len(installs) != 0 {
				t.Errorf("a detection failure ran the installer: %v", installs)
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
	cli := rebuildOMCli(t, d, rebuildOMHeadSHA, omInstalled(rebuildOMHeadSHA))

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
	cli := rebuildOMCli(t, d, rebuildOMHeadSHA, omInstalled(rebuildOMHeadSHA))
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
	if installs := installCalls(cli); len(installs) != 1 {
		t.Fatalf("an enabled patrol ran %d installs, want 1", len(installs))
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

func TestParseOMInstallResult(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, out string
		want      rebuildGTInstallResult
	}{
		{"installed", "log\ninstall-om: RESULT installed abc def -\n", rebuildGTInstallResult{"installed", "abc", "def", ""}},
		{"dashes are empty", "install-om: RESULT refused - - no-source\n", rebuildGTInstallResult{"refused", "", "", "no-source"}},
		{"short line pads", "install-om: RESULT noop\n", rebuildGTInstallResult{event: "noop"}},
		{"last line wins", "install-om: RESULT installed a b -\ninstall-om: RESULT failed a b x\n", rebuildGTInstallResult{"failed", "a", "b", "x"}},
		{"install-gt's line is not om's", "install-gt: RESULT installed a b -\n", rebuildGTInstallResult{}},
		{"no line", "boom\n", rebuildGTInstallResult{}},
	} {
		if got := parseOMInstallResult(tc.out); got != tc.want {
			t.Errorf("%s: result = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
