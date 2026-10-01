package land

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// lockedBuffer is Lander.Out for a test whose alarm logs from its own goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// stageGate is a Gate whose run sees the stage's context.
type stageGate func(ctx context.Context) GateResult

func (g stageGate) Run(ctx context.Context, _ string) GateResult { return g(ctx) }

// stageReviewer is a Reviewer whose run sees the stage's context.
type stageReviewer func(ctx context.Context) (Verdict, error)

func (r stageReviewer) Review(ctx context.Context, _, _, _ string) (Verdict, error) { return r(ctx) }

type slowEscalation struct{ bead, stage, message string }

// cannedPS is the host's process table while the gate's make (4242) runs go
// test (4243) under it, beside a process that is none of the stage's.
const cannedPS = `    1     0     1 Ss    0.0 10-01:02:03 /sbin/launchd
 4242     1  4242 S     0.0       08:00 sh -c make gate-test
 4243  4242  4242 R    97.5       07:59 go test ./...
 5151     1  5151 S     0.1       08:00 om review --base abc
 9000     1  9000 S     0.0       09:00 unrelated
`

const cannedLsof = `COMMAND  PID USER   FD   TYPE DEVICE SIZE/OFF NODE NAME
om      5151 gt    12u  IPv4 0x1234      0t0  TCP 10.0.0.5:50123->104.18.1.1:443 (ESTABLISHED)
`

// slowRig is a landing whose gate and om stages block until the test lets
// them finish, on a clock the test advances.
type slowRig struct {
	f         *landFixture
	clock     *clockwork.FakeClock
	out       *lockedBuffer
	evidence  string
	escalated chan slowEscalation
	gateGo    chan struct{}
	gateUp    chan struct{} // closed once the gate stage has registered its pid
	omUp      chan struct{}
	omGo      chan struct{}
	stopped   chan string

	mu      sync.Mutex
	lsofPID []string // the -p argument of each lsof call
}

func newSlowRig(t *testing.T, after time.Duration) (*slowRig, *Lander) {
	t.Helper()
	r := &slowRig{
		f:         newLandFixture(t),
		clock:     clockwork.NewFakeClockAt(time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)),
		out:       &lockedBuffer{},
		evidence:  filepath.Join(t.TempDir(), "landing-logs"),
		escalated: make(chan slowEscalation, 8),
		gateGo:    make(chan struct{}),
		gateUp:    make(chan struct{}),
		omUp:      make(chan struct{}),
		omGo:      make(chan struct{}),
		stopped:   make(chan string, 2),
	}
	l := r.f.lander()
	l.Out = r.out
	// A stage whose context ends was stopped; the alarm must never do that.
	l.Gate = stageGate(func(ctx context.Context) GateResult {
		defer trackPID(ctx, 4242)()
		close(r.gateUp)
		select {
		case <-r.gateGo:
		case <-ctx.Done():
			r.stopped <- "gate"
		}
		return GateResult{Passed: true, Steps: []StepResult{{Name: "test"}}}
	})
	l.Reviewer = stageReviewer(func(ctx context.Context) (Verdict, error) {
		defer trackPID(ctx, 5151)()
		close(r.omUp)
		select {
		case <-r.omGo:
		case <-ctx.Done():
			r.stopped <- "om"
		}
		return Verdict{Verdict: VerdictApprove, Score: 0.9}, nil
	})
	l.Slow = &SlowAlarm{
		After: after,
		Clock: r.clock,
		Escalate: func(beadID, stage, message string) {
			r.escalated <- slowEscalation{beadID, stage, message}
		},
		EvidenceDir: func(context.Context, string) string { return r.evidence },
		run: func(_ context.Context, name string, args ...string) (string, error) {
			switch name {
			case "ps":
				return cannedPS, nil
			case "lsof":
				r.mu.Lock()
				r.lsofPID = append(r.lsofPID, args[len(args)-1])
				r.mu.Unlock()
				return cannedLsof, nil
			}
			return "", errors.New("unexpected command " + name)
		},
	}
	return r, l
}

// awaitTimer blocks until n stage timers are armed on the fake clock.
func (r *slowRig) awaitTimers(t *testing.T, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.clock.BlockUntilContext(ctx, n); err != nil {
		t.Fatalf("want %d armed alarm timers: %v", n, err)
	}
}

func (r *slowRig) awaitEscalation(t *testing.T) slowEscalation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	select {
	case e := <-r.escalated:
		return e
	case <-ctx.Done():
		t.Fatal("no escalation filed")
		return slowEscalation{}
	}
}

// TestLandSlowStageAlarmsOncePerStage: a gate stage and an om stage that each
// run past alarm_after log one SLOW line and file one escalation naming the
// bead, the stage and the stage's pids, with the process tree and TCP peers
// saved beside the landing's logs; neither stage is stopped and the landing
// lands (gt-lcu5p).
func TestLandSlowStageAlarmsOncePerStage(t *testing.T) {
	t.Parallel()
	r, l := newSlowRig(t, 8*time.Minute)
	type landed struct {
		res Result
		err error
	}
	done := make(chan landed, 1)
	go func() {
		res, err := l.Land(context.Background(), r.f.work)
		done <- landed{res, err}
	}()

	// The gate stage, still running at 8 minutes.
	r.awaitTimers(t, 1)
	awaitClosed(t, r.gateUp)
	r.clock.Advance(8 * time.Minute)
	gate := r.awaitEscalation(t)
	if gate.bead != "gt-abc" || gate.stage != "gate" {
		t.Errorf("escalation = %+v, want the gate stage of gt-abc", gate)
	}
	for _, want := range []string{"gt-abc", "gate stage", "8m0s", "Child pids: 4242 4243", "Nothing was killed", filepath.Join(r.evidence, "slow-gate.txt")} {
		if !strings.Contains(gate.message, want) {
			t.Errorf("gate escalation lacks %q:\n%s", want, gate.message)
		}
	}
	body, err := os.ReadFile(filepath.Join(r.evidence, "slow-gate.txt"))
	if err != nil {
		t.Fatalf("gate evidence file: %v", err)
	}
	for _, want := range []string{"4243  4242  4242 R    97.5", "go test ./...", "== open TCP peers", "104.18.1.1:443 (ESTABLISHED)"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("gate evidence lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(string(body), "unrelated") || strings.Contains(string(body), "om review") {
		t.Errorf("gate evidence lists processes outside the stage:\n%s", body)
	}
	if info, err := os.Stat(filepath.Join(r.evidence, "slow-gate.txt")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("evidence file mode = %v, %v; want 0600", info.Mode(), err)
	}
	close(r.gateGo)

	// The om stage gets its own alarm.
	r.awaitTimers(t, 1)
	awaitClosed(t, r.omUp)
	r.clock.Advance(8 * time.Minute)
	om := r.awaitEscalation(t)
	if om.stage != "om" || !strings.Contains(om.message, "Child pids: 5151") || !strings.Contains(om.message, "slow-om.txt") {
		t.Errorf("om escalation = %+v", om)
	}
	close(r.omGo)

	if got := <-done; got.err != nil {
		t.Fatalf("Land: %v", got.err)
	}
	if n := len(r.escalated); n != 0 {
		t.Errorf("%d escalations beyond one per stage", n)
	}
	select {
	case stage := <-r.stopped:
		t.Errorf("the %s stage was stopped by the alarm", stage)
	default:
	}
	log := r.out.String()
	for _, want := range []string{"[land] gt-abc: SLOW gate 8m0s\n", "[land] gt-abc: SLOW om 8m0s\n"} {
		if strings.Count(log, want) != 1 {
			t.Errorf("log has %d of %q, want 1:\n%s", strings.Count(log, want), want, log)
		}
	}
	if n := strings.Count(log, "SLOW"); n != 2 {
		t.Errorf("log has %d SLOW lines, want 2:\n%s", n, log)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lsofPID) != 2 || r.lsofPID[0] != "4242,4243" || r.lsofPID[1] != "5151" {
		t.Errorf("lsof asked about %v, want [4242,4243 5151]", r.lsofPID)
	}
}

// TestLandFastStagesAddNothing: stages that end before the threshold leave no
// timer armed, so no time passing later logs or files anything.
func TestLandFastStagesAddNothing(t *testing.T) {
	t.Parallel()
	r, l := newSlowRig(t, 8*time.Minute)
	close(r.gateGo)
	close(r.omGo)
	if _, err := l.Land(context.Background(), r.f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
	r.awaitTimers(t, 0)
	r.clock.Advance(time.Hour)
	if n := len(r.escalated); n != 0 {
		t.Errorf("%d escalations for a landing that was never slow", n)
	}
	if log := r.out.String(); strings.Contains(log, "SLOW") {
		t.Errorf("log names a slow stage:\n%s", log)
	}
	if _, err := os.Stat(r.evidence); err == nil {
		t.Error("evidence written for a landing that was never slow")
	}
}

// TestLandSlowAlarmOffWithoutAThreshold: no Slow, or After 0, arms no timer.
func TestLandSlowAlarmOffWithoutAThreshold(t *testing.T) {
	t.Parallel()
	for name, slow := range map[string]*SlowAlarm{"nil": nil, "zero": {After: 0}} {
		r, l := newSlowRig(t, 0)
		l.Slow = slow
		if slow != nil {
			slow.Clock = r.clock
		}
		close(r.gateGo)
		close(r.omGo)
		if _, err := l.Land(context.Background(), r.f.work); err != nil {
			t.Fatalf("%s: Land: %v", name, err)
		}
		r.awaitTimers(t, 0)
	}
}

// TestSlowEscalationSaysWhenNothingRuns: a stage waiting on the container
// slot has no child process, and the message says so instead of naming none.
func TestSlowEscalationSaysWhenNothingRuns(t *testing.T) {
	t.Parallel()
	msg := slowMessage(Work{BeadID: "gt-abc", Branch: "b", Head: "h"}, "gate", 9*time.Minute, 8*time.Minute, nil, nil, "", nil)
	if !strings.Contains(msg, "No child process is running") {
		t.Errorf("message = %q", msg)
	}
}

// TestSnapshotProcessesWhenToolsFail: an evidence command that fails is
// written into the evidence and never stops the alarm; lsof's exit 1 with no
// output means no TCP peers.
func TestSnapshotProcessesWhenToolsFail(t *testing.T) {
	t.Parallel()
	psFails := func(_ context.Context, name string, _ ...string) (string, error) {
		return "", errors.New(name + " is not installed")
	}
	snap := snapshotProcesses(context.Background(), []int{4242}, psFails)
	if !strings.Contains(snap.Text, "ps failed: ps is not installed") || len(snap.PIDs) != 0 {
		t.Errorf("snapshot = %+v", snap)
	}

	noPeers := func(_ context.Context, name string, _ ...string) (string, error) {
		if name == "ps" {
			return cannedPS, nil
		}
		return "", &exec.ExitError{}
	}
	snap = snapshotProcesses(context.Background(), []int{4242}, noPeers)
	if !strings.Contains(snap.Text, "== open TCP peers (lsof -nP -a -iTCP -p <pids>)\nnone\n") || len(snap.PIDs) != 2 {
		t.Errorf("snapshot = %+v", snap)
	}

	lsofFails := func(_ context.Context, name string, _ ...string) (string, error) {
		if name == "ps" {
			return cannedPS, nil
		}
		return "", errors.New("lsof is not installed")
	}
	snap = snapshotProcesses(context.Background(), []int{4242}, lsofFails)
	if !strings.Contains(snap.Text, "lsof failed: lsof is not installed") {
		t.Errorf("snapshot = %+v", snap)
	}
}

func TestParsePSAndDescendants(t *testing.T) {
	t.Parallel()
	ps := `    1     0     1 Ss    0.0 10-01:02:03 /sbin/launchd
  100     1   100 Ss    0.1       08:00 sh -c make gate-test
  101   100   100 S     0.0       07:59 make gate-test
  102   101   100 R    98.2       07:58 go test ./...
  103   102   103 S     0.0       07:57 pkg.test -test.run X
  104     1   100 S     0.0       07:50 reparented-child
  200     1   200 S     0.0       07:00 unrelated
`
	var got []int
	for _, r := range descendants(parsePS(ps), []int{100}) {
		got = append(got, r.pid)
	}
	if want := []int{100, 101, 102, 103, 104}; !equalInts(got, want) {
		t.Errorf("descendants of 100 = %v, want %v", got, want)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// awaitClosed blocks until ch is closed.
func awaitClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal("stage never started")
	}
}
