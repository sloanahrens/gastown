package polecat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
)

func TestSandboxTownRootIsTheSandboxWorkDirMinusTheWorktreeTail(t *testing.T) {
	t.Parallel()
	got, err := sandboxTownRoot("/h/gt", "/h/gt/r/polecats/p/r", "/sandbox/gt/r/polecats/p/r")
	if err != nil || got != "/sandbox/gt" {
		t.Fatalf("sandboxTownRoot = %q, %v; want /sandbox/gt", got, err)
	}
	for name, c := range map[string][3]string{
		"different tail":       {"/h/gt", "/h/gt/r/polecats/p/r", "/sandbox/elsewhere/q"},
		"worktree outside":     {"/h/gt", "/tmp/else/p", "/sandbox/gt/p"},
		"worktree is the root": {"/h/gt", "/h/gt", "/sandbox/gt"},
	} {
		if root, err := sandboxTownRoot(c[0], c[1], c[2]); err == nil {
			t.Errorf("%s: sandboxTownRoot = %q, want an error", name, root)
		}
	}
}

// pathBackend is a sandbox backend whose Prepare reports a work dir inside
// the sandbox's town root.
type pathBackend struct {
	preparingBackend
	workDir string
}

func (b *pathBackend) Prepare(ctx context.Context, spec session.SpawnSpec) (*session.Prepared, error) {
	if _, err := b.preparingBackend.Prepare(ctx, spec); err != nil {
		return nil, err
	}
	return &session.Prepared{WorkDir: b.workDir}, nil
}

// capturingTmux records the command the launch starts the session with, then
// refuses, which ends the launch before any agent wait.
type capturingTmux struct {
	*fakeSessionTmux
	command string
}

func (c *capturingTmux) NewSessionWithCommandAndEnv(_, _, command string, _ map[string]string) error {
	c.command = command
	return errors.New("stop after capturing the command")
}

// A sandbox launch must start the agent with sandbox paths: the host town
// root in --settings and --append-system-prompt-file does not exist inside
// the sandbox, and the agent exits 1 at once (sling 6, gt-nad3h).
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestLaunchStartsASandboxAgentWithSandboxPaths(t *testing.T) {
	workDir, _, w := setupSessionBranchTestRepo(t)
	town := filepath.Dir(workDir)
	session.ResetSandboxRoutingForTest(town)
	t.Cleanup(func() { session.ResetSandboxRoutingForTest("") })
	rigPath := filepath.Join(town, "gastown")
	if err := os.MkdirAll(filepath.Join(rigPath, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"type":"rig-settings","version":1,"merge_queue":{"forgejo":{"remote_url":"http://forgejo:3000/sloan/gastown.git"}}}`
	if err := os.WriteFile(filepath.Join(rigPath, "settings", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	tm := &capturingTmux{fakeSessionTmux: newFakeSessionTmux()}
	m := &SessionManager{
		tmux:           tm,
		rig:            &rig.Rig{Name: "gastown", Path: rigPath},
		gits:           w.opener(),
		sandboxDeleter: func(string, session.SandboxRecord) error { return nil },
	}
	sandboxWorkDir := "/sandbox/gt/" + filepath.Base(workDir)

	_ = m.launch("box", m.SessionName("box"), SessionStartOptions{
		WorkDir:          workDir,
		Backend:          &pathBackend{preparingBackend: preparingBackend{town: town}, workDir: sandboxWorkDir},
		RuntimeConfigDir: t.TempDir(),
	})

	if tm.command == "" {
		t.Fatal("the session was never started with a command")
	}
	if strings.Contains(tm.command, town) {
		t.Errorf("the startup command still names the host town root %s:\n%s", town, tm.command)
	}
	if !strings.Contains(tm.command, "/sandbox/gt/gastown/polecats/.claude/settings.json") {
		t.Errorf("the startup command does not name the sandbox settings file:\n%s", tm.command)
	}
}
