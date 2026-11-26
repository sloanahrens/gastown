package notify

import (
	"errors"
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
	// Name is the notify command to push the text through; "" texts nothing.
	Name string
	// Mail sends the email with the text's line; nil emails nothing.
	Mail func(line string) error
}

// Run sends the owed red-hold text and email, each on its own, and records each
// one that is made: a failed send leaves its stamp unset for the next pass. A
// marker that cannot be written after a send is returned, not hidden.
func (r RedHoldPushRun) Run(now time.Time) error {
	texts := r.Name != "" && r.Push != nil
	if !texts && r.Mail == nil {
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
	owed, _ := RedHoldPushesOwed(since, acked, m, now)
	textOwed, mailOwed := owed.Text && texts, owed.Mail && r.Mail != nil
	if !textOwed && !mailOwed {
		return nil
	}
	// RedHoldPushesOwed stamps pushes this pass cannot make, so its marker is not
	// used: each stamp is set below only once its send succeeds.
	next, sent := m, false
	if !m.Since.Equal(since) {
		next = RedHoldMarker{Since: since}
	}
	line := redHoldPushLine(since, rec.Reasons, now)
	var errs []error
	if textOwed {
		if err := r.Push(r.Name, line, ""); err != nil {
			errs = append(errs, fmt.Errorf("texting the red hold: %w", err))
		} else {
			next.TextedAt, sent = now, true
		}
	}
	if mailOwed {
		if err := r.Mail(line); err != nil {
			errs = append(errs, fmt.Errorf("emailing the red hold: %w", err))
		} else {
			next.MailedAt, sent = now, true
		}
	}
	if sent {
		if err := r.Write(next); err != nil {
			errs = append(errs, fmt.Errorf("recording the red-hold push: %w", err))
		}
	}
	return errors.Join(errs...)
}

// emailSender is sendEscalationEmail's shape, so a test can stand in for it.
type emailSender func(cfg *config.EscalationConfig, beadID, severity, description string) error

// redHoldEmail emails the red hold's line through send, or returns an error and
// sends nothing when the town names no email contact.
func redHoldEmail(cfg *config.EscalationConfig, send emailSender, line string) error {
	if cfg.Contacts.HumanEmail == "" || cfg.Contacts.SMTPHost == "" {
		return errors.New("no email contact configured (contacts.human_email, contacts.smtp_host): the red hold was not emailed")
	}
	return send(cfg, redHoldAckKey, "high", line)
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
		Mail:   func(line string) error { return redHoldEmail(cfg, sendEscalationEmail, line) },
	}.Run(now)
}
