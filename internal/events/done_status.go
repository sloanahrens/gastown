package events

// doneEventStatus reads a done event's exit status. known is false when the
// payload carries a status that is present but is not a string or not one of
// the DoneStatus constants: an unreadable value, which no reader may count as a
// submission or rule out one (gt-qqmae).
func doneEventStatus(ev Event) (status string, known bool) {
	raw, ok := ev.Payload["status"]
	if !ok {
		return "", true
	}
	status, ok = raw.(string)
	if !ok {
		return "", false
	}
	switch status {
	case "", DoneStatusCompleted, DoneStatusEscalated, DoneStatusDeferred:
		return status, true
	default:
		return "", false
	}
}

// DoneEventIsSubmission reports whether a done event submitted work for
// landing: its status is absent or empty (an event written before gt done
// recorded the exit type, or the host's repair of a sandbox submission), or the
// completed exit. The escalated and deferred exits submit nothing, and a status
// that cannot be read is not read as a submission either (gt-qqmae).
func DoneEventIsSubmission(ev Event) bool {
	status, known := doneEventStatus(ev)
	return known && (status == "" || status == DoneStatusCompleted)
}

// DoneEventStatusKnown reports whether a done event's status could be read: true
// for an absent status, an empty value, or one of the DoneStatus constants, and
// false for a value this package does not know or one that is not a string. A
// reader deciding whether a duplicate can be ruled out refuses when it is false,
// because such an event may or may not be a submission (gt-qqmae).
func DoneEventStatusKnown(ev Event) bool {
	_, known := doneEventStatus(ev)
	return known
}
