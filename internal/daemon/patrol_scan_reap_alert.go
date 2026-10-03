package daemon

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/patrolscan"
)

// The blocked-seat alerts of the reap pass. A seat the pass leaves behind is
// reported as a `blocked` finding, which is one line in the daemon log that
// nobody may read; these alerts put the same fact in front of an operator and
// keep it there until the seat recovers.
//
// Every alert carries a stable key, passed to `gt escalate` as --fingerprint:
// an unchanged blocker set records a repeat on the open escalation instead of
// raising another, and `gt escalate clear --fingerprint` closes it once the
// condition goes away. A seat whose blocker set changes gets a new key, and
// the old one is cleared so it cannot outlive what it described (design Q6,
// gt-wnmwt).

const (
	// reapAlertSource labels every escalation this pass raises.
	reapAlertSource = "patrol-scan"

	// reapFindingKind is the Kind the pure pass stamps on a reap decision
	// (internal/patrolscan/reap.go).
	reapFindingKind = "reap"

	// defaultReapBlockedThreshold backs AlertThreshold() for a caller that
	// passes an unset value.
	defaultReapBlockedThreshold = 5
)

// reapAlertSink is the alert side of the blocked-seat pass: raise one alert
// under a stable key, and clear the keys whose condition went away. The
// daemon's own escalateAlert/clearAlerts satisfy it through daemonAlertSink;
// tests inject a recorder.
type reapAlertSink interface {
	Raise(key, source, message string)
	Clear(reason string, keys ...string)
}

// daemonAlertSink is the production reapAlertSink.
type daemonAlertSink struct{ d *Daemon }

func (s daemonAlertSink) Raise(key, source, message string) {
	s.d.escalateAlert(key, source, message)
}

func (s daemonAlertSink) Clear(reason string, keys ...string) {
	s.d.clearAlerts(reason, keys...)
}

// alerts returns the sink the blocked-seat pass sends through.
func (d *Daemon) alerts() reapAlertSink {
	if d.reapAlertSink != nil {
		return d.reapAlertSink
	}
	return daemonAlertSink{d: d}
}

// reapAlertState is what the pass remembers between ticks so it can clear an
// alert whose condition went away: the key last raised per seat ("<rig>/<name>")
// and whether each rig's threshold alert is open.
type reapAlertState struct {
	seats     map[string]string
	threshold map[string]bool
}

// reapCleanupConfig returns the worktree_cleanup block of the patrol config,
// or nil when there is none.
func reapCleanupConfig(config *DaemonPatrolConfig) *agentconfig.WorktreeCleanupConfig {
	if c := patrolScanConfig(config); c != nil {
		return c.WorktreeCleanup
	}
	return nil
}

// reapAlertsIfEnabled raises the blocked-seat alerts for one rig's report when
// the rig's worktree_cleanup block is on. A disabled block, or one that does
// not cover the rig, raises nothing: the pass itself did not run, so anything
// it might have alerted on was never observed.
func (d *Daemon) reapAlertsIfEnabled(rig string, r patrolscan.Report) {
	c := reapCleanupConfig(d.patrolConfig)
	if !c.IsEnabled() || !c.CoversRig(rig) {
		return
	}
	d.reapAlerts(rig, r, c.AlertThreshold())
}

// reapAlerts raises one deduped alert per blocked reap candidate in r, plus
// one threshold alert when the blocked count reaches threshold. It clears a
// seat's alert once that seat stops being blocked, and the threshold alert
// once the count falls back below the threshold.
//
// It runs inline, inside the tick: escalateAlert retries on failure, so a
// blocked backlog costs wall-clock, but the tick is single-flight and the
// state here is only safe while one goroutine owns it.
func (d *Daemon) reapAlerts(rig string, r patrolscan.Report, threshold int) {
	if threshold <= 0 {
		threshold = defaultReapBlockedThreshold
	}
	sink := d.alerts()
	st := &d.reapAlertState
	if st.seats == nil {
		st.seats = make(map[string]string)
	}
	if st.threshold == nil {
		st.threshold = make(map[string]bool)
	}

	blocked := blockedReapSeats(r)
	still := make(map[string]bool, len(blocked))
	for _, f := range blocked {
		still[f.Subject] = true
	}

	for _, f := range blocked {
		seat := rig + "/" + f.Subject
		key := reapBlockedKey(rig, f.Subject, f.Detail)
		if prev, ok := st.seats[seat]; ok && prev != key {
			// The blockers changed: the alert under the old key describes a
			// condition that no longer holds.
			sink.Clear(fmt.Sprintf("%s is blocked by a different set of reasons now", seat), prev)
		}
		sink.Raise(key, reapAlertSource, reapBlockedMessage(rig, f.Subject, f.Detail))
		st.seats[seat] = key
	}

	// A rig-level read failure makes the walk incomplete: a seat with no
	// blocked finding may simply not have been seen, so its absence proves
	// nothing and clears nothing below. A failed read never acts.
	complete := len(r.Errors) == 0

	// A seat with no blocked finding this tick either recovered, was reaped,
	// went quiet, or left the walk. Either way the alert it raised is stale.
	if complete {
		stale := make([]string, 0, len(st.seats))
		for seat := range st.seats {
			if !strings.HasPrefix(seat, rig+"/") {
				continue
			}
			if !still[strings.TrimPrefix(seat, rig+"/")] {
				stale = append(stale, seat)
			}
		}
		sort.Strings(stale)
		for _, seat := range stale {
			key := st.seats[seat]
			delete(st.seats, seat)
			sink.Clear(fmt.Sprintf("%s is no longer a blocked reap candidate", seat), key)
		}
	}

	switch {
	case len(blocked) >= threshold:
		st.threshold[rig] = true
		sink.Raise(reapThresholdKey(rig), reapAlertSource, reapThresholdMessage(rig, len(blocked), threshold))
	case complete && st.threshold[rig]:
		delete(st.threshold, rig)
		sink.Clear(fmt.Sprintf("%s is below the blocked reap threshold", rig), reapThresholdKey(rig))
	}
}

// blockedReapSeats returns the report's blocked reap findings, ordered by
// seat: the pass must raise the same alerts in the same order every tick, and
// a map walk would not.
func blockedReapSeats(r patrolscan.Report) []patrolscan.Finding {
	var out []patrolscan.Finding
	for _, f := range r.Findings {
		if f.Kind == reapFindingKind && f.Outcome == patrolscan.OutcomeBlocked {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject < out[j].Subject })
	return out
}

// reapBlockedKey is the alert key for one blocked seat: the seat, plus the
// first 8 hex characters of the SHA-256 of its detail, so an unchanged blocker
// set reuses the key and a changed one raises a new alert.
func reapBlockedKey(rig, name, detail string) string {
	sum := sha256.Sum256([]byte(detail))
	return fmt.Sprintf("reap-blocked:%s/%s:%x", rig, name, sum[:4])
}

// reapThresholdKey is the alert key for one rig's blocked count.
func reapThresholdKey(rig string) string { return "reap-blocked-threshold:" + rig }

// reapBlockedMessage names the seat, why it is blocked, and the operator's
// next step. The first line becomes the escalation's headline (escalationTitle
// takes it), so it carries the seat and the reason.
func reapBlockedMessage(rig, name, detail string) string {
	seat := rig + "/" + name
	return fmt.Sprintf("%s is a blocked reap candidate: %s\n"+
		"Operator: run `gt polecat check-recovery %s` for the full verdict. If its worktree is dead, read the diff and remove it by hand (`gt polecat nuke %s --force`, adding --acknowledge-unpreserved when the branch's only copy is local) — patrol_scan never passes --force itself.",
		seat, detail, seat, seat)
}

// reapThresholdMessage reports the rig-wide count that tripped the threshold.
func reapThresholdMessage(rig string, n, threshold int) string {
	return fmt.Sprintf("%s has %d blocked reap candidate(s), at or above the alert threshold of %d; they are not being removed and each needs an operator. Each is a `blocked` line in the daemon log (gt tail); this alert closes once the count falls back below the threshold.",
		rig, n, threshold)
}
