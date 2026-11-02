package events

// DoneEventIsSubmission reports whether a done event submitted work for
// landing: its status is absent (an event written before gt done recorded the
// exit type, or the host's repair in internal/land) or the completed exit,
// while an escalated, deferred or unreadable status is not a submission
// (gt-qqmae).
func DoneEventIsSubmission(ev Event) bool {
	raw, ok := ev.Payload["status"]
	if !ok {
		return true
	}
	status, ok := raw.(string)
	if !ok {
		return false
	}
	switch status {
	case "", DoneStatusCompleted:
		return true
	default:
		return false
	}
}
