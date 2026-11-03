package daemon

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/update"
)

// The update job's alert keys. A key is also the escalation fingerprint, so a
// repeat of the same condition files onto the escalation that already exists
// and the clear that ends it names the same key.
const (
	updateAlertKeyStale         = "update:stale"
	updateAlertKeyOverrun       = "update:overrun"
	updateAlertKeyLastRunFailed = "update:last-run-failed"

	// updateAlertSource is the escalation source every update alert is filed
	// under.
	updateAlertSource = "update"

	// updateStaleAfter is how old a running run's heartbeat may be before the
	// daemon reads the job as dead. The job refreshes it every 15 seconds, so
	// three minutes of silence is a dead run, not a slow step.
	updateStaleAfter = 3 * time.Minute

	// updateEndedAlertWindow is how recently a run must have ended badly for
	// the daemon to announce it. A daemon restarted the next day must not
	// re-announce a failure the operator already saw.
	updateEndedAlertWindow = 2 * time.Hour
)

// updateAlert is one condition the update job's state calls for: the key that
// identifies it to the escalation store, and the message that says what to do.
type updateAlert struct {
	Key, Message string
}

// updateAlertsFor decides, from the job's state alone, which alerts should be
// open. checkUpdateAlerts reads the state, delivers what this returns, and
// clears the keys this no longer returns.
//
// A running run with no heartbeat is stale: nothing proves the job is alive,
// and a running record the daemon cannot see a heartbeat for is exactly the
// dead job the stale alert exists for.
func updateAlertsFor(now time.Time, st update.State, maxRuntime time.Duration) []updateAlert {
	if st.Outcome != update.OutcomeRunning {
		if alert, ok := updateEndedAlert(now, st); ok {
			return []updateAlert{alert}
		}
		return nil
	}

	var alerts []updateAlert
	if st.HeartbeatAt.IsZero() {
		alerts = append(alerts, updateAlert{
			Key:     updateAlertKeyStale,
			Message: updateStaleMessage(st, "it has recorded no heartbeat"),
		})
	} else if age := now.Sub(st.HeartbeatAt); age > updateStaleAfter {
		alerts = append(alerts, updateAlert{
			Key:     updateAlertKeyStale,
			Message: updateStaleMessage(st, "its heartbeat is "+formatUpdateAge(age)+" old"),
		})
	}
	if elapsed := now.Sub(st.StartedAt); elapsed > maxRuntime {
		alerts = append(alerts, updateAlert{
			Key: updateAlertKeyOverrun,
			Message: fmt.Sprintf("update run %s (%s) has been running at step %s for %s, past the max_runtime of %s; "+
				"if the job is stuck, read %s",
				st.RunID, updateSourceName(st.Source), updateStepName(st.Current),
				formatUpdateAge(elapsed), maxRuntime, updateRunLogPath(st.RunID)),
		})
	}
	return alerts
}

// updateEndedAlert is the alert for a run that ended badly inside the
// announcement window. An ok or interrupted run ends cleanly, and an old
// failure is history the operator has already had one chance to see.
func updateEndedAlert(now time.Time, st update.State) (updateAlert, bool) {
	switch st.Outcome {
	case update.OutcomeFailed, update.OutcomePartial, update.OutcomeAborted:
	default:
		return updateAlert{}, false
	}
	if st.EndedAt.IsZero() {
		return updateAlert{}, false
	}
	age := now.Sub(st.EndedAt)
	if age > updateEndedAlertWindow {
		return updateAlert{}, false
	}
	return updateAlert{
		Key:     updateAlertKeyLastRunFailed,
		Message: updateEndedMessage(st, age),
	}, true
}

// updateStaleMessage says what stopped and the one way out of it: a dead run
// leaves the dispatch hold behind, so the operator has to remove that file
// before dispatchers work again.
func updateStaleMessage(st update.State, heartbeat string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "update run %s (%s) is running at step %s, but %s",
		st.RunID, updateSourceName(st.Source), updateStepName(st.Current), heartbeat)
	if st.Hold.Owned {
		fmt.Fprintf(&b, "; it owns the dispatch hold (%s)", updateHoldNote(st.Hold.Note))
	}
	fmt.Fprintf(&b, ". If the job died, remove the dispatch hold file it set and read %s",
		updateRunLogPath(st.RunID))
	return b.String()
}

// updateEndedMessage names the run, its outcome and every step that failed or
// deferred, with the detail the step recorded.
func updateEndedMessage(st update.State, age time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "update run %s (%s) ended %s %s ago",
		st.RunID, updateSourceName(st.Source), st.Outcome, formatUpdateAge(age))
	if steps := updateBadSteps(st.Steps); len(steps) > 0 {
		fmt.Fprintf(&b, ": %s", strings.Join(steps, "; "))
	}
	return b.String()
}

// updateBadSteps lists the failed and deferred steps, each with its detail.
func updateBadSteps(steps []update.StepRecord) []string {
	var out []string
	for _, step := range steps {
		if step.Result != update.ResultFailed && step.Result != update.ResultDeferred {
			continue
		}
		line := fmt.Sprintf("%s (%s)", step.Name, step.Result)
		if detail := strings.TrimSpace(step.Detail); detail != "" {
			line += ": " + detail
		}
		out = append(out, line)
	}
	return out
}

// updateSourceName names where the run came from; an empty source is named
// rather than left blank in the message.
func updateSourceName(source string) string {
	if strings.TrimSpace(source) == "" {
		return "unknown source"
	}
	return source
}

// updateStepName names the step a run is on; an empty step is named rather
// than left blank.
func updateStepName(step update.StepName) string {
	if strings.TrimSpace(string(step)) == "" {
		return "unknown"
	}
	return string(step)
}

// updateHoldNote is the dispatch hold's note, or a plain statement that it
// has none, so the message never trails off after an empty note.
func updateHoldNote(note string) string {
	if strings.TrimSpace(note) == "" {
		return "no note"
	}
	return note
}

// updateRunLogPath is the run's log, relative to the town root, named in the
// alert so the operator knows where the job's output went.
func updateRunLogPath(runID string) string {
	return filepath.Join(".runtime", "update", "run-"+runID+".log")
}

// formatUpdateAge renders a wait as a duration rounded to the second, and
// never negative: a timestamp from the future is still "0s" old.
func formatUpdateAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}

// checkUpdateAlerts runs on every heartbeat, whether or not the update trigger
// is enabled: a hand-started run can die too, and nothing else would say so.
// The daemon only reads the job's state; it never writes it.
func (d *Daemon) checkUpdateAlerts(now time.Time) {
	store := update.Store{Dir: update.Dir(d.config.TownRoot)}
	st, err := store.Read()
	switch {
	case err == nil:
		d.updateAlerts.forgetReadError()
		d.applyUpdateAlerts(updateAlertsFor(now, st, d.updateAlertMaxRuntime()))
	case errors.Is(err, update.ErrNoState):
		// No run has written state, so there is no running job to watch:
		// whatever was open has ended.
		d.updateAlerts.forgetReadError()
		d.applyUpdateAlerts(nil)
	default:
		// An unreadable state is unknown, not ended: keep whatever is open
		// and raise nothing this heartbeat. Log it once per distinct message.
		if d.updateAlerts.noteReadError(err.Error()) {
			d.logger.Printf("update: cannot read the job's state: %v; the update alerts stay as they are", err)
		}
	}
}

// applyUpdateAlerts raises the wanted alerts this daemon run has not raised
// yet and clears the latched keys no longer wanted. A raise or clear the
// notifier refused is left unrecorded, so the next heartbeat tries again
// rather than treating a silence as an open alert.
func (d *Daemon) applyUpdateAlerts(wanted []updateAlert) {
	want := make(map[string]bool, len(wanted))
	for _, alert := range wanted {
		want[alert.Key] = true
		if d.updateAlerts.isRaised(alert.Key) {
			continue
		}
		if err := d.escalateAlertSeverity("MEDIUM", alert.Key, updateAlertSource, alert.Message); err != nil {
			d.logger.Printf("update: cannot raise %s: %v", alert.Key, err)
			continue
		}
		d.updateAlerts.markRaised(alert.Key)
	}
	for _, key := range d.updateAlerts.openKeys() {
		if want[key] {
			continue
		}
		if err := d.clearAlertsErr(updateClearReason(key), key); err != nil {
			continue
		}
		d.updateAlerts.markCleared(key)
	}
}

// updateClearReason says why an update alert closed.
func updateClearReason(key string) string {
	switch key {
	case updateAlertKeyStale, updateAlertKeyOverrun:
		return "update job heartbeat resumed or run ended"
	default:
		return "the update job's last run is no longer a recent failure"
	}
}

// updateAlertMaxRuntime is the overrun limit: the configured max_runtime, or
// the default when the update block is unconfigured or does not parse. The
// alerts must work without the feature, so a broken block is not a reason to
// skip them.
func (d *Daemon) updateAlertMaxRuntime() time.Duration {
	settings, ok := d.updateSettings()
	if !ok || settings.MaxRuntime <= 0 {
		return config.DefaultUpdateMaxRuntime
	}
	return settings.MaxRuntime
}

// updateAlertLatch is this daemon run's memory of the update alerts: the keys
// it has raised and not yet cleared, and the last state-read error it logged.
// Without it a stale job would file an escalation every heartbeat.
type updateAlertLatch struct {
	mu sync.Mutex
	// raised holds the keys raised and not yet cleared.
	raised map[string]bool
	// readErr is the last state-read error logged; an unchanged error logs
	// once, not once per heartbeat.
	readErr string
}

// isRaised reports whether key was raised and not yet cleared.
func (l *updateAlertLatch) isRaised(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.raised[key]
}

// markRaised records that key's escalation was delivered.
func (l *updateAlertLatch) markRaised(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.raised == nil {
		l.raised = map[string]bool{}
	}
	l.raised[key] = true
}

// markCleared records that key's escalation was cleared.
func (l *updateAlertLatch) markCleared(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.raised, key)
}

// openKeys returns the raised keys in sorted order, so a pass that clears
// several does so deterministically.
func (l *updateAlertLatch) openKeys() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	keys := make([]string, 0, len(l.raised))
	for key := range l.raised {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// noteReadError records a state-read error and reports whether it differs
// from the last one logged.
func (l *updateAlertLatch) noteReadError(msg string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	fresh := l.readErr != msg
	l.readErr = msg
	return fresh
}

// forgetReadError drops the last read error, so a later recurrence is logged
// again.
func (l *updateAlertLatch) forgetReadError() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.readErr = ""
}
