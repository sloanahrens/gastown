package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The om-skew check compares the installed om with the om rig's origin/main
// at daemon start and then hourly. gastown feeds om new flags and new gate
// directives, and a landing is reviewed by whichever om is installed, so an
// om installed before a flag landed reviews against the older binary's
// reading — the om-cix, om-rym and om-c05 incidents, where merges landed
// unreviewed or under a drifted gate.
//
// It reports and nothing else: no path here blocks, rejects or delays a
// landing, the readings run on their own goroutine, and a reading that cannot
// be taken is not an alarm but a log line.
const (
	// omSkewAlertKey is the stable identity of the one condition this check
	// owns. Every escalation goes out under it, so gt escalate records a
	// persisting skew as a repeat on the bead the key already names rather
	// than one bead an hour (gt-vwry).
	omSkewAlertKey = "om-skew"
	// omSkewSource is the escalation source: om reviews landings, so the
	// landing worker owns this condition.
	omSkewSource = "landing-worker"

	// omSkewInterval is the gap between checks after the first. An hour is
	// the shortest wait a landing-affecting skew is worth re-reading at; the
	// first check runs as the monitor starts, so an operator who just
	// installed om does not wait one out.
	omSkewInterval = time.Hour

	// omSkewVersionTimeout bounds one `<omPath> version` call and
	// omSkewRemoteTimeout one remote read. Both are short enough that a hung
	// check cannot outlive the interval in which the next one is due.
	omSkewVersionTimeout = 20 * time.Second
	omSkewRemoteTimeout  = 20 * time.Second

	// omSkewRigWorktree is the om rig's canonical worktree, the only place
	// the daemon reads om's origin url from (never a fetch: an om build runs
	// in that worktree, and a probe must not move its refs).
	omSkewRigWorktree = "om/mayor/rig"
	// omSkewRemoteRef is the branch the installed om is meant to come from.
	omSkewRemoteRef = "refs/heads/main"
)

// omSkewMonitor is the check's state: the start guard, the open alert, and
// the last unreadable reading logged.
type omSkewMonitor struct {
	start sync.Once

	mu sync.Mutex
	// raised is true while an om-skew alert is open. It is what keeps the
	// hourly check from re-raising a condition the operator already holds.
	raised bool
	// settled is true once this process has closed any om-skew alert a
	// previous process left open. raised lives only in memory, so a fresh
	// daemon starts unraised whatever the town holds; the first matching
	// reading therefore clears once, and settled keeps every later one from
	// repeating it.
	settled bool
	// lastReadErr is the error last logged, so an offline town logs once per
	// change rather than once an hour.
	lastReadErr string
}

// startOMSkewMonitor starts the check, once per daemon, from the landing
// worker manager (lagging a patrol is a per-daemon condition, not a per-rig
// one). It returns at once, so the manager's pass loop never waits on a
// reading.
func (d *Daemon) startOMSkewMonitor() {
	if d.ctx == nil {
		return
	}
	d.omSkew.start.Do(func() { go d.runOMSkewMonitor() })
}

// runOMSkewMonitor checks at start, then hourly, until the daemon stops.
func (d *Daemon) runOMSkewMonitor() {
	d.omSkewOnce()
	ticker := d.clk().NewTicker(omSkewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.Chan():
			d.omSkewOnce()
		}
	}
}

// omSkewOnce runs one check. It is the landing worker's check — om reviews
// landings — so it stands down while that patrol is off or its review is
// disabled, leaving the alert state as it found it.
func (d *Daemon) omSkewOnce() {
	if d.config == nil || !d.isPatrolActive("landing_worker") || !landingReviewEnabled(d.patrolConfig) {
		return
	}
	d.omSkewJudge(d.omSkewRead(d.config.TownRoot))
}

// omSkewReading is one comparison: the installed om's sha and the om rig's
// origin/main sha, or the error that stopped one of them being taken.
type omSkewReading struct {
	installed string
	remote    string
	err       error
}

// omSkewRead takes both readings, the installed side first: an om that cannot
// describe itself is a local condition, and a network read for a comparison
// that cannot happen is a wasted timeout.
func (d *Daemon) omSkewRead(townRoot string) omSkewReading {
	omPath := d.omSkewPath()
	installed, err := d.omSkewInstalledSHA(omPath)
	if err != nil {
		return omSkewReading{err: fmt.Errorf("the installed om at %s: %w", omPath, err)}
	}
	remote, err := d.omSkewRemoteSHA(townRoot)
	if err != nil {
		return omSkewReading{err: fmt.Errorf("the om rig's origin/main: %w", err)}
	}
	return omSkewReading{installed: installed, remote: remote}
}

// omSkewJudge applies one reading to the alert state: a mismatch raises
// om-skew once, an equal pair clears an open one (one a previous daemon
// process left open too: this process's first equal pair clears once), and a reading that could not
// be taken does neither — an unreadable om or an unreachable remote is not
// evidence that a skew ended.
func (d *Daemon) omSkewJudge(r omSkewReading) {
	m := &d.omSkew
	m.mu.Lock()
	defer m.mu.Unlock()

	if r.err != nil {
		msg := r.err.Error()
		if msg == m.lastReadErr {
			return
		}
		m.lastReadErr = msg
		d.logger.Printf("om_skew: %s; leaving the alert state as it is", msg)
		return
	}
	m.lastReadErr = ""

	if r.installed == r.remote {
		if !m.raised && m.settled {
			return
		}
		if err := d.clearAlertsErr("the installed om matches the om rig's origin/main", omSkewAlertKey); err != nil {
			// The clear never reached the town: stay raised so the next check
			// retries it, rather than leaving an alert nothing will close.
			d.logger.Printf("om_skew: clearing the alert did not reach the town; retrying next check: %v", err)
			return
		}
		m.raised = false
		m.settled = true
		return
	}

	if m.raised {
		return
	}
	m.raised = true
	if err := d.escalateAlertSeverity("MEDIUM", omSkewAlertKey, omSkewSource, omSkewMessage(r.installed, r.remote)); err != nil {
		// The escalation never reached the town: drop back to unraised so the
		// next check retries rather than record an alert nobody received.
		d.logger.Printf("om_skew: the escalation did not reach the town; retrying next check: %v", err)
		m.raised = false
	}
}

// omSkewMessage names both shas short: the operator reads them against `om
// version` and a git log, and the alert is a pointer to a condition, not a
// log line.
func omSkewMessage(installed, remote string) string {
	return fmt.Sprintf("the installed om is %s but the om rig's origin/main is %s: landings run the older binary against the flags and gates of the newer one; install om from %s to clear this",
		shortOMSHA(installed), shortOMSHA(remote), omSkewRigWorktree)
}

// shortOMSHA is the first 8 characters of a commit sha, for a message a human
// matches by eye.
func shortOMSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// omSkewPath is the om the landing worker reviews with (resolveOMPath): the
// configured om, else om on the daemon's PATH, else the go install location.
func (d *Daemon) omSkewPath() string {
	configured := ""
	if cfg := landingWorkerConfig(d.patrolConfig); cfg != nil {
		configured = cfg.OMPath
	}
	home, _ := os.UserHomeDir()
	return resolveOMPath(configured, exec.LookPath, home)
}

// omSkewInstalledSHA reads the installed om's version. om prints one
// "om version <sha>" line; output without it is a reading that could not be
// taken, which is not a skew.
func (d *Daemon) omSkewInstalledSHA(omPath string) (string, error) {
	if d.omVersionFn != nil {
		return d.omVersionFn(omPath)
	}
	ctx, cancel := context.WithTimeout(context.Background(), omSkewVersionTimeout)
	defer cancel()
	out, err := d.combinedOutput(exec.CommandContext(ctx, omPath, "version"))
	if err != nil {
		return "", fmt.Errorf("%s version: %w", omPath, err)
	}
	sha, err := parseOMVersion(string(out))
	if err != nil {
		return "", fmt.Errorf("%s version: %w", omPath, err)
	}
	return sha, nil
}

// parseOMVersion returns the sha on om's "om version <sha>" line. The line
// may carry trailing text, so the sha is its first field.
func parseOMVersion(out string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "om version ")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			break
		}
		if !isCommitSHA(fields[0]) {
			break
		}
		return fields[0], nil
	}
	return "", fmt.Errorf("no \"om version <sha>\" line in %q", strings.TrimSpace(out))
}

// omSkewRemoteSHA reads the om rig's origin/main tip: the url from the rig's
// own origin remote, the tip from an ls-remote against that url. The read is
// never a fetch — the om rig's worktree is what an om build runs in, and a
// probe must not move its refs.
func (d *Daemon) omSkewRemoteSHA(townRoot string) (string, error) {
	if d.omHeadFn != nil {
		return d.omHeadFn(townRoot)
	}
	rigPath := filepath.Join(townRoot, filepath.FromSlash(omSkewRigWorktree))
	url, err := d.omSkewGit("-C", rigPath, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	if url == "" {
		return "", fmt.Errorf("git -C %s remote get-url origin: no url", rigPath)
	}
	out, err := d.omSkewGit("ls-remote", url, omSkewRemoteRef)
	if err != nil {
		return "", err
	}
	sha := parseLSRemoteMain(out)
	if sha == "" {
		return "", fmt.Errorf("git ls-remote %s %s: no tip", url, omSkewRemoteRef)
	}
	return sha, nil
}

// omSkewGit runs one read-only git call, bounded by omSkewRemoteTimeout and
// returning its trimmed stdout.
func (d *Daemon) omSkewGit(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), omSkewRemoteTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	// A background probe must never sit on a credential prompt: the daemon has
	// no terminal to answer one, so the timeout would spend itself waiting.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := d.combinedOutput(cmd)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// parseLSRemoteMain returns the sha ls-remote printed for refs/heads/main,
// and "" when it printed none. The ref is matched whole, so a branch named
// refs/heads/main-old is not the tip.
func parseLSRemoteMain(out string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != omSkewRemoteRef {
			continue
		}
		// A first field that is not a commit id is a malformed answer, not a
		// tip: returning it would raise a skew against garbage.
		if !isCommitSHA(fields[0]) {
			return ""
		}
		return fields[0]
	}
	return ""
}

// isCommitSHA reports whether s is hex and long enough to be a git object id.
func isCommitSHA(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
