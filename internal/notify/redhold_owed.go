package notify

import "time"

// How long a red hold may stand unacknowledged before its text, then its email, is owed.
const (
	RedHoldTextAfter = 30 * time.Minute
	RedHoldMailAfter = 3 * time.Hour
)

// RedHoldMarker records what was pushed for one red hold. Since is when the hold
// first stood; TextedAt and MailedAt are zero until that push is made.
type RedHoldMarker struct {
	Since    time.Time
	TextedAt time.Time
	MailedAt time.Time
}

// RedHoldOwed says which pushes are owed for a red hold right now.
type RedHoldOwed struct {
	Text bool
	Mail bool
}

// RedHoldPushesOwed says which red-hold pushes are owed, given when the hold first
// stood, whether it is acknowledged, the marker of what was pushed, and now. An
// acknowledged hold owes nothing. A marker whose Since differs is an earlier hold,
// so the new one starts empty. The returned marker stamps each owed push with now:
// the caller records it only after that push is made, never when it fails.
// Its callers are the red-hold marker file and PushRedHold, which sends the pushes.
func RedHoldPushesOwed(since time.Time, acked bool, m RedHoldMarker, now time.Time) (RedHoldOwed, RedHoldMarker) {
	if since.IsZero() {
		return RedHoldOwed{}, RedHoldMarker{}
	}
	if !m.Since.Equal(since) {
		m = RedHoldMarker{Since: since}
	}
	if acked {
		return RedHoldOwed{}, m
	}
	age := now.Sub(since)
	owed := RedHoldOwed{
		Text: m.TextedAt.IsZero() && age >= RedHoldTextAfter,
		Mail: m.MailedAt.IsZero() && age >= RedHoldMailAfter,
	}
	if owed.Text {
		m.TextedAt = now
	}
	if owed.Mail {
		m.MailedAt = now
	}
	return owed, m
}
