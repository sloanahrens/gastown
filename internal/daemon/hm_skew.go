package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// The hm-skew check compares the installed hm with the hm rig's origin/main at
// daemon start and then hourly. hm syncs the index history-man reads, and an hm
// installed before a fix landed keeps running the old code with nothing
// reporting it.
//
// It reports and nothing else: no path here blocks, rejects or delays anything,
// the readings run on their own goroutine, and a reading that cannot be taken
// is a log line, not an alarm. Its one write is the job-owned clone under
// .runtime. It shares om_skew's reads of the remote and its
// escalation key convention, but not om's rule for what counts as a skew: a
// diff that touches only markdown or docs is not one.
const (
	// hmSkewAlertKey is the stable identity of the one condition this check
	// owns. Each skewed reading goes out under it, so gt escalate records a
	// persisting skew as a repeat on the bead the key names (gt-vwry).
	hmSkewAlertKey = "hm-skew"
	// hmSkewSource names the hm owner the escalation is filed under: the
	// hm_sync patrol, which runs hm.
	hmSkewSource = "hm_sync"

	// hmSkewInterval is the gap between checks after the first.
	hmSkewInterval = omSkewInterval

	// hmSkewRigWorktree is the hm rig's canonical worktree, the only place the
	// daemon reads hm's origin url from (never a fetch, as om_skew).
	hmSkewRigWorktree = "hm/mayor/rig"
	// hmSkewCloneDir is the job-owned clone the diff runs in, relative to the
	// town. The monitor clones it from the rig's origin on first use and fetches
	// origin's main into it before each diff. It is never the rig worktree.
	hmSkewCloneDir = ".runtime/hm-src"
	// hmSkewCloneTimeout bounds the first clone; hmSkewFetchTimeout bounds one
	// fetch of origin's main into the clone.
	hmSkewCloneTimeout = 5 * time.Minute
	hmSkewFetchTimeout = 2 * time.Minute
)

// hmSkewMonitor is the check's state: the start guard, the test seam for the
// installed hm's path, the open alert, and the last unreadable reading logged.
type hmSkewMonitor struct {
	start sync.Once

	// pathFn is the installed hm's path; nil is the production lookup
	// (hmSkewPath). Tests answer it so no binary is resolved from the host.
	pathFn func() string

	// removeAll clears a partial clone; nil is os.RemoveAll. Tests answer it, so
	// a clone that cannot be cleared is driven without a permission fixture.
	removeAll func(string) error

	mu sync.Mutex
	// raised is true while the last reading was a skew, so the next clean
	// reading knows an alert is open to close.
	raised bool
	// settled is true once this process has closed any hm-skew alert a previous
	// process left open. raised lives only in memory, so a fresh daemon clears
	// once on its first clean reading and never after.
	settled bool
	// lastReadErr is the error last logged, so a town where the check cannot
	// answer logs once per change rather than once an hour.
	lastReadErr string
}

// startHMSkewMonitor starts the check, once per daemon, from the landing worker
// manager beside the om-skew monitor. It returns at once, so the manager's pass
// loop never waits on a reading.
func (d *Daemon) startHMSkewMonitor() {
	if d.ctx == nil {
		return
	}
	d.hmSkew.start.Do(func() { go d.runHMSkewMonitor() })
}

// runHMSkewMonitor checks at start, then hourly, until the daemon stops.
func (d *Daemon) runHMSkewMonitor() {
	d.hmSkewOnce()
	ticker := d.clk().NewTicker(hmSkewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.Chan():
			d.hmSkewOnce()
		}
	}
}

// hmSkewOnce runs one check: one reading, judged against the alert state.
func (d *Daemon) hmSkewOnce() {
	if d.config == nil {
		return
	}
	d.hmSkewJudge(d.hmSkewRead(d.config.TownRoot))
}

// hmSkewReading is one comparison: whether the installed hm is behind origin's
// main, or the error that stopped the reading being taken. A clone that cannot
// be brought up to origin's main stops the reading the same way, so a stale
// clone is never judged and never clears an alert.
type hmSkewReading struct {
	installed string
	remote    string
	// behind is true when the installed commit differs from origin's main in a
	// way that can change the binary. It is false whenever that is not
	// established, since err then says why.
	behind bool
	err    error
}

// hmSkewRead takes the readings, the installed side first: an hm that cannot
// describe itself is a local condition, and a network read for a comparison
// that cannot happen is a wasted timeout.
func (d *Daemon) hmSkewRead(townRoot string) hmSkewReading {
	installed, err := d.hmSkewInstalledSHA()
	if err != nil {
		return hmSkewReading{err: fmt.Errorf("the installed hm: %w", err)}
	}
	url, remote, err := d.hmSkewRemote(townRoot)
	if err != nil {
		return hmSkewReading{err: fmt.Errorf("the hm rig's origin/main: %w", err)}
	}
	r := hmSkewReading{installed: installed, remote: remote}
	if hmSameCommit(installed, remote) {
		return r
	}
	behind, err := d.hmSkewBehind(townRoot, url, installed, remote)
	if err != nil {
		return hmSkewReading{installed: installed, remote: remote, err: err}
	}
	r.behind = behind
	return r
}

// hmSameCommit reports whether the installed hm's commit is origin's tip. hm
// prints a short sha, so the installed one matches when the tip starts with it.
func hmSameCommit(installed, remote string) bool {
	return strings.HasPrefix(remote, installed)
}

// hmSkewBehind reports whether the installed hm is behind origin's main in a way
// that can change the binary. The clone is first brought up to origin's main, so
// both answers come from the current history. The clone answers two questions:
// whether it has the installed commit at all (a commit it lacks is not main's
// history, so a skew), and which files differ between the two (a diff over
// markdown and docs alone is not one). A clone that cannot be brought up to
// main, or cannot answer, is an error, not a skew: a stale clone could name the
// wrong files.
func (d *Daemon) hmSkewBehind(townRoot, url, installed, remote string) (bool, error) {
	// Both revisions reach git's argv, so each is held to a commit id before
	// the call (the omSkewCommitRevs guard, gt-8n9hj).
	if err := omSkewCommitRevs(installed, remote); err != nil {
		return false, err
	}
	src, err := d.hmSkewRefreshClone(townRoot, url)
	if err != nil {
		return false, err
	}
	// The probe runs before the diff: a commit the clone lacks is a skew by
	// itself, and a diff about it would only fail. A probe that cannot answer is
	// an error, not a skew.
	missing, err := d.hmSkewCloneLacks(src, installed)
	if err != nil {
		return false, fmt.Errorf("cannot ask the hm clone about %s: %w", shortOMSHA(installed), err)
	}
	if missing {
		return true, nil
	}
	out, err := d.omSkewGit("-C", src, "diff", "--name-only", "-z", "--no-renames", installed, remote, "--")
	if err != nil {
		return false, fmt.Errorf("cannot list the files between %s and %s: %w",
			shortOMSHA(installed), shortOMSHA(remote), err)
	}
	for _, p := range strings.Split(out, "\x00") {
		if p != "" && hmSkewBinaryPath(p) {
			return true, nil
		}
	}
	return false, nil
}

// hmSkewCloneLacks reports whether the clone has no commit for sha. A rev-parse
// that exits 1 with -q is git's answer that the object is absent; any other
// failure (a clone that is not a repository, say) is a question the clone could
// not answer.
func (d *Daemon) hmSkewCloneLacks(src, sha string) (bool, error) {
	out, err := d.omSkewGitNoMatch("-C", src, "rev-parse", "--verify", "-q", sha+"^{commit}")
	if err != nil {
		return false, err
	}
	// omSkewGitNoMatch returns "" with a nil error for exit 1, and the sha for a
	// commit the clone has.
	return out == "", nil
}

// hmSkewBinaryPath reports whether a changed path can change the hm binary. A
// markdown file or anything under docs/ cannot: every other path can.
func hmSkewBinaryPath(p string) bool {
	return !strings.HasSuffix(p, ".md") && !strings.HasPrefix(p, "docs/")
}

// hmSkewRefreshClone makes the job-owned clone current with origin's main. The
// clone is cloned from the rig's origin url on first use, and origin's main is
// fetched into it on every call, so a skew is judged against the tip the
// reading names rather than against whatever the clone last saw. The fetch is
// by url, so the clone follows the url the rig reports now. The rig worktree is
// only read for that url, never written.
func (d *Daemon) hmSkewRefreshClone(townRoot, url string) (string, error) {
	src := filepath.Join(townRoot, filepath.FromSlash(hmSkewCloneDir))
	_, err := os.Stat(filepath.Join(src, ".git"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := d.hmSkewClone(src, url); err != nil {
			return "", err
		}
	case err != nil:
		return "", fmt.Errorf("the hm clone at %s: %w", src, err)
	}
	// The refspec moves the remote-tracking ref to main, so the commits of earlier
	// fetches stay reachable and an installed commit on main's history is found.
	if err := d.hmSkewGit(hmSkewFetchTimeout, "fetching origin's main into the hm clone",
		"-C", src, "fetch", "--quiet", "--no-tags", "--", url,
		"+"+omSkewRemoteRef+":refs/remotes/origin/main"); err != nil {
		return "", err
	}
	return src, nil
}

// hmSkewClone clones the hm origin into src without a worktree: the check reads
// commits and paths, never files. A clone that fails is removed, so a partial
// directory does not read as a clone on the next reading and fail there for
// good; the next reading clones again.
func (d *Daemon) hmSkewClone(src, url string) error {
	// git creates the leading directories of src itself.
	err := d.hmSkewGit(hmSkewCloneTimeout, "cloning the hm origin into the hm clone",
		"clone", "--quiet", "--no-checkout", "--", url, src)
	if err == nil {
		return nil
	}
	if rmErr := d.hmSkewRemoveAll(src); rmErr != nil {
		return fmt.Errorf("%w; and removing the partial clone at %s failed: %w", err, src, rmErr)
	}
	return err
}

// hmSkewRemoveAll removes a partial clone. The seam is hmSkewMonitor.removeAll;
// nil is os.RemoveAll.
func (d *Daemon) hmSkewRemoveAll(src string) error {
	if d.hmSkew.removeAll != nil {
		return d.hmSkew.removeAll(src)
	}
	return os.RemoveAll(src)
}

// hmSkewGit runs one git call for the hm clone, bounded by timeout. A failure is
// named by what the call was for, never by its argv or its output: the url can
// carry a credential, and captured output is not put in an error (rule 11).
func (d *Daemon) hmSkewGit(timeout time.Duration, what string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	// A background call must never sit on a credential prompt (omSkewGitRun).
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if _, err := d.combinedOutput(cmd); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// hmSkewJudge applies one reading to the alert state. A skewed reading escalates
// under hm-skew every time it is taken, and gt escalate records the repeats on
// one bead. A clean reading clears the key, once per process, and after any
// skewed one. A reading that could not be taken does neither: an unreadable hm
// or an unreachable remote is not evidence that a skew ended.
func (d *Daemon) hmSkewJudge(r hmSkewReading) {
	m := &d.hmSkew
	m.mu.Lock()
	defer m.mu.Unlock()

	if r.err != nil {
		msg := r.err.Error()
		if msg == m.lastReadErr {
			return
		}
		m.lastReadErr = msg
		d.logger.Printf("hm_skew: %s; leaving the alert state as it is", msg)
		return
	}
	m.lastReadErr = ""

	if !r.behind {
		if !m.raised && m.settled {
			return
		}
		if err := d.clearAlertsErr("the installed hm is current with the hm rig's origin/main, or nothing between them changes the binary", hmSkewAlertKey); err != nil {
			// The clear never reached the town: stay raised so the next check
			// retries it, rather than leave an alert nothing will close.
			d.logger.Printf("hm_skew: clearing the alert did not reach the town; retrying next check: %v", err)
			return
		}
		m.raised = false
		m.settled = true
		return
	}

	m.raised = true
	if err := d.escalateAlertSeverity("MEDIUM", hmSkewAlertKey, hmSkewSource, hmSkewMessage(r.installed, r.remote)); err != nil {
		d.logger.Printf("hm_skew: the escalation did not reach the town; retrying next check: %v", err)
	}
}

// hmSkewMessage names both shas short, so the operator matches them by eye
// against `hm version` and a git log, and says what clears it.
func hmSkewMessage(installed, remote string) string {
	return fmt.Sprintf("the installed hm is %s but the hm rig's origin/main is %s: the difference can change the hm binary; run `make install` in the hm rig (%s) to clear this",
		shortOMSHA(installed), shortOMSHA(remote), hmSkewRigWorktree)
}

// hmSkewPath is the installed hm the check reads (see hmSkewPathOf).
func (d *Daemon) hmSkewPath() string {
	if d.hmSkew.pathFn != nil {
		return d.hmSkew.pathFn()
	}
	home, _ := os.UserHomeDir()
	return hmSkewPathOf(exec.LookPath, home)
}

// hmSkewPathOf is hm on the lookup's PATH, else the go install location under
// home, else hm by name. There is no configured path for hm. The lookup and the
// home directory are arguments, so the order is tested without the host's PATH
// or HOME.
func hmSkewPathOf(lookPath func(string) (string, error), home string) string {
	if p, err := lookPath("hm"); err == nil {
		return p
	}
	if home != "" {
		return filepath.Join(home, "go", "bin", "hm")
	}
	return "hm"
}

// hmSkewInstalledSHA reads the installed hm's version. hm prints one
// "hm <sha>" line; output without it is a reading that could not be taken.
func (d *Daemon) hmSkewInstalledSHA() (string, error) {
	hmPath := d.hmSkewPath()
	ctx, cancel := context.WithTimeout(context.Background(), omSkewVersionTimeout)
	defer cancel()
	out, err := d.combinedOutput(exec.CommandContext(ctx, hmPath, "version"))
	if err != nil {
		return "", fmt.Errorf("%s version: %w", hmPath, err)
	}
	sha, err := parseHMVersion(string(out))
	if err != nil {
		return "", fmt.Errorf("%s version: %w", hmPath, err)
	}
	return sha, nil
}

// parseHMVersion returns the sha on hm's "hm <sha>" line. The line may carry
// trailing text, so the sha is its first field.
func parseHMVersion(out string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "hm ")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 || !isCommitSHA(fields[0]) {
			break
		}
		return strings.ToLower(fields[0]), nil
	}
	// The output is not echoed: it is captured command output, and an error
	// is logged (rule 11).
	return "", fmt.Errorf("no \"hm <sha>\" line in the output of hm version")
}

// hmSkewRemote reads the hm rig's origin url and origin/main's tip: the url from
// the rig's own origin remote, read from its bare repository rather than the
// mayor/rig clone, and the tip from an ls-remote against that url. The read is
// never a fetch, so the rig's refs do not move.
func (d *Daemon) hmSkewRemote(townRoot string) (url, sha string, err error) {
	bare := constants.RigBareRepoPath(filepath.Join(townRoot, "hm"))
	url, err = d.omSkewGit("--git-dir="+bare, "remote", "get-url", "origin")
	if err != nil {
		return "", "", err
	}
	if url == "" {
		return "", "", fmt.Errorf("git --git-dir=%s remote get-url origin: no url", bare)
	}
	// The "--" keeps a url that begins with "-" from being read as an option.
	out, err := d.omSkewGit("ls-remote", "--", url, omSkewRemoteRef)
	if err != nil {
		return "", "", err
	}
	sha = parseLSRemoteMain(out)
	if sha == "" {
		return "", "", fmt.Errorf("git ls-remote %s %s: no tip", url, omSkewRemoteRef)
	}
	return url, sha, nil
}
