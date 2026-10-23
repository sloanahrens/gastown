package polecat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/liveness"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// preparingBackend is a session.Backend whose Prepare succeeds and records
// its sandbox in the registry, as the openshell backend's does. The sandbox
// itself never exists: the test's deleter drops the record only.
type preparingBackend struct {
	town     string
	prepared bool
}

var _ session.Backend = (*preparingBackend)(nil)

func (b *preparingBackend) Name() string                       { return session.BackendOpenShell }
func (b *preparingBackend) Tmux() *tmux.Tmux                   { return nil }
func (b *preparingBackend) SessionExists(string) (bool, error) { return false, nil }
func (b *preparingBackend) ControlPlaneHost() string           { return "host.docker.internal" }
func (b *preparingBackend) ProgressSample(string, liveness.Input) liveness.Result {
	return liveness.Result{}
}
func (b *preparingBackend) Stop(string) error   { return nil }
func (b *preparingBackend) Delete(string) error { return nil }

func (b *preparingBackend) Prepare(_ context.Context, spec session.SpawnSpec) (*session.Prepared, error) {
	rec := session.SandboxRecord{Session: spec.Session, Rig: spec.Rig, Polecat: spec.Polecat}
	if err := session.WriteSandboxRecord(b.town, rec); err != nil {
		return nil, err
	}
	b.prepared = true
	return &session.Prepared{}, nil
}

// refusingCreateTmux is a fake tmux that refuses to create the session.
type refusingCreateTmux struct{ *fakeSessionTmux }

func (refusingCreateTmux) NewSessionWithCommandAndEnv(string, string, string, map[string]string) error {
	return errors.New("tmux: no server")
}

// A launch that fails after the sandbox backend's Prepare made a sandbox and
// its registry record must drop both. The record counts as a live session, so
// leaving it makes the seat look occupied and strands the sandbox (gt-8a1ql).
//
//testpolicy:allow parallel — points the process-wide sandbox routing and deleter at a temp town and a stub, which another test must not see mid-run
func TestLaunchCleansUpSandboxWhenTheSessionIsNotCreated(t *testing.T) {
	town := t.TempDir()
	session.ResetSandboxRoutingForTest(town)
	t.Cleanup(func() { session.ResetSandboxRoutingForTest("") })
	// session's init links the openshell deleter; stub it so the cleanup never
	// shells out to a real sandbox CLI (no gateway runs in the unit tier).
	var deleted []string
	t.Cleanup(session.SetSandboxDeleterForTest(func(_ string, rec session.SandboxRecord) error {
		deleted = append(deleted, rec.Session)
		return nil
	}))
	rigPath := filepath.Join(town, "gastown")
	settings := filepath.Join(rigPath, "settings")
	if err := os.MkdirAll(settings, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"type":"rig-settings","version":1,"merge_queue":{"forgejo":{"remote_url":"http://forgejo:3000/sloan/gastown.git"}}}`
	if err := os.WriteFile(filepath.Join(settings, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	workDir, _, w := setupSessionBranchTestRepo(t)
	m := &SessionManager{
		tmux: refusingCreateTmux{newFakeSessionTmux()},
		rig:  &rig.Rig{Name: "gastown", Path: rigPath},
		gits: w.opener(),
	}
	sessionID := m.SessionName("box")
	backend := &preparingBackend{town: town}

	err := m.launch("box", sessionID, SessionStartOptions{
		WorkDir:          workDir,
		Backend:          backend,
		RuntimeConfigDir: t.TempDir(),
		Command:          "claude",
	})
	if err == nil || !strings.Contains(err.Error(), "no server") {
		t.Fatalf("launch error = %v, want the session creation failure", err)
	}
	if !backend.prepared {
		t.Fatal("Prepare never ran; the launch failed before the sandbox existed")
	}
	if still, serr := session.IsSandboxed(town, sessionID); serr != nil || still {
		t.Fatalf("the sandbox record survived a failed launch (still=%v err=%v); it counts as a live session, so the seat looks occupied", still, serr)
	}
	if len(deleted) != 1 || deleted[0] != sessionID {
		t.Fatalf("sandbox deletions = %v, want exactly %s", deleted, sessionID)
	}
}
