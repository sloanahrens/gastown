package notify

import (
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dispatch"
)

// redHoldAckKey is the attention item key that acknowledges the red hold.
const redHoldAckKey = "hold:red"

// RedHoldPushRun is one pass of the red-hold push over injected readers and
// senders, so a unit test touches no file, database or process.
type RedHoldPushRun struct {
	Hold   func() (dispatch.RedHoldRecord, error)
	Acks   func() (attention.Acks, error)
	Marker func() (RedHoldMarker, error)
	Write  func(RedHoldMarker) error
	Push   func(name, title, body string) error
	// Name is the notify command to push through; "" pushes nothing.
	Name string
}

// Run sends the owed red-hold text once and records it. An owed email is the
// next bead's push, so MailedAt is left as the marker had it and a pass with no
// owed text writes nothing; a failed send leaves the marker for the next pass,
// and a marker that cannot be written after a send is returned, not hidden.
func (r RedHoldPushRun) Run(now time.Time) error {
	if r.Name == "" || r.Push == nil {
		return nil
	}
	rec, err := r.Hold()
	if err != nil {
		return fmt.Errorf("reading the red hold: %w", err)
	}
	if len(rec.Reasons) == 0 {
		return nil
	}
	since := oldestRedHoldSince(rec.Reasons)
	acks, err := r.Acks()
	if err != nil {
		return fmt.Errorf("reading the attention acks: %w", err)
	}
	acked := attention.AckedKey(acks, redHoldAckKey)
	m, err := r.Marker()
	if err != nil {
		return fmt.Errorf("reading the red-hold push marker: %w", err)
	}
	owed, next := RedHoldPushesOwed(since, acked, m, now)
	if !owed.Text {
		return nil
	}
	if err := r.Push(r.Name, redHoldPushLine(since, rec.Reasons, now), ""); err != nil {
		return fmt.Errorf("texting the red hold: %w", err)
	}
	if owed.Mail {
		// The text goes alone on this pass: recording MailedAt now would keep
		// the email from ever being sent.
		next.MailedAt = time.Time{}
	}
	if err := r.Write(next); err != nil {
		return fmt.Errorf("recording the red-hold text: %w", err)
	}
	return nil
}

// oldestRedHoldSince is the earliest nonzero reason Since, or the zero time when
// none has one, which owes nothing.
func oldestRedHoldSince(reasons []dispatch.RedHoldReason) time.Time {
	var oldest time.Time
	for _, r := range reasons {
		if !r.Since.IsZero() && (oldest.IsZero() || r.Since.Before(oldest)) {
			oldest = r.Since
		}
	}
	return oldest
}

// redHoldPushLine is the text line: the hold's age, hold:red, then the kinds
// holding dispatch.
func redHoldPushLine(since time.Time, reasons []dispatch.RedHoldReason, now time.Time) string {
	kinds := make([]string, len(reasons))
	for i, r := range reasons {
		kinds[i] = r.Kind
	}
	return fmt.Sprintf("Red hold unacknowledged for %s: %s %s",
		ageText(now.Sub(since)), redHoldAckKey, strings.Join(kinds, ", "))
}

// PushRedHold is one pass against townRoot, reading the red hold, the acks, the
// marker and the notify command from the town's own files. A town with no notify
// command and no email contact has nowhere to send and reads nothing further. Its
// caller is the daemon heartbeat, in a later bead.
func PushRedHold(townRoot string, now time.Time) error {
	cfg, err := config.LoadOrCreateEscalationConfig(config.EscalationConfigPath(townRoot))
	if err != nil {
		return fmt.Errorf("loading escalation config: %w", err)
	}
	pushName := config.LoadOperationalConfig(townRoot).GetOverseerConfig().SilentPushName()
	if pushName == "" && (cfg.Contacts.HumanEmail == "" || cfg.Contacts.SMTPHost == "") {
		return nil
	}
	return RedHoldPushRun{
		Hold:   func() (dispatch.RedHoldRecord, error) { return dispatch.ReadRedHold(townRoot) },
		Acks:   func() (attention.Acks, error) { return attention.ReadAcks(townRoot) },
		Marker: func() (RedHoldMarker, error) { return ReadRedHoldMarker(townRoot) },
		Write:  func(m RedHoldMarker) error { return WriteRedHoldMarker(townRoot, m) },
		Push:   func(name, title, body string) error { return pushThrough(runNotifyCommand, cfg, name, title, body) },
		Name:   pushName,
	}.Run(now)
}
