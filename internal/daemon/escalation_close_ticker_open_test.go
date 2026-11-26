package daemon

import (
	"log"
	"strings"
	"testing"
)

// TestOpenEscalationCloseTicker: the daemon opens the escalation-close ticker
// through one method that logs the interval it started and hands back the
// ticker's channel and stop, so the run loop keeps both and a seat answered
// without a session resumes inside a minute (gt-3nygtn). Rows are the log line,
// the channel and the stop.
func TestOpenEscalationCloseTicker(t *testing.T) {
	t.Parallel()

	open := func(t *testing.T) (*Daemon, *strings.Builder) {
		t.Helper()
		var logs strings.Builder
		return &Daemon{logger: log.New(&logs, "", 0)}, &logs
	}

	t.Run("the log line names the one-minute interval", func(t *testing.T) {
		t.Parallel()
		d, logs := open(t)
		_, stop := d.openEscalationCloseTicker()
		stop()
		const want = "Escalation close ticker started (interval 1m0s)\n"
		if got := logs.String(); got != want {
			t.Fatalf("log = %q, want %q", got, want)
		}
	})

	t.Run("the channel is non-nil and stop is safe to call twice", func(t *testing.T) {
		t.Parallel()
		d, _ := open(t)
		ticks, stop := d.openEscalationCloseTicker()
		if ticks == nil {
			t.Fatal("openEscalationCloseTicker returned a nil channel")
		}
		if stop == nil {
			t.Fatal("openEscalationCloseTicker returned a nil stop func")
		}
		stop()
		stop() // a second stop must not panic
	})
}
