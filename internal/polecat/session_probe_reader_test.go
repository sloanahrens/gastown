package polecat

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
)

// failingSeamReader is a reader whose listing or probe failed: it answers err
// and counts the reads it was asked.
type failingSeamReader struct {
	err error

	mu  sync.Mutex
	has int
}

func (r *failingSeamReader) HasSession(string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.has++
	return false, r.err
}

func (r *failingSeamReader) hasCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.has
}

// TestManagerSessionProbesTakeNotLiveFromTheReader: a reader that answers
// session.ErrSeatNotLive makes the read not running, and tmux is never asked.
func TestManagerSessionProbesTakeNotLiveFromTheReader(t *testing.T) {
	t.Parallel()
	mgr, _, _, _, _ := canonicalWithPolecats(t, true, "p1")
	tm := &probeTmux{}
	mgr.tmux = tm
	reader := &failingSeamReader{err: session.ErrSeatNotLive}
	end := mgr.ShareSessionProbes(reader)
	defer end()

	running, _ := mgr.polecatSessionState("p1")
	if running {
		t.Errorf("polecatSessionState reported running for a seat the reader answers not live")
	}
	if got := reader.hasCount(); got != 1 {
		t.Errorf("reader answered %d has-session reads, want 1", got)
	}
	if got := tm.hasCount(); got != 0 {
		t.Errorf("tmux answered %d has-session reads for a not-live seat, want 0", got)
	}
}

// TestManagerSessionProbesFallBackToTmuxOnAReaderError: a reader error other
// than ErrSeatNotLive means the reader could not answer, so the read takes the
// Manager's own tmux client, as it does with no reader installed.
func TestManagerSessionProbesFallBackToTmuxOnAReaderError(t *testing.T) {
	t.Parallel()
	mgr, _, _, _, _ := canonicalWithPolecats(t, true, "p1")
	tm := &probeTmux{}
	mgr.tmux = tm
	reader := &failingSeamReader{err: errors.New("sandbox listing failed")}
	end := mgr.ShareSessionProbes(reader)
	defer end()

	running, _ := mgr.polecatSessionState("p1")
	if !running {
		t.Errorf("polecatSessionState reported not running, want tmux's running answer after the reader's error")
	}
	if got := reader.hasCount(); got != 1 {
		t.Errorf("reader answered %d has-session reads, want 1", got)
	}
	if got := tm.hasCount(); got != 1 {
		t.Errorf("tmux answered %d has-session reads after the reader's error, want 1", got)
	}
}

// TestShareSessionProbesNestedInstallsUnwindInOrder: an inner install takes
// the reader over, its end func gives the outer reader back, and the outer end
// func leaves the Manager on its own tmux client. A second end func is harmless.
func TestShareSessionProbesNestedInstallsUnwindInOrder(t *testing.T) {
	t.Parallel()
	mgr, _, _, _, _ := canonicalWithPolecats(t, true, "p1")
	tm := &probeTmux{}
	mgr.tmux = tm
	outer := &seamReader{running: true}
	inner := &seamReader{running: false}

	endOuter := mgr.ShareSessionProbes(outer)
	endInner := mgr.ShareSessionProbes(inner)
	endInner()

	running, _ := mgr.polecatSessionState("p1")
	if !running {
		t.Errorf("after the inner end, polecatSessionState reported not running; want the outer reader's running answer")
	}
	if got := outer.hasCount(); got != 1 {
		t.Errorf("outer reader answered %d reads after the inner end, want 1", got)
	}
	if got := inner.hasCount(); got != 0 {
		t.Errorf("inner reader answered %d reads after its end func, want 0", got)
	}

	endOuter()
	mgr.polecatSessionState("p1")
	if got := tm.hasCount(); got != 1 {
		t.Errorf("tmux answered %d has-session reads after the outer end, want 1", got)
	}
	endOuter()
}

// promptAtCreateTmux is a fake tmux whose new session shows the agent's ready
// prompt at once, so the launch's readiness wait returns instead of timing out.
type promptAtCreateTmux struct{ *fakeSessionTmux }

func (f promptAtCreateTmux) NewSessionWithCommandAndEnv(name, workDir, command string, env map[string]string) error {
	if err := f.fakeSessionTmux.NewSessionWithCommandAndEnv(name, workDir, command, env); err != nil {
		return err
	}
	f.SetScreen(name, "❯ ")
	return nil
}

// TestLaunchDropsTheCensusAnswerForTheSessionItCreates: the census holds a seat
// its gateway lists Stopped as not live, with no probe. The sandbox comes up
// and the launch creates the seat's session. The next has-session read must
// ask tmux and see the session; a census that kept its answer would still read
// the seat not live, because the listing it holds still says Stopped.
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestLaunchDropsTheCensusAnswerForTheSessionItCreates(t *testing.T) {
	town, rigPath, workDir, w := sandboxLaunchTown(t)
	// A launch that starts a session also starts its startup-nudge check, which
	// sleeps before it looks. Zero retries makes that check return at once, so no
	// goroutine outlives the test.
	noRetries := 0
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(town))
	if err != nil {
		t.Fatal(err)
	}
	ts.Operational = &config.OperationalConfig{Session: &config.SessionThresholds{StartupNudgeMaxRetries: &noRetries}}
	if err := config.SaveTownSettings(config.TownSettingsPath(town), ts); err != nil {
		t.Fatal(err)
	}
	tm := newFakeSessionTmux()
	m := &SessionManager{
		tmux: promptAtCreateTmux{tm},
		rig:  &rig.Rig{Name: "gastown", Path: rigPath},
		gits: w.opener(),
	}
	sessionID := m.SessionName("box")
	// The record names the default gateway, as preparingBackend's record does, so
	// the listing the census holds is the one the launch's sandbox is read from.
	if err := session.WriteSandboxRecord(town, session.SandboxRecord{Session: sessionID, Rig: "gastown", Polecat: "box"}); err != nil {
		t.Fatal(err)
	}
	listing := func(phase string) string {
		b, err := json.Marshal(map[string]any{"sandboxes": []map[string]string{{"name": sessionID, "phase": phase}}})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	gateway := &sandbox.Fake{Reply: map[string]string{"list": listing("Stopped")}}
	t.Cleanup(session.BeginSandboxCensus(gateway.Runner()))
	read := func() (bool, error) {
		return session.SandboxCensusRead(sessionID, "has-session", nil, func() (bool, error) {
			return tm.HasSession(sessionID)
		})
	}

	if _, err := read(); !errors.Is(err, session.ErrSeatNotLive) {
		t.Fatalf("before the launch: err = %v, want ErrSeatNotLive for a Stopped seat", err)
	}

	// The sandbox is Ready once the launch has created it; the gateway now says so.
	gateway.Reply["list"] = listing("Ready")
	err = m.launch("box", sessionID, SessionStartOptions{
		WorkDir:          workDir,
		Backend:          &preparingBackend{town: town},
		RuntimeConfigDir: t.TempDir(),
		Command:          "claude",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	running, err := read()
	if err != nil || !running {
		t.Fatalf("after the launch created %s: running=%v err=%v, want running with no error (the census answer must be dropped)", sessionID, running, err)
	}
}
