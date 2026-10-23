package cmd

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/nudge"
)

func TestShouldSkipDrainUntilIdle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		hasPromptDetection bool
		waitErr            error
		want               bool
	}{
		{"prompt aware idle", true, nil, false},
		{"prompt aware busy", true, errors.New("timeout"), true},
		{"no prompt detection busy", false, errors.New("timeout"), false},
		{"no prompt detection idle", false, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSkipDrainUntilIdle(tt.hasPromptDetection, tt.waitErr); got != tt.want {
				t.Errorf("shouldSkipDrainUntilIdle(%v, %v) = %v, want %v", tt.hasPromptDetection, tt.waitErr, got, tt.want)
			}
		})
	}
}

// pollerHarness drives nudgePoller.run with a manual tick channel.
type pollerHarness struct {
	p     *nudgePoller
	ticks chan time.Time
	sig   chan os.Signal
	done  chan error
}

func newPollerHarness(probe func(string) (bool, error)) *pollerHarness {
	h := &pollerHarness{
		ticks: make(chan time.Time),
		sig:   make(chan os.Signal, 1),
		done:  make(chan error, 1),
	}
	h.p = &nudgePoller{
		sessionName: "gt-test",
		hasSession:  probe,
		pending:     func() int { return 0 },
		waitForIdle: func() error { return nil },
		drain:       func() ([]nudge.QueuedNudge, error) { return nil, nil },
		inject:      func(string) error { return nil },
		requeue:     func([]nudge.QueuedNudge) {},
		stderr:      &bytes.Buffer{},
	}
	go func() { h.done <- h.p.run(h.sig, h.ticks) }()
	return h
}

// exited reports whether run returned within a short wait.
func (h *pollerHarness) exited() bool {
	select {
	case <-h.done:
		return true
	case <-time.After(50 * time.Millisecond):
		return false
	}
}

func TestNudgePollerRunTransientProbeErrorsKeepPolling(t *testing.T) {
	t.Parallel()

	calls := 0
	h := newPollerHarness(func(string) (bool, error) {
		calls++
		// Fail one short of the bound, then succeed, repeatedly: the streak
		// resets on success so the poller never reaches the limit.
		if calls%nudgePollerMaxProbeErrors != 0 {
			return false, errors.New("openshell sandbox exec: exit 1")
		}
		return true, nil
	})

	for i := 0; i < 3*nudgePollerMaxProbeErrors; i++ {
		h.ticks <- time.Now()
	}
	if h.exited() {
		t.Fatal("poller exited although probe errors never reached the bound in a row")
	}
	h.sig <- syscall.SIGTERM
	if !h.exited() {
		t.Fatal("poller did not stop on SIGTERM")
	}
}

func TestNudgePollerRunPersistentProbeErrorsEndPoller(t *testing.T) {
	t.Parallel()

	h := newPollerHarness(func(string) (bool, error) {
		return false, errors.New("server unreachable")
	})

	for i := 0; i < nudgePollerMaxProbeErrors; i++ {
		select {
		case h.ticks <- time.Now():
		case <-time.After(time.Second):
			t.Fatalf("poller stopped after %d errors, want %d", i, nudgePollerMaxProbeErrors)
		}
	}
	if !h.exited() {
		t.Fatalf("poller still running after %d consecutive probe errors", nudgePollerMaxProbeErrors)
	}
}

func TestNudgePollerRunExitsWhenSessionAbsent(t *testing.T) {
	t.Parallel()

	h := newPollerHarness(func(string) (bool, error) { return false, nil })
	h.ticks <- time.Now()
	if !h.exited() {
		t.Fatal("poller still running after the session was confirmed absent")
	}
}

func TestNudgePollerRunDeliversQueuedNudges(t *testing.T) {
	t.Parallel()

	var injected []string
	var requeued int
	h := newPollerHarness(func(string) (bool, error) { return true, nil })
	h.p.pending = func() int { return 1 }
	h.p.drain = func() ([]nudge.QueuedNudge, error) {
		return []nudge.QueuedNudge{{Sender: "a", Message: "hello"}}, nil
	}
	h.p.inject = func(msg string) error {
		injected = append(injected, msg)
		if len(injected) == 1 {
			return errors.New("composer busy")
		}
		return nil
	}
	h.p.requeue = func([]nudge.QueuedNudge) { requeued++ }

	h.ticks <- time.Now() // injection fails: requeued
	h.ticks <- time.Now() // injection succeeds
	h.sig <- syscall.SIGTERM
	if !h.exited() {
		t.Fatal("poller did not stop on SIGTERM")
	}

	if len(injected) != 2 || !strings.Contains(injected[0], "hello") {
		t.Errorf("injected = %q, want two injections carrying the nudge", injected)
	}
	if requeued != 1 {
		t.Errorf("requeued = %d, want 1 (only the failed injection)", requeued)
	}
}
