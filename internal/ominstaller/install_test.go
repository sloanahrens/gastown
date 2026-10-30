package ominstaller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/installlock"
)

// installSrc is the om clone the build reads. Its git answers come from the rig.
const installSrc = "/town/.runtime/om-src"

// versionOut is the "om version <sha>" line an om built from sha prints.
func versionOut(sha string) string {
	return "om version " + sha + "\n"
}

// installRig is one install run over in-memory seams. om at installedOM starts as
// the previous commit, the signing material is in place (newSignRig), and the
// clone answers git from the fields below. steps records, in order, each version
// probe, the build, the signing commands, and the swap.
type installRig struct {
	files *fakeFiles
	run   *fakeRun
	acq   *fakeAcquire
	logs  *logRecorder
	cfg   Config

	steps []string
	// ldflags is the -ldflags value the build was run with.
	ldflags string

	// git answers: repoErr fails the work-tree check (repoStderr names a
	// non-repository), headErr fails HEAD, statusOut and statusErr answer status.
	repoErr    error
	repoStderr string
	headErr    error
	statusOut  string
	statusErr  error

	buildErr error
	swapErr  error
	// swaps counts the swaps, the first being the install's and any later one the
	// rollback's.
	swaps int
	// rollbackErr fails the rollback's swap.
	rollbackErr error
	// swapBytes, when set, is what the install's swap leaves at om instead of the build.
	swapBytes string
	// versionErr fails the version probe that runs after the swap, as a hung om does.
	versionErr error
	// prevAtSwap is om.prev as the swap found it, so a test can see the previous
	// om was kept before the swap.
	prevAtSwap []byte
}

// newInstallRig is an install over a clone at commitNew with om at commitOld.
func newInstallRig(t *testing.T) *installRig {
	t.Helper()
	sr := newSignRig()
	delete(sr.files.files, omBuild)
	r := &installRig{
		files: sr.files,
		run:   sr.run,
		acq:   &fakeAcquire{},
		logs:  sr.logs,
	}
	r.files.files[installedOM] = []byte(versionOut(commitOld))
	r.files.modes[installedOM] = 0o755
	r.cfg = sr.cfg
	r.cfg.OMPath = installedOM
	r.cfg.SrcDir = installSrc
	r.cfg.DaemonDir = t.TempDir()
	r.cfg.Source = "manual"
	r.cfg.LockWait = 300 * time.Second
	r.cfg.SmokeWait = 30 * time.Second
	r.cfg.Acquire = r.acq.acquire
	r.cfg.Exec = r.exec
	r.cfg.Swap = r.swap
	r.cfg.Now = func() time.Time { return fixedNow }
	r.cfg.Logf = r.logs.logf
	return r
}

// exec answers every command the install runs. git answers from the rig's fields,
// om answers from the file at its path, go writes the built om, and the signing
// commands go to the sign rig's runner.
func (r *installRig) exec(ctx context.Context, c Command) (string, string, error) {
	if c.Name == "git" {
		return r.git(c)
	}
	switch c.Name {
	case r.cfg.OMPath:
		r.steps = append(r.steps, "version")
		if r.swaps > 0 && r.versionErr != nil {
			return "", "", r.versionErr
		}
		data, ok := r.files.files[c.Name]
		if !ok {
			return "", "", notExist("exec", c.Name)
		}
		return string(data), "", nil
	case "go":
		r.steps = append(r.steps, "build")
		r.ldflags = c.Args[1]
		if r.buildErr != nil {
			return "", "", r.buildErr
		}
		sha := strings.TrimPrefix(r.ldflags, "-ldflags=-X main.version=")
		r.files.files[c.Args[3]] = []byte(versionOut(sha))
		return "", "", nil
	case "security", "codesign":
		r.steps = append(r.steps, c.Name)
		return r.run.exec(ctx, c)
	}
	return "", "", fmt.Errorf("unexpected command %s", c.Name)
}

func (r *installRig) git(c Command) (string, string, error) {
	if c.Args[0] == "status" {
		return r.statusOut, "", r.statusErr
	}
	switch c.Args[1] {
	case "--is-inside-work-tree":
		if r.repoErr != nil {
			return "", r.repoStderr, r.repoErr
		}
		return "true\n", "", nil
	case "--verify":
		if r.headErr != nil {
			return "", "", r.headErr
		}
		return commitNew + "\n", "", nil
	}
	return "", "", fmt.Errorf("unexpected git %v", c.Args)
}

// swap is the Replace seam. The install's swap records what om.prev held, then puts
// the build at dst, or the test's bytes for it, unless the test fails the swap. The
// rollback's swap copies src to dst, unless the test fails it.
func (r *installRig) swap(src, dst string) error {
	r.steps = append(r.steps, "swap")
	r.swaps++
	if r.swaps > 1 {
		if r.rollbackErr != nil {
			return r.rollbackErr
		}
		r.files.files[dst] = r.files.files[src]
		r.files.modes[dst] = 0o755
		return nil
	}
	r.prevAtSwap = append([]byte(nil), r.files.files[keptPrev]...)
	if r.swapErr != nil {
		return r.swapErr
	}
	if r.swapBytes != "" {
		r.files.files[dst] = []byte(r.swapBytes)
		return nil
	}
	r.files.files[dst] = r.files.files[src]
	r.files.modes[dst] = 0o755
	return nil
}

// tempPath is the build's temp file beside the rig's om.
func (r *installRig) tempPath() string {
	return filepath.Join(filepath.Dir(r.cfg.OMPath), newBinName)
}

// receipts reads the receipts the run appended.
func (r *installRig) receipts(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.cfg.DaemonDir, "install-om-receipts.jsonl"))
	if err != nil {
		t.Fatalf("read receipts: %v", err)
	}
	return string(data)
}

func TestInstallRefusesBeforeWriting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		setup      func(r *installRig)
		wantReason string
	}{
		{
			name: "no clone",
			setup: func(r *installRig) {
				r.repoErr = errors.New("exit status 128")
				r.repoStderr = "fatal: not a git repository (or any of the parent directories): .git"
			},
			wantReason: "no-source",
		},
		{
			name:       "git cannot run to tell whether there is a clone",
			setup:      func(r *installRig) { r.repoErr = errors.New(`exec: "git": not found`) },
			wantReason: "no-source",
		},
		{
			name:       "unreadable HEAD",
			setup:      func(r *installRig) { r.headErr = errors.New("exit status 128") },
			wantReason: "no-source",
		},
		{
			name:       "dirty tree",
			setup:      func(r *installRig) { r.statusOut = " M go.mod\n" },
			wantReason: "dirty",
		},
		{
			name:       "failing git status",
			setup:      func(r *installRig) { r.statusErr = errors.New("exit status 128") },
			wantReason: "dirty",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newInstallRig(t)
			tc.setup(r)
			before := string(r.files.files[installedOM])

			res, exit := Run(context.Background(), r.cfg)

			if want := (Result{Event: "refused", Reason: tc.wantReason}); res != want {
				t.Errorf("result = %+v, want %+v", res, want)
			}
			if exit != ExitRefused {
				t.Errorf("exit = %d, want %d", exit, ExitRefused)
			}
			if len(r.steps) != 0 {
				t.Errorf("steps = %v, want none: a refusal runs no build, sign or swap", r.steps)
			}
			if len(r.files.writes) != 0 || len(r.files.renames) != 0 {
				t.Errorf("writes = %v, renames = %v, want none", r.files.writes, r.files.renames)
			}
			if got := string(r.files.files[installedOM]); got != before {
				t.Errorf("om = %q, want it untouched", got)
			}
			if _, ok := r.files.files[r.tempPath()]; ok {
				t.Error("a build temp file was left beside om")
			}
		})
	}
}

func TestInstallNothingToDoWhenTheInstalledCommitMatches(t *testing.T) {
	t.Parallel()
	r := newInstallRig(t)
	r.files.files[installedOM] = []byte(versionOut(commitNew))

	res, exit := Run(context.Background(), r.cfg)

	if want := (Result{Event: "noop", Commit: commitNew, Prev: commitNew, Reason: "already-installed"}); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	if exit != ExitInstalled {
		t.Errorf("exit = %d, want %d", exit, ExitInstalled)
	}
	if want := []string{"version"}; !reflect.DeepEqual(r.steps, want) {
		t.Errorf("steps = %v, want %v: nothing is built or swapped", r.steps, want)
	}
	if !strings.Contains(r.receipts(t), `"event": "noop"`) {
		t.Errorf("receipts = %q, want one noop receipt", r.receipts(t))
	}
}

func TestInstallHappyPath(t *testing.T) {
	t.Parallel()
	r := newInstallRig(t)

	res, exit := Run(context.Background(), r.cfg)

	if want := (Result{Event: "installed", Commit: commitNew, Prev: commitOld}); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	if exit != ExitInstalled {
		t.Errorf("exit = %d, want %d", exit, ExitInstalled)
	}
	wantSteps := []string{"version", "build", "security", "codesign", "swap", "version"}
	if !reflect.DeepEqual(r.steps, wantSteps) {
		t.Errorf("steps = %v, want %v", r.steps, wantSteps)
	}
	if want := "-ldflags=-X main.version=" + commitNew; r.ldflags != want {
		t.Errorf("build ldflags = %q, want %q", r.ldflags, want)
	}
	if got := string(r.files.files[installedOM]); got != versionOut(commitNew) {
		t.Errorf("om = %q, want the built om in force", got)
	}
	if got := string(r.prevAtSwap); got != versionOut(commitOld) {
		t.Errorf("om.prev at the swap = %q, want the previous om kept before the swap", got)
	}
	if _, ok := r.files.files[r.tempPath()]; ok {
		t.Error("the build temp file was left after the swap")
	}
	if !strings.Contains(r.receipts(t), `"prev_commit": "`+commitOld+`"`) {
		t.Errorf("receipts = %q, want the previous commit recorded", r.receipts(t))
	}
}

func TestInstallBusyWhenTheLockWaitRunsOut(t *testing.T) {
	t.Parallel()
	r := newInstallRig(t)
	r.acq.err = installlock.ErrBusy

	res, exit := Run(context.Background(), r.cfg)

	if want := (Result{Event: "refused", Reason: "lock-busy"}); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	if exit != ExitBusy {
		t.Errorf("exit = %d, want %d", exit, ExitBusy)
	}
	if len(r.steps) != 0 {
		t.Errorf("steps = %v, want none: a busy run builds nothing", r.steps)
	}
	want := "[install-om] Another install held the lock for 300s; not installing."
	if !containsLine(r.logs.lines, want) {
		t.Errorf("logs = %q, want the busy message %q", r.logs.lines, want)
	}
}

// TestInstallFailureBeforeTheSwapLeavesOMUntouched drives each failure that comes
// before the rename. Each one leaves om as it was and no build behind.
func TestInstallFailureBeforeTheSwapLeavesOMUntouched(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		setup      func(r *installRig)
		wantReason string
		wantPrev   string
		wantSteps  []string
	}{
		{
			name:       "the build cannot be made executable",
			setup:      func(r *installRig) { r.files.chmodErr = errors.New("permission denied") },
			wantReason: "build-failed",
			wantPrev:   commitOld,
			wantSteps:  []string{"version", "build"},
		},
		{
			name:       "a build temp that would be a flag is refused before the build",
			setup:      func(r *installRig) { r.cfg.OMPath = "-x/om" },
			wantReason: "build-failed",
			wantPrev:   "",
			wantSteps:  []string{"version"},
		},
		{
			name:       "codesign fails",
			setup:      func(r *installRig) { r.run.codesignErr = errors.New("exit status 1") },
			wantReason: "sign",
			wantPrev:   commitOld,
			wantSteps:  []string{"version", "build", "security", "codesign"},
		},
		{
			name:       "keeping the previous om fails",
			setup:      func(r *installRig) { r.files.writeErr = errors.New("disk full") },
			wantReason: "keep-previous",
			wantPrev:   commitOld,
			wantSteps:  []string{"version", "build", "security", "codesign"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newInstallRig(t)
			tc.setup(r)
			omPath := r.cfg.OMPath
			before := string(r.files.files[omPath])

			res, exit := Run(context.Background(), r.cfg)

			if want := (Result{Event: "failed", Commit: commitNew, Prev: tc.wantPrev, Reason: tc.wantReason}); res != want {
				t.Errorf("result = %+v, want %+v", res, want)
			}
			if exit != ExitFailed {
				t.Errorf("exit = %d, want %d", exit, ExitFailed)
			}
			if !reflect.DeepEqual(r.steps, tc.wantSteps) {
				t.Errorf("steps = %v, want %v", r.steps, tc.wantSteps)
			}
			if got := string(r.files.files[omPath]); got != before {
				t.Errorf("om = %q, want it untouched", got)
			}
			if _, ok := r.files.files[filepath.Join(filepath.Dir(omPath), newBinName)]; ok {
				t.Error("the build temp file was left behind")
			}
		})
	}
}

// commitOther is a commit the built om is not, for an om that reports the wrong sha.
const commitOther = "2222222222222222222222222222222222222222"

// TestBuildAndRenameFailuresLeavePathUntouched drives the two failures that stop the
// swap from putting anything in force: a build that fails and a rename that is
// refused. Each leaves om as it was, reports failed with its reason, exits 1 and
// leaves no build behind.
func TestBuildAndRenameFailuresLeavePathUntouched(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		setup      func(r *installRig)
		wantReason string
		wantSteps  []string
	}{
		{
			name:       "go build fails",
			setup:      func(r *installRig) { r.buildErr = errors.New("exit status 2") },
			wantReason: "build-failed",
			wantSteps:  []string{"version", "build"},
		},
		{
			name:       "the rename fails",
			setup:      func(r *installRig) { r.swapErr = errors.New("rename denied") },
			wantReason: "swap-failed",
			wantSteps:  []string{"version", "build", "security", "codesign", "swap"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newInstallRig(t)
			tc.setup(r)
			before := string(r.files.files[installedOM])

			res, exit := Run(context.Background(), r.cfg)

			if want := (Result{Event: "failed", Commit: commitNew, Prev: commitOld, Reason: tc.wantReason}); res != want {
				t.Errorf("result = %+v, want %+v", res, want)
			}
			if exit != ExitFailed {
				t.Errorf("exit = %d, want %d", exit, ExitFailed)
			}
			if !reflect.DeepEqual(r.steps, tc.wantSteps) {
				t.Errorf("steps = %v, want %v: nothing after the failed step runs", r.steps, tc.wantSteps)
			}
			if got := string(r.files.files[installedOM]); got != before {
				t.Errorf("om = %q, want it untouched", got)
			}
			if _, ok := r.files.files[r.tempPath()]; ok {
				t.Error("the build temp file was left behind")
			}
		})
	}
}

// TestSmokeFailureRollsBackToThePreviousBinary drives each way the smoke check can
// fail after the swap: the new om reports another commit, prints no version line, or
// its probe fails as a hung om's does. Each puts the previous om back over om through
// the swap, reports rolled_back with smoke-failed and exits 1.
func TestSmokeFailureRollsBackToThePreviousBinary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(r *installRig)
	}{
		{
			name:  "the new om reports another commit",
			setup: func(r *installRig) { r.swapBytes = versionOut(commitOther) },
		},
		{
			name:  "the new om prints no version line",
			setup: func(r *installRig) { r.swapBytes = "usage: om <command>\n" },
		},
		{
			name:  "the version probe fails",
			setup: func(r *installRig) { r.versionErr = errBoom },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newInstallRig(t)
			tc.setup(r)

			res, exit := Run(context.Background(), r.cfg)

			if want := (Result{Event: "rolled_back", Commit: commitNew, Prev: commitOld, Reason: "smoke-failed"}); res != want {
				t.Errorf("result = %+v, want %+v", res, want)
			}
			if exit != ExitFailed {
				t.Errorf("exit = %d, want %d", exit, ExitFailed)
			}
			wantSteps := []string{"version", "build", "security", "codesign", "swap", "version", "swap"}
			if !reflect.DeepEqual(r.steps, wantSteps) {
				t.Errorf("steps = %v, want %v", r.steps, wantSteps)
			}
			if got := string(r.files.files[installedOM]); got != versionOut(commitOld) {
				t.Errorf("om = %q, want the previous om back in force", got)
			}
			if _, ok := r.files.files[r.tempPath()]; ok {
				t.Error("the build temp file was left behind")
			}
			if !strings.Contains(r.receipts(t), `"event": "rolled_back"`) {
				t.Errorf("receipts = %q, want one rolled_back receipt", r.receipts(t))
			}
			if want := "[install-om] Rolled back to " + commitOld + "."; !containsLine(r.logs.lines, want) {
				t.Errorf("logs = %q, want %q", r.logs.lines, want)
			}
		})
	}
}

// TestSmokeFailureOnAFirstInstallRemovesTheNewOM is the rollback with no previous
// om: the new om is removed, so nothing broken is left in force, and the run is
// rolled_back with no prev commit.
func TestSmokeFailureOnAFirstInstallRemovesTheNewOM(t *testing.T) {
	t.Parallel()
	r := newInstallRig(t)
	delete(r.files.files, installedOM)
	r.swapBytes = versionOut(commitOther)

	res, exit := Run(context.Background(), r.cfg)

	if want := (Result{Event: "rolled_back", Commit: commitNew, Reason: "smoke-failed"}); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	if exit != ExitFailed {
		t.Errorf("exit = %d, want %d", exit, ExitFailed)
	}
	if _, ok := r.files.files[installedOM]; ok {
		t.Error("the new om is still in force after a first-install rollback")
	}
	if want := []string{"version", "build", "security", "codesign", "swap", "version"}; !reflect.DeepEqual(r.steps, want) {
		t.Errorf("steps = %v, want %v: a first install rolls back by removal, not a swap", r.steps, want)
	}
}

// TestFirstInstallRollbackFailureIsReportedAsFailure: with no om.prev, a removal of
// the broken new om that fails must not read as a rollback. The run is failed with
// rollback-failed, and the removal error is named in the log.
func TestFirstInstallRollbackFailureIsReportedAsFailure(t *testing.T) {
	t.Parallel()
	r := newInstallRig(t)
	delete(r.files.files, installedOM)
	r.swapBytes = versionOut(commitOther)
	r.files.removeErr[installedOM] = errBoom

	res, exit := Run(context.Background(), r.cfg)

	if want := (Result{Event: "failed", Commit: commitNew, Reason: "rollback-failed"}); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	if exit != ExitFailed {
		t.Errorf("exit = %d, want %d", exit, ExitFailed)
	}
	if !containsSubstring(r.logs.lines, errBoom.Error()) {
		t.Errorf("logs = %q, want the removal error %q named", r.logs.lines, errBoom.Error())
	}
	if !strings.Contains(r.receipts(t), `"reason": "rollback-failed"`) {
		t.Errorf("receipts = %q, want a rollback-failed receipt", r.receipts(t))
	}
}

// TestRollbackThatFailsReportsBothReasons: when the previous om cannot be put back,
// the run is failed with rollback-failed, and the log names both the smoke failure
// and the rollback failure on the one CRITICAL line.
func TestRollbackThatFailsReportsBothReasons(t *testing.T) {
	t.Parallel()
	r := newInstallRig(t)
	r.swapBytes = versionOut(commitOther)
	r.rollbackErr = errBoom

	res, exit := Run(context.Background(), r.cfg)

	if want := (Result{Event: "failed", Commit: commitNew, Prev: commitOld, Reason: "rollback-failed"}); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	if exit != ExitFailed {
		t.Errorf("exit = %d, want %d", exit, ExitFailed)
	}
	smoke := installedOM + " does not report the built commit " + commitNew
	rollback := "cannot put " + keptPrev + " back over " + installedOM
	if !containsSubstring(r.logs.lines, "CRITICAL: rollback failed after "+smoke+": "+rollback) {
		t.Errorf("logs = %q, want one CRITICAL line naming the smoke failure and the rollback failure", r.logs.lines)
	}
}

// TestRollbackThatCannotReadOMPrevFailsTheRun: when om.prev cannot be looked at, the
// rollback does not guess that there is no previous om. It fails the run without
// swapping anything.
func TestRollbackThatCannotReadOMPrevFailsTheRun(t *testing.T) {
	t.Parallel()
	r := newInstallRig(t)
	r.swapBytes = versionOut(commitOther)
	r.files.statErr[keptPrev] = errBoom

	res, exit := Run(context.Background(), r.cfg)

	if want := (Result{Event: "failed", Commit: commitNew, Prev: commitOld, Reason: "rollback-failed"}); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	if exit != ExitFailed {
		t.Errorf("exit = %d, want %d", exit, ExitFailed)
	}
	if want := []string{"version", "build", "security", "codesign", "swap", "version"}; !reflect.DeepEqual(r.steps, want) {
		t.Errorf("steps = %v, want %v: no swap runs without a readable om.prev", r.steps, want)
	}
	if !containsSubstring(r.logs.lines, "cannot read "+keptPrev+": "+errBoom.Error()) {
		t.Errorf("logs = %q, want the unreadable om.prev named", r.logs.lines)
	}
}

func TestInstallWithoutSigningMaterialInstallsAdHocWithOneWarning(t *testing.T) {
	t.Parallel()
	r := newInstallRig(t)
	delete(r.files.files, keychainFile)
	delete(r.files.files, passwordFile)

	res, exit := Run(context.Background(), r.cfg)

	if want := (Result{Event: "installed", Commit: commitNew, Prev: commitOld}); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	if exit != ExitInstalled {
		t.Errorf("exit = %d, want %d", exit, ExitInstalled)
	}
	if want := []string{"version", "build", "swap", "version"}; !reflect.DeepEqual(r.steps, want) {
		t.Errorf("steps = %v, want %v: no signing command runs without material", r.steps, want)
	}
	if n := r.logs.warnings(); n != 1 {
		t.Errorf("WARNING lines = %d, want exactly 1", n)
	}
}

func TestInstallFirstInstallKeepsNothing(t *testing.T) {
	t.Parallel()
	r := newInstallRig(t)
	delete(r.files.files, installedOM)

	res, exit := Run(context.Background(), r.cfg)

	if want := (Result{Event: "installed", Commit: commitNew}); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	if exit != ExitInstalled {
		t.Errorf("exit = %d, want %d", exit, ExitInstalled)
	}
	if _, ok := r.files.files[keptPrev]; ok {
		t.Error("om.prev was kept on a first install, which has no previous om")
	}
	if got := string(r.files.files[installedOM]); got != versionOut(commitNew) {
		t.Errorf("om = %q, want the built om in force", got)
	}
}

// TestReplaceFilesRunsItsCommandsThroughExec drives the swap's command seam
// through the production adapter, so a failing flag command reaches Replace as
// its error and the command's output reaches it unchanged.
func TestReplaceFilesRunsItsCommandsThroughExec(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		execErr error
	}{
		{name: "a command that succeeds returns its output", execErr: nil},
		{name: "a command that fails returns its error", execErr: errBoom},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var sent Command
			cfg := Config{Exec: func(_ context.Context, c Command) (string, string, error) {
				sent = c
				return "flag output", "", tc.execErr
			}}

			out, err := cfg.replaceFiles().Exec(context.Background(), "chattr", "-i", "/home/op/go/bin/om")

			if string(out) != "flag output" {
				t.Errorf("output = %q, want the command's output", out)
			}
			if !errors.Is(err, tc.execErr) {
				t.Errorf("error = %v, want %v", err, tc.execErr)
			}
			if sent.Name != "chattr" || !reflect.DeepEqual(sent.Args, []string{"-i", "/home/op/go/bin/om"}) {
				t.Errorf("command = %+v, want chattr -i on om", sent)
			}
		})
	}
}

// TestReplaceFilesTakesItsLockWithTheRunsWait checks the swap's lock is taken
// through Acquire with the run's wait, and released by the release it returns.
func TestReplaceFilesTakesItsLockWithTheRunsWait(t *testing.T) {
	t.Parallel()
	acq := &fakeAcquire{}
	cfg := Config{LockWait: 42 * time.Second, Acquire: acq.acquire}

	release, err := cfg.replaceFiles().Lock("/home/op/go/bin/.om.replace.lock")
	if err != nil {
		t.Fatalf("Lock = %v, want nil", err)
	}
	release()

	if acq.path != "/home/op/go/bin/.om.replace.lock" || acq.wait != 42*time.Second || !acq.released {
		t.Errorf("lock = path %q wait %v released %v, want the replace lock, 42s, released", acq.path, acq.wait, acq.released)
	}
}

// containsLine reports whether want is one of lines.
func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

// containsSubstring reports whether one of lines holds want.
func containsSubstring(lines []string, want string) bool {
	for _, line := range lines {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}
