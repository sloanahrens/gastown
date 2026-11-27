package done

import (
	"github.com/steveyegge/gastown/internal/outbox"
)

// deferredAfterFailedPush returns r as the result of a push that failed: status
// deferred, no submission, and an escalation whose text names the failure. An
// escalation the seat wrote earlier keeps its text in front of the failure;
// every other field is unchanged.
func deferredAfterFailedPush(r outbox.Result, pushErr error) outbox.Result {
	r.Status = outbox.StatusDeferred
	r.Submission = nil
	text := "the push of the branch failed"
	if pushErr != nil {
		text += ": " + pushErr.Error()
	}
	if r.Escalation != nil && r.Escalation.Text != "" {
		text = r.Escalation.Text + "\n\n" + text
	}
	r.Escalation = &outbox.Escalation{Text: text}
	return r
}
