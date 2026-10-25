package session

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/liveness"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/tmux/tmuxfake"
)

// A sandbox whose gateway phase is a terminal failure cannot answer tmux — an
// exec into it is refused — and the gateway keeps listing it, so without a
// phase rule its seat reads Unknown forever (gt-tj446). Provisioning keeps its
// grace, a Ready answer starts the clock over, and a gateway that cannot answer
// keeps the seat.
func TestSandboxLivenessDeadAfterThePhaseGrace(t *testing.T) {
	t.Parallel()
	const session = "gt-gastown-sandbox-phase"
	t0 := time.Unix(1760000000, 0)
	tests := []struct {
		name  string
		after time.Duration
		want  liveness.Verdict
	}{
		{"the first reading starts the clock", 0, liveness.Unknown},
		{"a minute short of the grace still waits", SandboxDeadGrace - time.Minute, liveness.Unknown},
		{"the grace makes it dead", SandboxDeadGrace, liveness.Dead},
		{"past the grace stays dead", SandboxDeadGrace + 10*time.Minute, liveness.Dead},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, _ := sandboxPhaseBackend(t, session, phaseErrorReply(session))

			first := b.ProgressSample(session, liveness.Input{Now: t0})
			if first.Verdict != liveness.Unknown {
				t.Fatalf("first reading = %v (%s), want unknown while the clock starts", first.Verdict, first.Reason)
			}
			res := b.ProgressSample(session, liveness.Input{Now: t0.Add(tt.after)})
			if res.Verdict != tt.want {
				t.Fatalf("verdict at %s = %v (%s, err %v), want %v", tt.after, res.Verdict, res.Reason, res.Err, tt.want)
			}
			if tt.want == liveness.Dead {
				if !strings.Contains(res.Reason, "phase Error") {
					t.Errorf("reason %q does not name the phase", res.Reason)
				}
				if res.Sample == nil || res.Sample.DeadSamples != 1 {
					t.Errorf("sample = %+v, want one dead sample", res.Sample)
				}
			}
		})
	}
}

// The grace runs from the phase's own transition, so a patrol that first looks
// minutes into the failure declares the seat dead at once instead of holding it
// for the sample lag plus the grace (gt-j18af).
func TestSandboxLivenessDeadFromTheGatewayTransitionTime(t *testing.T) {
	t.Parallel()
	const session = "gt-gastown-sandbox-late-sample"
	t0 := time.Unix(1760000000, 0)
	b, _ := sandboxPhaseBackend(t, session, phaseErrorSince(session, t0))

	// The first sample is minutes after the failure: the grace is already
	// spent, so one sample is all the seat needs.
	res := b.ProgressSample(session, liveness.Input{Now: t0.Add(6 * time.Minute)})
	if res.Verdict != liveness.Dead {
		t.Fatalf("verdict six minutes after the failure = %v (%s), want dead", res.Verdict, res.Reason)
	}
	if !strings.Contains(res.Reason, "phase Error") || !strings.Contains(res.Reason, "6m0s") {
		t.Errorf("reason %q does not measure the failure from the gateway's transition time", res.Reason)
	}
	if res.Sample == nil || res.Sample.DeadSamples != 1 {
		t.Errorf("sample = %+v, want one dead sample", res.Sample)
	}
}

// A sample taken before the grace is spent still waits, even when it is the
// first: the clock is the transition time, not the reading.
func TestSandboxLivenessTransitionTimeKeepsTheGrace(t *testing.T) {
	t.Parallel()
	const session = "gt-gastown-sandbox-grace-kept"
	t0 := time.Unix(1760000000, 0)
	b, _ := sandboxPhaseBackend(t, session, phaseErrorSince(session, t0))

	if res := b.ProgressSample(session, liveness.Input{Now: t0.Add(SandboxDeadGrace - time.Second)}); res.Verdict == liveness.Dead {
		t.Fatalf("dead one second short of the grace: %s", res.Reason)
	}
	if res := b.ProgressSample(session, liveness.Input{Now: t0.Add(SandboxDeadGrace)}); res.Verdict != liveness.Dead {
		t.Fatalf("verdict at the grace = %v (%s), want dead", res.Verdict, res.Reason)
	}
}

// A gateway clock ahead of the host's is not a failure duration: the seat
// waits for readings to show the grace, and the future time is not kept.
func TestSandboxLivenessFutureTransitionTimeWaits(t *testing.T) {
	t.Parallel()
	const session = "gt-gastown-sandbox-skew"
	t0 := time.Unix(1760000000, 0)
	b, _ := sandboxPhaseBackend(t, session, phaseErrorSince(session, t0.Add(10*time.Minute)))

	if res := b.ProgressSample(session, liveness.Input{Now: t0}); res.Verdict == liveness.Dead {
		t.Fatalf("dead on a transition time ahead of the host clock: %s", res.Reason)
	}
	if res := b.ProgressSample(session, liveness.Input{Now: t0.Add(time.Minute)}); res.Verdict == liveness.Dead {
		t.Fatalf("dead a minute in, from a future transition time: %s", res.Reason)
	}
	if res := b.ProgressSample(session, liveness.Input{Now: t0.Add(SandboxDeadGrace)}); res.Verdict != liveness.Dead {
		t.Fatalf("verdict at the grace = %v (%s), want dead", res.Verdict, res.Reason)
	}
}

// A transition time the gateway spells in a way this client cannot read costs
// only the head start: the failure is then measured from the first reading, as
// before.
func TestSandboxLivenessUnreadableTransitionTimeMeasuredFromTheReading(t *testing.T) {
	t.Parallel()
	const session = "gt-gastown-sandbox-bad-time"
	t0 := time.Unix(1760000000, 0)
	reply := `{"name":"` + session + `","phase":"Error","conditions":[{"type":"Ready","status":"False","lastTransitionTime":"yesterday"}]}`
	b, _ := sandboxPhaseBackend(t, session, reply)

	if res := b.ProgressSample(session, liveness.Input{Now: t0}); res.Verdict != liveness.Unknown {
		t.Fatalf("first reading = %v (%s), want unknown while the clock starts", res.Verdict, res.Reason)
	}
	if res := b.ProgressSample(session, liveness.Input{Now: t0.Add(SandboxDeadGrace)}); res.Verdict != liveness.Dead {
		t.Fatalf("verdict at the grace = %v (%s), want dead", res.Verdict, res.Reason)
	}
}

// A tmux that answers is proof the sandbox runs, so the gateway is not asked
// for its phase: a healthy seat costs no gateway call per sample, and the
// failure clock it may carry is dropped (om f9ae48011e3e).
func TestSandboxLivenessAnAnsweringTmuxIsNotAsked(t *testing.T) {
	t.Parallel()
	const session = "gt-gastown-sandbox-answering"
	t0 := time.Unix(1760000000, 0)
	b, fake := sandboxPhaseBackend(t, session, phaseErrorReply(session))

	// It failed once, so the clock is running.
	if res := b.ProgressSample(session, liveness.Input{Now: t0}); res.Verdict != liveness.Unknown {
		t.Fatalf("failing reading = %v (%s), want unknown while the clock starts", res.Verdict, res.Reason)
	}

	// It answers again. The gateway's reply is still Error; the answer stands.
	b.probe = func(string) liveness.Probe { return answeringProbe(t, session) }
	before := gatewayQuestions(fake)
	res := b.ProgressSample(session, liveness.Input{Now: t0.Add(time.Minute)})
	if res.Verdict != liveness.Alive {
		t.Fatalf("answering reading = %v (%s), want alive", res.Verdict, res.Reason)
	}
	if asked := gatewayQuestions(fake) - before; asked != 0 {
		t.Fatalf("the gateway was asked for the phase %d time(s) for a sandbox whose tmux answered, want 0", asked)
	}

	// The clock went with it: the next failure is measured from its own reading.
	b.probe = func(string) liveness.Probe {
		return unansweredProbe{err: errors.New("sandbox '" + session + "' is not ready (phase: Error)")}
	}
	res = b.ProgressSample(session, liveness.Input{Now: t0.Add(2 * time.Minute)})
	if res.Verdict == liveness.Dead {
		t.Fatalf("dead %s after the answering reading: %s", time.Minute, res.Reason)
	}
}

// The dead sample is counted on top of the one the record carried, so a patrol
// that confirms deaths across ticks reaches its threshold exactly as it does
// for a failed tmux probe.
func TestSandboxLivenessCountsTheDeadSampleFromTheRecord(t *testing.T) {
	t.Parallel()
	const session = "gt-gastown-sandbox-samples"
	t0 := time.Unix(1760000000, 0)
	b, _ := sandboxPhaseBackend(t, session, phaseErrorReply(session))
	prev := &intent.Progress{SampledAt: t0.Add(-time.Minute), DeadSamples: 2}

	b.ProgressSample(session, liveness.Input{Now: t0})
	res := b.ProgressSample(session, liveness.Input{Now: t0.Add(SandboxDeadGrace), Prev: prev})

	if res.Verdict != liveness.Dead {
		t.Fatalf("verdict = %v (%s), want dead", res.Verdict, res.Reason)
	}
	if res.Sample == nil || res.Sample.DeadSamples != 3 {
		t.Fatalf("sample = %+v, want 3 dead samples", res.Sample)
	}
}

// A sandbox that answers its tmux starts the phase clock over: the seat is
// alive, whatever its phase label said a moment ago, and a later failure is
// measured from that answer.
func TestSandboxLivenessAnAnsweringTmuxResetsThePhaseClock(t *testing.T) {
	t.Parallel()
	assertPhaseClockRestartsAfterTheBlip(t, func(t *testing.T, b *openshellBackend, _ *sandbox.Fake, t0 time.Time) time.Time {
		// The sandbox comes back: its tmux answers again.
		b.probe = func(string) liveness.Probe { return answeringProbe(t, "gt-gastown-sandbox-blip") }
		res := b.ProgressSample("gt-gastown-sandbox-blip", liveness.Input{Now: t0.Add(2 * time.Minute)})
		if res.Verdict != liveness.Alive {
			t.Fatalf("blip reading = %v (%s), want alive", res.Verdict, res.Reason)
		}
		return t0.Add(6 * time.Minute)
	})
}

// A phase the sandbox can leave — Ready, Provisioning, Starting — starts the
// clock over too, even while its tmux still cannot be asked: the sandbox is
// not failing, and the next failure is measured from its own first reading.
func TestSandboxLivenessANonTerminalPhaseResetsThePhaseClock(t *testing.T) {
	t.Parallel()
	assertPhaseClockRestartsAfterTheBlip(t, func(t *testing.T, b *openshellBackend, fake *sandbox.Fake, t0 time.Time) time.Time {
		// No tmux answer at all; only the gateway's phase changes to Ready.
		fake.Reply["get"] = openshellReadyReply
		res := b.ProgressSample("gt-gastown-sandbox-blip", liveness.Input{Now: t0.Add(2 * time.Minute)})
		if res.Verdict != liveness.Unknown {
			t.Fatalf("ready reading = %v (%s), want the tmux answer standing", res.Verdict, res.Reason)
		}
		return t0.Add(6 * time.Minute)
	})
}

// assertPhaseClockRestartsAfterTheBlip walks the one shape both resets share: a
// first failure at t0, the blip, then a failure that must be measured from the
// blip and not from t0.
func assertPhaseClockRestartsAfterTheBlip(t *testing.T, blip func(t *testing.T, b *openshellBackend, fake *sandbox.Fake, t0 time.Time) time.Time) {
	t.Helper()
	const session = "gt-gastown-sandbox-blip"
	t0 := time.Unix(1760000000, 0)
	b, fake := sandboxPhaseBackend(t, session, phaseErrorReply(session))
	if res := b.ProgressSample(session, liveness.Input{Now: t0}); res.Verdict != liveness.Unknown {
		t.Fatalf("first failing reading = %v (%s), want unknown while the clock starts", res.Verdict, res.Reason)
	}

	again := blip(t, b, fake, t0)

	// It fails again. The clock starts at this reading, not at the first one.
	fake.Reply["get"] = phaseErrorReply(session)
	b.probe = func(string) liveness.Probe {
		return unansweredProbe{err: errors.New("sandbox is not ready (phase: Error)")}
	}
	res := b.ProgressSample(session, liveness.Input{Now: again})
	if res.Verdict != liveness.Unknown {
		t.Fatalf("first failure after the blip = %v (%s), want unknown", res.Verdict, res.Reason)
	}
	res = b.ProgressSample(session, liveness.Input{Now: again.Add(SandboxDeadGrace - time.Second)})
	if res.Verdict == liveness.Dead {
		t.Fatalf("dead one second short of the grace for the new failure: %s", res.Reason)
	}
	res = b.ProgressSample(session, liveness.Input{Now: again.Add(SandboxDeadGrace)})
	if res.Verdict != liveness.Dead {
		t.Fatalf("verdict a grace after the new failure = %v (%s), want dead", res.Verdict, res.Reason)
	}
}

// A sandbox still coming up is not dead however long it takes: it has its own
// grace, and the phase rule must not eat it.
func TestSandboxLivenessProvisioningIsNeverDead(t *testing.T) {
	t.Parallel()
	const session = "gt-gastown-sandbox-provisioning"
	t0 := time.Unix(1760000000, 0)
	b, _ := sandboxPhaseBackend(t, session, openshellProvisioningReply)

	b.ProgressSample(session, liveness.Input{Now: t0})
	res := b.ProgressSample(session, liveness.Input{Now: t0.Add(2 * time.Hour)})

	if res.Verdict != liveness.Unknown {
		t.Fatalf("verdict = %v (%s), want unknown", res.Verdict, res.Reason)
	}
}

// A gateway that cannot answer is an outage, never a batch of dead sandboxes:
// the seat is kept even after many readings.
func TestSandboxLivenessUnreadableGatewayKeepsTheSeat(t *testing.T) {
	t.Parallel()
	const session = "gt-gastown-sandbox-outage"
	t0 := time.Unix(1760000000, 0)
	b, fake := sandboxPhaseBackend(t, session, phaseErrorReply(session))
	fake.Err = map[string]error{"get": errors.New("exit status 1")}
	fake.Stderr = map[string]string{"get": "connection refused"}
	fake.Reply = map[string]string{}

	for _, at := range []time.Duration{0, time.Hour, 2 * time.Hour} {
		res := b.ProgressSample(session, liveness.Input{Now: t0.Add(at)})
		if res.Verdict != liveness.Unknown {
			t.Fatalf("verdict at %s = %v (%s), want unknown while the gateway is unreadable", at, res.Verdict, res.Reason)
		}
	}
}

// Only a failed phase is terminal. Provisioning, Pending, Starting, Ready,
// Stopped and an empty phase keep their existing grace.
func TestSandboxPhaseTerminal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		phase string
		want  bool
	}{
		{"Error", true},
		{"error", true},
		{" Failed ", true},
		{"Provisioning", false},
		{"Pending", false},
		{"Starting", false},
		{"Ready", false},
		{"Stopped", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.phase, func(t *testing.T) {
			t.Parallel()
			if got := sandboxPhaseTerminal(tt.phase); got != tt.want {
				t.Fatalf("sandboxPhaseTerminal(%q) = %v, want %v", tt.phase, got, tt.want)
			}
		})
	}
}

// The phase clock belongs to the process, keyed by sandbox name, so the patrol
// judges one sandbox across ticks even though each sample builds its backend
// from the registry anew (gt-tj446). Two backends over one watch are two ticks.
func TestSandboxPhaseClockIsSharedAcrossTicks(t *testing.T) {
	t.Parallel()
	const session = "gt-gastown-sandbox-shared"
	t0 := time.Unix(1760000000, 0)
	watch := newSandboxPhaseWatch()
	tick := func() *openshellBackend {
		b, _ := sandboxPhaseBackend(t, session, phaseErrorReply(session))
		b.phaseWatch = watch
		return b
	}

	if res := tick().ProgressSample(session, liveness.Input{Now: t0}); res.Verdict != liveness.Unknown {
		t.Fatalf("first tick = %v (%s), want unknown while the clock starts", res.Verdict, res.Reason)
	}
	if res := tick().ProgressSample(session, liveness.Input{Now: t0.Add(SandboxDeadGrace - time.Second)}); res.Verdict == liveness.Dead {
		t.Fatalf("dead one second short of the grace across ticks: %s", res.Reason)
	}
	if res := tick().ProgressSample(session, liveness.Input{Now: t0.Add(SandboxDeadGrace)}); res.Verdict != liveness.Dead {
		t.Fatalf("a grace across ticks = %v (%s), want dead", res.Verdict, res.Reason)
	}
}

// A backend built from a registry record carries no watch of its own, so the
// patrol's recordBackend — the backend every daemon tick builds — reads the
// process's clock. A backend with a watch of its own is a test's seam.
func TestRecordBackendReadsTheProcessClock(t *testing.T) {
	t.Parallel()
	b := recordBackend(t.TempDir(), SandboxRecord{Session: "gt-gastown-sandbox-record", Profile: "p", Gateway: "g"})
	if b.phaseWatch != nil {
		t.Fatal("recordBackend carries a watch of its own; the daemon's ticks would each start a fresh clock")
	}
	if b.phases() != sandboxPhases {
		t.Fatal("recordBackend does not read the process clock")
	}
}

// A clock that reads backwards — a wall-clock step, or a reading taken with an
// earlier clock — is never a failure duration: the seat waits, and the start
// the watch first recorded stays where it was.
func TestSandboxPhaseClockIgnoresABackwardsReading(t *testing.T) {
	t.Parallel()
	w := newSandboxPhaseWatch()
	t0 := time.Unix(1760000000, 0)

	if got := w.failing("gt-a", time.Time{}, t0); got != 0 {
		t.Fatalf("first reading = %s, want 0", got)
	}
	if got := w.failing("gt-a", time.Time{}, t0.Add(-time.Hour)); got != 0 {
		t.Fatalf("backwards reading = %s, want 0", got)
	}
	if got := w.failing("gt-a", time.Time{}, t0.Add(time.Minute)); got != time.Minute {
		t.Fatalf("a minute later = %s, want the failure measured from the first reading", got)
	}
	// A sandbox that answers is forgotten, so its next failure starts afresh.
	w.forget("gt-a")
	if got := w.failing("gt-a", time.Time{}, t0.Add(2*time.Minute)); got != 0 {
		t.Fatalf("reading after forget = %s, want a new clock", got)
	}
}

// sandboxPhaseBackend is an openshell backend for session whose gateway reports
// reply and whose tmux cannot be asked, the way a sandbox in a terminal failure
// phase answers: openshell refuses every exec into it.
func sandboxPhaseBackend(t *testing.T, session, reply string) (*openshellBackend, *sandbox.Fake) {
	t.Helper()
	fake := &sandbox.Fake{Reply: map[string]string{"get": reply}}
	b, _ := openshellTestBackend(t.TempDir(), openshellTestProfile, fake)
	b.phaseWatch = newSandboxPhaseWatch()
	b.probe = func(string) liveness.Probe {
		return unansweredProbe{err: errors.New("sandbox '" + session + "' is not ready (phase: Error)")}
	}
	return b, fake
}

// answeringProbe is an in-sandbox tmux that has session and a live agent.
func answeringProbe(t *testing.T, session string) *sandboxProbe {
	t.Helper()
	srv := &sandboxProbe{Server: tmuxfake.New(clockwork.NewFakeClockAt(time.Unix(1760000000, 0))), agentAlive: true}
	if err := srv.NewSession(session, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	return srv
}

// gatewayQuestions counts the phase questions among the fake's calls. A sample
// also execs into the sandbox for the transcript, so the count cannot be the
// call count.
func gatewayQuestions(fake *sandbox.Fake) int {
	n := 0
	for _, argv := range fake.Calls() {
		for _, arg := range argv[1:] {
			if arg == "get" {
				n++
				break
			}
		}
	}
	return n
}

func phaseErrorReply(session string) string {
	return `{"name":"` + session + `","phase":"Error","conditions":[{"type":"Ready","status":"False"}]}`
}

// phaseErrorSince is the gateway's answer for a sandbox that stopped being
// ready at since and has stayed Error.
func phaseErrorSince(session string, since time.Time) string {
	return `{"name":"` + session + `","phase":"Error","conditions":[{"type":"Ready","status":"False","lastTransitionTime":"` + since.UTC().Format(time.RFC3339) + `"}]}`
}

// unansweredProbe is an in-sandbox tmux that refuses every question, which is
// what a sandbox in a terminal failure phase answers.
type unansweredProbe struct{ err error }

func (p unansweredProbe) HasSession(string) (bool, error)          { return false, p.err }
func (p unansweredProbe) IsAgentAliveChecked(string) (bool, error) { return false, p.err }
func (p unansweredProbe) CapturePane(string, int) (string, error)  { return "", p.err }
func (p unansweredProbe) PaneDead(string) (bool, error)            { return false, p.err }

func (p unansweredProbe) GetSessionCreatedTime(string) (time.Time, error) {
	return time.Time{}, p.err
}

// With no transition time on the first reading, the clock starts at that
// reading; a later transition time that predates it moves the clock back.
func TestSandboxPhaseLaterEarlierTransitionTimeMovesTheClock(t *testing.T) {
	t.Parallel()
	w := newSandboxPhaseWatch()
	t0 := time.Unix(1760000000, 0)

	if got := w.failing("gt-a", time.Time{}, t0); got != 0 {
		t.Fatalf("first reading = %s, want 0", got)
	}
	if got := w.failing("gt-a", t0.Add(-time.Hour), t0.Add(time.Minute)); got != time.Hour+time.Minute {
		t.Fatalf("reading with an earlier transition time = %s, want 1h1m0s from the transition", got)
	}
}

// A transition time later than the clock start does not move the clock.
func TestSandboxPhaseLaterTransitionTimeDoesNotMoveTheClock(t *testing.T) {
	t.Parallel()
	w := newSandboxPhaseWatch()
	t0 := time.Unix(1760000000, 0)

	if got := w.failing("gt-a", t0, t0); got != 0 {
		t.Fatalf("first reading = %s, want 0", got)
	}
	if got := w.failing("gt-a", t0.Add(30*time.Second), t0.Add(time.Minute)); got != time.Minute {
		t.Fatalf("reading with a later transition time = %s, want the minute from the first transition", got)
	}
}

// A transition time older than the first reading is the start of the failure,
// so on that reading it spends the grace at once.
func TestSandboxPhaseStaleTransitionTimeOnTheFirstReadingSpendsTheGrace(t *testing.T) {
	t.Parallel()
	w := newSandboxPhaseWatch()
	t0 := time.Unix(1760000000, 0)

	if got := w.failing("gt-b", t0.Add(-time.Hour), t0); got != time.Hour {
		t.Fatalf("first reading with a transition time an hour old = %s, want 1h0m0s", got)
	}
}
