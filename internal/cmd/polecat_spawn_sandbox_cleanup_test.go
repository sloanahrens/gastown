package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/session"
)

// A session that died during startup in a sandbox must take the sandbox and
// its registry record with it: the supervisor's cleanup only kills the session,
// and a record left behind makes the seat look occupied (sling 7, gt-2awr8).
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
	deleter := func(string, session.SandboxRecord) error { return errors.New("gateway unreachable") }

	got := cleanupSandboxAfterStartupDeath(deleter, town, sess, cause)

	if !errors.Is(got, cause) {
		t.Errorf("error %v no longer wraps the startup-death cause", got)
	}
	if !strings.Contains(got.Error(), "gateway unreachable") {
		t.Errorf("error %q does not carry the deletion failure", got)
	}
	if still, _ := session.IsSandboxed(town, sess); !still {
		t.Error("the record was removed although the sandbox could not be deleted: it is the only trace left")
	}
}

// The supervisor's kill alone leaves the sandbox Ready: when the pane cannot be
// found, the startup-death path must kill the session AND drop the sandbox, and
// the caller sees the startup-death error, not a nil pane.
func TestPaneAfterStartKillsTheSessionAndDropsTheSandboxWhenThePaneIsGone(t *testing.T) {
	t.Parallel()
	var killed, dropped int
	var droppedCause error
	getPane := func(string) (string, error) { return "", errors.New("can't find pane") }

	pane, err := paneAfterStart("gt-gastown-box", getPane,
		func() { killed++ },
		func(cause error) error { dropped++; droppedCause = cause; return cause })

	if pane != "" || err == nil {
		t.Fatalf("pane=%q err=%v, want no pane and an error", pane, err)
	}
	if killed != 1 || dropped != 1 {
		t.Errorf("killed=%d dropped=%d, want the session killed once and the sandbox dropped once (the supervisor kill alone leaks the sandbox)", killed, dropped)
	}
	if droppedCause == nil || !strings.Contains(droppedCause.Error(), "died during startup") || !strings.Contains(err.Error(), "can't find pane") {
		t.Errorf("cause=%v err=%v, want the startup-death error carrying the pane failure", droppedCause, err)
	}
}

func TestPaneAfterStartLeavesAHealthySessionAlone(t *testing.T) {
	t.Parallel()
	touched := false
	pane, err := paneAfterStart("gt-gastown-box", func(string) (string, error) { return "%3", nil },
		func() { touched = true },
		func(cause error) error { touched = true; return cause })
	if err != nil || pane != "%3" || touched {
		t.Errorf("pane=%q err=%v touched=%v, want the pane returned and nothing cleaned up", pane, err, touched)
	}
}
