package notifyfake

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/notify"
)

// MissingTarget is a nudge target that has no session in the town a
// contract implementation runs against. The fake must be told so with
// Missing(MissingTarget).
const MissingTarget = "gt-notify-contract-missing"

// Mail is one message as its recipient's mailbox holds it.
type Mail struct {
	From    string
	Subject string
	Body    string
}

// OpenEscalation is an escalation that has not been cleared, as it is filed
// under its alert key.
type OpenEscalation struct {
	Title       string
	Severity    string
	Reason      string
	Source      string
	Occurrences int
}

// Observer reports what a Notifier delivered. The contract pins every
// successful send by its effect, so a send that "succeeds" without reaching
// the town fails it.
type Observer interface {
	// Inbox returns the mail delivered to addr, oldest first.
	Inbox(addr string) []Mail
	// OpenEscalations returns the open escalations filed under an alert key
	// (notify.AlertKey).
	OpenEscalations(key string) []OpenEscalation
}

// Subject is one Notifier under test and the observer of the town it sends
// into.
type Subject struct {
	Notifier notify.Notifier
	Observer Observer
}

// RunNotifierContract checks the behavior every Notifier must share: request
// validation, context handling, a nudge to a session that does not exist, and
// the effect of every successful mail send, escalation, repeat firing and
// clear. Each case gets a fresh Subject and runs in parallel with the others.
func RunNotifierContract(t *testing.T, newImpl func(t *testing.T) Subject) {
	t.Run("invalid requests are refused before sending", func(t *testing.T) {
		t.Parallel()
		n := newImpl(t).Notifier
		ctx := t.Context()
		cases := map[string]error{
			"mail without recipient":         n.MailSend(ctx, "  ", "subject", "body"),
			"nudge without target":           n.Nudge(ctx, "", "message"),
			"nudge with blank message":       n.Nudge(ctx, "mayor", " \n"),
			"escalation without description": n.Escalate(ctx, notify.Escalation{Severity: "high", Reason: "r"}),
			"multi-line description":         n.Escalate(ctx, notify.Escalation{Description: "line one\nline two"}),
			"carriage return in description": n.Escalate(ctx, notify.Escalation{Description: "line one\rline two"}),
			"unknown severity":               n.Escalate(ctx, notify.Escalation{Severity: "urgent", Description: "x"}),
			"clear without keys":             n.ClearEscalations(ctx, "cleared", "", "  "),
		}
		for name, err := range cases {
			if !errors.Is(err, notify.ErrInvalid) {
				t.Errorf("%s: err = %v, want notify.ErrInvalid", name, err)
			}
		}
	})

	t.Run("a dash-leading mail subject is refused before sending", func(t *testing.T) {
		t.Parallel()
		// The subject becomes the message bead's title, and bd refuses a
		// title that starts with "-".
		s := newImpl(t)
		err := s.Notifier.MailSend(t.Context(), "mayor/", "-contract subject", "body")
		if !errors.Is(err, notify.ErrInvalid) {
			t.Errorf("err = %v, want notify.ErrInvalid", err)
		}
		if got := s.Observer.Inbox("mayor/"); len(got) != 0 {
			t.Errorf("mayor/ inbox = %+v, want nothing delivered", got)
		}
	})

	t.Run("a dash-leading escalation description is filed as its title", func(t *testing.T) {
		t.Parallel()
		s := newImpl(t)
		e := notify.Escalation{Severity: "low", Description: "-contract alarm", Fingerprint: "contract-dash"}
		if err := s.Notifier.Escalate(t.Context(), e); err != nil {
			t.Fatalf("Escalate: %v", err)
		}
		if got := s.Observer.OpenEscalations("contract-dash"); len(got) != 1 || got[0].Title != "-contract alarm" {
			t.Fatalf("open = %+v, want one titled %q", got, e.Description)
		}
	})

	t.Run("a done context is reported as the context's error", func(t *testing.T) {
		t.Parallel()
		n := newImpl(t).Notifier
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		cases := map[string]error{
			"mail":     n.MailSend(ctx, "mayor/", "subject", "body"),
			"nudge":    n.Nudge(ctx, "mayor", "message"),
			"escalate": n.Escalate(ctx, notify.Escalation{Severity: "low", Description: "contract"}),
			"clear":    n.ClearEscalations(ctx, "cleared", "contract-key"),
		}
		for name, err := range cases {
			if !errors.Is(err, context.Canceled) {
				t.Errorf("%s: err = %v, want context.Canceled", name, err)
			}
		}
	})

	t.Run("a nudge to a missing session fails naming the session", func(t *testing.T) {
		t.Parallel()
		n := newImpl(t).Notifier
		err := n.Nudge(t.Context(), MissingTarget, "contract probe")
		if err == nil {
			t.Fatal("nudge to a missing session reported success")
		}
		if errors.Is(err, notify.ErrInvalid) {
			t.Fatalf("err = %v: a well-formed nudge must not read as invalid", err)
		}
		// The session lookup is the only thing that can have failed: a
		// usage or flag error would mean gt never got that far.
		if want := `session "` + MissingTarget + `" not found`; !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to contain %q", err, want)
		}
	})

	t.Run("mail is delivered to the recipient's inbox", func(t *testing.T) {
		t.Parallel()
		s := newImpl(t)
		ctx := t.Context()
		// A dash-leading body must arrive as the body, not as a flag.
		if err := s.Notifier.MailSend(ctx, "mayor/", "contract subject", "-contract body"); err != nil {
			t.Fatalf("MailSend: %v", err)
		}
		got := s.Observer.Inbox("mayor/")
		if len(got) != 1 || got[0].Subject != "contract subject" || got[0].Body != "-contract body" {
			t.Fatalf("mayor/ inbox = %+v, want the one message just sent", got)
		}
	})

	t.Run("mail options set the sender", func(t *testing.T) {
		t.Parallel()
		s := newImpl(t)
		err := s.Notifier.MailSend(t.Context(), "mayor/", "relayed", "relayed body",
			notify.From("convoy/contract"), notify.NoNotify())
		if err != nil {
			t.Fatalf("MailSend: %v", err)
		}
		got := s.Observer.Inbox("mayor/")
		if len(got) != 1 || got[0].From != "convoy/contract" || got[0].Subject != "relayed" {
			t.Fatalf("mayor/ inbox = %+v, want one message from convoy/contract", got)
		}
	})

	t.Run("an escalation is filed once and its repeat is counted", func(t *testing.T) {
		t.Parallel()
		s := newImpl(t)
		ctx := t.Context()
		e := notify.Escalation{
			Severity:    "low",
			Description: "contract alarm",
			Reason:      "contract reason",
			Source:      "contract-source",
			Fingerprint: "contract-key",
		}
		if err := s.Notifier.Escalate(ctx, e); err != nil {
			t.Fatalf("Escalate: %v", err)
		}
		want := OpenEscalation{Title: "contract alarm", Severity: "low", Reason: "contract reason", Source: "contract-source", Occurrences: 1}
		if got := s.Observer.OpenEscalations("contract-key"); len(got) != 1 || got[0] != want {
			t.Fatalf("after the first firing: open = %+v, want [%+v]", got, want)
		}

		e.Severity, e.Reason = "high", "worse now"
		if err := s.Notifier.Escalate(ctx, e); err != nil {
			t.Fatalf("Escalate (repeat): %v", err)
		}
		want.Severity, want.Reason, want.Occurrences = "high", "worse now", 2
		if got := s.Observer.OpenEscalations("contract-key"); len(got) != 1 || got[0] != want {
			t.Fatalf("after the repeat: open = %+v, want [%+v]", got, want)
		}
	})

	t.Run("an escalation without a fingerprint is keyed by source and description", func(t *testing.T) {
		t.Parallel()
		s := newImpl(t)
		ctx := t.Context()
		e := notify.Escalation{Description: "derived alarm", Source: "contract-source"}
		for range 2 {
			if err := s.Notifier.Escalate(ctx, e); err != nil {
				t.Fatalf("Escalate: %v", err)
			}
		}
		key := notify.AlertKey("", e.Source, e.Description)
		want := OpenEscalation{Title: "derived alarm", Severity: "medium", Source: "contract-source", Occurrences: 2}
		if got := s.Observer.OpenEscalations(key); len(got) != 1 || got[0] != want {
			t.Fatalf("open under %q = %+v, want [%+v]", key, got, want)
		}
	})

	t.Run("clearing closes only the named alert keys", func(t *testing.T) {
		t.Parallel()
		s := newImpl(t)
		ctx := t.Context()
		for _, key := range []string{"contract-clear-a", "contract-clear-b", "contract-keep"} {
			if err := s.Notifier.Escalate(ctx, notify.Escalation{Severity: "low", Description: "alarm " + key, Fingerprint: key}); err != nil {
				t.Fatalf("Escalate %s: %v", key, err)
			}
		}
		if err := s.Notifier.ClearEscalations(ctx, "condition cleared", "contract-clear-a", "contract-clear-b"); err != nil {
			t.Fatalf("ClearEscalations: %v", err)
		}
		for _, key := range []string{"contract-clear-a", "contract-clear-b"} {
			if got := s.Observer.OpenEscalations(key); len(got) != 0 {
				t.Errorf("%s still open after clear: %+v", key, got)
			}
		}
		if got := s.Observer.OpenEscalations("contract-keep"); len(got) != 1 {
			t.Errorf("contract-keep open = %+v, want it untouched by the clear", got)
		}
	})

	t.Run("clearing a key with nothing open succeeds", func(t *testing.T) {
		t.Parallel()
		n := newImpl(t).Notifier
		if err := n.ClearEscalations(t.Context(), "", "contract-never-raised"); err != nil {
			t.Fatalf("ClearEscalations: %v", err)
		}
	})
}
