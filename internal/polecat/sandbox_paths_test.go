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
		"worktree is above":    {"/h/gt", "/h", "/sandbox"},
	} {
		if root, err := sandboxTownRoot(c[0], c[1], c[2]); err == nil {
			t.Errorf("%s: sandboxTownRoot = %q, want an error", name, root)
		}
	}
}

// A worktree whose first path element merely starts with ".." is inside the
// town, not outside it.
func TestSandboxTownRootAcceptsAWorktreeNamedLikeDotDotFoo(t *testing.T) {
	t.Parallel()
	got, err := sandboxTownRoot("/h/gt", "/h/gt/..foo/p", "/sandbox/gt/..foo/p")
	if err != nil || got != "/sandbox/gt" {
		t.Fatalf("sandboxTownRoot = %q, %v; want /sandbox/gt", got, err)
	}
}

// Only path-valued entries move, and a path list moves whether or not its
// first element is a path: free text and flag values that mention the town
// root are not paths the sandbox needs rewritten.
func TestRewriteTownRootInPathEnvLeavesNonPathValuesAlone(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"GT_TOWN_ROOT":            "/h/gt",
		"GIT_CEILING_DIRECTORIES": "/h/gt",
		"GT_POLECAT_PATH":         "/h/gt/r/polecats/p/r",
		"PATH":                    "bin:/h/gt/bin",
		"GT_NOTE":                 "see /h/gt for the town",
		"GOFLAGS":                 "-modfile=/h/gt/go.mod",
	}
	rewriteTownRootInPathEnv(env, "/h/gt", "/sandbox/gt")
	want := map[string]string{
		"GT_TOWN_ROOT":            "/sandbox/gt",
		"GIT_CEILING_DIRECTORIES": "/sandbox/gt",
		"GT_POLECAT_PATH":         "/sandbox/gt/r/polecats/p/r",
		"PATH":                    "bin:/sandbox/gt/bin",
		"GT_NOTE":                 "see /h/gt for the town",
		"GOFLAGS":                 "-modfile=/h/gt/go.mod",
	}
	for k, w := range want {
		if env[k] != w {
			t.Errorf("%s = %q, want %q", k, env[k], w)
		}
	}
}

// pathBackend is a sandbox backend whose Prepare reports a work dir inside
// the sandbox's town root, and the environment the agent gets there.
type pathBackend struct {
	preparingBackend
	workDir string
	env     map[string]string
}

func (b *pathBackend) Prepare(ctx context.Context, spec session.SpawnSpec) (*session.Prepared, error) {
	if _, err := b.preparingBackend.Prepare(ctx, spec); err != nil {
		return nil, err
	}
	return &session.Prepared{WorkDir: b.workDir, Env: b.env}, nil
}

// capturingTmux records the command and the start directory the launch opens
// the session with, then refuses, which ends the launch before any agent wait.
type capturingTmux struct {
	*fakeSessionTmux
	command string
	startIn string
}

func (c *capturingTmux) NewSessionWithCommandAndEnv(_, startIn, command string, _ map[string]string) error {
	c.command, c.startIn = command, startIn
	return errors.New("stop after capturing the command")
}

// launchRig is a rig under a temp town with a rendered system prompt file for
// the polecat "box", so the startup command carries both --settings and
// --append-system-prompt-file.
type launchRig struct {
	m       *SessionManager
	tm      *capturingTmux
	town    string
	workDir string
	rigPath string
}

func newLaunchRig(t *testing.T) *launchRig {
	t.Helper()
	return newLaunchRigAt(t, "w")
}

// newLaunchRigAt is newLaunchRig with the worktree at tail under the town:
// "gastown/polecats/p/gastown" is the shape a backend's in-sandbox
// GT_POLECAT_PATH has, and the tail the sandbox town root is cut at.
func newLaunchRigAt(t *testing.T, tail string) *launchRig {
	t.Helper()
	w := newWorld()
	town := t.TempDir()
	workDir := filepath.Join(town, filepath.FromSlash(tail))
	w.InitRepo(t, workDir)
	head := w.Commit(t, workDir, "main", "Initial commit", map[string]string{"README.md": "# Test\n"})
	w.checkout(t, workDir)
	w.AddRemote(t, workDir, "origin", workDir)
	w.SetRef(t, workDir, "refs/remotes/origin/main", head)
	session.ResetSandboxRoutingForTest(town)
	t.Cleanup(func() { session.ResetSandboxRoutingForTest("") })
	rigPath := filepath.Join(town, "gastown")
	if err := os.MkdirAll(filepath.Join(rigPath, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"type":"rig-settings","version":1,"merge_queue":{"forgejo":{"remote_url":"http://forgejo:3000/owner/gastown.git"}}}`
	if err := os.WriteFile(filepath.Join(rigPath, "settings", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(town, "runtime-config"), 0o755); err != nil {
		t.Fatal(err)
	}
	promptDir := filepath.Join(rigPath, "polecats", ".claude")
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptDir, "system-prompt-box.md"), []byte("role text"), 0o644); err != nil {
		t.Fatal(err)
	}
	tm := &capturingTmux{fakeSessionTmux: newFakeSessionTmux()}
	m := &SessionManager{
		tmux:           tm,
		rig:            &rig.Rig{Name: "gastown", Path: rigPath},
		gits:           w.opener(),
		sandboxDeleter: func(string, session.SandboxRecord) error { return nil },
	}
	return &launchRig{m: m, tm: tm, town: town, workDir: workDir, rigPath: rigPath}
}

func (r *launchRig) launch(backend session.Backend) error {
	return r.m.launch("box", r.m.SessionName("box"), SessionStartOptions{
		WorkDir:          r.workDir,
		Backend:          backend,
		RuntimeConfigDir: filepath.Join(r.town, "runtime-config"),
	})
}

// A sandbox launch must start the agent with sandbox paths: the host town
// root in --settings and --append-system-prompt-file does not exist inside
// the sandbox, and the agent exits 1 at once (sling 6, gt-nad3h).
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestLaunchStartsASandboxAgentWithSandboxPaths(t *testing.T) {
	r := newLaunchRig(t)
	sandboxWorkDir := "/sandbox/gt/" + filepath.Base(r.workDir)

	_ = r.launch(&pathBackend{preparingBackend: preparingBackend{town: r.town}, workDir: sandboxWorkDir})

	command := r.tm.command
	if command == "" {
		t.Fatal("the session was never started with a command")
	}
	if strings.Contains(command, r.town) {
		t.Errorf("the startup command still names the host town root %s:\n%s", r.town, command)
	}
	for _, want := range []string{
		"--settings /sandbox/gt/gastown/polecats/.claude/settings.json",
		"--append-system-prompt-file /sandbox/gt/gastown/polecats/.claude/system-prompt-box.md",
	} {
		if !strings.Contains(command, want) {
			t.Errorf("the startup command lacks %q:\n%s", want, command)
		}
	}
}

// Without a sandbox backend the startup command keeps the host paths.
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestLaunchKeepsHostPathsWithoutASandboxBackend(t *testing.T) {
	r := newLaunchRig(t)

	_ = r.launch(nil)

	command := r.tm.command
	if command == "" {
		t.Fatal("the session was never started with a command")
	}
	if strings.Contains(command, "/sandbox/") {
		t.Errorf("a plain launch rewrote paths into the sandbox:\n%s", command)
	}
	claudeDir := filepath.Join(r.rigPath, "polecats", ".claude")
	for _, want := range []string{
		"--settings " + filepath.Join(claudeDir, "settings.json"),
		"--append-system-prompt-file " + filepath.Join(claudeDir, "system-prompt-box.md"),
	} {
		if !strings.Contains(command, want) {
			t.Errorf("the startup command lacks %q:\n%s", want, command)
		}
	}
}

// launchSandboxPaths launches backend on a polecat-shaped rig, fails when the
// session never started, and fails when the startup command lacks the
// sandbox's settings and prompt paths.
func launchSandboxPaths(t *testing.T, backend func(town string) session.Backend) *launchRig {
	t.Helper()
	r := newLaunchRigAt(t, "gastown/polecats/p/gastown")
	_ = r.launch(backend(r.town))
	if r.tm.command == "" {
		t.Fatal("the session was never started with a command")
	}
	for _, want := range []string{
		"--settings /sandbox/gt/gastown/polecats/.claude/settings.json",
		"--append-system-prompt-file /sandbox/gt/gastown/polecats/.claude/system-prompt-box.md",
	} {
		if !strings.Contains(r.tm.command, want) {
			t.Errorf("the startup command lacks %q:\n%s", want, r.tm.command)
		}
	}
	return r
}

// A sandbox backend may give every seat's worktree one link path and put the
// real worktree path in GT_POLECAT_PATH (gt-f80lq2). The link path does not end
// in the worktree's tail, so the sandbox town root comes from GT_POLECAT_PATH
// and the session still starts in the prepared work dir.
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestSandboxTownRootFromPolecatPath(t *testing.T) {
	const linkPath = "/sandbox/gt/work"
	r := launchSandboxPaths(t, func(town string) session.Backend {
		return &pathBackend{
			preparingBackend: preparingBackend{town: town},
			workDir:          linkPath,
			env:              map[string]string{"GT_POLECAT_PATH": "/sandbox/gt/gastown/polecats/p/gastown"},
		}
	})

	if r.tm.startIn != linkPath {
		t.Errorf("the session starts in %q, want the prepared work dir %q", r.tm.startIn, linkPath)
	}
}

// Before the link path the prepared environment named no GT_POLECAT_PATH and
// the prepared work dir was the real one: that maps to the same town root, and
// a work dir that maps to nothing is still refused.
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestSandboxTownRootFromPolecatPathOldShape(t *testing.T) {
	t.Run("real work dir", func(t *testing.T) {
		launchSandboxPaths(t, func(town string) session.Backend {
			return &pathBackend{preparingBackend: preparingBackend{town: town}, workDir: "/sandbox/gt/gastown/polecats/p/gastown"}
		})
	})
	t.Run("unmappable work dir", func(t *testing.T) {
		r := newLaunchRigAt(t, "gastown/polecats/p/gastown")
		err := r.launch(&pathBackend{preparingBackend: preparingBackend{town: r.town}, workDir: "/sandbox/elsewhere/q"})
		if err == nil || !strings.Contains(err.Error(), "no sandbox town root") {
			t.Fatalf("launch error = %v, want one naming the missing sandbox town root", err)
		}
	})
}

// A sandbox work dir that does not map back onto the town stops the launch
// before any session starts.
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestLaunchFailsWhenTheSandboxWorkDirHasNoTownRoot(t *testing.T) {
	r := newLaunchRig(t)

	err := r.launch(&pathBackend{preparingBackend: preparingBackend{town: r.town}, workDir: "/sandbox/elsewhere/q"})

	if err == nil || !strings.Contains(err.Error(), "no sandbox town root") {
		t.Fatalf("launch error = %v, want one naming the missing sandbox town root", err)
	}
	if r.tm.command != "" {
		t.Errorf("the session was started despite the unmappable work dir:\n%s", r.tm.command)
	}
}
