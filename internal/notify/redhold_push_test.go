package notify

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dispatch"
)

// redHoldPushFake is a pass over in-memory state, so a test touches no file or process.
type redHoldPushFake struct {
	hold      dispatch.RedHoldRecord
	acks      attention.Acks
	marker    RedHoldMarker
	texts     []string
	mails     []string
	holdErr   error
	acksErr   error
	markerErr error
	writeErr  error
	pushErr   error
	mailErr   error
}

func (f *redHoldPushFake) run(name string) RedHoldPushRun {
	return RedHoldPushRun{
		Hold:   func() (dispatch.RedHoldRecord, error) { return f.hold, f.holdErr },
		Acks:   func() (attention.Acks, error) { return f.acks, f.acksErr },
		Marker: func() (RedHoldMarker, error) { return f.marker, f.markerErr },
		Write: func(m RedHoldMarker) error {
			if f.writeErr == nil {
				f.marker = m
			}
			return f.writeErr
		},
		Push: func(_, title, _ string) error {
			if f.pushErr == nil {
				f.texts = append(f.texts, title)
			}
			return f.pushErr
		},
		Name: name,
		Mail: func(line string) error {
			if f.mailErr == nil {
				f.mails = append(f.mails, line)
			}
			return f.mailErr
		},
	}
}

// redHoldAt is a published red hold whose every reason first stood at since.
func redHoldAt(since time.Time, kinds ...string) dispatch.RedHoldRecord {
	var rec dispatch.RedHoldRecord
	for _, kind := range kinds {
		rec.Reasons = append(rec.Reasons, dispatch.RedHoldReason{Kind: kind, Since: since})
	}
	return rec
}

// An unacknowledged red hold texts once at 30 minutes with a line naming the
// dashboard item and the hold's age, and the marker records it.
func TestRedHoldPushText(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	acked := attention.Acks{Acks: []attention.Ack{{Key: "hold:red", At: ago(time.Hour)}}}
	sent := RedHoldMarker{Since: ago(3 * time.Hour), TextedAt: ago(150 * time.Minute)}
	cases := []struct {
		name       string
		command    string
		hold       dispatch.RedHoldRecord
		acks       attention.Acks
		marker     RedHoldMarker
		want       []string
		wantMarker RedHoldMarker
	}{
		{"nothing owed under 30 minutes", "imessage", redHoldAt(ago(10*time.Minute), "sweep-red"), attention.Acks{}, RedHoldMarker{}, nil, RedHoldMarker{}},
		{"no red hold reads nothing further", "imessage", dispatch.RedHoldRecord{}, attention.Acks{}, RedHoldMarker{}, nil, RedHoldMarker{}},
		{"text owed: sent with the age and item", "imessage", redHoldAt(ago(45*time.Minute), "sweep-red", "main-red"), attention.Acks{}, RedHoldMarker{},
			[]string{"Red hold unacknowledged for 45 min: hold:red sweep-red, main-red"}, RedHoldMarker{Since: ago(45 * time.Minute), TextedAt: now}},
		{"text and email owed: both sent", "imessage", redHoldAt(ago(3*time.Hour), "sweep-red"), attention.Acks{}, RedHoldMarker{},
			[]string{"Red hold unacknowledged for 3h: hold:red sweep-red"}, RedHoldMarker{Since: ago(3 * time.Hour), TextedAt: now, MailedAt: now}},
		{"the oldest reason sets the age, a zero Since is skipped", "imessage", dispatch.RedHoldRecord{Reasons: []dispatch.RedHoldReason{
			{Kind: "sweep-red", Since: ago(10 * time.Minute)}, {Kind: "main-red", Since: ago(3 * time.Hour)}, {Kind: "behind-main"}}},
			attention.Acks{}, RedHoldMarker{},
			[]string{"Red hold unacknowledged for 3h: hold:red sweep-red, main-red, behind-main"},
			RedHoldMarker{Since: ago(3 * time.Hour), TextedAt: now, MailedAt: now}},
		{"acknowledged: nothing owed", "imessage", redHoldAt(ago(3*time.Hour), "sweep-red"), acked, RedHoldMarker{}, nil, RedHoldMarker{}},
		{"a different ack does not acknowledge it", "imessage", redHoldAt(ago(45*time.Minute), "sweep-red"), attention.Acks{Acks: []attention.Ack{{Key: "hold:landing"}}}, RedHoldMarker{},
			[]string{"Red hold unacknowledged for 45 min: hold:red sweep-red"}, RedHoldMarker{Since: ago(45 * time.Minute), TextedAt: now}},
		{"no notify command texts nothing and records nothing", "", redHoldAt(ago(time.Hour), "sweep-red"), attention.Acks{}, RedHoldMarker{}, nil, RedHoldMarker{}},
		{"an owed email alone is sent and recorded", "imessage", redHoldAt(ago(3*time.Hour), "sweep-red"), attention.Acks{}, sent, nil, RedHoldMarker{Since: ago(3 * time.Hour), TextedAt: ago(150 * time.Minute), MailedAt: now}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &redHoldPushFake{hold: tc.hold, acks: tc.acks, marker: tc.marker}
			if len(tc.hold.Reasons) == 0 {
				f.acksErr = errors.New("read the acks with no red hold")
			}
			if err := f.run(tc.command).Run(now); err != nil {
				t.Fatalf("Run = %v", err)
			}
			if strings.Join(f.texts, "\n") != strings.Join(tc.want, "\n") || !sameMarker(f.marker, tc.wantMarker) {
				t.Errorf("texts = %q, marker = %+v; want %q, %+v", f.texts, f.marker, tc.want, tc.wantMarker)
			}
			if err := f.run(tc.command).Run(now); err != nil || len(f.texts) != len(tc.want) {
				t.Errorf("second Run = %v, texts = %q; want nothing sent again", err, f.texts)
			}
		})
	}
}

// A failed send leaves the marker for the next pass, and a failed marker write or
// an unreadable input is an error, never a pass that looks clean.
func TestRedHoldPushFailures(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	since := now.Add(-45 * time.Minute)
	unchanged := RedHoldMarker{Since: since}
	cases := []struct {
		name string
		fake redHoldPushFake
		want string
	}{
		{"unreadable red hold", redHoldPushFake{holdErr: errors.New("garbage")}, "red hold"},
		{"unreadable acks", redHoldPushFake{acksErr: errors.New("garbage")}, "acks"},
		{"unreadable marker", redHoldPushFake{markerErr: errors.New("garbage")}, "marker"},
		{"failed send leaves the marker", redHoldPushFake{pushErr: errors.New("timed out")}, "timed out"},
		{"failed marker write", redHoldPushFake{writeErr: errors.New("disk full")}, "disk full"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.fake.hold, tc.fake.marker = redHoldAt(since, "sweep-red"), unchanged
			err := tc.fake.run("imessage").Run(now)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run = %v, want an error naming %q", err, tc.want)
			}
			if !sameMarker(tc.fake.marker, unchanged) {
				t.Errorf("marker = %+v, want it unchanged", tc.fake.marker)
			}
		})
	}
}

// An owed email is sent once at three hours with the text's line, and the marker
// records MailedAt; a later pass sends nothing more.
func TestRedHoldPushMail(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	since := now.Add(-3 * time.Hour)
	const line = "Red hold unacknowledged for 3h: hold:red sweep-red"
	f := &redHoldPushFake{hold: redHoldAt(since, "sweep-red"), marker: RedHoldMarker{Since: since, TextedAt: now.Add(-150 * time.Minute)}}
	if err := f.run("imessage").Run(now); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if strings.Join(f.mails, "\n") != line || !sameMarker(f.marker, RedHoldMarker{Since: since, TextedAt: now.Add(-150 * time.Minute), MailedAt: now}) {
		t.Errorf("mails = %q, marker = %+v; want the line once and MailedAt recorded", f.mails, f.marker)
	}
	if err := f.run("imessage").Run(now); err != nil || len(f.mails) != 1 {
		t.Errorf("second Run = %v, mails = %q; want no email sent again", err, f.mails)
	}
}

// The email goes out as the hold's escalation: its item key at high severity, with
// the text's line as the description.
func TestRedHoldPushMailSendsTheLine(t *testing.T) {
	t.Parallel()
	cfg := &config.EscalationConfig{}
	cfg.Contacts.HumanEmail, cfg.Contacts.SMTPHost = "operator@example.test", "smtp.example.test"
	const line = "Red hold unacknowledged for 3h: hold:red sweep-red"
	var bead, severity, description string
	send := func(_ *config.EscalationConfig, b, s, d string) error {
		bead, severity, description = b, s, d
		return nil
	}
	err := redHoldEmail(cfg, send, line)
	if err != nil || bead != "hold:red" || severity != "high" || description != line {
		t.Errorf("redHoldEmail = %v, sent %q %q %q; want hold:red, high, %q", err, bead, severity, description, line)
	}
}

// A failed email leaves MailedAt unset and the text is still recorded; a failed
// text does not hold back the email; a town with no email contact returns an error
// that names it, sends nothing, and still records the text.
func TestRedHoldPushMailFailures(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	since := now.Add(-3 * time.Hour)
	const line = "Red hold unacknowledged for 3h: hold:red sweep-red"
	cases := []struct {
		name       string
		fake       redHoldPushFake
		noContact  bool
		want       string
		wantTexts  []string
		wantMails  []string
		wantMarker RedHoldMarker
	}{
		{"failed email leaves MailedAt unset, the text is recorded", redHoldPushFake{mailErr: errors.New("smtp refused")}, false,
			"smtp refused", []string{line}, nil, RedHoldMarker{Since: since, TextedAt: now}},
		{"a failed text does not hold back the email", redHoldPushFake{pushErr: errors.New("timed out")}, false,
			"timed out", nil, []string{line}, RedHoldMarker{Since: since, MailedAt: now}},
		{"no email contact: error naming it, nothing sent, text recorded", redHoldPushFake{}, true,
			"no email contact", []string{line}, nil, RedHoldMarker{Since: since, TextedAt: now}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.fake.hold, tc.fake.marker = redHoldAt(since, "sweep-red"), RedHoldMarker{Since: since}
			run := tc.fake.run("imessage")
			if tc.noContact {
				run.Mail = func(line string) error {
					return redHoldEmail(&config.EscalationConfig{}, func(*config.EscalationConfig, string, string, string) error {
						t.Error("sent an email with no email contact")
						return nil
					}, line)
				}
			}
			err := run.Run(now)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run = %v, want an error naming %q", err, tc.want)
			}
			if strings.Join(tc.fake.texts, "\n") != strings.Join(tc.wantTexts, "\n") || strings.Join(tc.fake.mails, "\n") != strings.Join(tc.wantMails, "\n") {
				t.Errorf("texts = %q, mails = %q; want %q, %q", tc.fake.texts, tc.fake.mails, tc.wantTexts, tc.wantMails)
			}
			if !sameMarker(tc.fake.marker, tc.wantMarker) {
				t.Errorf("marker = %+v, want %+v", tc.fake.marker, tc.wantMarker)
			}
		})
	}
}

// A town with no notify command and no email contact has nowhere to send.
func TestRedHoldPushDoesNothingWithoutAChannel(t *testing.T) {
	t.Parallel()
	if err := PushRedHold(t.TempDir(), time.Now()); err != nil {
		t.Fatalf("PushRedHold = %v, want nil", err)
	}
	noSend := RedHoldPushRun{Name: "imessage", Hold: func() (dispatch.RedHoldRecord, error) {
		t.Error("read the red hold with no sender")
		return dispatch.RedHoldRecord{}, nil
	}}
	if err := noSend.Run(time.Now()); err != nil {
		t.Fatalf("Run with no sender = %v, want nil", err)
	}
}

// A town whose escalation config cannot be parsed is an error, never a pass read
// as no hold.
func TestRedHoldPushRefusesABadConfig(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := config.EscalationConfigPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PushRedHold(root, time.Now()); err == nil || !strings.Contains(err.Error(), "escalation config") {
		t.Fatalf("PushRedHold = %v, want an escalation config error", err)
	}
}
