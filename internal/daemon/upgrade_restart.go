package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrRestartForUpgrade is returned by Run after a normal shutdown when the
// daemon exits so launchd restarts it on a newly installed binary. The
// caller maps it to exit code 75: launchd's KeepAlive {SuccessfulExit: false}
// restarts only a nonzero exit.
var ErrRestartForUpgrade = errors.New("daemon: restart for upgrade")

// upgradeStuckAfter is how long a newer marker may wait for an idle heartbeat
// before the daemon escalates (it keeps waiting afterwards).
const upgradeStuckAfter = 30 * time.Minute

// restartPendingMarker is daemon/restart-pending.json, written by
// scripts/install-gt.sh after a smoke-tested install. The daemon adds
// attempted_from (its own commit) just before it exits for the marker.
type restartPendingMarker struct {
	Commit        string `json:"commit"`
	RequestedAt   string `json:"requested_at,omitempty"`
	Source        string `json:"source,omitempty"`
	Repo          string `json:"repo,omitempty"`
	AttemptedFrom string `json:"attempted_from,omitempty"`
}

// installReceipt is one line of daemon/install-receipts.jsonl; the shell
// scripts (scripts/lib/install-gt-lib.sh igt_receipt) write the same fields.
// Absent values differ by writer: the shell writes JSON null, Go writes ""
// for strings and omits duration_s. Consumers treat null, "" and a missing
// duration_s as absent.
type installReceipt struct {
	TS         string `json:"ts"`
	Event      string `json:"event"`
	Commit     string `json:"commit"`
	PrevCommit string `json:"prev_commit"`
	Source     string `json:"source"`
	MergedAt   string `json:"merged_at"`
	Reason     string `json:"reason"`
	DurationS  int    `json:"duration_s,omitempty"`
}

func restartMarkerPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "restart-pending.json")
}

func installReceiptsPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "install-receipts.jsonl")
}

// isAncestorFn reports (isAncestor, known). known is false when git could not
// answer (no repo, unknown commit, git error); a test seam.
var isAncestorFn = func(repo, ancestor, descendant string) (bool, bool) {
	if repo == "" || ancestor == "" || descendant == "" {
		return false, false
	}
	err := exec.Command("git", "-C", repo, "merge-base", "--is-ancestor", ancestor, descendant).Run()
	if err == nil {
		return true, true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, true
	}
	return false, false
}

// provenAncestor is true only when git answered and said yes. A failed or
// erroring check is never treated as ancestry.
func provenAncestor(repo, ancestor, descendant string) bool {
	ok, known := isAncestorFn(repo, ancestor, descendant)
	return known && ok
}

// provenNotAncestor is true only when git answered and said no.
func provenNotAncestor(repo, ancestor, descendant string) bool {
	ok, known := isAncestorFn(repo, ancestor, descendant)
	return known && !ok
}

// upgradeEscalateFn raises an upgrade-restart alert; a test seam. The real
// escalation retries for minutes under load, so it runs off the heartbeat.
var upgradeEscalateFn = func(d *Daemon, key, msg string) {
	go d.escalateAlert(key, "upgrade-restart", msg)
}

// mergedAtFn returns the committer time of commit in repo as UTC RFC3339
// (the shell writer's format), or "" when git cannot answer; a test seam.
var mergedAtFn = func(repo, commit string) string {
	if repo == "" {
		return ""
	}
	out, err := exec.Command("git", "-C", repo, "show", "-s", "--format=%cI", commit).Output()
	if err != nil {
		return ""
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(out)))
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func readRestartMarker(townRoot string) (*restartPendingMarker, error) {
	data, err := os.ReadFile(restartMarkerPath(townRoot))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m restartPendingMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.Commit == "" {
		return nil, fmt.Errorf("restart marker has no commit")
	}
	return &m, nil
}

// stampRestartAttempt sets attempted_from in the marker file, preserving any
// fields this daemon does not know about. It writes through a temp file and a
// rename, like every other writer of daemon/.
func stampRestartAttempt(townRoot, own string) error {
	path := restartMarkerPath(townRoot)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	v, err := json.Marshal(own)
	if err != nil {
		return err
	}
	raw["attempted_from"] = v
	out, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func appendInstallReceipt(townRoot string, r installReceipt) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(installReceiptsPath(townRoot), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// markerCovered reports whether the marker's commit is already running: equal
// to, or an ancestor of, own (merge-base --is-ancestor A A is true). Decided
// only by git ancestry in the marker's repo; when git cannot answer the marker
// is NOT covered, so the daemon restarts rather than silently clearing it.
func markerCovered(m *restartPendingMarker, own string) bool {
	return provenAncestor(m.Repo, m.Commit, own)
}

// restartHadNoEffect reports whether a previous restart for this marker
// (attempted_from) failed to move the daemon forward. Only a proven strict
// advance (attempted_from an ancestor of own, own not an ancestor of
// attempted_from) counts as progress; anything git cannot prove is treated as
// no effect so an unanswerable check can never loop the daemon.
func restartHadNoEffect(m *restartPendingMarker, own string) bool {
	if m.AttemptedFrom == "" {
		return false
	}
	advanced := provenAncestor(m.Repo, m.AttemptedFrom, own) &&
		provenNotAncestor(m.Repo, own, m.AttemptedFrom)
	return !advanced
}

// ownCommitForUpgrade is the daemon's own build commit (full SHA when the
// gastown checkout can resolve it), or "" when the build does not know it.
// "unknown" is treated as absent.
func (d *Daemon) ownCommitForUpgrade() string {
	raw := strings.TrimSpace(buildCommitFn())
	if raw == "" || raw == "unknown" {
		return ""
	}
	return d.resolveOwnCommit()
}

// loadMarkerForUpgrade reads the marker and the daemon's own commit. ok is
// false when there is nothing to act on.
func (d *Daemon) loadMarkerForUpgrade() (m *restartPendingMarker, own string, ok bool) {
	townRoot := d.config.TownRoot
	m, err := readRestartMarker(townRoot)
	if err != nil {
		// Writers use temp+rename, so this is corruption, not a partial write.
		d.logger.Printf("upgrade-restart: unreadable marker %s, removing: %v", restartMarkerPath(townRoot), err)
		_ = os.Remove(restartMarkerPath(townRoot))
		return nil, "", false
	}
	if m == nil {
		d.upgradeWaitCommit = ""
		return nil, "", false
	}
	own = d.ownCommitForUpgrade()
	if own == "" {
		if d.upgradeWaitCommit != m.Commit {
			d.upgradeWaitCommit = m.Commit
			d.upgradeWaitSince = time.Now()
			d.upgradeWaitEscalated = false
			d.logger.Printf("upgrade-restart: own build commit unknown; ignoring marker for %s", m.Commit)
		}
		return nil, "", false
	}
	return m, own, true
}

// clearCoveredMarker deletes a marker own already covers and appends its
// daemon_restarted receipt. Returns true when it cleared the marker.
func (d *Daemon) clearCoveredMarker(m *restartPendingMarker, own string, now time.Time) bool {
	if !markerCovered(m, own) {
		return false
	}
	townRoot := d.config.TownRoot
	rec := installReceipt{
		TS:         now.UTC().Format(time.RFC3339),
		Event:      "daemon_restarted",
		Commit:     m.Commit,
		PrevCommit: m.AttemptedFrom,
		Source:     m.Source,
		MergedAt:   mergedAtFn(m.Repo, m.Commit),
	}
	if t, err := time.Parse(time.RFC3339, m.RequestedAt); err == nil {
		rec.DurationS = int(now.Sub(t).Seconds())
	}
	if err := appendInstallReceipt(townRoot, rec); err != nil {
		d.logger.Printf("upgrade-restart: writing receipt: %v", err)
	}
	if err := os.Remove(restartMarkerPath(townRoot)); err != nil && !os.IsNotExist(err) {
		d.logger.Printf("upgrade-restart: removing covered marker: %v", err)
	}
	d.upgradeWaitCommit = ""
	d.logger.Printf("upgrade-restart: running %s covers marker %s; cleared", own, m.Commit)
	return true
}

// clearCoveredRestartMarker is the startup check: it clears a marker the
// running binary already covers (the daemon may have been restarted onto it
// by launchd or an operator before the marker was written). It never requests
// a restart; a newer marker is left for the heartbeat.
func (d *Daemon) clearCoveredRestartMarker(now time.Time) {
	m, own, ok := d.loadMarkerForUpgrade()
	if !ok {
		return
	}
	d.clearCoveredMarker(m, own, now)
}

// checkUpgradeRestart handles daemon/restart-pending.json at the top of every
// heartbeat. It returns true (and sets upgradeRestartRequested) when the
// daemon should shut down now so launchd restarts it on the installed binary.
// Run-loop goroutine only.
func (d *Daemon) checkUpgradeRestart(now time.Time) bool {
	m, own, ok := d.loadMarkerForUpgrade()
	if !ok {
		return false
	}
	if d.clearCoveredMarker(m, own, now) {
		return false
	}

	if d.upgradeWaitCommit != m.Commit {
		d.upgradeWaitCommit = m.Commit
		d.upgradeWaitSince = now
		d.upgradeWaitEscalated = false
	}

	if restartHadNoEffect(m, own) {
		if !d.upgradeWaitEscalated {
			d.upgradeWaitEscalated = true
			d.logger.Printf("upgrade-restart: restarted from %s for %s but now running %s; not restarting again", m.AttemptedFrom, m.Commit, own)
			upgradeEscalateFn(d, "daemon:restart-pending-no-effect",
				fmt.Sprintf("Daemon restarted for upgrade to %s (from %s) but is running %s, which is not provably newer. The installed binary is probably not the marker's commit. Not restarting again.",
					m.Commit, m.AttemptedFrom, own))
		}
		return false
	}

	// Read live, immediately before deciding: never cached across heartbeats.
	if !d.isIdleForUpgrade() {
		if !d.upgradeWaitEscalated && now.Sub(d.upgradeWaitSince) >= upgradeStuckAfter {
			d.upgradeWaitEscalated = true
			upgradeEscalateFn(d, "daemon:restart-pending-stuck",
				fmt.Sprintf("Restart for upgrade to %s has waited %s for an idle daemon (running %s). Still waiting.",
					m.Commit, now.Sub(d.upgradeWaitSince).Round(time.Minute), own))
		}
		return false
	}

	if err := stampRestartAttempt(d.config.TownRoot, own); err != nil {
		d.logger.Printf("upgrade-restart: could not stamp attempted_from, not restarting: %v", err)
		return false
	}
	d.logger.Printf("upgrade-restart: idle; restarting from %s to pick up %s", own, m.Commit)
	d.upgradeRestartRequested.Store(true)
	return true
}
