package daemon

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// pingTiming is a container lifecycle whose SQL ping advances the fake clock by
// the next step, so a tick's ping can be timed.
type pingTiming struct {
	fakeContainerLifecycle
	clock *clockwork.FakeClock
	steps []pingStep
	i     int
}

// pingStep is one SQL ping answer: how long it took, and its error, nil when
// the Dolt answered.
type pingStep struct {
	d   time.Duration
	err error
}

func (f *pingTiming) IsRunning(string) (bool, int, error) {
	s := f.steps[f.i]
	f.i++
	f.clock.Advance(s.d)
	return s.err == nil, 0, s.err
}

// pingLatencyManager is a container-mode manager that logs every line it
// writes; its SQL ping answers are the given steps.
func pingLatencyManager(t *testing.T, steps ...pingStep) (*DoltServerManager, *strings.Builder) {
	t.Helper()
	townRoot := t.TempDir()
	writeExternalContainerTown(t, townRoot, 3307, "gt-dolt")
	lifecycle := &pingTiming{
		fakeContainerLifecycle: fakeContainerLifecycle{running: true},
		clock:                  clockwork.NewFakeClockAt(time.Unix(0, 0)),
		steps:                  steps,
	}
	m, _ := containerManager(t, townRoot, lifecycle, nil)
	m.clock = lifecycle.clock
	// No database is listed, so a healthy ping's write probe stops here
	// instead of reaching the dolt client.
	m.listDatabasesFn = func() ([]string, error) { return nil, errors.New("no databases") }
	logs := &strings.Builder{}
	m.logger = func(format string, args ...interface{}) { fmt.Fprintf(logs, format+"\n", args...) }
	return m, logs
}

// TestDoltPingLatencyLogsAFailureWithItsDuration: a failed SQL ping logs how
// long it took along with its error (gt-lno4d3).
func TestDoltPingLatencyLogsAFailureWithItsDuration(t *testing.T) {
	t.Parallel()
	pingErr := errors.New("dial tcp 127.0.0.1:3307: i/o timeout")
	m, logs := pingLatencyManager(t, pingStep{d: 3 * time.Second, err: pingErr})
	if err := m.EnsureRunning(); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	want := "did not answer its SQL ping after 3s: " + pingErr.Error()
	if !strings.Contains(logs.String(), want) {
		t.Errorf("logs = %q, want a line holding %q", logs.String(), want)
	}
}

// TestDoltPingLatencySlowOnlyAtTheThreshold: a successful ping at or above
// doltPingSlowAfter logs one slow line, a faster one logs nothing (gt-lno4d3).
func TestDoltPingLatencySlowOnlyAtTheThreshold(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		d        time.Duration
		wantSlow bool
	}{
		{"a 600 ms answer is slow", 600 * time.Millisecond, true},
		{"a 100 ms answer is not", 100 * time.Millisecond, false},
		{"an answer one tick under doltPingSlowAfter is not slow", doltPingSlowAfter - time.Millisecond, false},
		{"an answer at doltPingSlowAfter is slow", doltPingSlowAfter, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m, logs := pingLatencyManager(t, pingStep{d: tt.d})
			if err := m.EnsureRunning(); err != nil {
				t.Fatalf("EnsureRunning: %v", err)
			}
			want := "answered its SQL ping slowly: " + tt.d.String()
			if got := strings.Contains(logs.String(), want); got != tt.wantSlow {
				t.Errorf("logs = %q, slow line present = %v, want %v", logs.String(), got, tt.wantSlow)
			}
			if !tt.wantSlow && logs.Len() != 0 {
				t.Errorf("a %s ping logged %q, want nothing", tt.d, logs.String())
			}
		})
	}
}
