package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// noSleepClock is a clock whose Sleep returns at once, so the escalation's
// retry backoff costs the test nothing.
type noSleepClock struct{ clockwork.Clock }

func (noSleepClock) Sleep(time.Duration) {}

func unnamedHost(t *testing.T) (*patrolScanHost, *notifyfake.Recorder) {
	t.Helper()
	rec := notifyfake.New()
	return &patrolScanHost{d: &Daemon{
		logger:   discardLogger,
		config:   &Config{TownRoot: t.TempDir()},
		notifier: rec,
		clock:    noSleepClock{clockwork.NewFakeClockAt(time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC))},
		// The notes read answers a bead with no notes unless a test sets another reader.
		openWorkBeads: func([]string, time.Duration) workBeadReader { return &notesReader{} },
	}}, rec
}

// TestPatrolScanUnnamedFilesOneEscalation: the host files the missing escalation of a seat that
// stopped on ESCALATED under one key per bead, naming the seat and the bead and what to do; a
// second call for the same bead sends the same key, so gt escalate records it onto the
// escalation that exists, and a delivery error is returned wrapped with the seat and the bead
// (gt-xtx6sl).
func TestPatrolScanUnnamedFilesOneEscalation(t *testing.T) {
	t.Parallel()
	h, rec := unnamedHost(t)

	if err := h.FileUnnamedEscalation("gastown", "capable", "gt-ze3t74"); err != nil {
		t.Fatalf("FileUnnamedEscalation: %v", err)
	}
	if err := h.FileUnnamedEscalation("gastown", "capable", "gt-ze3t74"); err != nil {
		t.Fatalf("second FileUnnamedEscalation: %v", err)
	}

	calls := rec.Escalations()
	if len(calls) != 2 {
		t.Fatalf("escalations sent = %d, want 2 (one per call, one key)", len(calls))
	}
	for _, c := range calls {
		if c.Escalation.Fingerprint != "seat-escalated-unnamed:gt-ze3t74" {
			t.Errorf("fingerprint = %q, want seat-escalated-unnamed:gt-ze3t74", c.Escalation.Fingerprint)
		}
		for _, want := range []string{"gastown/capable", "gt-ze3t74", "ESCALATED", "bd show gt-ze3t74", "nuke"} {
			if !strings.Contains(c.Escalation.Reason, want) {
				t.Errorf("reason %q lacks %q", c.Escalation.Reason, want)
			}
		}
	}
	other := h
	if err := other.FileUnnamedEscalation("gastown", "capable", "gt-other"); err != nil {
		t.Fatal(err)
	}
	if got := rec.Escalations()[2].Escalation.Fingerprint; got != "seat-escalated-unnamed:gt-other" {
		t.Errorf("a different bead's fingerprint = %q, want its own key", got)
	}
}

func TestPatrolScanUnnamedDeliveryError(t *testing.T) {
	t.Parallel()
	h, rec := unnamedHost(t)
	injected := errors.New("gt escalate: exit status 1")
	rec.Fail(notifyfake.KindEscalate, injected)

	err := h.FileUnnamedEscalation("gastown", "capable", "gt-ze3t74")

	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "gastown/capable") || !strings.Contains(err.Error(), "gt-ze3t74") || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("err = %v, want the delivery error wrapped with the seat and the bead", err)
	}
}

// TestPatrolScanMemoryLedgerOutlivesTheTick: the scanner is rebuilt every tick, so the in-memory
// stand-in for an unreadable ledger file must be the daemon's, the same one in every tick's
// options: a mark made in one tick is read in the next, and the unnamed-escalation filing is
// made once per exit per daemon run instead of once per tick (gt-5wmwv4).
func TestPatrolScanMemoryLedgerOutlivesTheTick(t *testing.T) {
	t.Parallel()
	h, _ := unnamedHost(t)
	first := h.d.patrolScanRunOptions(h).MemoryLedger
	if first == nil {
		t.Fatal("the tick's options carry no MemoryLedger")
	}
	at := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	if err := first.MarkReported("unnamed-escalation:gastown/capable/gt-x", at); err != nil {
		t.Fatal(err)
	}

	second := h.d.patrolScanRunOptions(h).MemoryLedger

	if got, ok := second.LastReported("unnamed-escalation:gastown/capable/gt-x"); !ok || !got.Equal(at) {
		t.Errorf("next tick's MemoryLedger = %v, %v; want the mark of the first tick", got, ok)
	}
}
