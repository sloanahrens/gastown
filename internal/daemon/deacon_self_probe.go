package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/deacon"
	"github.com/steveyegge/gastown/internal/mail"
)

// DeaconSelfProbeSubjectPrefix marks a mail message as a doctor-dog
// self-probe: a known event injected through the deacon's own mail inbox
// (glossary "Self-probe"), followed by a nonce for correlation. The
// deacon patrol's inbox-hygiene step recognizes this prefix and
// acknowledges the message mechanically via `gt deacon ack-probes` — no
// LLM judgment involved. Probes are always sent with Message.Wisp=true
// (ephemeral, GC'd on ack, never a durable bead) and are excluded from
// `gt mail inbox`'s default view and `gt status` unread counts by this
// same prefix. Re-exported from internal/constants, which internal/mail
// also depends on to exclude probes from unread counts.
const DeaconSelfProbeSubjectPrefix = constants.DeaconSelfProbeSubjectPrefix

// defaultDeaconSelfProbeBudget is used when the deacon role's
// self_probe_budget cannot be loaded, matching config/roles/deacon.toml's
// built-in default. It bounds the patrol CYCLE the ack has to wait out, not
// the doctor-dog send interval: a probe is acked when the patrol reaches its
// ack-probes step, which is at most one cycle away (hq-90m15).
const defaultDeaconSelfProbeBudget = 45 * time.Minute

// deaconSelfProbeMissedCycleFactor sizes the ceiling on an unacked probe that
// no ack-probes run has covered: past this multiple of the budget, the patrol
// stopped reaching its ack step rather than merely missing this one probe
// (hq-90m15). Generous because a dead session is already caught in minutes by
// heartbeat staleness (deacon.HeartbeatVeryStaleThreshold) — this ceiling is
// for a session that is alive but not cycling.
const deaconSelfProbeMissedCycleFactor = 2

// deaconSelfProbeErrorThreshold is the number of consecutive Error verdicts
// before recordDeaconSelfProbeVerdict escalates to the mayor.
const deaconSelfProbeErrorThreshold = 3

// deaconSelfProbeAlertKey is the escalation fingerprint for an unacked or
// late deacon self-probe — stable across repeats so `gt escalate` dedupes
// onto one open bead instead of one per doctor-dog tick (see jsonl_git_backup.go's
// alert-key doc comment).
const deaconSelfProbeAlertKey = "deacon-self-probe:unacked"

// deaconSelfProbeBaseline is the runtime state for the deacon self-probe,
// persisted at <town>/.runtime/deacon-self-probe.json. Nonce/SentAt
// identify the last probe SendDeaconSelfProbe successfully delivered —
// Router.Send does not hand back the bead ID bd create assigns, so the
// nonce embedded in the subject is the only correlation handle available.
// LastSendError records a failed send attempt without discarding the last
// successful probe, so EvaluateDeaconSelfProbe can still judge it.
// ConsecutiveErrors survives across sends (sendDeaconSelfProbeWith carries it
// forward) — it counts consecutive Error verdicts across probes, not
// anything about one probe alone.
type deaconSelfProbeBaseline struct {
	Nonce             string    `json:"nonce,omitempty"`
	SentAt            time.Time `json:"sent_at,omitempty"`
	LastSendError     string    `json:"last_send_error,omitempty"`
	ConsecutiveErrors int       `json:"consecutive_errors,omitempty"`
}

// deaconInboxLister is the narrow mail-reading surface EvaluateDeaconSelfProbe
// depends on, satisfied by *mail.Mailbox. A dedicated interface lets tests
// inject a fake instead of shelling out to bd.
type deaconInboxLister interface {
	List() ([]*mail.Message, error)
}

func deaconSelfProbeStatePath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "deacon-self-probe.json")
}

// deaconAckProbesRun is the runtime record of the patrol's last
// `gt deacon ack-probes` run — the ack step's own heartbeat, which dates the
// first moment the probe could have been acked.
type deaconAckProbesRun struct {
	LastRun time.Time `json:"last_run,omitempty"`
}

func deaconAckProbesRunPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "deacon-ack-probes.json")
}

// RecordDeaconAckProbesRun records that the deacon patrol's ack-probes step ran
// now, so a probe the patrol never had the chance to ack is not read as a
// missed one (hq-90m15). Called even when no probe is pending: the step running
// at all is the signal.
func RecordDeaconAckProbesRun(townRoot string) error {
	return writeDeaconAckProbesRun(deaconAckProbesRunPath(townRoot), deaconAckProbesRun{LastRun: time.Now()})
}

func writeDeaconAckProbesRun(path string, run deaconAckProbesRun) error {
	return atomicfile.EnsureDirAndWriteJSON(path, run)
}

func readDeaconAckProbesRun(townRoot string) (time.Time, bool) {
	data, err := os.ReadFile(deaconAckProbesRunPath(townRoot))
	if err != nil {
		return time.Time{}, false
	}
	var run deaconAckProbesRun
	if err := json.Unmarshal(data, &run); err != nil || run.LastRun.IsZero() {
		return time.Time{}, false
	}
	return run.LastRun, true
}

// deaconSelfProbeBudget returns the deacon role's configured
// self_probe_budget (config/roles/deacon.toml [health] self_probe_budget),
// falling back to the built-in default if the role definition can't be
// loaded. Deliberately does not consult ping_timeout — see
// defaultDeaconSelfProbeBudget's doc comment for why sharing that value was
// the bug.
func deaconSelfProbeBudget(townRoot string) time.Duration {
	def, err := config.LoadRoleDefinition(townRoot, "", constants.RoleDeacon)
	if err != nil || def.Health.SelfProbeBudget.Duration <= 0 {
		return defaultDeaconSelfProbeBudget
	}
	return def.Health.SelfProbeBudget.Duration
}

// SendDeaconSelfProbe sends one probe mail into the deacon's real inbox
// through the normal gt mail path (mail.Router.Send — the same entry point
// the deacon's real mail uses) and records it as the baseline for the next
// EvaluateDeaconSelfProbe call. Called once per doctor-dog cycle from
// runDoctorDog; volume control (one per run) lives entirely in that single
// call site.
func SendDeaconSelfProbe(townRoot string) error {
	return sendDeaconSelfProbeWith(mail.NewRouterWithTownRoot(townRoot, townRoot), townRoot)
}

func sendDeaconSelfProbeWith(sender mailSender, townRoot string) error {
	nonce := mail.GenerateID()
	sentAt := time.Now()
	probe := mail.NewMessage("daemon", constants.RoleDeacon, deaconSelfProbeSubject(nonce), deaconSelfProbeBody(nonce))
	probe.Wisp = true
	// A mechanically-acked probe reacting on a 5-minute cadence shouldn't
	// also interrupt the deacon's session with a notification.
	probe.SuppressNotify = true

	statePath := deaconSelfProbeStatePath(townRoot)
	prev, _ := readDeaconSelfProbeBaseline(statePath)
	if err := sender.Send(probe); err != nil {
		// Preserve whatever baseline already exists (a still-evaluable,
		// previously sent probe, if any) and just record this failure so
		// EvaluateDeaconSelfProbe can report it without a live send call.
		prev.LastSendError = err.Error()
		_ = writeDeaconSelfProbeBaseline(statePath, prev)
		return err
	}

	// ConsecutiveErrors tracks a run across probes, not anything about this
	// one alone — carry it forward rather than resetting it on every send.
	// recordDeaconSelfProbeVerdict is the only thing that changes it.
	return writeDeaconSelfProbeBaseline(statePath, deaconSelfProbeBaseline{
		Nonce:             nonce,
		SentAt:            sentAt,
		ConsecutiveErrors: prev.ConsecutiveErrors,
	})
}

// DeaconSelfProbeVerdict is "ok", "error", or "skipped" — the doctor check
// (internal/doctor.DeaconSelfProbeCheck) maps this to its CheckStatus
// vocabulary. Kept as plain strings here so this package doesn't need to
// depend on internal/doctor's types (internal/doctor already depends on
// internal/daemon for daemon.LoadState, so the reverse import would cycle).
type DeaconSelfProbeVerdict struct {
	Verdict string
	Message string
}

const (
	deaconSelfProbeVerdictOK      = "ok"
	deaconSelfProbeVerdictError   = "error"
	deaconSelfProbeVerdictSkipped = "skipped"
	// deaconSelfProbeVerdictPending marks an unacked probe that is still
	// inside its budget: neither evidence of failure (Error) nor of health
	// (OK). Distinct from Skipped so runDeaconSelfProbeCycle can tell "wait
	// for this same probe to resolve" apart from the ambiguous cases
	// (missing message, unreadable inbox) where sending a replacement is
	// safe.
	deaconSelfProbeVerdictPending = "pending"
)

// EvaluateDeaconSelfProbe reads back whether the last probe
// SendDeaconSelfProbe sent was acknowledged within the deacon role's
// self_probe_budget. It never sends a probe itself, never acks, and never
// mutates ConsecutiveErrors — sending, acking and escalation state all
// belong to the daemon's own cycle (Daemon.runDeaconSelfProbe) alone, so
// this read-only judgment may be called as often as desired (e.g. by `gt
// doctor`) without affecting probe volume or the escalation counter.
func EvaluateDeaconSelfProbe(townRoot string) DeaconSelfProbeVerdict {
	return evaluateDeaconSelfProbeWith(mail.NewMailboxFromAddress(constants.RoleDeacon, townRoot), townRoot)
}

func evaluateDeaconSelfProbeWith(reader deaconInboxLister, townRoot string) DeaconSelfProbeVerdict {
	statePath := deaconSelfProbeStatePath(townRoot)
	baseline, exists := readDeaconSelfProbeBaseline(statePath)
	if !exists {
		return DeaconSelfProbeVerdict{Verdict: deaconSelfProbeVerdictSkipped, Message: "unknown: no probe has been sent yet"}
	}
	if baseline.LastSendError != "" {
		return DeaconSelfProbeVerdict{Verdict: deaconSelfProbeVerdictSkipped, Message: "unknown: the last probe send failed: " + baseline.LastSendError}
	}
	if baseline.Nonce == "" {
		return DeaconSelfProbeVerdict{Verdict: deaconSelfProbeVerdictSkipped, Message: "unknown: no probe has been sent yet"}
	}

	messages, err := reader.List()
	if err != nil {
		return DeaconSelfProbeVerdict{Verdict: deaconSelfProbeVerdictSkipped, Message: "unknown: could not read the deacon inbox: " + err.Error()}
	}

	var found *mail.Message
	for _, msg := range messages {
		if msg != nil && deaconSelfProbeNonce(msg.Subject) == baseline.Nonce {
			found = msg
			break
		}
	}

	if found == nil {
		// The mechanical ack step in mol-deacon-patrol only removes a probe
		// after acking it, so a probe missing entirely (rather than present
		// and unacked) is ambiguous — it could mean the patrol acked and
		// cleaned it up already, or that something unrelated GC'd it
		// unacked. Treat as skipped rather than infer OK.
		return DeaconSelfProbeVerdict{Verdict: deaconSelfProbeVerdictSkipped, Message: "unknown: previous probe is no longer present in the deacon inbox, could not verify ack"}
	}

	budget := deaconSelfProbeBudget(townRoot)

	if found.DeliveryState != mail.DeliveryStateAcked || found.DeliveryAckedAt == nil {
		elapsed := time.Since(baseline.SentAt)
		if elapsed <= budget {
			// Still inside the budget: not evidence of failure. Judged
			// against self_probe_budget BEFORE being counted as Error, so
			// a probe that would ack any second now doesn't trip the
			// counter early (see runDeaconSelfProbeCycle, which holds this
			// same probe rather than replacing it while pending).
			return DeaconSelfProbeVerdict{
				Verdict: deaconSelfProbeVerdictPending,
				Message: fmt.Sprintf("deacon patrol has not yet acked the probe sent %s ago (budget %s)", elapsed.Round(time.Second), budget),
			}
		}
		return deaconSelfProbeUnackedPastBudgetVerdict(townRoot, baseline.SentAt, elapsed, budget)
	}

	latency := found.DeliveryAckedAt.Sub(baseline.SentAt)
	if latency > budget {
		return DeaconSelfProbeVerdict{
			Verdict: deaconSelfProbeVerdictError,
			Message: fmt.Sprintf("deacon patrol acked the probe late: %s after send (budget %s)", latency.Round(time.Second), budget),
		}
	}

	return DeaconSelfProbeVerdict{
		Verdict: deaconSelfProbeVerdictOK,
		Message: fmt.Sprintf("deacon patrol acked the probe in %s (budget %s)", latency.Round(time.Second), budget),
	}
}

// deaconSelfProbeUnackedPastBudgetVerdict judges a probe still unacked after
// its budget. The patrol acks only on reaching its ack-probes step, so the
// probe is evidence of failure only once an ack-probes run has covered it and
// left it behind; a patrol that never reached the step has not had its chance
// yet (hq-90m15). Past the missed-cycle ceiling the step is not being reached
// at all, which is the failure the probe exists to catch.
func deaconSelfProbeUnackedPastBudgetVerdict(townRoot string, sentAt time.Time, elapsed, budget time.Duration) DeaconSelfProbeVerdict {
	ackRun, ran := readDeaconAckProbesRun(townRoot)
	if ran && !ackRun.Before(sentAt) {
		return DeaconSelfProbeVerdict{
			Verdict: deaconSelfProbeVerdictError,
			Message: fmt.Sprintf("deacon patrol ran ack-probes at %s and left the probe sent %s ago unacked (budget %s)",
				ackRun.Format(time.RFC3339), elapsed.Round(time.Second), budget),
		}
	}
	if elapsed <= budget*deaconSelfProbeMissedCycleFactor {
		return DeaconSelfProbeVerdict{
			Verdict: deaconSelfProbeVerdictPending,
			Message: fmt.Sprintf("deacon patrol has not run ack-probes since the probe was sent %s ago; no ack opportunity has passed yet (budget %s)",
				elapsed.Round(time.Second), budget),
		}
	}
	return DeaconSelfProbeVerdict{
		Verdict: deaconSelfProbeVerdictError,
		Message: fmt.Sprintf("deacon patrol has not run ack-probes since the probe was sent %s ago — %d budgets with no ack step (budget %s)",
			elapsed.Round(time.Second), deaconSelfProbeMissedCycleFactor, budget),
	}
}

// recordDeaconSelfProbeVerdict advances the persisted ConsecutiveErrors
// counter from the latest verdict and fires the escalation hooks. Unlike
// EvaluateDeaconSelfProbe (read-only, safe for `gt doctor` to call any
// number of times), this mutates state and must run exactly once per
// doctor-dog tick — Daemon.runDeaconSelfProbe is the only caller in
// production.
//
// alert/clear may be nil (e.g. a zero-value Daemon in tests that don't care
// about escalation): the counter still advances, escalation is just skipped.
func recordDeaconSelfProbeVerdict(townRoot string, verdict DeaconSelfProbeVerdict, alert func(key, source, message string), clear func(reason string, keys ...string)) {
	statePath := deaconSelfProbeStatePath(townRoot)
	baseline, exists := readDeaconSelfProbeBaseline(statePath)
	if !exists {
		// Nothing sent yet; evaluateDeaconSelfProbeWith already reports this
		// verdict as Skipped, so there is no counter to advance.
		return
	}

	switch verdict.Verdict {
	case deaconSelfProbeVerdictOK:
		baseline.ConsecutiveErrors = 0
		_ = writeDeaconSelfProbeBaseline(statePath, baseline)
		if clear != nil {
			clear("deacon self-probe acked within budget", deaconSelfProbeAlertKey)
		}
	case deaconSelfProbeVerdictError:
		baseline.ConsecutiveErrors++
		_ = writeDeaconSelfProbeBaseline(statePath, baseline)
		if baseline.ConsecutiveErrors >= deaconSelfProbeErrorThreshold && alert != nil {
			alert(deaconSelfProbeAlertKey, "deacon_self_probe", fmt.Sprintf(
				"deacon patrol has failed %d consecutive self-probes: %s",
				baseline.ConsecutiveErrors, verdict.Message))
		}
	case deaconSelfProbeVerdictSkipped, deaconSelfProbeVerdictPending:
		// Not evidence in either direction (e.g. probe missing, inbox
		// unreadable, or still inside its budget) — leave ConsecutiveErrors
		// untouched rather than let an ambiguous or premature read either
		// mask a real failure streak or falsely trip one.
	}
}

// runDeaconSelfProbeCycle is the full evaluate-then-send half of the
// doctor-dog self-probe tick: gate on deacon pause, judge the outstanding
// probe, advance/escalate, then send the next one. Daemon.runDeaconSelfProbe
// (doctor_dog.go) is the production entry point, wiring the real mailbox,
// mail router and escalation hooks; this signature exists so the whole cycle
// — including the pause gate — can be driven by a test with fakes, never a
// live townRoot mailbox or a real `gt escalate` subprocess.
//
// Sending after evaluating (rather than unconditionally, as before this
// change) is what keeps at most one probe tracked as outstanding: each
// successful send replaces the baseline's nonce, so only the newest probe is
// ever judged again.
func runDeaconSelfProbeCycle(townRoot string, mailbox deaconInboxLister, sender mailSender, alert func(key, source, message string), clear func(reason string, keys ...string)) error {
	if paused, _, err := deacon.IsPaused(townRoot); err != nil || paused {
		// Fail closed: an unreadable pause state must not be read as "not
		// paused" (heartbeat.go:156's precedent), and a deliberately paused
		// deacon legitimately will not ack — either way, evaluating now
		// would raise a false alarm rather than reflect deacon health.
		//
		// Drop any outstanding probe and its error streak so a probe sent
		// before the pause is never judged after resume: without this, the
		// first tick after `gt deacon resume` judges an hours-old probe
		// against the budget and can page the mayor immediately.
		resetDeaconSelfProbeBaseline(townRoot)
		return nil
	}

	verdict := evaluateDeaconSelfProbeWith(mailbox, townRoot)
	recordDeaconSelfProbeVerdict(townRoot, verdict, alert, clear)

	if verdict.Verdict == deaconSelfProbeVerdictPending {
		// Still inside its budget and unacked: keep judging this same
		// probe rather than replacing it, so the elapsed time being
		// measured against the budget survives across ticks. Sending a
		// replacement here is what made the budget check inert — every
		// probe was judged at ~one doctor-dog interval old, never at its
		// real age.
		return nil
	}

	// sendDeaconSelfProbeWith already records a failure in the baseline for
	// the next evaluation; the returned error is only for the caller to log.
	return sendDeaconSelfProbeWith(sender, townRoot)
}

// resetDeaconSelfProbeBaseline drops any outstanding probe and its error
// streak. Called while the deacon is paused (see runDeaconSelfProbeCycle) so
// a probe sent before the pause is never judged after resume.
func resetDeaconSelfProbeBaseline(townRoot string) {
	statePath := deaconSelfProbeStatePath(townRoot)
	baseline, exists := readDeaconSelfProbeBaseline(statePath)
	if !exists || (baseline == deaconSelfProbeBaseline{}) {
		return
	}
	_ = writeDeaconSelfProbeBaseline(statePath, deaconSelfProbeBaseline{})
}

func deaconSelfProbeSubject(nonce string) string {
	return DeaconSelfProbeSubjectPrefix + " " + nonce
}

// deaconSelfProbeNonce extracts the nonce from a probe subject, or ""
// if subject doesn't carry the probe prefix.
func deaconSelfProbeNonce(subject string) string {
	prefix := DeaconSelfProbeSubjectPrefix + " "
	if !strings.HasPrefix(subject, prefix) {
		return ""
	}
	return strings.TrimPrefix(subject, prefix)
}

func deaconSelfProbeBody(nonce string) string {
	return fmt.Sprintf(
		"Doctor-dog self-probe (nonce %s). Mechanical ack only — the mol-deacon-patrol "+
			"inbox-hygiene step acknowledges %s messages without an LLM decision. See docs/glossary.md#self-probe.",
		nonce, DeaconSelfProbeSubjectPrefix,
	)
}

func readDeaconSelfProbeBaseline(path string) (deaconSelfProbeBaseline, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return deaconSelfProbeBaseline{}, false
	}
	var baseline deaconSelfProbeBaseline
	if err := json.Unmarshal(data, &baseline); err != nil {
		return deaconSelfProbeBaseline{}, false
	}
	return baseline, true
}

func writeDeaconSelfProbeBaseline(path string, baseline deaconSelfProbeBaseline) error {
	return atomicfile.EnsureDirAndWriteJSON(path, baseline)
}
