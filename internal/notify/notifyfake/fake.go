// Package notifyfake is a recording notify.Notifier for unit tests. Its
// behavior is pinned to the production notify.CLI by RunNotifierContract,
// which runs against the fake in the unit tier and against a real gt binary
// in a scratch town in the integration tier.
package notifyfake

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/steveyegge/gastown/internal/notify"
)

// Kind names which Notifier method a Call recorded.
type Kind string

// The Notifier methods.
const (
	KindMail     Kind = "mail"
	KindNudge    Kind = "nudge"
	KindEscalate Kind = "escalate"
	KindClear    Kind = "clear"
)

// Call is one send the Recorder accepted. Only the fields for its Kind are set.
type Call struct {
	Kind Kind

	// KindMail
	To      string
	Subject string
	Body    string
	Mail    notify.MailOptions

	// KindNudge
	Target  string
	Message string

	// KindEscalate
	Escalation notify.Escalation

	// KindClear
	Reason       string
	Fingerprints []string

	// Err is what the send returned: nil, or the scripted failure.
	Err error
}

// Recorder is an in-memory Notifier. It validates requests exactly as
// notify.CLI does, then records every send it attempts, including the ones
// it was scripted to fail. It is safe for concurrent use.
type Recorder struct {
	mu      sync.Mutex
	calls   []Call
	fail    map[Kind]error
	missing map[string]bool
}

// New returns a Recorder on which every valid send succeeds.
func New() *Recorder {
	return &Recorder{fail: map[Kind]error{}, missing: map[string]bool{}}
}

// Fail makes every later send of kind return err; nil restores success.
func (r *Recorder) Fail(kind Kind, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		delete(r.fail, kind)
		return
	}
	r.fail[kind] = err
}

// Missing marks nudge targets whose session does not exist: a nudge to one
// fails, as `gt nudge` fails for a session it cannot find.
func (r *Recorder) Missing(targets ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, target := range targets {
		r.missing[target] = true
	}
}

// Calls returns every send attempted so far, in order.
func (r *Recorder) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// Of returns the sends of one kind, in order.
func (r *Recorder) Of(kind Kind) []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Call
	for _, c := range r.calls {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

// Mails returns the mail sends.
func (r *Recorder) Mails() []Call { return r.Of(KindMail) }

// Nudges returns the nudges.
func (r *Recorder) Nudges() []Call { return r.Of(KindNudge) }

// Escalations returns the escalations.
func (r *Recorder) Escalations() []Call { return r.Of(KindEscalate) }

// Clears returns the escalation clears.
func (r *Recorder) Clears() []Call { return r.Of(KindClear) }

func (r *Recorder) record(ctx context.Context, c Call) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("gt %s: %w", c.Kind, ctx.Err())
	default:
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.fail[c.Kind] != nil:
		c.Err = r.fail[c.Kind]
	case c.Kind == KindNudge && r.missing[c.Target]:
		c.Err = fmt.Errorf("gt nudge: exit status 1 (Error: session %q not found)", c.Target)
	}
	r.calls = append(r.calls, c)
	return c.Err
}

// MailSend records a mail send.
func (r *Recorder) MailSend(ctx context.Context, to, subject, body string, opts ...notify.MailOption) error {
	if err := notify.ValidateMail(to); err != nil {
		return err
	}
	return r.record(ctx, Call{Kind: KindMail, To: to, Subject: subject, Body: body, Mail: notify.ApplyMailOptions(opts)})
}

// Nudge records a nudge.
func (r *Recorder) Nudge(ctx context.Context, target, message string) error {
	if err := notify.ValidateNudge(target, message); err != nil {
		return err
	}
	return r.record(ctx, Call{Kind: KindNudge, Target: target, Message: message})
}

// Escalate records an escalation.
func (r *Recorder) Escalate(ctx context.Context, e notify.Escalation) error {
	if err := e.Validate(); err != nil {
		return err
	}
	return r.record(ctx, Call{Kind: KindEscalate, Escalation: e})
}

// ClearEscalations records a clear. Blank keys are dropped, as notify.CLI
// drops them.
func (r *Recorder) ClearEscalations(ctx context.Context, reason string, fingerprints ...string) error {
	if err := notify.ValidateClear(fingerprints); err != nil {
		return err
	}
	var keys []string
	for _, fp := range fingerprints {
		if strings.TrimSpace(fp) != "" {
			keys = append(keys, fp)
		}
	}
	return r.record(ctx, Call{Kind: KindClear, Reason: reason, Fingerprints: keys})
}

var _ notify.Notifier = (*Recorder)(nil)
