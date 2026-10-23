package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/templates"
)

// upgradeHealWait is how long the heal helper gives launchd to respawn the
// daemon after an upgrade exit before it judges the job.
const upgradeHealWait = 20 * time.Second

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
// launchd has the job loaded without a pid and no process holds daemon.lock.
// A give-up is also sent to the system log, so a heal that failed leaves a
// trail outside the files it shares with the stranded daemon.
const upgradeHealScriptTemplate = `#!/bin/sh
# gt upgrade-restart heal: arms after an upgrade exit (gt-eigyu.1).
LABEL=@LABEL@
UID_NUM=@UID@
PLIST=@PLIST@
LOCK=@LOCK@
LOG=@LOG@
HEAL=@HEAL@
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

state=$(launchctl print "$SVC" 2>&1)
if printf '%s\n' "$state" | grep -q '^[[:space:]]*pid = [0-9]'; then
	log "healthy: $SVC runs a pid; nothing to heal"
	finish 0
fi
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
		"@WAIT@", fmt.Sprintf("%d", int(upgradeHealWait/time.Second)),
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

// runUpgradeHealCommand runs one argv for the heal helper and returns its
// combined output in the error when it fails.
func runUpgradeHealCommand(argv []string) error {
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput() //nolint:gosec // fixed argv from submitUpgradeHeal
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
