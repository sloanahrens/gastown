package notify

import (
	"testing"
	"time"
)

// sameMarker compares markers by instant, so the location a time was built in does not count.
func sameMarker(a, b RedHoldMarker) bool {
	return a.Since.Equal(b.Since) && a.TextedAt.Equal(b.TextedAt) && a.MailedAt.Equal(b.MailedAt)
}

// A red hold owes its text at 30 minutes and its email at 3 hours, once each; a
// hold with a later Since is a new hold and owes both again (gt-s3rec.3).
func TestRedHoldPushesOwed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	cases := []struct {
		name       string
		since      time.Time
		acked      bool
		marker     RedHoldMarker
		want       RedHoldOwed
		wantMarker RedHoldMarker
	}{
		{"no hold: nothing owed, zero marker", time.Time{}, false, RedHoldMarker{}, RedHoldOwed{}, RedHoldMarker{}},
		{"under 30 minutes: nothing owed", ago(10 * time.Minute), false, RedHoldMarker{},
			RedHoldOwed{}, RedHoldMarker{Since: ago(10 * time.Minute)}},
		{"at 30 minutes: text owed", ago(30 * time.Minute), false, RedHoldMarker{},
			RedHoldOwed{Text: true}, RedHoldMarker{Since: ago(30 * time.Minute), TextedAt: now}},
		{"text already sent: not owed again", ago(45 * time.Minute), false,
			RedHoldMarker{Since: ago(45 * time.Minute), TextedAt: ago(15 * time.Minute)},
			RedHoldOwed{}, RedHoldMarker{Since: ago(45 * time.Minute), TextedAt: ago(15 * time.Minute)}},
		{"at 3 hours: email owed, text already sent", ago(3 * time.Hour), false,
			RedHoldMarker{Since: ago(3 * time.Hour), TextedAt: ago(150 * time.Minute)},
			RedHoldOwed{Mail: true}, RedHoldMarker{Since: ago(3 * time.Hour), TextedAt: ago(150 * time.Minute), MailedAt: now}},
		{"email already sent: not owed again", ago(4 * time.Hour), false,
			RedHoldMarker{Since: ago(4 * time.Hour), TextedAt: ago(210 * time.Minute), MailedAt: ago(time.Hour)},
			RedHoldOwed{}, RedHoldMarker{Since: ago(4 * time.Hour), TextedAt: ago(210 * time.Minute), MailedAt: ago(time.Hour)}},
		{"acknowledged: nothing owed, marker kept", ago(4 * time.Hour), true,
			RedHoldMarker{Since: ago(4 * time.Hour), TextedAt: ago(3 * time.Hour)},
			RedHoldOwed{}, RedHoldMarker{Since: ago(4 * time.Hour), TextedAt: ago(3 * time.Hour)}},
		{"later Since after an earlier hold: both owed again", ago(3 * time.Hour), false,
			RedHoldMarker{Since: ago(5 * time.Hour), TextedAt: ago(270 * time.Minute), MailedAt: ago(4 * time.Hour)},
			RedHoldOwed{Text: true, Mail: true}, RedHoldMarker{Since: ago(3 * time.Hour), TextedAt: now, MailedAt: now}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			owed, got := RedHoldPushesOwed(tc.since, tc.acked, tc.marker, now)
			if owed != tc.want {
				t.Errorf("RedHoldPushesOwed owed = %+v, want %+v", owed, tc.want)
			}
			if !sameMarker(got, tc.wantMarker) {
				t.Errorf("RedHoldPushesOwed marker = %+v, want %+v", got, tc.wantMarker)
			}
		})
	}
}
