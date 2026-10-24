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

// The phase decides even when the sandbox's tmux still answers: what the seat
// needs is a sandbox openshell calls ready, and one that has said Error for the
// whole grace is dead whatever else it reports (gt-tj446).
func TestSandboxLivenessDeadPhaseBeatsAnAnsweringTmux(t *testing.T) {
	t.Parallel()
	const session = "gt-gastown-sandbox-answering"
	t0 := time.Unix(1760000000, 0)
	b, _ := sandboxPhaseBackend(t, session, phaseErrorReply(session))
	b.probe = func(string) liveness.Probe { return answeringProbe(t, session) }

	b.ProgressSample(session, liveness.Input{Now: t0})
	res := b.ProgressSample(session, liveness.Input{Now: t0.Add(SandboxDeadGrace)})

	if res.Verdict != liveness.Dead {
		t.Fatalf("verdict = %v (%s), want dead", res.Verdict, res.Reason)
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
func TestSandboxLivenessReadyBlipResetsThePhaseClock(t *testing.T) {
	t.Parallel()
	const session = "gt-gastown-sandbox-blip"
	t0 := time.Unix(1760000000, 0)
	b, fake := sandboxPhaseBackend(t, session, phaseErrorReply(session))

	b.ProgressSample(session, liveness.Input{Now: t0})

	// The sandbox comes back: the gateway says Ready and its tmux answers.
	fake.Reply["get"] = openshellReadyReply
	b.probe = func(string) liveness.Probe { return answeringProbe(t, session) }
	res := b.ProgressSample(session, liveness.Input{Now: t0.Add(2 * time.Minute)})
	if res.Verdict != liveness.Alive {
		t.Fatalf("ready reading = %v (%s), want alive", res.Verdict, res.Reason)
	}

	// It fails again. The clock starts at this reading, not at the first one.
	fake.Reply["get"] = phaseErrorReply(session)
	b.probe = func(string) liveness.Probe {
		return unansweredProbe{err: errors.New("sandbox is not ready (phase: Error)")}
	}
	res = b.ProgressSample(session, liveness.Input{Now: t0.Add(6 * time.Minute)})
	if res.Verdict != liveness.Unknown {
		t.Fatalf("first failure after the blip = %v (%s), want unknown", res.Verdict, res.Reason)
	}
	res = b.ProgressSample(session, liveness.Input{Now: t0.Add(6*time.Minute + SandboxDeadGrace - time.Second)})
	if res.Verdict == liveness.Dead {
		t.Fatalf("dead one second short of the grace for the new failure: %s", res.Reason)
	}
	res = b.ProgressSample(session, liveness.Input{Now: t0.Add(6*time.Minute + SandboxDeadGrace)})
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

// A host session has no registry record, so the phase read has nothing to ask
// the gateway about; a session whose sandbox reports Error is terminal, and a
// gateway that cannot answer is an error so the seat is kept.
func TestSandboxPhaseTerminalOfASession(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if terminal, err := SandboxPhaseTerminal(town, openshellTestSession); err != nil || terminal {
		t.Fatalf("host session = (%v, %v), want (false, nil)", terminal, err)
	}

	tests := []struct {
		name    string
		phaseOf func(string, string) (string, error)
		want    bool
		wantErr bool
	}{
		{"terminal", func(string, string) (string, error) { return "Error", nil }, true, false},
		{"not terminal", func(string, string) (string, error) { return "Provisioning", nil }, false, false},
		{"gateway unreadable", func(string, string) (string, error) { return "", errors.New("connection refused") }, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := sandboxPhaseTerminalWith("", openshellTestSession, tt.phaseOf)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("terminal = %v, want %v", got, tt.want)
			}
		})
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

func phaseErrorReply(session string) string {
	return `{"name":"` + session + `","phase":"Error","conditions":[{"type":"Ready","status":"False"}]}`
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
