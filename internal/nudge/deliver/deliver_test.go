package deliver

import (
	"bytes"
	"context"
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

// probeRecordingTmux records the window of each consumption probe and runs
// onNudge right after a nudge is sent, so a test can end the context while
// the delivery is between its nudge and its probe.
type probeRecordingTmux struct {
	*deliveryTmux
	onNudge func()

	mu      sync.Mutex
	windows []time.Duration
}

func (p *probeRecordingTmux) NudgeSessionWithOpts(session, message string, opts tmux.NudgeOpts) error {
	err := p.deliveryTmux.NudgeSessionWithOpts(session, message, opts)
	if p.onNudge != nil {
		p.onNudge()
	}
	return err
}

func (p *probeRecordingTmux) WaitForInputConsumed(session string, window time.Duration) (tmux.InputConsumption, error) {
	p.mu.Lock()
	p.windows = append(p.windows, window)
	p.mu.Unlock()
	return p.deliveryTmux.WaitForInputConsumed(session, window)
}

func (p *probeRecordingTmux) probeWindows() []time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]time.Duration(nil), p.windows...)
}

// skipProbeAfterCtxEnds: a delivery whose context ends between the nudge and
// the consumption probe reports the end and does not start the probe
// (gt-2a2a6). The immediate and wait-idle twins cover both direct-delivery
// sites: immediate's send, and wait-idle's send once the target has gone idle.
func skipProbeAfterCtxEnds(t *testing.T, mode string, idle bool) {
	t.Helper()
	ft := newDeliveryTmux(t, deliveryTarget)
	ft.SetIdle(deliveryTarget, idle)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	probe := &probeRecordingTmux{deliveryTmux: ft, onNudge: cancel}
	d := testDelivery(ft, t.TempDir(), &pollerLog{}, &bytes.Buffer{})
	d.Tmux, d.Mode, d.Force = probe, mode, true
	d.ProbeWindow = 3 * time.Second

	if err := d.Deliver(ctx, deliveryTarget, "m", "mayor"); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("deliver: %v", err)
	}
	if got := probe.probeWindows(); len(got) != 0 {
		t.Errorf("consumption probes = %v, want none after the context ended", got)
	}
}

func TestDeliveryImmediateSkipsConsumptionProbeAfterCtxEnds(t *testing.T) {
	t.Parallel()
	skipProbeAfterCtxEnds(t, ModeImmediate, false)
}

func TestDeliveryWaitIdleSkipsConsumptionProbeAfterCtxEnds(t *testing.T) {
	t.Parallel()
	skipProbeAfterCtxEnds(t, ModeWaitIdle, true)
}

// clampProbeToDeadline: a probe starts with at most the time left on the
// caller's deadline, not the full ProbeWindow, so a 5ms deadline is not held
// for a 3s probe (gt-2a2a6).
func clampProbeToDeadline(t *testing.T, mode string, idle bool) {
	t.Helper()
	ft := newDeliveryTmux(t, deliveryTarget)
	ft.SetIdle(deliveryTarget, idle)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
	defer cancel()
	probe := &probeRecordingTmux{deliveryTmux: ft}
	d := testDelivery(ft, t.TempDir(), &pollerLog{}, &bytes.Buffer{})
	d.Tmux, d.Mode, d.Force = probe, mode, true
	d.ProbeWindow = 3 * time.Second

	if err := d.Deliver(ctx, deliveryTarget, "m", "mayor"); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deliver: %v", err)
	}
	for _, w := range probe.probeWindows() {
		if w > 5*time.Millisecond {
			t.Errorf("consumption probe window = %v, want at most the 5ms left on the deadline", w)
		}
	}
}

func TestDeliveryImmediateClampsConsumptionProbeToDeadline(t *testing.T) {
	t.Parallel()
	clampProbeToDeadline(t, ModeImmediate, false)
}

func TestDeliveryWaitIdleClampsConsumptionProbeToDeadline(t *testing.T) {
	t.Parallel()
	clampProbeToDeadline(t, ModeWaitIdle, true)
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

// TestRequeueAllNudgesGoBack: the ordinary case — nothing was refused, so
// nothing is reported (gt-isb0g).
func TestRequeueAllNudgesGoBack(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	session := "gt-test-requeue-clean"
	drained := []nudge.QueuedNudge{{Sender: "a", Message: "one"}, {Sender: "b", Message: "two"}}

	var stderr bytes.Buffer
	Requeue(&stderr, townRoot, session, "test", drained)

	if got := stderr.String(); got != "" {
		t.Errorf("stderr = %q, want no report when every nudge went back", got)
	}
	if pending, err := nudge.Pending(townRoot, session); err != nil {
		t.Fatalf("Pending: %v", err)
	} else if pending != len(drained) {
		t.Errorf("Pending = %d, want %d", pending, len(drained))
	}
}

// TestRequeueNamesHowManyNudgesWereNotRequeued: when the queue is full the
// nudges cannot go back, and the report says how many were not re-queued and
// where the preserved copies are (gt-isb0g).
func TestRequeueNamesHowManyNudgesWereNotRequeued(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	session := "gt-test-requeue-full"
	for i := 0; i < nudge.MaxQueueDepth; i++ {
		if err := nudge.Enqueue(townRoot, session, nudge.QueuedNudge{Sender: "s", Message: "fill"}); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	drained := []nudge.QueuedNudge{{Sender: "a", Message: "one"}, {Sender: "b", Message: "two"}}

	var stderr bytes.Buffer
	Requeue(&stderr, townRoot, session, "test", drained)

	report := stderr.String()
	if want := "2 of 2 nudge(s) not re-queued"; !strings.Contains(report, want) {
		t.Errorf("stderr = %q, want it to contain %q", report, want)
	}
	if want := nudge.UnqueuedDir(townRoot, session); !strings.Contains(report, want) {
		t.Errorf("stderr = %q, want it to name the preserved copies at %q", report, want)
	}
}

// TestRequeueNamesOnlyTheNudgesThatDidNotGoBack: the batch fills the queue
// partway through, so the report distinguishes the nudge that went back from
// the one that did not (gt-isb0g).
func TestRequeueNamesOnlyTheNudgesThatDidNotGoBack(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	session := "gt-test-requeue-partial"
	for i := 0; i < nudge.MaxQueueDepth-1; i++ {
		if err := nudge.Enqueue(townRoot, session, nudge.QueuedNudge{Sender: "s", Message: "fill"}); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	now := time.Now()
	drained := []nudge.QueuedNudge{
		{Sender: "a", Message: "one", Timestamp: now.Add(-time.Second)},
		{Sender: "b", Message: "two", Timestamp: now},
	}

	var stderr bytes.Buffer
	Requeue(&stderr, townRoot, session, "test", drained)

	report := stderr.String()
	if want := "1 of 2 nudge(s) not re-queued"; !strings.Contains(report, want) {
		t.Errorf("stderr = %q, want it to contain %q", report, want)
	}
	if pending, err := nudge.Pending(townRoot, session); err != nil {
		t.Fatalf("Pending: %v", err)
	} else if pending != nudge.MaxQueueDepth {
		t.Errorf("Pending = %d, want %d (the first nudge went back)", pending, nudge.MaxQueueDepth)
	}
}
