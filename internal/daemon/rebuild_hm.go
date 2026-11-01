package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// The rebuild_hm job installs the hm origin's main when the hm-skew monitor reads
// the installed hm as behind it (gt-bllo8). hm syncs the index history-man reads,
// so an hm left behind keeps serving old code and nothing reports it. The monitor
// reports; this job does the install an operator would otherwise run by hand.
//
// It shares the monitor's reading (hmSkewRead) and its job-owned clone
// (hmSkewCloneDir). It builds only in that clone, never in <town>/hm/mayor/rig.
// Unlike rebuild_om it has no landing drain: no landing worker execs hm, so the
// install is one swap.
//
// Opt-in (patrols.rebuild_hm.enabled): the swap replaces the binary hm runs
// from. A cycle that did nothing returns false and retries on the next heartbeat;
// one that reached a verdict holds the interval, as in rebuild_om.

const (
	defaultRebuildHMInterval = time.Hour

	// rebuildHMRunBudget bounds the build and its version check, the steps the
	// cycle runs under its context. The reading, the checkout and the ancestry
	// check each carry their own bound (hmSkewCloneTimeout, rebuildHMGitTimeout).
	rebuildHMRunBudget = 30 * time.Minute

	// rebuildHMGitTimeout bounds one git call in the clone.
	rebuildHMGitTimeout = 2 * time.Minute

	// rebuildHMStageName names the build's GOBIN: a job directory beside the
	// installed hm, not under the town. go install writes the new hm there and the
	// swap renames it into place, so both renames stay on the installed hm's volume.
	// A build under the town would fail the rename with a cross-device link
	// whenever the install lives on another volume.
	rebuildHMStageName = ".hm-rebuild"

	// rebuildHMPrevName is the installed hm once the swap has kept it, beside it.
	rebuildHMPrevName = "hm.prev"

	// rebuildHMVersionVar is the -X target the build stamps the commit into. The
	// bead names it as Version. -X needs the package-qualified symbol, and hm's
	// cmd/hm is package main, so the target is main.Version. The hm source is not
	// on this host, so the name is unverified here: a wrong name fails the version
	// check and nothing is installed.
	rebuildHMVersionVar = "main.Version"
)

// rebuildHMRenameFunc is the seam for the two renames the swap makes. nil is
// os.Rename. Tests answer it, so the order of renames is recorded and no file
// moves.
type rebuildHMRenameFunc func(from, to string) error

func rebuildHMInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.RebuildHM != nil {
		if d, err := time.ParseDuration(config.Patrols.RebuildHM.Interval); err == nil && d > 0 {
			return d
		}
	}
	return defaultRebuildHMInterval
}

// triggerRebuildHM runs one cycle on its own goroutine when the job is due: the
// build can run for minutes, so inline it would stall the heartbeat.
func (d *Daemon) triggerRebuildHM() {
	if !d.isPatrolActive("rebuild_hm") {
		return
	}
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "rebuild_hm", d.rebuildHMRun.last(), now, rebuildHMInterval(d.patrolConfig))
	if !dec.due {
		return
	}
	if !d.rebuildHMRunning.CompareAndSwap(false, true) {
		return
	}
	if dec.warn != "" {
		d.logger.Printf("rebuild_hm: WARNING: %s — %s", dec.warn, dec.note)
	}
	go func() {
		defer d.rebuildHMRunning.Store(false)
		if !d.runRebuildHM() {
			return
		}
		d.recordPatrolRun(&d.rebuildHMRun, "rebuild_hm", d.clk().Now())
	}()
}

// runRebuildHM is one cycle under its budget. It reports whether the cycle
// reached a verdict (hm is current, left in place, or the install was refused
// or failed) as opposed to deferring with nothing accomplished.
func (d *Daemon) runRebuildHM() bool {
	ctx, cancel := context.WithTimeout(context.Background(), rebuildHMRunBudget)
	defer cancel()

	cycle := d.startDogCycle("rebuild_hm")
	defer cycle.close()
	return d.rebuildHMCycle(ctx, cycle)
}

func (d *Daemon) rebuildHMCycle(ctx context.Context, cycle *dogCycle) bool {
	r := d.hmSkewRead(d.config.TownRoot)
	if r.err != nil {
		cycle.skipStep("reading", r.err.Error())
		d.logger.Printf("rebuild_hm: cannot read the installed hm against its origin: %v; deferring", r.err)
		return false
	}
	if !r.behind {
		cycle.closeStep("force")
		d.logger.Printf("rebuild_hm: the installed hm %s is current with origin/main %s", shortOMSHA(r.installed), shortOMSHA(r.remote))
		return true
	}

	src := filepath.Join(d.config.TownRoot, filepath.FromSlash(hmSkewCloneDir))
	head, err := d.rebuildHMHead(src)
	if err != nil {
		cycle.skipStep("head", err.Error())
		d.logger.Printf("rebuild_hm: %v; deferring", err)
		return false
	}

	// Installing origin's main moves hm forward only when the installed commit is
	// on its history. An hm ahead of main (a hotfix not yet landed), or one the
	// clone has never seen, is left in place. This is the ancestry rule rebuild_om
	// applies, and it names the same failures.
	ancestor, err := d.rebuildOMBehind(src, r.installed, head)
	if err != nil {
		// git's stderr is captured output and can hold a secret, so it is not echoed.
		cycle.skipStep("ancestry", "git merge-base in the hm clone did not answer")
		d.logger.Printf("rebuild_hm: cannot tell whether the installed hm %s is an ancestor of origin/main %s; deferring",
			shortOMSHA(r.installed), shortOMSHA(head))
		return false
	}
	if !ancestor {
		cycle.skipStep("ancestry", fmt.Sprintf("the installed hm %s is not an ancestor of origin/main %s: ahead of it or unknown to the clone, left in place",
			shortOMSHA(r.installed), shortOMSHA(head)))
		d.logger.Printf("rebuild_hm: the installed hm %s is not an ancestor of origin/main %s; leaving it in place",
			shortOMSHA(r.installed), shortOMSHA(head))
		return true
	}

	installPath := d.hmSkewPath()
	if !filepath.IsAbs(installPath) {
		cycle.skipStep("installed", "the installed hm has no absolute path to swap: "+installPath)
		d.logger.Printf("rebuild_hm: the installed hm at %q is not an absolute path; deferring", installPath)
		return false
	}

	unlock, err := d.rebuildHMAcquire(filepath.Join(d.config.TownRoot, "daemon", "install-hm.lock"))
	if err != nil {
		cycle.skipStep("lock", err.Error())
		d.logger.Printf("rebuild_hm: %v; deferring", err)
		return false
	}
	defer unlock()

	d.logger.Printf("rebuild_hm: installing hm %s -> %s", shortOMSHA(r.installed), shortOMSHA(head))
	return d.rebuildHMInstall(ctx, cycle, src, installPath, head)
}

// rebuildHMInstall runs the steps after the decision: checkout, build, version
// check, swap. Each failure before the swap leaves the installed hm where it is;
// a failure of the second rename rolls the first back.
func (d *Daemon) rebuildHMInstall(ctx context.Context, cycle *dogCycle, src, installPath, head string) bool {
	// The reading and this job share the clone without a lock, and need none: the
	// reading only fetches refs and reads commits, never the worktree, and every
	// read here names a sha, so a fetch between two calls moves origin/main but not
	// the commit this job builds.
	if _, err := d.rebuildOMGit(rebuildHMGitTimeout, "-C", src,
		"-c", "advice.detachedHead=false", "checkout", "--quiet", "--force", "--detach", head); err != nil {
		return d.rebuildHMFail(cycle, "checkout", "the hm clone cannot check out "+shortOMSHA(head), head)
	}

	binDir := filepath.Join(filepath.Dir(installPath), rebuildHMStageName)
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return d.rebuildHMFail(cycle, "build", "cannot create "+binDir+": "+err.Error(), head)
	}
	// The stage is cleared on every path: a refused build leaves nothing beside the
	// installed hm, and a swap leaves the directory empty.
	defer func() {
		if err := os.RemoveAll(binDir); err != nil {
			d.logger.Printf("rebuild_hm: removing the build directory %s failed: %v", binDir, err)
		}
	}()
	if err := d.rebuildHMBuild(ctx, src, binDir, head); err != nil {
		return d.rebuildHMFail(cycle, "build", "go install failed for "+shortOMSHA(head), head)
	}
	built := filepath.Join(binDir, "hm")

	// The new binary must print the commit it was built from before it replaces
	// the installed one. An hm that does not is refused, and the installed one stays.
	got, err := d.rebuildHMBuiltSHA(ctx, built)
	if err != nil {
		return d.rebuildHMFail(cycle, "version", "the built hm did not report its version: "+err.Error(), head)
	}
	if !hmSameCommit(got, head) {
		return d.rebuildHMFail(cycle, "version", fmt.Sprintf("the built hm reports %s, not the commit it was built from, %s",
			shortOMSHA(got), shortOMSHA(head)), head)
	}

	// Renames only, never a copy: on macOS codesign kills a copied binary, so the
	// build is renamed into place. The path is briefly absent between the two
	// renames; if the second fails, the first is undone.
	prev := filepath.Join(filepath.Dir(installPath), rebuildHMPrevName)
	if err := d.rebuildHMRename(installPath, prev); err != nil {
		return d.rebuildHMFail(cycle, "swap", "cannot keep the installed hm as "+prev+": "+err.Error(), head)
	}
	if err := d.rebuildHMRename(built, installPath); err != nil {
		if rbErr := d.rebuildHMRename(prev, installPath); rbErr != nil {
			return d.rebuildHMHalted(cycle, fmt.Sprintf("cannot rename the build over %s (%v), and restoring the installed hm from %s failed (%v)",
				installPath, err, prev, rbErr), head)
		}
		return d.rebuildHMFail(cycle, "swap", "cannot rename the build over "+installPath+": "+err.Error(), head)
	}

	cycle.closeStep("install")
	d.logger.Printf("rebuild_hm: installed hm %s; the previous one is kept at %s", shortOMSHA(head), prev)
	return true
}

// rebuildHMFail ends a cycle whose install did not land: the step is recorded,
// the failure is escalated under the hm-skew key at MEDIUM, and the cycle reaches
// its verdict so the interval holds. The installed hm is still at its path: a
// half-done swap is undone by the caller before it calls this.
func (d *Daemon) rebuildHMFail(cycle *dogCycle, step, msg, head string) bool {
	return d.rebuildHMReport(cycle, "MEDIUM", step, msg, head, "the installed hm stays in force")
}

// rebuildHMHalted ends a cycle whose rollback failed: the installed path is empty,
// so it escalates at HIGH and names the file to restore from.
func (d *Daemon) rebuildHMHalted(cycle *dogCycle, msg, head string) bool {
	return d.rebuildHMReport(cycle, "HIGH", "swap", msg, head,
		"the installed hm is not at its path: restore it by hand from hm.prev beside it")
}

// rebuildHMReport records the failed step, escalates it under hm-skew at severity,
// and says in the escalation what state the install was left in.
func (d *Daemon) rebuildHMReport(cycle *dogCycle, severity, step, msg, head, standing string) bool {
	cycle.failStep(step, msg)
	d.logger.Printf("rebuild_hm: install of %s failed (%s); %s", shortOMSHA(head), msg, standing)
	if err := d.escalateAlertSeverity(severity, hmSkewAlertKey, hmSkewSource,
		fmt.Sprintf("rebuild_hm: installing hm %s failed (%s); %s", shortOMSHA(head), msg, standing)); err != nil {
		// The escalation never reached the town: log it. The next interval finds
		// the same skew and tries again.
		d.logger.Printf("rebuild_hm: the failure escalation did not reach the town: %v", err)
	}
	return true
}

// rebuildHMHead reads origin/main's tip in the clone. The reading has already
// fetched it, so this only names the commit the build will be made from.
func (d *Daemon) rebuildHMHead(src string) (string, error) {
	out, err := d.rebuildOMGit(rebuildHMGitTimeout, "-C", src, "rev-parse", "origin/main")
	if err != nil {
		// git's stderr is captured output and can hold a secret, so it is not echoed.
		return "", errors.New("cannot read origin/main in the hm clone: git rev-parse did not answer")
	}
	head := strings.TrimSpace(out)
	if !isCommitSHA(head) {
		return "", errors.New("origin/main in the hm clone does not name a commit")
	}
	return head, nil
}

// rebuildHMBuild runs go install of ./cmd/hm from the clone, the commit stamped
// into the binary and GOBIN set to the job directory. The clone is the only
// directory the build runs in.
func (d *Daemon) rebuildHMBuild(ctx context.Context, src, binDir, head string) error {
	cmd := exec.CommandContext(ctx, "go", "install", "-ldflags=-X "+rebuildHMVersionVar+"="+head, "--", "./cmd/hm")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOBIN="+binDir)
	_, err := d.combinedOutput(cmd)
	return err
}

// rebuildHMBuiltSHA runs the built binary's version and returns the sha it
// prints. Its output is not echoed into an error, since it can hold a secret.
func (d *Daemon) rebuildHMBuiltSHA(ctx context.Context, built string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, omSkewVersionTimeout)
	defer cancel()
	out, err := d.combinedOutput(exec.CommandContext(ctx, built, "version"))
	if err != nil {
		return "", fmt.Errorf("%s version: %w", built, err)
	}
	return parseHMVersion(string(out))
}

// rebuildHMRename is the swap's rename: the seam when a test set one, else
// os.Rename.
func (d *Daemon) rebuildHMRename(from, to string) error {
	if d.rebuildHMRenameFn != nil {
		return d.rebuildHMRenameFn(from, to)
	}
	return os.Rename(from, to)
}

// rebuildHMAcquire takes the install lock without waiting. A held lock means
// another install is running now, and the cycle defers to the next heartbeat, so
// no wait is needed to bound.
func (d *Daemon) rebuildHMAcquire(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("cannot create the install lock directory: %w", err)
	}
	lock := flock.New(path)
	locked, err := lock.TryLock()
	if err != nil {
		// The error names the path and the step only: it is not the lock's own output.
		return nil, fmt.Errorf("cannot take the install lock at %s", path)
	}
	if !locked {
		return nil, errors.New("another install holds the install lock")
	}
	return func() {
		if err := lock.Unlock(); err != nil {
			d.logger.Printf("rebuild_hm: releasing the install lock failed: %v", err)
		}
	}, nil
}
