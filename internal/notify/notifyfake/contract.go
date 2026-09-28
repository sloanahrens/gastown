package notifyfake

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/notify"
)

// MissingTarget is a nudge target that has no session in the town a
// contract implementation runs against. The fake must be told so with
// Missing(MissingTarget).
const MissingTarget = "gt-notify-contract-missing"

// RunNotifierContract checks the behavior every Notifier must share.
//
// A successful mail send or escalation needs a town with a Dolt server, which
// the integration tier's scratch town does not have, so the contract pins
// what holds without one: request validation, context handling, and a nudge
// to a session that does not exist.
func RunNotifierContract(t *testing.T, newImpl func(t *testing.T) notify.Notifier) {
	t.Run("invalid requests are refused before sending", func(t *testing.T) {
		n := newImpl(t)
		ctx := t.Context()
		cases := map[string]error{
			"mail without recipient":         n.MailSend(ctx, "  ", "subject", "body"),
			"nudge without target":           n.Nudge(ctx, "", "message"),
			"nudge with blank message":       n.Nudge(ctx, "mayor", " \n"),
			"escalation without description": n.Escalate(ctx, notify.Escalation{Severity: "high", Reason: "r"}),
			"multi-line description":         n.Escalate(ctx, notify.Escalation{Description: "line one\nline two"}),
			"unknown severity":               n.Escalate(ctx, notify.Escalation{Severity: "urgent", Description: "x"}),
			"clear without keys":             n.ClearEscalations(ctx, "cleared", "", "  "),
		}
		for name, err := range cases {
			if !errors.Is(err, notify.ErrInvalid) {
				t.Errorf("%s: err = %v, want notify.ErrInvalid", name, err)
			}
		}
	})

	t.Run("a done context is reported as the context's error", func(t *testing.T) {
		n := newImpl(t)
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

	t.Run("a nudge to a missing session fails", func(t *testing.T) {
		n := newImpl(t)
		err := n.Nudge(t.Context(), MissingTarget, "contract probe")
		if err == nil {
			t.Fatal("nudge to a missing session reported success")
		}
		if errors.Is(err, notify.ErrInvalid) {
			t.Fatalf("err = %v: a well-formed nudge must not read as invalid", err)
		}
		t.Logf("nudge to %s: %v", MissingTarget, err)
	})
}
