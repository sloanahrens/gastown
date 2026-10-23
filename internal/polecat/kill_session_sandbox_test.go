package polecat

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
)

// A failing probe: KillSessionWithProcesses errors, so the test shows the
// sandbox cleanup still runs.
type failingKillProbe struct{ *fakeProbe }

func (p failingKillProbe) KillSessionWithProcesses(string) error {
	return errors.New("tmux: server gone")
}

//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestKillSessionCleansUpTheSandboxEvenWhenTheKillFails(t *testing.T) {
	town := t.TempDir()
	session.ResetSandboxRoutingForTest(town)
	t.Cleanup(func() { session.ResetSandboxRoutingForTest("") })
	// session's init links the openshell deleter; stub it so the test never
	// shells out to a real sandbox CLI (a failed delete keeps the record).
	var deleted []string
	prev := session.SandboxDeleter
	session.SandboxDeleter = func(_ string, rec session.SandboxRecord) error {
		deleted = append(deleted, rec.Session)
		return nil
	}
	t.Cleanup(func() { session.SandboxDeleter = prev })
	r := &rig.Rig{Name: "gastown", Path: filepath.Join(town, "gastown")}
	m := newTestManager(r, nil, failingKillProbe{newFakeProbe()}, nil)
	sessionName := m.sessionName("box")
	if err := session.WriteSandboxRecord(town, session.SandboxRecord{Session: sessionName, Rig: "gastown", Polecat: "box"}); err != nil {
		t.Fatal(err)
	}

	err := m.killSession("box", "test")
	if err == nil || !strings.Contains(err.Error(), "server gone") {
		t.Fatalf("the kill error must surface: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != sessionName {
		t.Fatalf("the sandbox must be deleted once after a failed kill: %v", deleted)
	}
	if still, serr := session.IsSandboxed(town, sessionName); serr != nil || still {
		t.Fatalf("the sandbox record survived a failed kill (still=%v err=%v); the sandbox must still be cleaned up", still, serr)
	}
}
