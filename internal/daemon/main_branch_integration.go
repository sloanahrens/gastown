package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The integration tier (//go:build integration: real tmux, bd, Dolt, the gt
// binary) ran nowhere: the refinery gate is `make test`, the CI integration job
// failed before running a test, and this patrol ran only the rig's test
// command. Every test moved to that tier was dead (gt-22hdp.39).
//
// main_branch_test now also runs a rig's integration tier, inside the cycle
// that already holds the container-gate slot on a fresh worktree of main, at
// its own cadence (daily by default, so it adds one run a day to the slot, not
// one an hour) and with its own clock and alert. The command is found in the
// worktree rather than in config: a rig whose Makefile declares a
// test-integration target gets `make test-integration`, so the gastown rig is
// covered with no change to a live town's config.

const (
	defaultMainBranchIntegrationInterval = 24 * time.Hour
	defaultMainBranchIntegrationTimeout  = 30 * time.Minute

	// integrationMakeTarget is the Makefile target that defines a rig's
	// integration tier.
	integrationMakeTarget = "test-integration"
)

// mainBranchIntegrationInterval returns how often a rig's integration tier
// runs, or 0 when it is turned off. An unparseable value turns it off rather
// than falling back to the default, for the reason mainBranchTestGateBusyStarveAfter
// gives: reading a typo as agreement hides it.
func mainBranchIntegrationInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.MainBranchTest != nil {
		if s := config.Patrols.MainBranchTest.IntegrationIntervalStr; s != "" {
			d, err := time.ParseDuration(s)
			if err != nil || d <= 0 {
				return 0
			}
			return d
		}
	}
	return defaultMainBranchIntegrationInterval
}

// mainBranchIntegrationTimeout returns the configured integration budget, or
// the default when it is unset or invalid.
func mainBranchIntegrationTimeout(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.MainBranchTest != nil {
		if s := config.Patrols.MainBranchTest.IntegrationTimeoutStr; s != "" {
			if d, err := time.ParseDuration(s); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultMainBranchIntegrationTimeout
}

// integrationCommandFor returns the command that runs the integration tier of
// the tree at workDir, or "" when the tree defines none: `make
// test-integration` when its Makefile declares that target.
func integrationCommandFor(workDir string) string {
	f, err := os.Open(filepath.Join(workDir, "Makefile"))
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		// A rule line starts at column 0: "test-integration:" or
		// "test-integration: deps". A recipe line is tab-indented, and a
		// longer target ("test-integration-x:") is a different rule.
		if rest, ok := strings.CutPrefix(line, integrationMakeTarget); ok &&
			strings.HasPrefix(strings.TrimLeft(rest, " \t"), ":") &&
			!strings.HasPrefix(strings.TrimLeft(rest, " \t"), ":=") {
			return "make " + integrationMakeTarget
		}
	}
	return ""
}

// mainBranchIntegrationPatrolKey names a rig's integration run in
// daemon/patrol_last_run.json. One key per rig: the rigs of a cycle are
// tested one after another, and one rig's run must not mark another's done.
func mainBranchIntegrationPatrolKey(rigName string) string {
	return "main_branch_integration:" + rigName
}

// mainBranchIntegrationAlertKey is the alert fingerprint for a red integration
// tier. It is separate from alertKeyMainBranchTest: a cycle in which the
// integration run is not due still clears that key when the gates pass, and
// sharing it would retire a red integration alert on a run that never looked.
func mainBranchIntegrationAlertKey(rigName string) string {
	return "main_branch_test:integration:" + rigName
}

// runRigIntegration runs rigName's integration tier on workDir when it is due,
// escalating a failure under the rig's integration alert and clearing that
// alert on a pass. The caller holds the container-gate slot: the tier starts
// Docker-backed suites, and this is the same hold `gt slot run` takes.
//
// A run the daemon stopped (parent canceled) is no verdict: it neither
// escalates, clears nor records a last run, so the next cycle tries again. A
// finished run records its last run whatever its verdict, so a red tier is
// re-run on its interval, not every cycle, while its alert stands.
func (d *Daemon) runRigIntegration(parent context.Context, rigName, commit, workDir string) {
	interval := mainBranchIntegrationInterval(d.patrolConfig)
	if interval <= 0 {
		return
	}
	command := integrationCommandFor(workDir)
	if command == "" {
		return
	}
	key := mainBranchIntegrationPatrolKey(rigName)
	dec := evaluatePatrolDue(d.config.TownRoot, key, time.Time{}, time.Now(), interval)
	if !dec.due {
		d.logger.Printf("main_branch_test: %s: integration not due — %s", rigName, dec.note)
		return
	}
	if dec.warn != "" {
		d.logger.Printf("main_branch_test: %s: WARNING: %s — %s", rigName, dec.warn, dec.note)
	}

	ctx, cancel := context.WithTimeout(parent, mainBranchIntegrationTimeout(d.patrolConfig))
	defer cancel()
	err := d.runCommandOnWorktree(ctx, rigName, commit, workDir, "integration", command)

	if errors.Is(err, errMainBranchTestInterrupted) {
		d.logger.Printf("main_branch_test: %s: integration interrupted, not a verdict about main: %s", rigName, oneLine(err.Error()))
		return
	}
	if saveErr := savePatrolLastRun(d.config.TownRoot, key, time.Now()); saveErr != nil {
		d.logger.Printf("main_branch_test: %s: WARNING: cannot persist integration last-run (%v) — it may run again next cycle", rigName, saveErr)
	}
	if err != nil {
		d.logger.Printf("main_branch_test: %s: integration FAILED: %s", rigName, oneLine(err.Error()))
		d.escalateAlert(mainBranchIntegrationAlertKey(rigName), "main_branch_test",
			fmt.Sprintf("integration tier failed on %s main (%s):\n%v", rigName, command, err))
		return
	}
	d.logger.Printf("main_branch_test: %s: integration passed", rigName)
	d.clearAlerts(fmt.Sprintf("integration tier green on %s main", rigName), mainBranchIntegrationAlertKey(rigName))
}
