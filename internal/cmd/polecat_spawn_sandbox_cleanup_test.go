package cmd

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
)

// A sandboxed session that died during startup takes its sandbox and registry
// record with it: the supervisor's cleanup only kills the session, and a record
// left behind makes the seat look occupied (gt-2awr8).
func TestStartupDeathCleanupDeletesTheSandboxAndItsRecord(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	const sess = "gt-gastown-box"
	if err := session.WriteSandboxRecord(town, session.SandboxRecord{Session: sess, Rig: "gastown", Polecat: "box"}); err != nil {
		t.Fatal(err)
	}
	var deleted []string
	deleter := func(_ string, rec session.SandboxRecord) error {
		deleted = append(deleted, rec.Session)
		return nil
	}
	cause := errors.New("session likely died during startup")

	got := cleanupSandboxAfterStartupDeath(deleter, town, sess, cause)

	if got != cause {
		t.Errorf("error = %v, want the original cause unchanged when cleanup succeeds", got)
	}
	if len(deleted) != 1 || deleted[0] != sess {
		t.Errorf("deleted sandboxes = %v, want exactly %s", deleted, sess)
	}
	if still, err := session.IsSandboxed(town, sess); err != nil || still {
		t.Errorf("the sandbox record survived (still=%v err=%v)", still, err)
	}
}

func TestStartupDeathCleanupLeavesAHostSessionAlone(t *testing.T) {
	t.Parallel()
	called := false
	deleter := func(string, session.SandboxRecord) error { called = true; return nil }
	cause := errors.New("died")

	if got := cleanupSandboxAfterStartupDeath(deleter, t.TempDir(), "gt-gastown-host", cause); got != cause {
		t.Errorf("error = %v, want the original cause", got)
	}
	if called {
		t.Error("the deleter ran for a session with no sandbox record")
	}
}

func TestStartupDeathCleanupKeepsTheCauseWhenDeletionFails(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	const sess = "gt-gastown-box"
	if err := session.WriteSandboxRecord(town, session.SandboxRecord{Session: sess, Rig: "gastown", Polecat: "box"}); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("session likely died during startup")
	deleteErr := errors.New("gateway unreachable")
	deleter := func(string, session.SandboxRecord) error { return deleteErr }

	got := cleanupSandboxAfterStartupDeath(deleter, town, sess, cause)

	if !errors.Is(got, cause) {
		t.Errorf("error %v no longer wraps the startup-death cause", got)
	}
	if !errors.Is(got, deleteErr) {
		t.Errorf("error %v does not wrap the deletion failure", got)
	}
	if !strings.Contains(got.Error(), "gateway unreachable") {
		t.Errorf("error %q does not carry the deletion failure", got)
	}
	if still, _ := session.IsSandboxed(town, sess); !still {
		t.Error("the record was removed although the sandbox could not be deleted: it is the only trace left")
	}
}

// The supervisor's kill alone leaves the sandbox Ready: when the pane cannot be
// found, the startup-death path must kill the session AND drop the sandbox, in
// that order (the sandbox is deleted after its session is gone), and the caller
// sees the startup-death error, not a nil pane.
func TestPaneAfterStartKillsTheSessionAndDropsTheSandboxWhenThePaneIsGone(t *testing.T) {
	t.Parallel()
	var calls []string
	var droppedCause error
	getPane := func(string) (string, error) { return "", errors.New("can't find pane") }

	pane, err := paneAfterStart("gt-gastown-box", getPane,
		func(time.Duration) {},
		func() { calls = append(calls, "kill") },
		func(cause error) error { calls = append(calls, "drop"); droppedCause = cause; return cause })

	if pane != "" || err == nil {
		t.Fatalf("pane=%q err=%v, want no pane and an error", pane, err)
	}
	if want := []string{"kill", "drop"}; !slices.Equal(calls, want) {
		t.Errorf("calls = %v, want %v: the session must be killed before its sandbox is dropped", calls, want)
	}
	if droppedCause == nil || !strings.Contains(droppedCause.Error(), "died during startup") || !strings.Contains(err.Error(), "can't find pane") {
		t.Errorf("cause=%v err=%v, want the startup-death error carrying the pane failure", droppedCause, err)
	}
}

func TestPaneAfterStartLeavesAHealthySessionAlone(t *testing.T) {
	t.Parallel()
	touched := false
	pane, err := paneAfterStart("gt-gastown-box", func(string) (string, error) { return "%3", nil },
		func(time.Duration) { touched = true },
		func() { touched = true },
		func(cause error) error { touched = true; return cause })
	if err != nil || pane != "%3" || touched {
		t.Errorf("pane=%q err=%v touched=%v, want the pane returned and nothing cleaned up", pane, err, touched)
	}
}

// A tmux transport error is not a dead seat: the pane lookup is tried again
// through the pause seam, and a pane found on a later try is the pane returned.
func TestPaneAfterStartRetriesATransportError(t *testing.T) {
	t.Parallel()
	transport := errors.New("tmux display-message: Error: × transport error")
	lookups := 0
	getPane := func(string) (string, error) {
		lookups++
		if lookups < 3 {
			return "", transport
		}
		return "%7", nil
	}
	var pauses []time.Duration
	touched := false

	pane, err := paneAfterStart("gt-gastown-box", getPane,
		func(d time.Duration) { pauses = append(pauses, d) },
		func() { touched = true },
		func(cause error) error { touched = true; return cause })

	if err != nil || pane != "%7" {
		t.Fatalf("pane=%q err=%v, want the pane found on the third try", pane, err)
	}
	if touched {
		t.Error("the session was killed or its sandbox dropped although a later try found the pane")
	}
	if len(pauses) != 2 {
		t.Errorf("paused %d times, want 2 (one before each retry)", len(pauses))
	}
}

// Three transport errors in a row are a dead session: it is killed and its
// sandbox dropped once each, and the error names the number of tries.
func TestPaneAfterStartDropsTheSandboxWhenTheTransportErrorPersists(t *testing.T) {
	t.Parallel()
	transport := errors.New("tmux display-message: Error: × transport error")
	lookups := 0
	getPane := func(string) (string, error) { lookups++; return "", transport }
	var calls []string
	var droppedCause error

	pane, err := paneAfterStart("gt-gastown-box", getPane,
		func(time.Duration) {},
		func() { calls = append(calls, "kill") },
		func(cause error) error { calls = append(calls, "drop"); droppedCause = cause; return cause })

	if pane != "" || err == nil {
		t.Fatalf("pane=%q err=%v, want no pane and an error", pane, err)
	}
	if lookups != 3 {
		t.Errorf("pane lookups = %d, want 3 tries in all", lookups)
	}
	if want := []string{"kill", "drop"}; !slices.Equal(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
	if droppedCause == nil || !strings.Contains(droppedCause.Error(), "3 tries") || !strings.Contains(err.Error(), "transport error") {
		t.Errorf("cause=%v err=%v, want the startup-death error naming 3 tries and carrying the transport error", droppedCause, err)
	}
}

// Only a transport error is retried: any other lookup failure is a dead
// session at once, with no pause.
func TestPaneAfterStartDoesNotRetryAnOtherError(t *testing.T) {
	t.Parallel()
	lookups := 0
	getPane := func(string) (string, error) { lookups++; return "", errors.New("can't find pane") }
	pauses := 0

	_, err := paneAfterStart("gt-gastown-box", getPane,
		func(time.Duration) { pauses++ },
		func() {},
		func(cause error) error { return cause })

	if err == nil || lookups != 1 || pauses != 0 {
		t.Errorf("err=%v lookups=%d pauses=%d, want one lookup, no pause, and the error", err, lookups, pauses)
	}
}

// A transport error followed by any other lookup failure is a dead session at
// that second try: the other error is not retried, and the session is killed
// and its sandbox dropped once each.
func TestPaneAfterStartStopsRetryingWhenATransportErrorTurnsIntoAnOtherError(t *testing.T) {
	t.Parallel()
	transport := errors.New("tmux display-message: Error: × transport error")
	lookups := 0
	getPane := func(string) (string, error) {
		lookups++
		if lookups == 1 {
			return "", transport
		}
		return "", errors.New("can't find pane")
	}
	pauses := 0
	var calls []string
	var droppedCause error

	pane, err := paneAfterStart("gt-gastown-box", getPane,
		func(time.Duration) { pauses++ },
		func() { calls = append(calls, "kill") },
		func(cause error) error { calls = append(calls, "drop"); droppedCause = cause; return cause })

	if pane != "" || err == nil {
		t.Fatalf("pane=%q err=%v, want no pane and an error", pane, err)
	}
	if lookups != 2 {
		t.Errorf("pane lookups = %d, want 2 (the transport retry, then the other error ends the retries)", lookups)
	}
	if pauses != 1 {
		t.Errorf("paused %d times, want 1 (only before the transport retry)", pauses)
	}
	if want := []string{"kill", "drop"}; !slices.Equal(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
	if droppedCause == nil || !strings.Contains(droppedCause.Error(), "can't find pane") || strings.Contains(droppedCause.Error(), "tries") {
		t.Errorf("cause=%v, want the startup-death error carrying the other error and not a retry count", droppedCause)
	}
}

// recordingSeatCleaner is the supervisor surface a startup-death kill calls.
type recordingSeatCleaner struct {
	seat   supervisor.Seat
	reason string
	actor  string
}

func (c *recordingSeatCleaner) Cleanup(seat supervisor.Seat, reason, actor string) error {
	c.seat, c.reason, c.actor = seat, reason, actor
	return nil
}

func TestKillStartupDeathEndsThisSpawnsSeat(t *testing.T) {
	t.Parallel()
	s := &SpawnedPolecatInfo{RigName: "gastown", PolecatName: "box", SessionName: "gt-gastown-box"}
	cleaner := &recordingSeatCleaner{}

	s.killStartupDeath(cleaner, "gt sling/tester")

	if cleaner.seat.Rig != "gastown" || cleaner.seat.Name != "box" || cleaner.seat.Role != session.Role(constants.RolePolecat) {
		t.Errorf("killed seat %+v, want the gastown polecat box", cleaner.seat)
	}
	if cleaner.actor != "gt sling/tester" {
		t.Errorf("actor = %q, want the actor the spawn was given", cleaner.actor)
	}
}

// startSession hands the sandbox drop the spawn's own session name and town,
// so the record it removes is the one this spawn's session ran in.
func TestDropSandboxAfterStartupDeathDeletesThisSpawnsSandbox(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	s := &SpawnedPolecatInfo{RigName: "gastown", PolecatName: "box", SessionName: "gt-gastown-box"}
	if err := session.WriteSandboxRecord(town, session.SandboxRecord{Session: s.SessionName, Rig: "gastown", Polecat: "box"}); err != nil {
		t.Fatal(err)
	}
	var deleted []string
	deleter := func(_ string, rec session.SandboxRecord) error {
		deleted = append(deleted, rec.Session)
		return nil
	}
	cause := errors.New("session likely died during startup")

	if got := s.dropSandboxAfterStartupDeath(deleter, town, cause); got != cause {
		t.Errorf("error = %v, want the original cause when cleanup succeeds", got)
	}
	if len(deleted) != 1 || deleted[0] != s.SessionName {
		t.Errorf("deleted sandboxes = %v, want exactly %s", deleted, s.SessionName)
	}
	if still, err := session.IsSandboxed(town, s.SessionName); err != nil || still {
		t.Errorf("the sandbox record survived (still=%v err=%v)", still, err)
	}
}
