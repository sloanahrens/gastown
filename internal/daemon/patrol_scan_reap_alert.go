package daemon

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/patrolscan"
)

// The blocked-seat alerts of the reap pass. A seat the pass leaves behind is
// reported as a `blocked` finding, which is one line in the daemon log that
// nobody may read; these alerts put the same fact in front of an operator and
// keep it there until the seat recovers.
//
// Every alert carries a stable key, passed to `gt escalate` as --fingerprint:
// an unchanged blocker set reuses the key, `gt escalate` records the repeat on
// the open escalation, and `gt escalate clear --fingerprint` closes it once
// the condition goes away. A seat whose blocker set changes gets a new key,
// and the old one is cleared so it cannot outlive what it described (design
// Q6, gt-wnmwt).
//
// Two pieces of state make that survive a daemon that restarts every few hours
// (gt-g49yk): a throttle, so a persistent blocker costs one write per report
// window instead of one per two-minute tick, kept in the persisted patrolscan
// Ledger under its own key prefix; and the set of open alerts, persisted in
// reapAlertStorePath so a restarted daemon can clear an alert whose condition
// ended while it was down. The Ledger interface cannot hold that set — it has
// no key listing and no delete — so the set is a second small file.
//
// Both pieces of state record a delivery, not an attempt: a raise or clear that
// gt escalate dropped leaves them untouched, so the next tick retries it
// instead of the throttle window suppressing it (gt-s3u9a).

const (
	// reapAlertSource labels every escalation this pass raises.
	reapAlertSource = "patrol-scan"

	// reapFindingKind is the Kind the pure pass stamps on a reap decision
	// (internal/patrolscan/reap.go).
	reapFindingKind = "reap"

	// defaultReapBlockedThreshold backs AlertThreshold() for a caller that
	// passes an unset value.
	defaultReapBlockedThreshold = 5

	// reapAlertKeyPrefix namespaces this pass's throttle entries in the
	// patrolscan ledger. The recovery comment keys share that file and look
	// like "<rig>/<beadID>", so the prefix keeps the two key spaces apart.
	reapAlertKeyPrefix = "reap-alert:"

	// reapAlertStoreName is the raised-key store, a sibling of the patrolscan
	// ledger (patrolscan.LedgerPath) under the town's patrol_scan runtime dir.
	reapAlertStoreName = "reap_alerts.json"
)

// reapAlertSink is the alert side of the blocked-seat pass: raise one alert
// under a stable key, and clear the keys whose condition went away. Both return
// the delivery outcome, because the pass records an alert as raised or cleared
// only once gt escalate delivered it; tests inject a recorder that can fail on
// demand.
type reapAlertSink interface {
	Raise(key, source, message string) error
	Clear(reason string, keys ...string) error
}

// daemonAlertSink is the production reapAlertSink.
type daemonAlertSink struct{ d *Daemon }

func (s daemonAlertSink) Raise(key, source, message string) error {
	return s.d.escalateAlertErr(key, source, message)
}

func (s daemonAlertSink) Clear(reason string, keys ...string) error {
	return s.d.clearAlertsErr(reason, keys...)
}

// alerts returns the sink the blocked-seat pass sends through.
func (d *Daemon) alerts() reapAlertSink {
	if d.reapAlertSink != nil {
		return d.reapAlertSink
	}
	return daemonAlertSink{d: d}
}

// reapAlertState is what the pass remembers between ticks: the alert key last
// raised per seat ("<rig>/<name>") and whether each rig's threshold alert is
// open. Both are the on-disk shape, so a daemon restart re-reads the alerts it
// left open rather than forgetting them.
type reapAlertState struct {
	// Seats maps "<rig>/<name>" to the key raised and not yet cleared.
	Seats map[string]string `json:"seats,omitempty"`
	// Threshold records the rigs whose threshold alert is open.
	Threshold map[string]bool `json:"threshold,omitempty"`

	// loaded is set once the store has been read this process. storeErr
	// records a read that failed: the state is then incomplete, so it clears
	// nothing and stays off disk, leaving the record it could not read intact.
	loaded   bool
	storeErr error
}

// reapAlertStorePath is the persisted raised-key store.
func reapAlertStorePath(townRoot string) string {
	return filepath.Join(filepath.Dir(patrolscan.LedgerPath(townRoot)), reapAlertStoreName)
}

// load seeds the state from the store, once per process. A read that failed
// (present but unreadable or corrupt) leaves the state empty — so the pass
// clears nothing on the strength of it — and disables saving, so the record
// it could not read survives the tick.
func (st *reapAlertState) load(townRoot string) {
	if st.loaded {
		return
	}
	st.loaded = true
	st.Seats = map[string]string{}
	st.Threshold = map[string]bool{}
	if townRoot == "" {
		return // a Daemon without config: memory only, as before
	}
	data, err := os.ReadFile(reapAlertStorePath(townRoot)) //nolint:gosec // G304: town runtime path
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			st.storeErr = err
		}
		return
	}
	if err := json.Unmarshal(data, st); err != nil {
		st.storeErr = fmt.Errorf("parse %s: %w", reapAlertStoreName, err)
		st.Seats = map[string]string{}
		st.Threshold = map[string]bool{}
	}
}

// save writes the state back, unless the read that preceded it failed: a
// partial record would drop every other rig's keys.
func (st *reapAlertState) save(townRoot string) error {
	if townRoot == "" || st.storeErr != nil {
		return nil
	}
	return atomicfile.EnsureDirAndWriteJSONWithPerm(reapAlertStorePath(townRoot), st, 0o644)
}

// alertTownRoot is the town root the alert store lives under, or "" for a
// Daemon built as a test literal (no config): then the pass is memory-only.
func (d *Daemon) alertTownRoot() string {
	if d.config == nil {
		return ""
	}
	return d.config.TownRoot
}

// reapReportWindow is the alert throttle: the minimum gap between two raises
// of one unchanged alert, the same window patrol_scan uses for its recovery
// comments. It defaults to 24h.
func (d *Daemon) reapReportWindow() time.Duration {
	if c := patrolScanConfig(d.patrolConfig); c != nil && c.ReportWindow != "" {
		if parsed, err := time.ParseDuration(c.ReportWindow); err == nil && parsed > 0 {
			return parsed
		}
	}
	return patrolscan.DefaultReportWindow
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
// it might have alerted on was never observed. ledger is the tick's persisted
// patrolscan ledger, where the throttle records the keys it raised.
func (d *Daemon) reapAlertsIfEnabled(rig string, r patrolscan.Report, ledger patrolscan.Ledger) {
	c := reapCleanupConfig(d.patrolConfig)
	if !c.IsEnabled() || !c.CoversRig(rig) {
		return
	}
	d.reapAlerts(rig, r, c.AlertThreshold(), ledger)
}

// reapAlerts raises one alert per blocked reap candidate in r, plus one
// threshold alert when the blocked count reaches threshold. It clears a
// seat's alert once that seat stops being blocked, and the threshold alert
// once the count falls back below the threshold.
//
// A repeat under a key that is already open is throttled to once per report
// window; a new or changed key is always raised, so an unreadable throttle
// ledger can delay repeats but never silence a first alert. The set of open
// keys is persisted (reapAlertState), so clearing keeps working across the
// daemon restarts that lose the in-memory map.
//
// The throttle ledger and the open-key state are written only after the sink
// reports delivery. A raise gt escalate dropped is therefore raised again next
// tick instead of being suppressed for the window, and a dropped clear leaves
// its key in place to be cleared again — the two hazards the delivery-aware
// sink exists for (gt-s3u9a).
//
// It runs inline, inside the tick: escalateAlertErr retries on failure, so a
// blocked backlog costs wall-clock, but the tick is single-flight and the
// state here is only safe while one goroutine owns it.
func (d *Daemon) reapAlerts(rig string, r patrolscan.Report, threshold int, ledger patrolscan.Ledger) {
	if threshold <= 0 {
		threshold = defaultReapBlockedThreshold
	}
	sink := d.alerts()
	townRoot := d.alertTownRoot()
	st := &d.reapAlertState
	st.load(townRoot)
	now := d.clk().Now()
	window := d.reapReportWindow()

	blocked := blockedReapSeats(r)
	still := make(map[string]bool, len(blocked))
	for _, f := range blocked {
		still[f.Subject] = true
	}

	for _, f := range blocked {
		seat := rig + "/" + f.Subject
		key := reapBlockedKey(rig, f.Subject, f.Detail)
		prev := st.Seats[seat]
		if prev != "" && prev != key {
			// The blockers changed: the alert under the old key describes a
			// condition that no longer holds. Clear it first, and only then
			// raise the new key; a clear that did not land keeps the old key so
			// the next tick clears it again rather than orphaning the
			// escalation.
			if err := sink.Clear(fmt.Sprintf("%s is blocked by a different set of reasons now", seat), prev); err != nil {
				d.logReapAlertUndelivered("clear", seat, prev, err)
				continue
			}
			delete(st.Seats, seat)
			prev = ""
		}
		if prev == key && ledgerReportedWithin(ledger, key, now, window) {
			// The alert is open under this key and was raised inside the
			// window: a repeat now would only bump the occurrence count.
			continue
		}
		if err := sink.Raise(key, reapAlertSource, reapBlockedMessage(rig, f.Subject, f.Detail)); err != nil {
			// A raise that did not land records nothing: the throttle entry
			// would suppress the retry for the whole window, and the seat's
			// state would claim an alert that never reached an operator.
			d.logReapAlertUndelivered("raise", seat, key, err)
			continue
		}
		markAlertReported(ledger, key, now)
		st.Seats[seat] = key
	}

	// A rig-level read failure makes the walk incomplete: a seat with no
	// blocked finding may simply not have been seen, so its absence proves
	// nothing and clears nothing below. A failed read never acts.
	complete := len(r.Errors) == 0

	// A seat with no blocked finding this tick either recovered, was reaped,
	// went quiet, or left the walk. Either way the alert it raised is stale.
	if complete {
		stale := make([]string, 0, len(st.Seats))
		for seat := range st.Seats {
			if !strings.HasPrefix(seat, rig+"/") {
				continue
			}
			if !still[strings.TrimPrefix(seat, rig+"/")] {
				stale = append(stale, seat)
			}
		}
		sort.Strings(stale)
		for _, seat := range stale {
			key := st.Seats[seat]
			if err := sink.Clear(fmt.Sprintf("%s is no longer a blocked reap candidate", seat), key); err != nil {
				// The escalation is still open: keep the key so the next tick
				// clears it again instead of forgetting it.
				d.logReapAlertUndelivered("clear", seat, key, err)
				continue
			}
			delete(st.Seats, seat)
		}
	}

	thresholdKey := reapThresholdKey(rig)
	switch {
	case len(blocked) >= threshold:
		if !st.Threshold[rig] || !ledgerReportedWithin(ledger, thresholdKey, now, window) {
			if err := sink.Raise(thresholdKey, reapAlertSource, reapThresholdMessage(rig, len(blocked), threshold)); err != nil {
				d.logReapAlertUndelivered("raise", rig, thresholdKey, err)
				break
			}
			markAlertReported(ledger, thresholdKey, now)
		}
		st.Threshold[rig] = true
	case complete && st.Threshold[rig]:
		if err := sink.Clear(fmt.Sprintf("%s is below the blocked reap threshold", rig), thresholdKey); err != nil {
			d.logReapAlertUndelivered("clear", rig, thresholdKey, err)
			break
		}
		delete(st.Threshold, rig)
	}

	if err := st.save(townRoot); err != nil && d.logger != nil {
		d.logger.Printf("patrol_scan: reap alerts: saving state: %v", err)
	}
}

// logReapAlertUndelivered records a raise or clear gt escalate did not deliver,
// naming the seat or rig it concerned. The escalation_dropped feed line
// escalateAlertErr writes is the durable record; this line ties the drop to the
// key the pass will retry next tick, once per tick and key because the retry
// itself is the next tick.
func (d *Daemon) logReapAlertUndelivered(action, subject, key string, err error) {
	if d.logger == nil {
		return
	}
	d.logger.Printf("patrol_scan: reap alerts: %s for %s (%s) was not delivered: %v — retrying next tick",
		action, subject, key, err)
}

// ledgerReportedWithin reports whether key was raised inside the window. The
// patrolscan FileLedger answers "just reported" for a key it cannot read, so
// the caller consults this only for a repeat of an alert it already knows is
// open: an unreadable ledger delays repeats, never a first alert.
func ledgerReportedWithin(ledger patrolscan.Ledger, key string, now time.Time, window time.Duration) bool {
	if ledger == nil {
		return false
	}
	last, ok := ledger.LastReported(reapAlertKeyPrefix + key)
	return ok && now.Sub(last) < window
}

// markAlertReported records a raise in the throttle ledger. A ledger that
// cannot be written is not fatal: the alert was raised, and the next raise
// simply is not throttled.
func markAlertReported(ledger patrolscan.Ledger, key string, at time.Time) {
	if ledger == nil {
		return
	}
	_ = ledger.MarkReported(reapAlertKeyPrefix+key, at)
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
