package daemon

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/townstate"
)

// publishGateOver returns a boot gate that publishes its reason under town,
// with its wait between polls on a fake clock fixed at now, as bootGateOver
// does. The probes keep the real clock for their timeout.
func publishGateOver(t *testing.T, town string, now time.Time, probes ...bootProbe) (*bootGate, *clockwork.FakeClock) {
	t.Helper()
	gate := newBootGate(town, probes)
	clk := clockwork.NewFakeClockAt(now)
	gate.clock = clk
	gate.interval = bootGateTestInterval
	return gate, clk
}

// readBootHealth returns the boot-health file's content under town and whether
// the file exists.
func readBootHealth(t *testing.T, town string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(dispatch.BootHealthPath(town))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatalf("reading the boot-health file: %v", err)
	}
	return string(data), true
}

// gatedProbe holds its first check until release is closed, and closes entered
// when that check starts, so a test reads the file while the poll is inside its
// first poll. Once up is set the probe answers.
type gatedProbe struct {
	entered     chan struct{}
	release     chan struct{}
	once        sync.Once
	releaseOnce sync.Once
	up          atomic.Bool
}

func newGatedProbe() *gatedProbe {
	return &gatedProbe{entered: make(chan struct{}), release: make(chan struct{})}
}

// open lets the held check return. It is safe to call more than once, so a
// test's cleanup can call it after the test has already released the probe.
func (p *gatedProbe) open() {
	p.releaseOnce.Do(func() { close(p.release) })
}

func (p *gatedProbe) probe(name string) bootProbe {
	return bootProbe{Name: name, Check: func(context.Context) error {
		p.once.Do(func() { close(p.entered) })
		<-p.release
		if p.up.Load() {
			return nil
		}
		return errors.New("not answering")
	}}
}

// TestBootGatePublishWritesAndClears: the poller writes the waiting reason at
// start, over a stale file a dead daemon left; writes the reason once the probe
// answers it; and removes the file at the first healthy answer.
func TestBootGatePublishWritesAndClears(t *testing.T) {
	t.Parallel()
	now := standbyTestNow
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(dispatch.BootHealthPath(town)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dispatch.BootHealthPath(town), []byte("stale reason from a dead daemon\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gp := newGatedProbe()
	gate, clk := publishGateOver(t, town, now, gp.probe("dolt"))

	d, _, _ := standbyHeartbeat(t, now)
	d.bootGate = gate
	ctx, cancel := context.WithCancel(context.Background())
	done := startBootPoll(d, ctx)
	// A failed assertion above ends the test with the poll still inside its
	// held probe; releasing it and ending the poll keeps the goroutine from
	// outliving the test.
	t.Cleanup(func() {
		gp.open()
		cancel()
		<-done
	})

	guard, stop := context.WithTimeout(context.Background(), bootHangGuard) //testpolicy:allow no-sleep — a hang guard only: the first probe's start is the event the test waits for, and a poll that never starts it fails here rather than hanging the test
	defer stop()
	select {
	case <-gp.entered:
	case <-guard.Done():
		t.Fatal("the boot poll never started its first probe")
	}
	if got, ok := readBootHealth(t, town); !ok || got != bootGateWaiting+"\n" {
		t.Fatalf("boot-health before the first poll answers = %q (exists %v), want %q: the start write replaces the stale file", got, ok, bootGateWaiting+"\n")
	}

	gp.open()
	awaitBootPoll(t, clk)
	reason := gate.reason()
	if reason == "" || reason == bootGateWaiting {
		t.Fatalf("the gate's reason after the first poll = %q, want the probe's reason", reason)
	}
	if got, ok := readBootHealth(t, town); !ok || got != reason+"\n" {
		t.Fatalf("boot-health after a changed answer = %q (exists %v), want %q", got, ok, reason+"\n")
	}

	gp.up.Store(true)
	clk.Advance(gate.interval)
	awaitBootPollEnd(t, done)
	if got, ok := readBootHealth(t, town); ok {
		t.Fatalf("boot-health after the first healthy answer = %q, want the file removed", got)
	}
}

// TestBootGatePublishWriteErrorStillHoldsInProcess: when the file cannot be
// written, the failure is logged, and the in-process standby still holds on the
// reason. Here a regular file where the daemon directory belongs makes the write
// fail.
func TestBootGatePublishWriteErrorStillHoldsInProcess(t *testing.T) {
	t.Parallel()
	now := standbyTestNow
	town := t.TempDir()
	if err := os.WriteFile(filepath.Join(town, "daemon"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dolt := &fakeProbe{}
	gate, clk := publishGateOver(t, town, now, dolt.probe("dolt"))

	d, _, lines := standbyHeartbeat(t, now)
	d.bootGate = gate
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startBootPoll(d, ctx)
	awaitBootPoll(t, clk)

	reason := d.standbyReason(now)
	if !strings.Contains(reason, "dolt") {
		t.Fatalf("standbyReason with the boot file unwritable = %q, want it to hold on dolt", reason)
	}
	if n := countContaining(lines(), "boot gate: publishing \""+bootGateWaiting+"\""); n != 1 {
		t.Errorf("the start write failure was logged %d time(s), want once; log: %v", n, lines())
	}
	cancel()
	awaitBootPollEnd(t, done)
}

// TestBootGatePublishOwnerIsTheDaemonRow: the boot-health file sits under the
// daemon row of the town-state manifest, which owns it, and the manifest has no
// row of its own for it.
func TestBootGatePublishOwnerIsTheDaemonRow(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rel, err := filepath.Rel(town, dispatch.BootHealthPath(town))
	if err != nil {
		t.Fatal(err)
	}
	rel = filepath.ToSlash(rel)
	m := townstate.Default()
	entry, ok := townstate.Lookup(m.Entries, rel)
	if !ok || entry.Path != "daemon" || entry.Owner != townstate.OwnerDaemon {
		t.Fatalf("the manifest's row for %s = %+v (found %v), want the daemon row owned by the daemon", rel, entry, ok)
	}
	for _, e := range m.Entries {
		if e.Path == rel {
			t.Errorf("the manifest has its own row for %s; the daemon row should cover it", rel)
		}
	}
}
