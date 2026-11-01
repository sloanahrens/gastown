package daemon

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

const (
	rebuildHMInstalledSHA = "1111111111111111111111111111111111111111"
	rebuildHMHeadSHA      = "2222222222222222222222222222222222222222"
	rebuildHMOtherSHA     = "3333333333333333333333333333333333333333"
	rebuildHMURL          = "https://example.invalid/hm.git"
)

// rebuildHMAnswers is what the fake hm and git answer for one cycle. Start from
// rebuildHMBehindAnswers: the zero value is not a town the tests mean.
type rebuildHMAnswers struct {
	installedSHA    string   // what the installed hm's version prints
	installedErr    bool     // the installed hm's version fails to run
	headSHA         string   // origin/main's tip, from ls-remote and rev-parse
	headErr         bool     // rev-parse origin/main fails in the clone
	headNotSHA      bool     // rev-parse origin/main answers with something that is not a commit
	changed         []string // the paths the clone's diff names
	notAncestor     bool     // the installed commit is not on origin/main's history
	ancestorErr     bool     // the ancestry check fails to run
	checkoutErr     bool     // the checkout of the built commit fails
	buildErr        bool     // go install fails
	builtOut        string   // what the built hm's version prints
	builtErr        bool     // the built hm's version fails to run
	renameAsideErr  bool     // renaming the installed hm to hm.prev fails
	renameIntoErr   bool     // renaming the build over the installed hm fails
	rollbackErr     bool     // restoring hm.prev over the installed hm fails
	lockBusy        bool     // another install holds the install lock
	lockDirIsFile   bool     // the install lock's directory is a regular file
	lockPathIsDir   bool     // the install lock's path is a directory
	relativeInstall bool     // the installed hm is at a relative path
}

// rebuildHMBehindAnswers is an hm at installed sha, origin's main at head, and a
// diff naming one code path: the case that must install.
func rebuildHMBehindAnswers() rebuildHMAnswers {
	return rebuildHMAnswers{
		installedSHA: rebuildHMInstalledSHA,
		headSHA:      rebuildHMHeadSHA,
		changed:      []string{"internal/cli/cmd_query.go"},
		builtOut:     "hm " + rebuildHMHeadSHA + "\n",
	}
}

// rebuildHMCall is one hm or git call the cycle made: its argv, the directory it
// ran in and its GOBIN, if it was given one.
type rebuildHMCall struct {
	argv  []string
	dir   string
	gobin string
}

// rebuildHMRig is one town the cycle runs in. Every hm and git call goes to its
// runner, and every rename goes to its rename seam, so nothing runs or moves a
// file. order records the calls and the renames in the sequence they happened.
type rebuildHMRig struct {
	t           *testing.T
	a           rebuildHMAnswers
	rec         *notifyfake.Recorder
	town        string
	src         string
	binDir      string
	built       string
	installPath string
	prev        string
	rigPath     string
	calls       []rebuildHMCall
	renames     [][2]string
	order       []string
	d           *Daemon
}

// newRebuildHMRig builds a daemon whose town holds an hm clone already in place,
// with its runner, rename seam and lock seam answering from a.
func newRebuildHMRig(t *testing.T, a rebuildHMAnswers) *rebuildHMRig {
	t.Helper()
	d, rec := daemonWithRecorder(t)
	town := d.config.TownRoot
	r := &rebuildHMRig{t: t, a: a, rec: rec, town: town, d: d}
	r.installPath = filepath.Join(town, "bin", "hm")
	if a.relativeInstall {
		r.installPath = "hm"
	}
	r.prev = filepath.Join(filepath.Dir(r.installPath), rebuildHMPrevName)
	r.src = filepath.Join(town, filepath.FromSlash(hmSkewCloneDir))
	r.binDir = filepath.Join(filepath.Dir(r.installPath), rebuildHMStageName)
	r.built = filepath.Join(r.binDir, "hm")
	r.rigPath = filepath.Join(town, filepath.FromSlash(hmSkewRigWorktree))
	mkdirs(t, filepath.Join(r.src, ".git"))

	d.clock = newFixedClock()
	d.execCmd = r.run
	d.hmSkew.pathFn = func() string { return r.installPath }
	d.rebuildHMRenameFn = r.rename

	// The install lock is the real flock under the town's daemon directory. The
	// lock-path faults are set up on disk, so the failure paths run for real.
	lockDir := filepath.Join(town, "daemon")
	lockPath := filepath.Join(lockDir, "install-hm.lock")
	switch {
	case a.lockDirIsFile:
		mkdirs(t, filepath.Dir(lockDir))
		if err := os.WriteFile(lockDir, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	case a.lockPathIsDir:
		mkdirs(t, lockPath)
	case a.lockBusy:
		mkdirs(t, lockDir)
		held := flock.New(lockPath)
		locked, err := held.TryLock()
		if err != nil || !locked {
			t.Fatalf("could not hold the install lock for the test: locked=%v err=%v", locked, err)
		}
		t.Cleanup(func() {
			if err := held.Unlock(); err != nil {
				t.Errorf("releasing the test's install lock: %v", err)
			}
		})
	}
	return r
}

// run answers one hm or git call from the rig's answers and records it.
func (r *rebuildHMRig) run(cmd *exec.Cmd) ([]byte, []byte, error) {
	joined := strings.Join(cmd.Args, " ")
	r.calls = append(r.calls, rebuildHMCall{argv: slices.Clone(cmd.Args), dir: cmd.Dir, gobin: gobinOf(cmd.Env)})
	r.order = append(r.order, joined)

	a := r.a
	var out string
	var err error
	switch {
	case cmd.Args[0] == r.installPath:
		out = "hm " + a.installedSHA + "\n"
		if a.installedErr {
			err = errors.New("exit status 1")
		}
	case cmd.Args[0] == r.built:
		out = a.builtOut
		if a.builtErr {
			err = errors.New("exit status 1")
		}
	case cmd.Args[0] == "go":
		if a.buildErr {
			err = errors.New("exit status 2")
		}
	case strings.Contains(joined, " remote get-url origin"):
		out = rebuildHMURL + "\n"
	case strings.Contains(joined, " ls-remote "):
		out = a.headSHA + "\trefs/heads/main\n"
	case strings.Contains(joined, " fetch "):
	case strings.Contains(joined, " diff --name-only "):
		for _, p := range a.changed {
			out += p + "\x00"
		}
	case strings.Contains(joined, " rev-parse --verify -q "):
		out = a.installedSHA
	case strings.Contains(joined, " rev-parse origin/main"):
		out = a.headSHA + "\n"
		if a.headNotSHA {
			out = "main\n"
		}
		if a.headErr {
			err = errors.New("exit status 128")
		}
	case strings.Contains(joined, " merge-base --is-ancestor "):
		switch {
		case a.ancestorErr:
			err = hmSkewExit(128)
		case a.notAncestor:
			err = hmSkewExit(1)
		}
	case strings.Contains(joined, " checkout "):
		if a.checkoutErr {
			err = errors.New("exit status 1")
		}
	default:
		r.t.Errorf("unexpected call: %s", joined)
		err = errors.New("unexpected call")
	}
	// combinedOutput reads cmd.Stdout; runCmd reads the returned stdout. The fake
	// answers both, so either caller sees the same output.
	if cmd.Stdout != nil {
		if _, werr := io.WriteString(cmd.Stdout, out); werr != nil {
			r.t.Errorf("writing the fake output of %s: %v", joined, werr)
		}
	}
	return []byte(out), nil, err
}

// rename records one rename of the swap and fails the one the answers name.
func (r *rebuildHMRig) rename(from, to string) error {
	r.renames = append(r.renames, [2]string{from, to})
	r.order = append(r.order, "rename "+from+" "+to)
	switch {
	case from == r.installPath && to == r.prev && r.a.renameAsideErr:
		return errors.New("permission denied")
	case from == r.built && to == r.installPath && r.a.renameIntoErr:
		return errors.New("cross-device link")
	case from == r.prev && to == r.installPath && r.a.rollbackErr:
		return errors.New("permission denied")
	}
	return nil
}

// gobinOf is the GOBIN an environment sets, or "" when it sets none.
func gobinOf(env []string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GOBIN="); ok {
			return v
		}
	}
	return ""
}

// buildCall is the go install call the cycle made, or fails the test.
func (r *rebuildHMRig) buildCall() rebuildHMCall {
	r.t.Helper()
	for _, c := range r.calls {
		if c.argv[0] == "go" {
			return c
		}
	}
	r.t.Fatal("the cycle made no go install call")
	return rebuildHMCall{}
}

// wantEscalation asserts exactly one escalation went out, under key at severity.
func (r *rebuildHMRig) wantEscalation(key, severity string) {
	r.t.Helper()
	esc := r.rec.Escalations()
	if len(esc) != 1 {
		r.t.Fatalf("escalations = %+v, want exactly one under %q", esc, key)
	}
	if got := esc[0].Escalation; got.Fingerprint != key || got.Severity != severity {
		r.t.Errorf("escalation fingerprint/severity = %q/%q, want %q/%q", got.Fingerprint, got.Severity, key, severity)
	}
}

// wantNoRename asserts the installed hm was not touched: no rename ran.
func (r *rebuildHMRig) wantNoRename() {
	r.t.Helper()
	if len(r.renames) != 0 {
		r.t.Errorf("renames = %v, want none: the installed hm must stay where it is", r.renames)
	}
}

// wantNoEscalation asserts nothing went out: the cycle deferred or found nothing
// to do, and a deferral is not an alarm.
func (r *rebuildHMRig) wantNoEscalation() {
	r.t.Helper()
	if esc := r.rec.Escalations(); len(esc) != 0 {
		r.t.Errorf("escalations = %+v, want none", esc)
	}
}

// wantNoInstall asserts the installed hm was not touched and nothing escalated.
func (r *rebuildHMRig) wantNoInstall() {
	r.t.Helper()
	r.wantNoRename()
	r.wantNoEscalation()
}

// TestRebuildHMInstallsWhenBehind: an installed hm behind origin's main on a code
// path builds in the job clone into the job directory, checks the new binary's
// version, renames the old one to hm.prev and the new one into place (acceptance 1).
func TestRebuildHMInstallsWhenBehind(t *testing.T) {
	t.Parallel()
	r := newRebuildHMRig(t, rebuildHMBehindAnswers())

	if !r.d.runRebuildHM() {
		t.Fatal("the cycle reached no verdict after a successful install")
	}

	build := r.buildCall()
	if build.dir != r.src {
		t.Errorf("go install ran in %q, want the job clone %q", build.dir, r.src)
	}
	if build.gobin != r.binDir {
		t.Errorf("GOBIN = %q, want the job directory %q", build.gobin, r.binDir)
	}
	// The build lands beside the installed hm, so the swap's renames stay on its
	// volume: a build under the town would fail with a cross-device link there.
	if filepath.Dir(build.gobin) != filepath.Dir(r.installPath) {
		t.Errorf("GOBIN = %q, want a directory beside the installed hm %q", build.gobin, r.installPath)
	}
	if want := "-ldflags=-X " + rebuildHMVersionVar + "=" + rebuildHMHeadSHA; !slices.Contains(build.argv, want) {
		t.Errorf("go install argv = %v, want it to stamp the built commit as %q", build.argv, want)
	}

	wantRenames := [][2]string{{r.installPath, r.prev}, {r.built, r.installPath}}
	if !slices.Equal(r.renames, wantRenames) {
		t.Errorf("renames = %v, want %v", r.renames, wantRenames)
	}
	versionAt := slices.IndexFunc(r.order, func(s string) bool { return strings.Contains(s, r.built+" version") })
	renameAt := slices.IndexFunc(r.order, func(s string) bool { return strings.HasPrefix(s, "rename ") })
	if versionAt < 0 || renameAt < 0 || versionAt > renameAt {
		t.Errorf("the version check must run before the first rename: order = %v", r.order)
	}
	if esc := r.rec.Escalations(); len(esc) != 0 {
		t.Errorf("escalations = %+v, want none after a successful install", esc)
	}
}

// TestRebuildHMRefusesABuildThatDoesNotReportItsSha: a new binary whose version
// does not print the commit it was built from is not installed, the installed hm
// is untouched, and the failure escalates under hm-skew (acceptance 2).
func TestRebuildHMRefusesABuildThatDoesNotReportItsSha(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		builtOut string
		builtErr bool
	}{
		{name: "reports another commit", builtOut: "hm " + rebuildHMOtherSHA + "\n"},
		{name: "prints no sha line", builtOut: "hm dev\n"},
		{name: "prints nothing", builtOut: ""},
		{name: "fails to run", builtErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := rebuildHMBehindAnswers()
			a.builtOut = tc.builtOut
			a.builtErr = tc.builtErr
			r := newRebuildHMRig(t, a)

			if !r.d.runRebuildHM() {
				t.Fatal("a refused install reached no verdict, so the interval would not hold")
			}
			r.wantNoRename()
			r.wantEscalation(hmSkewAlertKey, "MEDIUM")
		})
	}
}

// TestRebuildHMIsOptIn: with patrols.rebuild_hm absent or enabled:false a trigger
// does nothing: no git or go call, no rename, no escalation, no single-flight guard
// taken (acceptance 3). The control shows the same config turns the job on once
// enabled.
func TestRebuildHMIsOptIn(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  *DaemonPatrolConfig
	}{
		{name: "no patrols block", cfg: &DaemonPatrolConfig{}},
		{name: "block absent", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}},
		{name: "enabled false", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{RebuildHM: &PatrolConfig{Enabled: false}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRebuildHMRig(t, rebuildHMBehindAnswers())
			r.d.patrolConfig = tc.cfg

			if r.d.isPatrolActive("rebuild_hm") {
				t.Error("rebuild_hm is on with its block absent or disabled")
			}
			// With the gate shut, triggerRebuildHM returns before it starts its
			// goroutine, so nothing runs concurrently with the checks below.
			r.d.triggerRebuildHM()
			if len(r.calls) != 0 {
				t.Errorf("a trigger with the job off made calls: %v", r.calls)
			}
			r.wantNoInstall()
			if r.d.rebuildHMRunning.Load() {
				t.Error("a trigger with the job off took the single-flight guard")
			}
		})
	}

	t.Run("enabled true turns it on", func(t *testing.T) {
		t.Parallel()
		r := newRebuildHMRig(t, rebuildHMBehindAnswers())
		r.d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{RebuildHM: &PatrolConfig{Enabled: true}}}

		if !r.d.isPatrolActive("rebuild_hm") {
			t.Error("enabled:true did not turn rebuild_hm on")
		}
	})
}

// TestRebuildHMTouchesOnlyItsOwnPaths: the cycle builds only in the job clone,
// fetches only there, never runs in or writes under <town>/hm/mayor/rig, and
// swaps the binary by renames only, never a copy (acceptance 4).
func TestRebuildHMTouchesOnlyItsOwnPaths(t *testing.T) {
	t.Parallel()
	r := newRebuildHMRig(t, rebuildHMBehindAnswers())

	if !r.d.runRebuildHM() {
		t.Fatal("the cycle reached no verdict after a successful install")
	}

	for _, c := range r.calls {
		joined := strings.Join(c.argv, " ")
		inRig := c.dir == r.rigPath || strings.Contains(joined, r.rigPath)
		readsOrigin := strings.Contains(joined, " remote get-url origin")
		if inRig && !readsOrigin {
			t.Errorf("call touched the rig worktree: %s (dir %q)", joined, c.dir)
		}
		switch c.argv[0] {
		case "cp", "install", "mv", "ln":
			t.Errorf("call copies or links a binary: %s", joined)
		case "go":
			if c.dir != r.src {
				t.Errorf("go ran in %q, want only the job clone", c.dir)
			}
		}
	}

	for _, rn := range r.renames {
		for _, p := range rn {
			if strings.Contains(p, r.rigPath) {
				t.Errorf("rename touched the rig worktree: %v", rn)
			}
		}
	}
	wantRenames := [][2]string{{r.installPath, r.prev}, {r.built, r.installPath}}
	if !slices.Equal(r.renames, wantRenames) {
		t.Errorf("renames = %v, want exactly %v", r.renames, wantRenames)
	}
}

// TestRebuildHMFailedInstallLeavesTheInstalledHMAndEscalates: a failure at any
// step of the install escalates under hm-skew, and the installed hm is left in
// place. A failed second rename is rolled back from hm.prev, and the escalation
// says the installed hm stays in force. A failed rollback leaves the path empty:
// it escalates at HIGH and names hm.prev, and never says the installed hm stays.
func TestRebuildHMFailedInstallLeavesTheInstalledHMAndEscalates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		mutate       func(*rebuildHMAnswers)
		wantRenames  [][2]string
		wantSeverity string // "" is MEDIUM, the refused-install severity
		wantInMsg    string
		wantNotInMsg string
	}{
		{
			name:   "checkout fails",
			mutate: func(a *rebuildHMAnswers) { a.checkoutErr = true },
		},
		{
			name:   "go install fails",
			mutate: func(a *rebuildHMAnswers) { a.buildErr = true },
		},
		{
			name:        "keeping the installed hm fails",
			mutate:      func(a *rebuildHMAnswers) { a.renameAsideErr = true },
			wantRenames: [][2]string{{"bin/hm", "bin/hm.prev"}},
			wantInMsg:   "stays in force",
		},
		{
			name:        "the new hm does not rename into place, and the rollback holds",
			mutate:      func(a *rebuildHMAnswers) { a.renameIntoErr = true },
			wantRenames: [][2]string{{"bin/hm", "bin/hm.prev"}, {"bin/.hm-rebuild/hm", "bin/hm"}, {"bin/hm.prev", "bin/hm"}},
			wantInMsg:   "stays in force",
		},
		{
			name:         "the new hm does not rename into place, and the rollback fails",
			mutate:       func(a *rebuildHMAnswers) { a.renameIntoErr, a.rollbackErr = true, true },
			wantRenames:  [][2]string{{"bin/hm", "bin/hm.prev"}, {"bin/.hm-rebuild/hm", "bin/hm"}, {"bin/hm.prev", "bin/hm"}},
			wantSeverity: "HIGH",
			wantInMsg:    "restore it by hand from hm.prev",
			wantNotInMsg: "stays in force",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := rebuildHMBehindAnswers()
			tc.mutate(&a)
			r := newRebuildHMRig(t, a)

			if !r.d.runRebuildHM() {
				t.Fatal("a failed install reached no verdict, so the interval would not hold")
			}
			if tc.wantRenames == nil {
				if len(r.renames) != 0 {
					t.Errorf("renames = %v, want none before a swap", r.renames)
				}
			} else {
				got := make([][2]string, 0, len(r.renames))
				for _, rn := range r.renames {
					got = append(got, [2]string{relToTown(r.town, rn[0]), relToTown(r.town, rn[1])})
				}
				if !slices.Equal(got, tc.wantRenames) {
					t.Errorf("renames = %v, want %v", got, tc.wantRenames)
				}
			}
			severity := tc.wantSeverity
			if severity == "" {
				severity = "MEDIUM"
			}
			r.wantEscalation(hmSkewAlertKey, severity)
			if len(r.rec.Escalations()) == 1 {
				msg := r.rec.Escalations()[0].Escalation.Reason
				if tc.wantInMsg != "" && !strings.Contains(msg, tc.wantInMsg) {
					t.Errorf("escalation %q does not say %q", msg, tc.wantInMsg)
				}
				if tc.wantNotInMsg != "" && strings.Contains(msg, tc.wantNotInMsg) {
					t.Errorf("escalation %q says %q, which is false once the rollback failed", msg, tc.wantNotInMsg)
				}
			}
		})
	}
}

// TestRebuildHMDefersWithoutAVerdict: a reading that cannot be taken, a lock that
// is busy, a clone that cannot say which commit to build, an ancestry check that
// cannot answer, or an installed path that cannot be swapped, each leaves the
// installed hm alone and reports no verdict, so the next heartbeat asks again.
func TestRebuildHMDefersWithoutAVerdict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*rebuildHMAnswers)
	}{
		{name: "the installed hm cannot be read", mutate: func(a *rebuildHMAnswers) { a.installedErr = true }},
		{name: "origin/main cannot be read in the clone", mutate: func(a *rebuildHMAnswers) { a.headErr = true }},
		{name: "origin/main in the clone is not a commit", mutate: func(a *rebuildHMAnswers) { a.headNotSHA = true }},
		{name: "the ancestry check cannot run", mutate: func(a *rebuildHMAnswers) { a.ancestorErr = true }},
		{name: "the install lock is busy", mutate: func(a *rebuildHMAnswers) { a.lockBusy = true }},
		{name: "the lock directory is a file", mutate: func(a *rebuildHMAnswers) { a.lockDirIsFile = true }},
		{name: "the lock path is a directory", mutate: func(a *rebuildHMAnswers) { a.lockPathIsDir = true }},
		{name: "the installed hm is at a relative path", mutate: func(a *rebuildHMAnswers) { a.relativeInstall = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := rebuildHMBehindAnswers()
			tc.mutate(&a)
			r := newRebuildHMRig(t, a)

			if r.d.runRebuildHM() {
				t.Error("a cycle that did nothing reported a verdict, so the interval would hold")
			}
			if c := r.buildCallOrNil(); c != nil {
				t.Errorf("the cycle built after deferring: %v", c.argv)
			}
			r.wantNoInstall()
		})
	}
}

// TestRebuildHMLeavesAnHMAheadOfMainInPlace: a skew reading against an installed
// commit that is not on origin/main's history (a hotfix not yet landed) is no
// install: installing main would be a downgrade. The cycle reaches its verdict.
func TestRebuildHMLeavesAnHMAheadOfMainInPlace(t *testing.T) {
	t.Parallel()
	a := rebuildHMBehindAnswers()
	a.notAncestor = true
	r := newRebuildHMRig(t, a)

	if !r.d.runRebuildHM() {
		t.Error("an hm left in place reported no verdict")
	}
	if c := r.buildCallOrNil(); c != nil {
		t.Errorf("the cycle built an hm that is ahead of origin/main: %v", c.argv)
	}
	r.wantNoInstall()
}

// TestRebuildHMIsQuietWhenCurrent: an installed hm at origin's main is a verdict
// with nothing to do: no build, no swap, no escalation.
func TestRebuildHMIsQuietWhenCurrent(t *testing.T) {
	t.Parallel()
	a := rebuildHMBehindAnswers()
	a.installedSHA = rebuildHMHeadSHA
	r := newRebuildHMRig(t, a)

	if !r.d.runRebuildHM() {
		t.Error("a current hm reported no verdict")
	}
	if c := r.buildCallOrNil(); c != nil {
		t.Errorf("the cycle built an hm that is already current: %v", c.argv)
	}
	r.wantNoInstall()
}

// buildCallOrNil is the go install call the cycle made, or nil when it made none.
func (r *rebuildHMRig) buildCallOrNil() *rebuildHMCall {
	for i := range r.calls {
		if r.calls[i].argv[0] == "go" {
			return &r.calls[i]
		}
	}
	return nil
}

// relToTown names a path under the town as a town-relative slash path, so a test
// table reads the same on every host. A path outside the town is kept whole.
func relToTown(town, p string) string {
	if rel, ok := strings.CutPrefix(p, town+"/"); ok {
		return rel
	}
	return p
}

// TestRebuildHMInterval pins the due interval: the configured one when it parses,
// the hourly default otherwise.
func TestRebuildHMInterval(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  *DaemonPatrolConfig
		want time.Duration
	}{
		{name: "no config", cfg: nil, want: defaultRebuildHMInterval},
		{name: "no block", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, want: defaultRebuildHMInterval},
		{name: "configured", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{RebuildHM: &PatrolConfig{Interval: "30m"}}}, want: 30 * time.Minute},
		{name: "unparseable", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{RebuildHM: &PatrolConfig{Interval: "soon"}}}, want: defaultRebuildHMInterval},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := rebuildHMInterval(tc.cfg); got != tc.want {
				t.Errorf("interval = %v, want %v", got, tc.want)
			}
		})
	}
}
