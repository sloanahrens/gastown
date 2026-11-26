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
	holdErr   error
	acksErr   error
	markerErr error
	writeErr  error
	pushErr   error
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
		{"text and email owed: only the text is sent", "imessage", redHoldAt(ago(3*time.Hour), "sweep-red"), attention.Acks{}, RedHoldMarker{},
			[]string{"Red hold unacknowledged for 3h: hold:red sweep-red"}, RedHoldMarker{Since: ago(3 * time.Hour), TextedAt: now}},
		{"the oldest reason sets the age, a zero Since is skipped", "imessage", dispatch.RedHoldRecord{Reasons: []dispatch.RedHoldReason{
			{Kind: "sweep-red", Since: ago(10 * time.Minute)}, {Kind: "main-red", Since: ago(3 * time.Hour)}, {Kind: "behind-main"}}},
			attention.Acks{}, RedHoldMarker{},
			[]string{"Red hold unacknowledged for 3h: hold:red sweep-red, main-red, behind-main"},
			RedHoldMarker{Since: ago(3 * time.Hour), TextedAt: now}},
		{"acknowledged: nothing owed", "imessage", redHoldAt(ago(3*time.Hour), "sweep-red"), acked, RedHoldMarker{}, nil, RedHoldMarker{}},
		{"a different ack does not acknowledge it", "imessage", redHoldAt(ago(45*time.Minute), "sweep-red"), attention.Acks{Acks: []attention.Ack{{Key: "hold:landing"}}}, RedHoldMarker{},
			[]string{"Red hold unacknowledged for 45 min: hold:red sweep-red"}, RedHoldMarker{Since: ago(45 * time.Minute), TextedAt: now}},
		{"no notify command reads and sends nothing", "", redHoldAt(ago(time.Hour), "sweep-red"), attention.Acks{}, RedHoldMarker{}, nil, RedHoldMarker{}},
		{"an owed email alone is not sent or recorded", "imessage", redHoldAt(ago(3*time.Hour), "sweep-red"), attention.Acks{}, sent, nil, sent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &redHoldPushFake{hold: tc.hold, acks: tc.acks, marker: tc.marker}
			if tc.command == "" {
				f.holdErr = errors.New("read the red hold with no command configured")
			}
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
