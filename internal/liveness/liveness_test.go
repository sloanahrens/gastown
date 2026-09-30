package liveness

import (
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/intent"
)

// probe is a scripted Probe.
type probe struct {
	hasErr, aliveErr, paneErr, deadErr error
	has, alive, paneDead               bool
	pane                               string
	created                            time.Time
}

func (p *probe) HasSession(string) (bool, error)          { return p.has, p.hasErr }
func (p *probe) IsAgentAliveChecked(string) (bool, error) { return p.alive, p.aliveErr }
func (p *probe) CapturePane(string, int) (string, error)  { return p.pane, p.paneErr }
func (p *probe) PaneDead(string) (bool, error)            { return p.paneDead, p.deadErr }
func (p *probe) GetSessionCreatedTime(string) (time.Time, error) {
	return p.created, nil
}

var (
	t0      = time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	created = t0.Add(-time.Hour)
)

func live(pane string) *probe {
	return &probe{has: true, alive: true, pane: pane, created: created}
}

// transcript returns a Transcript func reporting a fixed file state.
func transcript(mtime time.Time, size int64) func(string) (string, time.Time, int64, error) {
	return func(string) (string, time.Time, int64, error) { return "/t/s.jsonl", mtime, size, nil }
}

func noTranscript(string) (string, time.Time, int64, error) { return "", time.Time{}, 0, nil }

func TestTmuxErrorIsUnknownAndKeepsTheSample(t *testing.T) {
	t.Parallel()
	prev := &intent.Progress{PaneHash: "abc", ChangedAt: t0.Add(-40 * time.Minute)}
	r := Assess(&probe{hasErr: errors.New("tmux: server timed out")}, Input{Session: "gt-flint", Prev: prev, Now: t0, Transcript: noTranscript})
	if r.Verdict != Unknown || r.Err == nil {
		t.Fatalf("verdict = %v err = %v, want Unknown with the error", r.Verdict, r.Err)
	}
	if r.Sample == nil || *r.Sample != *prev || r.Sample == prev {
		t.Fatalf("an Unknown verdict must hand back an unchanged copy of the previous sample")
	}
}

func TestAgentQueryErrorIsUnknown(t *testing.T) {
	t.Parallel()
	p := &probe{has: true, aliveErr: errors.New("show-environment: timeout")}
	if r := Assess(p, Input{Session: "s", Now: t0, Transcript: noTranscript}); r.Verdict != Unknown {
		t.Fatalf("verdict = %v, want Unknown", r.Verdict)
	}
}

// A pane kept after its process exited cannot be asked for its command, so
// the agent query errors; tmux still says the pane is dead, and that is a
// confirmed Dead. Without this the seat would read Unknown forever.
func TestDeadPaneIsDeadNotUnknown(t *testing.T) {
	t.Parallel()
	p := &probe{has: true, aliveErr: errors.New("empty command for session"), paneDead: true}
	if r := Assess(p, Input{Session: "s", Now: t0, Transcript: noTranscript}); r.Verdict != Dead || r.Reason != ReasonAgentGone {
		t.Fatalf("dead pane: %v %q, want Dead %q", r.Verdict, r.Reason, ReasonAgentGone)
	}
	p.paneDead, p.deadErr = false, errors.New("server exited")
	if r := Assess(p, Input{Session: "s", Now: t0, Transcript: noTranscript}); r.Verdict != Unknown {
		t.Fatalf("pane_dead unanswered: %v, want Unknown", r.Verdict)
	}
}

func TestNoSessionIsDeadAndCountsSamples(t *testing.T) {
	t.Parallel()
	r := Assess(&probe{}, Input{Session: "s", Now: t0, Transcript: noTranscript})
	if r.Verdict != Dead || r.Sample.DeadSamples != 1 {
		t.Fatalf("got %v dead=%d, want Dead 1", r.Verdict, r.Sample.DeadSamples)
	}
	r = Assess(&probe{has: true}, Input{Session: "s", Prev: r.Sample, Now: t0.Add(3 * time.Minute), Transcript: noTranscript})
	if r.Verdict != Dead || r.Sample.DeadSamples != 2 {
		t.Fatalf("agent gone from a live session: got %v dead=%d, want Dead 2", r.Verdict, r.Sample.DeadSamples)
	}
	r = Assess(live("work"), Input{Session: "s", Prev: r.Sample, Now: t0.Add(6 * time.Minute), Transcript: noTranscript})
	if r.Verdict != Alive || r.Sample.DeadSamples != 0 {
		t.Fatalf("recovered: got %v dead=%d, want Alive 0", r.Verdict, r.Sample.DeadSamples)
	}
}

func TestUnchangedEvidenceStallsOnlyAfterTheWindow(t *testing.T) {
	t.Parallel()
	in := Input{Session: "s", Now: t0, Transcript: transcript(t0.Add(-time.Minute), 100)}
	r := Assess(live("same screen"), in)
	if r.Verdict != Alive {
		t.Fatalf("baseline = %v, want Alive", r.Verdict)
	}
	in.Prev, in.Now = r.Sample, t0.Add(29*time.Minute)
	r = Assess(live("same screen"), in)
	if r.Verdict != Alive || r.QuietFor != 29*time.Minute {
		t.Fatalf("29m quiet = %v quiet=%v, want Alive 29m", r.Verdict, r.QuietFor)
	}
	in.Prev, in.Now = r.Sample, t0.Add(30*time.Minute)
	r = Assess(live("same screen"), in)
	if r.Verdict != Stalled {
		t.Fatalf("30m quiet = %v, want Stalled", r.Verdict)
	}
}

func TestAnySourceOfChangeIsProgress(t *testing.T) {
	t.Parallel()
	base := Assess(live("screen"), Input{Session: "s", Now: t0, Transcript: transcript(t0, 100), Heartbeat: &Heartbeat{Cycle: 4}})
	later := t0.Add(45 * time.Minute)
	cases := map[string]struct {
		p  *probe
		tr func(string) (string, time.Time, int64, error)
		hb *Heartbeat
	}{
		"pane":            {live("screen moved"), transcript(t0, 100), &Heartbeat{Cycle: 4}},
		"transcript size": {live("screen"), transcript(t0, 250), &Heartbeat{Cycle: 4}},
		"transcript time": {live("screen"), transcript(later, 100), &Heartbeat{Cycle: 4}},
		"heartbeat cycle": {live("screen"), transcript(t0, 100), &Heartbeat{Cycle: 5}},
	}
	for name, c := range cases {
		r := Assess(c.p, Input{Session: "s", Prev: base.Sample, Now: later, Transcript: c.tr, Heartbeat: c.hb})
		if r.Verdict != Alive || !r.Sample.ChangedAt.Equal(later) {
			t.Errorf("%s: verdict=%v changedAt=%v, want Alive changed now", name, r.Verdict, r.Sample.ChangedAt)
		}
	}
	r := Assess(live("screen"), Input{Session: "s", Prev: base.Sample, Now: later, Transcript: transcript(t0, 100), Heartbeat: &Heartbeat{Cycle: 4}})
	if r.Verdict != Stalled {
		t.Errorf("nothing moved in 45m: verdict=%v, want Stalled", r.Verdict)
	}
}

// TestLongTurnWithMovingTranscriptIsAlive: an 8-hour turn is not a stall
// when its transcript keeps growing (gt-xb27, opal).
func TestLongTurnWithMovingTranscriptIsAlive(t *testing.T) {
	t.Parallel()
	var prev *intent.Progress
	for i := 0; i <= 160; i++ { // every 3m for 8h
		now := t0.Add(time.Duration(i) * 3 * time.Minute)
		r := Assess(live("Imagining... (8h 54m)"), Input{Session: "s", Prev: prev, Now: now, Transcript: transcript(now, int64(1000+i))})
		if r.Verdict != Alive {
			t.Fatalf("sample %d: verdict = %v, want Alive", i, r.Verdict)
		}
		prev = r.Sample
	}
}

func TestNewIncarnationRebases(t *testing.T) {
	t.Parallel()
	old := &intent.Progress{SessionCreated: created.Add(-time.Hour), PaneHash: "x", ChangedAt: t0.Add(-5 * time.Hour)}
	r := Assess(live("screen"), Input{Session: "s", Prev: old, Now: t0, Transcript: noTranscript})
	if r.Verdict != Alive || !r.Sample.ChangedAt.Equal(t0) || !r.Sample.SessionCreated.Equal(created) {
		t.Fatalf("new session: %v %+v, want a fresh Alive baseline", r.Verdict, r.Sample)
	}
}

func TestNoEvidenceIsNeverStalled(t *testing.T) {
	t.Parallel()
	p := &probe{has: true, alive: true, paneErr: errors.New("capture failed"), created: created}
	r := Assess(p, Input{Session: "s", Now: t0, Transcript: noTranscript})
	r = Assess(p, Input{Session: "s", Prev: r.Sample, Now: t0.Add(5 * time.Hour), Transcript: noTranscript})
	if r.Verdict != Alive {
		t.Fatalf("no evidence for 5h: verdict = %v, want Alive (never Stalled without evidence)", r.Verdict)
	}
}

func TestMissingEvidenceCarriesForward(t *testing.T) {
	t.Parallel()
	r := Assess(live("screen"), Input{Session: "s", Now: t0, Transcript: noTranscript})
	flaky := live("")
	flaky.paneErr = errors.New("capture failed")
	r = Assess(flaky, Input{Session: "s", Prev: r.Sample, Now: t0.Add(10 * time.Minute), Transcript: noTranscript})
	if r.Sample.PaneHash == "" {
		t.Fatalf("a failed capture dropped the pane hash; the next sample would read as a change")
	}
	r = Assess(live("screen"), Input{Session: "s", Prev: r.Sample, Now: t0.Add(31 * time.Minute), Transcript: noTranscript})
	if r.Verdict != Stalled {
		t.Fatalf("verdict = %v, want Stalled (the capture failure was not progress)", r.Verdict)
	}
}

func TestVerdictString(t *testing.T) {
	t.Parallel()
	for v, want := range map[Verdict]string{Unknown: "unknown", Alive: "alive", Dead: "dead", Stalled: "stalled"} {
		if v.String() != want {
			t.Errorf("%d.String() = %q, want %q", v, v.String(), want)
		}
	}
}
