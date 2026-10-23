package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/templates"
)

// upgradeHealWait is how long the heal helper gives launchd to respawn the
// daemon after an upgrade exit before it judges the job.
const upgradeHealWait = 20 * time.Second

// upgradeHealRecheck is how long the helper waits before re-reading a job whose
// first sample carries a pid: that pid is the old daemon as often as it is a
// healthy respawn, and the old daemon's respawn may yet fail (gt-csq9g).
const upgradeHealRecheck = 10 * time.Second

// upgradeHealPidChecks is how many samples carrying a pid count as healthy.
// One sample cannot tell a respawn from a daemon still exiting, so the helper
// heals once the pid has survived the first re-check (gt-csq9g).
const upgradeHealPidChecks = 2

// upgradeHealCommandTimeout bounds one launchctl call made while arming the
// helper, on the run loop ahead of an exit: launchd wedged for a while must
// cost the exit its timeout, not the exit itself (gt-csq9g).
const upgradeHealCommandTimeout = 5 * time.Second

// upgradeHealLogName is the helper's own log: a failed respawn writes nothing
// to daemon.log (gt-eigyu.1).
const upgradeHealLogName = "upgrade-heal.log"

// upgradeHealScriptName is the helper script, written under the town's daemon
// directory beside the lock and the logs.
const upgradeHealScriptName = "upgrade-heal.sh"

// upgradeHealJob is the launchd job the daemon is supervised by: the label it
// is registered under, the uid of its domain and the plist it is loaded from.
type upgradeHealJob struct {
	label string
	uid   int
	plist string
}

// upgradeHealSupervisorJob returns the launchd job this town's daemon runs
// under, read from the same lookups the supervisor start path uses, or false
// when no launchd job is provisioned for townRoot (not darwin, no plist, or a
// plist that belongs to another town). A hand-started daemon has no job to
// heal, so nothing is armed for it.
func upgradeHealSupervisorJob(townRoot string) (upgradeHealJob, bool) {
	if runtime.GOOS != "darwin" {
		return upgradeHealJob{}, false
	}
	plist, err := templates.LaunchdPlistPath()
	if err != nil {
		return upgradeHealJob{}, false
	}
	isFor, err := templates.SupervisorFileIsFor(plist, townRoot)
	if err != nil || !isFor {
		return upgradeHealJob{}, false
	}
	return upgradeHealJob{label: templates.LaunchdLabel, uid: os.Getuid(), plist: plist}, true
}

// upgradeHealLabel is the label the helper is submitted under.
func upgradeHealLabel(job upgradeHealJob) string {
	return job.label + "-heal"
}

// upgradeHealScriptTemplate is the helper. It runs outside the daemon's job,
// so the bootout it may need does not kill it. It waits, and heals only when
// launchd has the job loaded without a pid, no process holds daemon.lock and
// no stop was requested while it waited. A give-up is also sent to the system
// log, so a heal that failed leaves a trail outside the files it shares with
// the stranded daemon.
const upgradeHealScriptTemplate = `#!/bin/sh
# gt upgrade-restart heal: arms after an upgrade exit (gt-eigyu.1).
LABEL=@LABEL@
UID_NUM=@UID@
PLIST=@PLIST@
LOCK=@LOCK@
LOG=@LOG@
HEAL=@HEAL@
STOP=@STOP@
ESTOP=@ESTOP@
SVC="gui/$UID_NUM/$LABEL"

log() {
	printf '%s %s\n' "$(date '+%Y-%m-%dT%H:%M:%S')" "$*" >> "$LOG"
}

finish() {
	launchctl remove "$HEAL" >/dev/null 2>&1
	exit "$1"
}

giveup() {
	log "gave up: $*"
	logger -p user.err -t gt-upgrade-heal "gave up: $*" 2>/dev/null
	finish 1
}

sleep @WAIT@

# A stop inside the wait window is the operator's, not a failed respawn:
# bootstrapping through it would undo a gt power off or a bootout (gt-csq9g).
if [ -e "$STOP" ]; then
	log "left alone: $STOP exists (a daemon stop was requested)"
	finish 0
fi
if [ -e "$ESTOP" ]; then
	log "left alone: $ESTOP exists (E-stop active)"
	finish 0
fi

state=$(launchctl print "$SVC" 2>&1)
# launchd keeps the pid of a job that is still shutting down, so a pid at the
# first sample vouches for nothing; only one that survives the re-check does.
checks=0
while printf '%s\n' "$state" | grep -q '^[[:space:]]*pid = [0-9]'; do
	checks=$((checks + 1))
	if [ "$checks" -ge @PID_CHECKS@ ]; then
		log "healthy: $SVC runs a pid; nothing to heal"
		finish 0
	fi
	sleep @RECHECK@
	state=$(launchctl print "$SVC" 2>&1)
done
if [ -n "$(lsof -t -- "$LOCK" 2>/dev/null)" ]; then
	log "left alone: $LOCK is held by a live process"
	finish 0
fi

log "healing: $SVC has no pid and $LOCK is free; launchctl print said: $(printf '%s' "$state" | tr '\n' ' ')"
launchctl bootout "$SVC" >> "$LOG" 2>&1

waited=0
while launchctl print "$SVC" >/dev/null 2>&1; do
	waited=$((waited + 1))
	if [ "$waited" -ge 60 ]; then
		giveup "$SVC still loaded 60s after bootout"
	fi
	sleep 1
done

tries=0
until launchctl bootstrap "gui/$UID_NUM" "$PLIST" >> "$LOG" 2>&1; do
	tries=$((tries + 1))
	if [ "$tries" -ge 60 ]; then
		giveup "bootstrap of $PLIST failed $tries times"
	fi
	log "bootstrap attempt $tries failed; retrying"
	sleep 3
done
log "bootstrapped: $SVC loaded from $PLIST"
finish 0
`

// upgradeHealScript renders the helper for job, with the lock and log under
// townRoot's daemon directory. Every value is single-quoted for the shell.
func upgradeHealScript(job upgradeHealJob, townRoot string) string {
	daemonDir := filepath.Join(townRoot, "daemon")
	return strings.NewReplacer(
		"@LABEL@", shellQuote(job.label),
		"@UID@", fmt.Sprintf("%d", job.uid),
		"@PLIST@", shellQuote(job.plist),
		"@LOCK@", shellQuote(filepath.Join(daemonDir, "daemon.lock")),
		"@LOG@", shellQuote(filepath.Join(daemonDir, upgradeHealLogName)),
		"@HEAL@", shellQuote(upgradeHealLabel(job)),
		"@STOP@", shellQuote(stopMarkerPath(townRoot)),
		"@ESTOP@", shellQuote(estop.FilePath(townRoot)),
		"@WAIT@", fmt.Sprintf("%d", int(upgradeHealWait/time.Second)),
		"@RECHECK@", fmt.Sprintf("%d", int(upgradeHealRecheck/time.Second)),
		"@PID_CHECKS@", fmt.Sprintf("%d", upgradeHealPidChecks),
	).Replace(upgradeHealScriptTemplate)
}

// shellQuote wraps s in single quotes for /bin/sh, escaping embedded quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// submitUpgradeHeal writes the helper script and submits it as a one-shot
// launchd job of its own, under the heal label. A helper left under that label
// by an earlier upgrade is removed first. run executes one argv.
func submitUpgradeHeal(run func(argv []string) error, townRoot string, job upgradeHealJob) error {
	script := filepath.Join(townRoot, "daemon", upgradeHealScriptName)
	if err := os.WriteFile(script, []byte(upgradeHealScript(job, townRoot)), 0o700); err != nil {
		return fmt.Errorf("writing heal script: %w", err)
	}
	heal := upgradeHealLabel(job)
	// Nothing loaded under the label is the usual case, so a remove error is
	// only worth reporting when the submit that follows it fails too.
	removeErr := run([]string{"launchctl", "remove", heal})
	if err := run([]string{"launchctl", "submit", "-l", heal, "--", "/bin/sh", script}); err != nil {
		if removeErr != nil {
			return fmt.Errorf("submitting %s: %w (removing a stale helper first failed: %v)", heal, err, removeErr)
		}
		return fmt.Errorf("submitting %s: %w", heal, err)
	}
	return nil
}

// armUpgradeHeal arms the heal helper ahead of an upgrade exit. A failure is
// logged and does not block the exit: the restart happens either way, and the
// helper only adds the self-heal when launchd fails to respawn the daemon.
func (d *Daemon) armUpgradeHeal() {
	job, ok := d.seams.upgradeHealJobFor(d.config.TownRoot)
	if !ok {
		return
	}
	if err := submitUpgradeHeal(d.seams.runUpgradeHeal, d.config.TownRoot, job); err != nil {
		d.logger.Printf("upgrade-restart: could not arm the heal helper, restarting anyway: %v", err)
	}
}

// runUpgradeHealCommand runs one argv for the heal helper with a bounded
// lifetime and returns its combined output in the error when it fails. The
// bound keeps a launchd that never answers from holding the exit that armed
// the helper (gt-csq9g).
func runUpgradeHealCommand(timeout time.Duration, argv []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput() //nolint:gosec // fixed argv from submitUpgradeHeal
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w after %s", strings.Join(argv, " "), ctxErr, timeout)
	}
	return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
}
