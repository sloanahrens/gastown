package deliver

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/tmux/tmuxfake"
)

// deliveryTmux is a tmuxfake server with the nudge-only parts of *tmux.Tmux
// scripted: a missing session's wait reports tmux.ErrSessionNotFound, as the
// real one does.
type deliveryTmux struct {
	*tmuxfake.Server

	mu          sync.Mutex
	waits       int
	idleOnWait  int         // the WaitForIdle call (1-based) on which the target goes idle; 0 never
	onWait      func(n int) // called with each WaitForIdle call's number (1-based)
	submitErr   error       // returned by NudgeSessionWithOpts after the text is sent
	consumption tmux.InputConsumption
	agent       string
	preset      *config.AgentPresetInfo
	presetOK    bool
}

func newDeliveryTmux(t *testing.T, sessions ...string) *deliveryTmux {
	t.Helper()
	ft := &deliveryTmux{Server: tmuxfake.New(clockwork.NewRealClock()), consumption: tmux.InputConsumptionStartedTurn}
	for _, s := range sessions {
		if err := ft.NewSession(s, ""); err != nil {
			t.Fatal(err)
		}
	}
	return ft
}

func (f *deliveryTmux) IsBusy(target string) bool {
	ok, _ := f.HasSession(target)
	return ok && !f.IsIdle(target)
}

func (f *deliveryTmux) WaitForIdle(session string, timeout time.Duration) error {
	if ok, _ := f.HasSession(session); !ok {
		return tmux.ErrSessionNotFound
	}
	f.mu.Lock()
	f.waits++
	n, becomeIdle, onWait := f.waits, f.idleOnWait != 0 && f.waits == f.idleOnWait, f.onWait
	f.mu.Unlock()
	if onWait != nil {
		onWait(n)
	}
	if becomeIdle {
		f.SetIdle(session, true)
	}
	return f.Server.WaitForIdle(session, timeout)
}

func (f *deliveryTmux) NudgeSessionWithOpts(session, message string, _ tmux.NudgeOpts) error {
	if err := f.NudgeSession(session, message); err != nil {
		return err
	}
	return f.submitErr
}

func (f *deliveryTmux) WaitForInputConsumed(string, time.Duration) (tmux.InputConsumption, error) {
	return f.consumption, nil
}

func (f *deliveryTmux) SessionAgentPreset(string, string) (string, *config.AgentPresetInfo, bool) {
	return f.agent, f.preset, f.presetOK
}

// pollerLog records the sessions a delivery started a nudge-poller for.
type pollerLog struct {
	mu      sync.Mutex
	started []string
}

func (p *pollerLog) start(_, session string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = append(p.started, session)
	return 1, nil
}

func (p *pollerLog) sessions() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.started...)
}

// testDelivery is a wait-idle delivery into townRoot with millisecond timings.
func testDelivery(ft *deliveryTmux, townRoot string, pollers *pollerLog, stderr *bytes.Buffer) *Delivery {
	return &Delivery{
		Tmux:            ft,
		TownRoot:        townRoot,
		Mode:            ModeWaitIdle,
		Priority:        nudge.PriorityNormal,
		WaitIdleTimeout: time.Millisecond,
		WatchTimeout:    20 * time.Millisecond,
		PollInterval:    time.Millisecond,
		ProbeWindow:     time.Millisecond,
		Clock:           clockwork.NewRealClock(),
		StartPoller:     pollers.start,
		Stderr:          stderr,
	}
}

const deliveryTarget = "gt-crew-max"

func TestDeliveryWaitIdleDeliversToIdleTarget(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget)
	ft.SetIdle(deliveryTarget, true)
	town, pollers, stderr := t.TempDir(), &pollerLog{}, &bytes.Buffer{}

	if err := testDelivery(ft, town, pollers, stderr).Deliver(t.Context(), deliveryTarget, "check mail", "mayor"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	want := nudge.FormatForInjection([]nudge.QueuedNudge{{Sender: "mayor", Message: "check mail", Priority: nudge.PriorityNormal}})
	if got := ft.Sent(deliveryTarget); len(got) != 1 || got[0] != want {
		t.Fatalf("sent = %q, want one %q", got, want)
	}
	if n := nudge.QueueLen(town, deliveryTarget); n != 0 {
		t.Errorf("queue = %d, want 0", n)
	}
	if p := pollers.sessions(); len(p) != 0 {
		t.Errorf("pollers started = %v, want none", p)
	}
}

func TestDeliveryWaitIdleQueuesForBusyTargetAndWatches(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget) // never idle
	town, pollers, stderr := t.TempDir(), &pollerLog{}, &bytes.Buffer{}

	if err := testDelivery(ft, town, pollers, stderr).Deliver(t.Context(), deliveryTarget, "check mail", "mayor"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if got := ft.Sent(deliveryTarget); len(got) != 0 {
		t.Fatalf("sent = %q, want nothing to a busy target", got)
	}
	if n := nudge.QueueLen(town, deliveryTarget); n != 1 {
		t.Errorf("queue = %d, want the nudge queued", n)
	}
	if p := pollers.sessions(); len(p) != 1 || p[0] != deliveryTarget {
		t.Errorf("pollers started = %v, want [%s]", p, deliveryTarget)
	}
	if !strings.Contains(stderr.String(), "gave up waiting for "+deliveryTarget) {
		t.Errorf("stderr = %q, want the watcher to report giving up", stderr)
	}
}

func TestDeliveryWaitIdleWatcherDeliversWhenTargetGoesIdle(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget)
	ft.idleOnWait = 2 // the first wait times out; the watcher's first poll sees idle
	town, pollers, stderr := t.TempDir(), &pollerLog{}, &bytes.Buffer{}

	if err := testDelivery(ft, town, pollers, stderr).Deliver(t.Context(), deliveryTarget, "check mail", "mayor"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	got := ft.Sent(deliveryTarget)
	if len(got) != 1 || !strings.Contains(got[0], "[from mayor] check mail") {
		t.Fatalf("sent = %q, want the drained nudge delivered once", got)
	}
	if n := nudge.QueueLen(town, deliveryTarget); n != 0 {
		t.Errorf("queue = %d, want it drained", n)
	}
}

func TestDeliveryWaitIdleRefusesMissingSession(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t)
	town, pollers, stderr := t.TempDir(), &pollerLog{}, &bytes.Buffer{}

	err := testDelivery(ft, town, pollers, stderr).Deliver(t.Context(), deliveryTarget, "check mail", "mayor")
	if !errors.Is(err, tmux.ErrSessionNotFound) {
		t.Fatalf("deliver = %v, want ErrSessionNotFound", err)
	}
	if n := nudge.QueueLen(town, deliveryTarget); n != 0 {
		t.Errorf("queue = %d, want nothing queued for a dead session", n)
	}
}

func TestDeliveryWaitIdleQueuesUnverifiedSubmit(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget)
	ft.SetIdle(deliveryTarget, true)
	ft.submitErr = tmux.ErrSubmitNotVerified
	town, pollers, stderr := t.TempDir(), &pollerLog{}, &bytes.Buffer{}

	if err := testDelivery(ft, town, pollers, stderr).Deliver(t.Context(), deliveryTarget, "check mail", "mayor"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if n := nudge.QueueLen(town, deliveryTarget); n != 1 {
		t.Errorf("queue = %d, want the unverified nudge queued", n)
	}
}

func TestDeliveryWaitIdleQueuesForAgentWithoutPromptDetection(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget)
	ft.SetIdle(deliveryTarget, true)
	ft.agent, ft.preset, ft.presetOK = "codex", &config.AgentPresetInfo{}, true
	town, pollers, stderr := t.TempDir(), &pollerLog{}, &bytes.Buffer{}

	if err := testDelivery(ft, town, pollers, stderr).Deliver(t.Context(), deliveryTarget, "check mail", "mayor"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if got := ft.Sent(deliveryTarget); len(got) != 0 {
		t.Fatalf("sent = %q, want nothing typed into an agent without prompt detection", got)
	}
	if n := nudge.QueueLen(town, deliveryTarget); n != 1 {
		t.Errorf("queue = %d, want the nudge queued", n)
	}
	if p := pollers.sessions(); len(p) != 1 {
		t.Errorf("pollers started = %v, want one", p)
	}
}

func TestDeliveryWaitIdleNeedsWorkspace(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget)
	if err := testDelivery(ft, "", &pollerLog{}, &bytes.Buffer{}).Deliver(t.Context(), deliveryTarget, "m", "mayor"); err == nil {
		t.Fatal("deliver without a town root succeeded, want the workspace error")
	}
	if got := ft.Sent(deliveryTarget); len(got) != 0 {
		t.Fatalf("sent = %q, want nothing", got)
	}
}

func TestDeliveryWaitIdleDeliversImmediatelyWhenQueueFails(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget) // busy, so wait-idle times out and queues
	town := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(town, nil, 0o644); err != nil { // a file: the queue cannot be created under it
		t.Fatal(err)
	}
	stderr := &bytes.Buffer{}

	if err := testDelivery(ft, town, &pollerLog{}, stderr).Deliver(t.Context(), deliveryTarget, "check mail", "mayor"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if got := ft.Sent(deliveryTarget); len(got) != 1 || !strings.Contains(got[0], "[from mayor] check mail") {
		t.Fatalf("sent = %q, want the last-resort immediate delivery", got)
	}
	if !strings.Contains(stderr.String(), "queue fallback failed") {
		t.Errorf("stderr = %q, want the queue failure reported", stderr)
	}
}

func TestDeliveryWaitIdleWarnsWhenNudgeNotConsumed(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget)
	ft.SetIdle(deliveryTarget, true)
	ft.consumption = tmux.InputConsumptionNotConsumed
	stderr := &bytes.Buffer{}

	if err := testDelivery(ft, t.TempDir(), &pollerLog{}, stderr).Deliver(t.Context(), deliveryTarget, "m", "mayor"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if !strings.Contains(stderr.String(), "wait-idle: "+deliveryTarget+" accepted the nudge but started no turn") {
		t.Errorf("stderr = %q, want the not-consumed warning", stderr)
	}
}

func TestDeliveryImmediateRefusesBusyTargetWithoutForce(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget) // busy
	town, stderr := t.TempDir(), &bytes.Buffer{}
	d := testDelivery(ft, town, &pollerLog{}, stderr)
	d.Mode = ModeImmediate

	if err := d.Deliver(t.Context(), deliveryTarget, "m", "mayor"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if got := ft.Sent(deliveryTarget); len(got) != 0 {
		t.Fatalf("sent = %q, want a busy target left alone", got)
	}
	if n := nudge.QueueLen(town, deliveryTarget); n != 1 {
		t.Errorf("queue = %d, want the wait-idle fallback to queue", n)
	}
}

func TestDeliveryImmediateForceInterruptsWithSenderPrefix(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget) // busy
	d := testDelivery(ft, t.TempDir(), &pollerLog{}, &bytes.Buffer{})
	d.Mode, d.Force = ModeImmediate, true

	if err := d.Deliver(t.Context(), deliveryTarget, "m", "mayor"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if got := ft.Sent(deliveryTarget); len(got) != 1 || got[0] != "[from mayor] m" {
		t.Fatalf("sent = %q, want [\"[from mayor] m\"]", got)
	}
}

func TestDeliveryWatcherExitsOnEmptyQueue(t *testing.T) {
	t.Parallel()
	// The watcher exits after its first poll when the queue is empty
	// (someone else drained it), before it consults the session.
	clk := clockwork.NewFakeClockAt(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	d := testDelivery(newDeliveryTmux(t), t.TempDir(), &pollerLog{}, &bytes.Buffer{})
	d.Clock, d.WatchTimeout, d.PollInterval = clk, time.Minute, time.Second
	done := make(chan struct{})
	go func() {
		d.Watch(t.Context(), "test-session")
		close(done)
	}()
	if err := clk.BlockUntilContext(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second) // one poll, not the whole minute
	<-done
}
