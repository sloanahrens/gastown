package polecat

import (
	"context"
	"errors"
	"fmt"
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

// sandboxLaunchTown is a temp town whose rig settings name a Forgejo remote a
// sandbox launch can clone from. It also points the process-wide sandbox
// routing at that town, so it belongs only to tests that run alone.
func sandboxLaunchTown(t *testing.T) (town, rigPath, workDir string, w *world) {
	t.Helper()
	town = t.TempDir()
	session.ResetSandboxRoutingForTest(town)
	t.Cleanup(func() { session.ResetSandboxRoutingForTest("") })
	rigPath = filepath.Join(town, "gastown")
	settings := filepath.Join(rigPath, "settings")
	if err := os.MkdirAll(settings, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"type":"rig-settings","version":1,"merge_queue":{"forgejo":{"remote_url":"http://forgejo:3000/sloan/gastown.git"}}}`
	if err := os.WriteFile(filepath.Join(settings, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	// The worktree sits inside the town, as a real polecat's does: the launch
	// derives the sandbox town root from where it sits.
	workDir = filepath.Join(rigPath, "polecats", "box", "gastown")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	w = newWorld()
	w.InitRepo(t, workDir)
	head := w.Commit(t, workDir, "main", "Initial commit", map[string]string{"README.md": "# Test\n"})
	w.checkout(t, workDir)
	w.AddRemote(t, workDir, "origin", workDir)
	w.SetRef(t, workDir, "refs/remotes/origin/main", head)
	return town, rigPath, workDir, w
}

// A launch that fails after the sandbox backend's Prepare made a sandbox and
// its registry record must drop both. The record counts as a live session, so
// leaving it makes the seat look occupied and strands the sandbox (gt-8a1ql).
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestLaunchCleansUpSandboxWhenTheSessionIsNotCreated(t *testing.T) {
	town, rigPath, workDir, w := sandboxLaunchTown(t)
	var deleted []string
	m := &SessionManager{
		tmux: refusingCreateTmux{newFakeSessionTmux()},
		rig:  &rig.Rig{Name: "gastown", Path: rigPath},
		gits: w.opener(),
		// session's init links the openshell deleter; stub it so the cleanup
		// never shells out to a real sandbox CLI (no gateway runs in the unit
		// tier).
		sandboxDeleter: func(_ string, rec session.SandboxRecord) error {
			deleted = append(deleted, rec.Session)
			return nil
		},
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

// A cleanup that cannot delete the sandbox must not swallow the launch's
// failure: the caller sees both, and the record stays as the only trace that
// the sandbox is still out there.
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestLaunchReportsWhenTheSandboxCleanupFails(t *testing.T) {
	town, rigPath, workDir, w := sandboxLaunchTown(t)
	m := &SessionManager{
		tmux: refusingCreateTmux{newFakeSessionTmux()},
		rig:  &rig.Rig{Name: "gastown", Path: rigPath},
		gits: w.opener(),
		sandboxDeleter: func(string, session.SandboxRecord) error {
			return errors.New("gateway unreachable")
		},
	}
	sessionID := m.SessionName("box")

	err := m.launch("box", sessionID, SessionStartOptions{
		WorkDir:          workDir,
		Backend:          &preparingBackend{town: town},
		RuntimeConfigDir: t.TempDir(),
		Command:          "claude",
	})
	if err == nil {
		t.Fatal("launch succeeded; the session was never created")
	}
	for _, want := range []string{"no server", "cleaning up", "gateway unreachable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("launch error = %v, want it to carry %q", err, want)
		}
	}
	if still, serr := session.IsSandboxed(town, sessionID); serr != nil || !still {
		t.Fatalf("the sandbox record is gone after a failed deletion (still=%v err=%v), so nothing marks the sandbox left behind", still, serr)
	}
}

// panickingCreateTmux is a fake tmux that panics where a session would be
// created.
type panickingCreateTmux struct{ *fakeSessionTmux }

func (panickingCreateTmux) NewSessionWithCommandAndEnv(string, string, string, map[string]string) error {
	panic("tmux: panic creating session")
}

// A panic after Prepare unwinds past the error returns, so the cleanup has to
// drop the sandbox from the panic path too, then let the same panic continue.
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestLaunchCleansUpSandboxWhenTheLaunchPanics(t *testing.T) {
	town, rigPath, workDir, w := sandboxLaunchTown(t)
	var deleted []string
	m := &SessionManager{
		tmux: panickingCreateTmux{newFakeSessionTmux()},
		rig:  &rig.Rig{Name: "gastown", Path: rigPath},
		gits: w.opener(),
		sandboxDeleter: func(_ string, rec session.SandboxRecord) error {
			deleted = append(deleted, rec.Session)
			return nil
		},
	}
	sessionID := m.SessionName("box")
	backend := &preparingBackend{town: town}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = m.launch("box", sessionID, SessionStartOptions{
			WorkDir:          workDir,
			Backend:          backend,
			RuntimeConfigDir: t.TempDir(),
			Command:          "claude",
		})
	}()
	if got := fmt.Sprint(recovered); got != "tmux: panic creating session" {
		t.Fatalf("recovered = %q, want the tmux panic to pass through", got)
	}
	if !backend.prepared {
		t.Fatal("Prepare never ran; the launch panicked before the sandbox existed")
	}
	if still, serr := session.IsSandboxed(town, sessionID); serr != nil || still {
		t.Fatalf("the sandbox record survived a panicked launch (still=%v err=%v); it counts as a live session, so the seat looks occupied", still, serr)
	}
	if len(deleted) != 1 || deleted[0] != sessionID {
		t.Fatalf("sandbox deletions = %v, want exactly %s", deleted, sessionID)
	}
}

// workDirBackend is a preparingBackend whose sandbox worktree mirrors the host
// one under a different town root, as the openshell backend's does.
type workDirBackend struct {
	*preparingBackend
	sandboxTown string
	hostTown    string
}

func (b workDirBackend) Prepare(ctx context.Context, spec session.SpawnSpec) (*session.Prepared, error) {
	if _, err := b.preparingBackend.Prepare(ctx, spec); err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(b.hostTown, spec.HostWorkDir)
	if err != nil {
		return nil, err
	}
	return &session.Prepared{WorkDir: b.sandboxTown + "/" + filepath.ToSlash(rel)}, nil
}

// envCapturingTmux records the environment the launch hands the new session,
// then refuses to create it.
type envCapturingTmux struct {
	*fakeSessionTmux
	env *map[string]string
}

func (c envCapturingTmux) NewSessionWithCommandAndEnv(_, _, _ string, env map[string]string) error {
	*c.env = env
	return errors.New("tmux: captured")
}

// The sandbox session's tmux environment names no host path: the agent
// command's own env overrides GIT_CEILING_DIRECTORIES, but later panes and
// exec'd commands read the session environment (gt-b3qkh).
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestLaunchSandboxSessionEnvNamesNoHostTownRoot(t *testing.T) {
	town, rigPath, workDir, w := sandboxLaunchTown(t)
	var got map[string]string
	m := &SessionManager{
		tmux:           envCapturingTmux{newFakeSessionTmux(), &got},
		rig:            &rig.Rig{Name: "gastown", Path: rigPath},
		gits:           w.opener(),
		sandboxDeleter: func(string, session.SandboxRecord) error { return nil },
	}
	sessionID := m.SessionName("box")
	const sandboxTown = "/sandbox/gt"
	backend := workDirBackend{&preparingBackend{town: town}, sandboxTown, town}

	err := m.launch("box", sessionID, SessionStartOptions{
		WorkDir:          workDir,
		Backend:          backend,
		RuntimeConfigDir: t.TempDir(),
		Command:          "claude",
	})
	if err == nil || !strings.Contains(err.Error(), "captured") {
		t.Fatalf("launch error = %v, want the capturing tmux's refusal", err)
	}
	if len(got) == 0 {
		t.Fatal("the tmux client got no session environment")
	}
	for k, v := range got {
		if strings.Contains(v, town) {
			t.Errorf("session env %s=%q names the host town root %s", k, v, town)
		}
	}
	if g := got["GIT_CEILING_DIRECTORIES"]; g != sandboxTown {
		t.Errorf("GIT_CEILING_DIRECTORIES = %q, want the sandbox town root %q", g, sandboxTown)
	}
}

// A plain tmux launch keeps its host paths: only a sandbox move rewrites them.
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestLaunchHostSessionEnvKeepsHostTownRoot(t *testing.T) {
	town, rigPath, workDir, w := sandboxLaunchTown(t)
	var got map[string]string
	m := &SessionManager{
		tmux: envCapturingTmux{newFakeSessionTmux(), &got},
		rig:  &rig.Rig{Name: "gastown", Path: rigPath},
		gits: w.opener(),
	}

	err := m.launch("box", m.SessionName("box"), SessionStartOptions{
		WorkDir:          workDir,
		RuntimeConfigDir: t.TempDir(),
		Command:          "claude",
	})
	if err == nil || !strings.Contains(err.Error(), "captured") {
		t.Fatalf("launch error = %v, want the capturing tmux's refusal", err)
	}
	if g := got["GIT_CEILING_DIRECTORIES"]; g != town {
		t.Errorf("GIT_CEILING_DIRECTORIES = %q, want the host town root %q", g, town)
	}
	if g := got["GT_POLECAT_PATH"]; g != workDir {
		t.Errorf("GT_POLECAT_PATH = %q, want the host worktree %q", g, workDir)
	}
}
