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

// pollerHarness drives nudgePoller.run with a manual tick channel. The tick
// channel is unbuffered, so a send is accepted only once run has finished the
// previous tick and is back in its select.
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
	return h
}

func (h *pollerHarness) start() {
	go func() { h.done <- h.p.run(h.sig, h.ticks) }()
}

// tick delivers one poll tick and reports whether the poller accepted it;
// false means run has already returned.
func (h *pollerHarness) tick() bool {
	select {
	case h.ticks <- time.Time{}:
		return true
	case <-h.done:
		h.done <- nil // keep the exit observable to stop()
		return false
	}
}

// stop sends SIGTERM and waits for run to return.
func (h *pollerHarness) stop() {
	h.sig <- syscall.SIGTERM
	<-h.done
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
	h.start()

	// One tick past the last probe proves run finished that tick and kept going.
	for i := 0; i <= 3*nudgePollerMaxProbeErrors; i++ {
		if !h.tick() {
			t.Fatalf("poller exited at tick %d although probe errors never reached the bound in a row", i)
		}
	}
	h.stop()
}

func TestNudgePollerRunPersistentProbeErrorsEndPoller(t *testing.T) {
	t.Parallel()

	h := newPollerHarness(func(string) (bool, error) {
		return false, errors.New("server unreachable")
	})
	h.start()

	for i := 1; i <= nudgePollerMaxProbeErrors; i++ {
		if !h.tick() {
			t.Fatalf("poller exited after %d probe errors, want %d", i-1, nudgePollerMaxProbeErrors)
		}
	}
	<-h.done // the last accepted tick was the bound-th error: run returns
}

func TestNudgePollerRunExitsWhenSessionAbsent(t *testing.T) {
	t.Parallel()

	h := newPollerHarness(func(string) (bool, error) { return false, nil })
	h.start()
	if !h.tick() {
		t.Fatal("poller refused its first tick")
	}
	<-h.done
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
	h.start()

	h.tick() // injection fails: requeued
	h.tick() // injection succeeds
	h.stop()

	if len(injected) != 2 || !strings.Contains(injected[0], "hello") {
		t.Errorf("injected = %q, want two injections carrying the nudge", injected)
	}
	if requeued != 1 {
		t.Errorf("requeued = %d, want 1 (only the failed injection)", requeued)
	}
}
