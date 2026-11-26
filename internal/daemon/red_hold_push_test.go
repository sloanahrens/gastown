package daemon

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/stretchr/testify/require"
)

// TestRedHoldPushStepRegistered pins the table entry: listed once, runs
// pushRedHold, and not lifecycle, so the E-stop does not hold it.
func TestRedHoldPushStepRegistered(t *testing.T) {
	t.Parallel()
	var found []heartbeatStep
	for _, s := range heartbeatSteps {
		if s.name == "red-hold-push" {
			found = append(found, s)
		}
	}
	require.Len(t, found, 1, "the heartbeat lists red-hold-push once")
	require.False(t, found[0].lifecycle, "the red-hold push runs under the E-stop")
	require.Equal(t, reflect.ValueOf((*Daemon).pushRedHold).Pointer(), reflect.ValueOf(found[0].run).Pointer())
}

// redHoldPushLog runs one push pass against townRoot and returns its log.
func redHoldPushLog(townRoot string, push func(string, time.Time) error) string {
	logs := &lockedBuffer{}
	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(logs, "", 0),
	}
	d.runRedHoldPush(push)
	return logs.String()
}

// TestRedHoldPushStepLogsSendAndError: a stamped send, a failed push, and a push owing nothing.
func TestRedHoldPushStepLogsSendAndError(t *testing.T) {
	t.Parallel()
	texted := func(townRoot string, at time.Time) error {
		return notify.WriteRedHoldMarker(townRoot, notify.RedHoldMarker{TextedAt: at})
	}
	emailed := func(townRoot string, at time.Time) error {
		return notify.WriteRedHoldMarker(townRoot, notify.RedHoldMarker{MailedAt: at})
	}
	cases := []struct {
		name string
		push func(string, time.Time) error
		want string
	}{
		{"text", texted, "red hold push: texted the red hold\n"},
		{"email", emailed, "red hold push: emailed the red hold\n"},
		{"error", func(string, time.Time) error { return errors.New("no notify command") }, "red hold push: no notify command\n"},
		{"nothing owed", func(string, time.Time) error { return nil }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, redHoldPushLog(t.TempDir(), tc.push))
		})
	}
}

// TestRedHoldPushStepUnreadableMarker: an unreadable marker means unknown sends, not none.
func TestRedHoldPushStepUnreadableMarker(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	require.NoError(t, notify.WriteRedHoldMarker(townRoot, notify.RedHoldMarker{TextedAt: time.Now()}))
	entries, err := os.ReadDir(attention.Dir(townRoot))
	require.NoError(t, err)
	require.Len(t, entries, 1, "the marker is the only file in the attention dir")
	// A directory in the marker's place fails the read rather than missing it.
	marker := filepath.Join(attention.Dir(townRoot), entries[0].Name())
	require.NoError(t, os.Remove(marker))
	require.NoError(t, os.Mkdir(marker, 0o700))
	got := redHoldPushLog(townRoot, func(string, time.Time) error { return nil })
	require.Contains(t, got, "red hold push: marker unreadable, sends unknown")
	require.NotContains(t, got, "texted the red hold")
}
