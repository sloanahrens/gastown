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

// defaultDeaconSelfProbeBudget is used when the deacon role's ping_timeout
// cannot be loaded, matching config/roles/deacon.toml's built-in default.
const defaultDeaconSelfProbeBudget = 30 * time.Second

// deaconSelfProbeBaseline is the runtime state for the deacon self-probe,
// persisted at <town>/.runtime/deacon-self-probe.json. Nonce/SentAt
// identify the last probe SendDeaconSelfProbe successfully delivered —
// Router.Send does not hand back the bead ID bd create assigns, so the
// nonce embedded in the subject is the only correlation handle available.
// LastSendError records a failed send attempt without discarding the last
// successful probe, so EvaluateDeaconSelfProbe can still judge it.
type deaconSelfProbeBaseline struct {
	Nonce         string    `json:"nonce,omitempty"`
	SentAt        time.Time `json:"sent_at,omitempty"`
	LastSendError string    `json:"last_send_error,omitempty"`
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

// deaconSelfProbeBudget returns the deacon role's configured ping_timeout
// (config/roles/deacon.toml [health] ping_timeout), falling back to the
// built-in default if the role definition can't be loaded.
func deaconSelfProbeBudget(townRoot string) time.Duration {
	def, err := config.LoadRoleDefinition(townRoot, "", constants.RoleDeacon)
	if err != nil || def.Health.PingTimeout.Duration <= 0 {
		return defaultDeaconSelfProbeBudget
	}
	return def.Health.PingTimeout.Duration
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
	if err := sender.Send(probe); err != nil {
		// Preserve whatever baseline already exists (a still-evaluable,
		// previously sent probe, if any) and just record this failure so
		// EvaluateDeaconSelfProbe can report it without a live send call.
		prev, _ := readDeaconSelfProbeBaseline(statePath)
		prev.LastSendError = err.Error()
		_ = writeDeaconSelfProbeBaseline(statePath, prev)
		return err
	}

	return writeDeaconSelfProbeBaseline(statePath, deaconSelfProbeBaseline{Nonce: nonce, SentAt: sentAt})
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
)

// EvaluateDeaconSelfProbe reads back whether the last probe
// SendDeaconSelfProbe sent was acknowledged within the deacon role's
// ping_timeout budget. It never sends a probe itself — sending is the
// daemon ticker's job alone, so this may be called as often as desired
// (e.g. by `gt doctor`) without affecting probe volume.
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

	if found.DeliveryState != mail.DeliveryStateAcked || found.DeliveryAckedAt == nil {
		return DeaconSelfProbeVerdict{
			Verdict: deaconSelfProbeVerdictError,
			Message: fmt.Sprintf("deacon patrol did not ack the probe sent %s ago", time.Since(baseline.SentAt).Round(time.Second)),
		}
	}

	budget := deaconSelfProbeBudget(townRoot)
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
