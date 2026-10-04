// Package notify is the town's one port for telling an agent something: mail,
// nudges and escalations. Packages that used to run `gt mail send`, `gt nudge`
// and `gt escalate` themselves take a Notifier instead, so their tests record
// what was sent (notifyfake) rather than stubbing a gt binary on PATH.
//
// The production implementation, CLI, still runs the gt binary for mail and
// escalations: `gt mail send` and `gt escalate` derive the sender from the
// process that runs them, and running them as a subprocess keeps those
// semantics and the per-call timeout isolation the daemon relies on, while
// the escalation logic itself lives here (escalate.go) instead of in
// internal/cmd. Nudges go in-process through TownNudger, which calls gt
// nudge's own delivery (internal/nudge/deliver): wait-idle delivery, queue
// fallback, DND, role-shortcut resolution and sender attribution, with the
// context's deadline in place of killing the process.
package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

// Notifier sends mail, nudges and escalations on behalf of its caller.
//
// Every method validates its input before sending anything and reports a
// malformed request as an error wrapping ErrInvalid. A context that is done
// before or during a send yields an error wrapping the context's error.
type Notifier interface {
	// MailSend sends one mail message to an address (e.g. "overseer",
	// "gastown/witness").
	MailSend(ctx context.Context, to, subject, body string, opts ...MailOption) error
	// Nudge delivers message to a live agent session (e.g. "gastown/witness",
	// "gastown/refinery", "gastown/furiosa").
	Nudge(ctx context.Context, target, message string) error
	// Escalate raises an escalation, or records a repeat of the open one
	// with the same alert key.
	Escalate(ctx context.Context, e Escalation) error
	// ClearEscalations closes the open escalations raised under the given
	// alert keys, because the condition behind them no longer holds.
	// Clearing a key that matches nothing is success.
	ClearEscalations(ctx context.Context, reason string, fingerprints ...string) error
}

// ErrInvalid is wrapped by every error a Notifier returns for a malformed
// request, before anything is sent.
var ErrInvalid = errors.New("invalid notification")

// Escalation is one firing of an alert.
type Escalation struct {
	// Severity is critical, high, medium or low (any case). Empty means
	// medium, gt escalate's default.
	Severity string
	// Description is the one-line headline. It becomes the escalation bead's
	// title, so it must be non-blank and must not contain a newline.
	Description string
	// Reason is the detail: free text, may be multi-line.
	Reason string
	// Source names the producer class (e.g. "witness-patrol:dialog-blocked").
	Source string
	// Fingerprint is the alert key; a repeat firing under the same key is
	// recorded on the open escalation instead of creating another. Empty
	// derives the key from Source and Description.
	Fingerprint string
}

// Validate reports whether e can be sent.
func (e Escalation) Validate() error {
	if strings.TrimSpace(e.Description) == "" {
		return fmt.Errorf("%w: escalation needs a description", ErrInvalid)
	}
	if strings.ContainsAny(e.Description, "\r\n") {
		return fmt.Errorf("%w: escalation description must be one line (put detail in Reason)", ErrInvalid)
	}
	if e.Severity != "" && !config.IsValidSeverity(strings.ToLower(e.Severity)) {
		return fmt.Errorf("%w: invalid severity %q: must be critical, high, medium, or low", ErrInvalid, e.Severity)
	}
	return nil
}

// MailOptions are the optional parts of a mail send.
type MailOptions struct {
	// From overrides the sender address, which is otherwise detected from
	// the sending process (relay/bridge use).
	From string
	// NoNotify suppresses the recipient's runtime notification; the message
	// is still delivered to the mailbox.
	NoNotify bool
}

// MailOption sets one MailOptions field.
type MailOption func(*MailOptions)

// From sets the sender address.
func From(addr string) MailOption { return func(o *MailOptions) { o.From = addr } }

// NoNotify suppresses the recipient's runtime notification.
func NoNotify() MailOption { return func(o *MailOptions) { o.NoNotify = true } }

// ApplyMailOptions folds opts into one MailOptions.
func ApplyMailOptions(opts []MailOption) MailOptions {
	var o MailOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// ValidateMail reports whether a mail send to the given address with the
// given subject can be made. The subject becomes the message bead's title,
// and bd refuses a title that starts with "-", so such a send is refused
// here rather than failing inside gt.
func ValidateMail(to, subject string) error {
	if strings.TrimSpace(to) == "" {
		return fmt.Errorf("%w: mail needs a recipient", ErrInvalid)
	}
	if strings.HasPrefix(subject, "-") {
		return fmt.Errorf("%w: mail subject %q starts with \"-\", which bd refuses as a title", ErrInvalid, subject)
	}
	return nil
}

// ValidateNudge reports whether a nudge can be made. An empty message is
// refused rather than delivered as a bare "[from x]" line.
func ValidateNudge(target, message string) error {
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("%w: nudge needs a target", ErrInvalid)
	}
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("%w: refusing to nudge %s with an empty message", ErrInvalid, target)
	}
	return nil
}

// ValidateClear reports whether a clear names at least one alert key.
func ValidateClear(fingerprints []string) error {
	for _, fp := range fingerprints {
		if strings.TrimSpace(fp) != "" {
			return nil
		}
	}
	return fmt.Errorf("%w: clear needs at least one fingerprint", ErrInvalid)
}

// contextErr returns ctx's error once ctx is done, and nil before.
func contextErr(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
