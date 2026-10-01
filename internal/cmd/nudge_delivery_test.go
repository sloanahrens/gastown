package cmd

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
	idleOnWait  int   // the WaitForIdle call (1-based) on which the target goes idle; 0 never
	submitErr   error // returned by NudgeSessionWithOpts after the text is sent
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
	becomeIdle := f.idleOnWait != 0 && f.waits == f.idleOnWait
	f.mu.Unlock()
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
func testDelivery(ft *deliveryTmux, townRoot string, pollers *pollerLog, stderr *bytes.Buffer) *nudgeDelivery {
	return &nudgeDelivery{
		tmux:            ft,
		townRoot:        townRoot,
		mode:            NudgeModeWaitIdle,
		priority:        nudge.PriorityNormal,
		waitIdleTimeout: time.Millisecond,
		watchTimeout:    20 * time.Millisecond,
		pollInterval:    time.Millisecond,
		probeWindow:     time.Millisecond,
		clock:           clockwork.NewRealClock(),
		startPoller:     pollers.start,
		stderr:          stderr,
	}
}

const deliveryTarget = "gt-crew-max"

func TestNudgeDeliveryWaitIdleDeliversToIdleTarget(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget)
	ft.SetIdle(deliveryTarget, true)
	town, pollers, stderr := t.TempDir(), &pollerLog{}, &bytes.Buffer{}

	if err := testDelivery(ft, town, pollers, stderr).deliver(deliveryTarget, "check mail", "mayor"); err != nil {
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

func TestNudgeDeliveryWaitIdleQueuesForBusyTargetAndWatches(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget) // never idle
	town, pollers, stderr := t.TempDir(), &pollerLog{}, &bytes.Buffer{}

	if err := testDelivery(ft, town, pollers, stderr).deliver(deliveryTarget, "check mail", "mayor"); err != nil {
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

func TestNudgeDeliveryWaitIdleWatcherDeliversWhenTargetGoesIdle(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget)
	ft.idleOnWait = 2 // the first wait times out; the watcher's first poll sees idle
	town, pollers, stderr := t.TempDir(), &pollerLog{}, &bytes.Buffer{}

	if err := testDelivery(ft, town, pollers, stderr).deliver(deliveryTarget, "check mail", "mayor"); err != nil {
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

func TestNudgeDeliveryWaitIdleRefusesMissingSession(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t)
	town, pollers, stderr := t.TempDir(), &pollerLog{}, &bytes.Buffer{}

	err := testDelivery(ft, town, pollers, stderr).deliver(deliveryTarget, "check mail", "mayor")
	if !errors.Is(err, tmux.ErrSessionNotFound) {
		t.Fatalf("deliver = %v, want ErrSessionNotFound", err)
	}
	if n := nudge.QueueLen(town, deliveryTarget); n != 0 {
		t.Errorf("queue = %d, want nothing queued for a dead session", n)
	}
}

func TestNudgeDeliveryWaitIdleQueuesUnverifiedSubmit(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget)
	ft.SetIdle(deliveryTarget, true)
	ft.submitErr = tmux.ErrSubmitNotVerified
	town, pollers, stderr := t.TempDir(), &pollerLog{}, &bytes.Buffer{}

	if err := testDelivery(ft, town, pollers, stderr).deliver(deliveryTarget, "check mail", "mayor"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if n := nudge.QueueLen(town, deliveryTarget); n != 1 {
		t.Errorf("queue = %d, want the unverified nudge queued", n)
	}
}

func TestNudgeDeliveryWaitIdleQueuesForAgentWithoutPromptDetection(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget)
	ft.SetIdle(deliveryTarget, true)
	ft.agent, ft.preset, ft.presetOK = "codex", &config.AgentPresetInfo{}, true
	town, pollers, stderr := t.TempDir(), &pollerLog{}, &bytes.Buffer{}

	if err := testDelivery(ft, town, pollers, stderr).deliver(deliveryTarget, "check mail", "mayor"); err != nil {
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

func TestNudgeDeliveryWaitIdleNeedsWorkspace(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget)
	if err := testDelivery(ft, "", &pollerLog{}, &bytes.Buffer{}).deliver(deliveryTarget, "m", "mayor"); err == nil {
		t.Fatal("deliver without a town root succeeded, want the workspace error")
	}
	if got := ft.Sent(deliveryTarget); len(got) != 0 {
		t.Fatalf("sent = %q, want nothing", got)
	}
}

func TestNudgeDeliveryWaitIdleDeliversImmediatelyWhenQueueFails(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget) // busy, so wait-idle times out and queues
	town := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(town, nil, 0o644); err != nil { // a file: the queue cannot be created under it
		t.Fatal(err)
	}
	stderr := &bytes.Buffer{}

	if err := testDelivery(ft, town, &pollerLog{}, stderr).deliver(deliveryTarget, "check mail", "mayor"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if got := ft.Sent(deliveryTarget); len(got) != 1 || !strings.Contains(got[0], "[from mayor] check mail") {
		t.Fatalf("sent = %q, want the last-resort immediate delivery", got)
	}
	if !strings.Contains(stderr.String(), "queue fallback failed") {
		t.Errorf("stderr = %q, want the queue failure reported", stderr)
	}
}

func TestNudgeDeliveryWaitIdleWarnsWhenNudgeNotConsumed(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget)
	ft.SetIdle(deliveryTarget, true)
	ft.consumption = tmux.InputConsumptionNotConsumed
	stderr := &bytes.Buffer{}

	if err := testDelivery(ft, t.TempDir(), &pollerLog{}, stderr).deliver(deliveryTarget, "m", "mayor"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if !strings.Contains(stderr.String(), "wait-idle: "+deliveryTarget+" accepted the nudge but started no turn") {
		t.Errorf("stderr = %q, want the not-consumed warning", stderr)
	}
}

func TestNudgeDeliveryImmediateRefusesBusyTargetWithoutForce(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget) // busy
	town, stderr := t.TempDir(), &bytes.Buffer{}
	d := testDelivery(ft, town, &pollerLog{}, stderr)
	d.mode = NudgeModeImmediate

	if err := d.deliver(deliveryTarget, "m", "mayor"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if got := ft.Sent(deliveryTarget); len(got) != 0 {
		t.Fatalf("sent = %q, want a busy target left alone", got)
	}
	if n := nudge.QueueLen(town, deliveryTarget); n != 1 {
		t.Errorf("queue = %d, want the wait-idle fallback to queue", n)
	}
}

func TestNudgeDeliveryImmediateForceInterruptsWithSenderPrefix(t *testing.T) {
	t.Parallel()
	ft := newDeliveryTmux(t, deliveryTarget) // busy
	d := testDelivery(ft, t.TempDir(), &pollerLog{}, &bytes.Buffer{})
	d.mode, d.force = NudgeModeImmediate, true

	if err := d.deliver(deliveryTarget, "m", "mayor"); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if got := ft.Sent(deliveryTarget); len(got) != 1 || got[0] != "[from mayor] m" {
		t.Fatalf("sent = %q, want [\"[from mayor] m\"]", got)
	}
}

func TestNudgeDeliveryWatcherExitsOnEmptyQueue(t *testing.T) {
	t.Parallel()
	// The watcher exits after its first poll when the queue is empty
	// (someone else drained it), before it consults the session.
	clk := clockwork.NewFakeClockAt(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	d := testDelivery(newDeliveryTmux(t), t.TempDir(), &pollerLog{}, &bytes.Buffer{})
	d.clock, d.watchTimeout, d.pollInterval = clk, time.Minute, time.Second
	done := make(chan struct{})
	go func() {
		d.watch("test-session")
		close(done)
	}()
	if err := clk.BlockUntilContext(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second) // one poll, not the whole minute
	<-done
}

// nudgeSenderCases pin who gt nudge says a nudge is from, by the caller's
// working directory (relative to the town root) and identity environment.
var nudgeSenderCases = []struct {
	name string
	cwd  string
	env  map[string]string
	want string
}{
	{"town root, no identity (the daemon)", ".", nil, "unknown"},
	{"mayor dir", "mayor", nil, "mayor"},
	{"crew dir", "gastown/crew/max", nil, "gastown/crew/max"},
	{"polecat dir", "gastown/polecats/toast", nil, "gastown/toast"},
	{"rig root", "gastown", nil, "unknown"},
	{"retired deacon dir", "deacon", nil, "unknown"},
	{"GT_ROLE mayor anywhere", ".", map[string]string{"GT_ROLE": "mayor"}, "mayor"},
	{"GT_ROLE crew", ".", map[string]string{"GT_ROLE": "gastown/crew/max"}, "gastown/crew/max"},
	{"GT_ROLE crew with GT_RIG and GT_CREW", ".", map[string]string{"GT_ROLE": "crew", "GT_RIG": "gastown", "GT_CREW": "max"}, "gastown/crew/max"},
	{"GT_ROLE polecat filled from cwd", "gastown/polecats/toast", map[string]string{"GT_ROLE": "polecat"}, "gastown/toast"},
	{"GT_ROLE beats cwd", "gastown/crew/max", map[string]string{"GT_ROLE": "mayor"}, "mayor"},
	{"GT_ROLE unknown simple role", ".", map[string]string{"GT_ROLE": "overseer"}, "overseer"},
	{"GT_ROLE retired witness", ".", map[string]string{"GT_ROLE": "gastown/witness"}, "gastown/witness"},
}

func TestNudgeSenderFromRole(t *testing.T) {
	t.Parallel()
	town := "/town"
	for _, tc := range nudgeSenderCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			info, err := getRoleWithContextEnv(filepath.Join(town, tc.cwd), town, func(k string) string { return tc.env[k] })
			if got := nudgeSender(info, err); got != tc.want {
				t.Errorf("sender = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNudgeSenderUnreadableRoleIsUnknown(t *testing.T) {
	t.Parallel()
	if got := nudgeSender(RoleInfo{Role: RoleMayor}, errors.New("not in a Gas Town workspace")); got != "unknown" {
		t.Errorf("sender = %q, want unknown", got)
	}
}
