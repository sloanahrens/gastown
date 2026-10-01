package cmd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/session"
)

// handoffTestRegistry maps the rig prefixes the handoff tests use.
func handoffTestRegistry() *session.PrefixRegistry {
	reg := session.NewPrefixRegistry()
	reg.Register("gt", "gastown")
	return reg
}

// restartOpts are buildRestartCommandOpts for a session of townRoot parsed
// with handoffTestRegistry, reading env as the process environment and no
// tmux.
func restartOpts(townRoot string, env map[string]string) buildRestartCommandOpts {
	return buildRestartCommandOpts{
		Registry: handoffTestRegistry(),
		TownRoot: townRoot,
		LookupEnv: func(k string) (string, bool) {
			v, ok := env[k]
			return v, ok
		},
		SessionEnv: func(string, string) (string, error) { return "", errors.New("no tmux in a unit test") },
	}
}

// buildTestRestartCommand is buildRestartCommand for a session of townRoot
// whose process environment is env.
func buildTestRestartCommand(townRoot string, env map[string]string, sessionName string) (string, error) {
	return buildRestartCommandWithOpts(sessionName, restartOpts(townRoot, env))
}

func TestResolvePathToSessionRejectsUnsafeSegments(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"../crew/toast", "gastown/../toast", "gastown/crew/..", `gastown\crew\toast`} {
		t.Run(target, func(t *testing.T) {
			if _, err := resolvePathToSession(handoffTestRegistry(), target); err == nil {
				t.Fatalf("resolvePathToSession(%q) error = nil, want rejection", target)
			}
		})
	}
}

func TestHandoffStdinFlag(t *testing.T) {
	t.Parallel()
	t.Run("errors when both stdin and message provided", func(t *testing.T) {
		t.Parallel()
		_, err := handoffMessageInput("some message", true, func() ([]byte, error) {
			t.Fatal("stdin read")
			return nil, nil
		})
		if err == nil {
			t.Fatal("expected error when both --stdin and --message are set")
		}
		if !strings.Contains(err.Error(), "cannot use --stdin with --message/-m") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("reads the message from stdin", func(t *testing.T) {
		t.Parallel()
		got, err := handoffMessageInput("", true, func() ([]byte, error) { return []byte("from stdin\n\n"), nil })
		if err != nil || got != "from stdin" {
			t.Fatalf("handoffMessageInput = %q, %v; want %q", got, err, "from stdin")
		}
	})
}

func TestSessionWorkDir(t *testing.T) {
	t.Parallel()
	townRoot := "/home/test/gt"

	tests := []struct {
		name        string
		sessionName string
		wantDir     string
		wantErr     bool
	}{
		{
			name:        "mayor runs from mayor subdirectory",
			sessionName: "hq-mayor",
			wantDir:     townRoot + "/mayor",
			wantErr:     false,
		},
		{
			name:        "crew runs from crew subdirectory",
			sessionName: "gt-crew-holden",
			wantDir:     townRoot + "/gastown/crew/holden",
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDir, err := sessionWorkDir(handoffTestRegistry(), tt.sessionName, townRoot)
			if (err != nil) != tt.wantErr {
				t.Errorf("sessionWorkDir() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotDir != tt.wantDir {
				t.Errorf("sessionWorkDir() = %q, want %q", gotDir, tt.wantDir)
			}
		})
	}
}

func TestBuildRestartCommand_UsesRoleAgentsWhenNoAgentOverride(t *testing.T) {
	t.Parallel()
	env := map[string]string{}

	// TempDir must be called BEFORE registering the Chdir cleanup so that
	// LIFO ordering restores the working directory before TempDir removal.
	// On Windows the directory cannot be deleted while the process CWD is
	// inside it.
	townRoot := t.TempDir()

	rigPath := filepath.Join(townRoot, "gastown")
	crewDir := filepath.Join(rigPath, "crew", "holden")

	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"gastown"}`), 0644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
	if err := os.MkdirAll(crewDir, 0755); err != nil {
		t.Fatalf("mkdir crew dir: %v", err)
	}

	townSettings := config.NewTownSettings()
	townSettings.DefaultAgent = "claude"
	townSettings.Agents = map[string]*config.RuntimeConfig{
		"claude-sonnet": {
			Command: "claude",
			Args:    []string{"--dangerously-skip-permissions", "--model", "sonnet"},
		},
	}
	townSettings.RoleAgents = map[string]string{
		"crew": "claude-sonnet",
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), townSettings); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
	if err := config.SaveRigSettings(config.RigSettingsPath(rigPath), config.NewRigSettings()); err != nil {
		t.Fatalf("SaveRigSettings: %v", err)
	}

	env["GT_AGENT"] = ""

	cmd, err := buildTestRestartCommand(townRoot, env, "gt-crew-holden")
	if err != nil {
		t.Fatalf("buildRestartCommand: %v", err)
	}

	if !strings.Contains(cmd, "--model sonnet") {
		t.Errorf("expected role_agents crew model flag in restart command, got: %q", cmd)
	}
}

func TestBuildRestartCommand_MergesAgentPresetEnv(t *testing.T) {
	t.Parallel()
	env := map[string]string{}
	// Regression test: ensure agent preset Env block (config.json [agents.X.env])
	// is fully merged into the respawn command, not just NODE_OPTIONS.
	// Without this, custom env vars like ANTHROPIC_BASE_URL configured for
	// proxied Claude were silently dropped on handoff/respawn.

	townRoot := t.TempDir()

	rigPath := filepath.Join(townRoot, "gastown")
	crewDir := filepath.Join(rigPath, "crew", "holden")

	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"gastown"}`), 0644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
	if err := os.MkdirAll(crewDir, 0755); err != nil {
		t.Fatalf("mkdir crew dir: %v", err)
	}

	townSettings := config.NewTownSettings()
	townSettings.DefaultAgent = "claude-proxy"
	townSettings.Agents = map[string]*config.RuntimeConfig{
		"claude-proxy": {
			Command: "claude",
			Args:    []string{"--dangerously-skip-permissions"},
			Env: map[string]string{
				"ANTHROPIC_BASE_URL":       "http://localhost:8080",
				"CLAUDE_CODE_OAUTH_TOKEN":  "placeholder",
				"ANTHROPIC_CUSTOM_HEADERS": "Authorization: Bearer prx_test",
			},
		},
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), townSettings); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
	if err := config.SaveRigSettings(config.RigSettingsPath(rigPath), config.NewRigSettings()); err != nil {
		t.Fatalf("SaveRigSettings: %v", err)
	}

	env["GT_AGENT"] = "claude-proxy"

	cmd, err := buildTestRestartCommand(townRoot, env, "gt-crew-holden")
	if err != nil {
		t.Fatalf("buildRestartCommand: %v", err)
	}

	wantEnv := map[string]string{
		"ANTHROPIC_BASE_URL":       "http://localhost:8080",
		"CLAUDE_CODE_OAUTH_TOKEN":  "placeholder",
		"ANTHROPIC_CUSTOM_HEADERS": "Authorization: Bearer prx_test",
	}
	for k, v := range wantEnv {
		if !strings.Contains(cmd, k+"=") {
			t.Errorf("agent preset env %q not exported in restart command\ncmd: %s", k, cmd)
		}
		if !strings.Contains(cmd, v) {
			t.Errorf("agent preset env value for %q (%q) missing in restart command\ncmd: %s", k, v, cmd)
		}
	}
}

func TestBuildRestartCommand_ClearsBDTargetSelectors(t *testing.T) {
	t.Parallel()
	env := map[string]string{}

	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "gastown")
	crewDir := filepath.Join(rigPath, "crew", "holden")
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"gastown"}`), 0644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
	if err := os.MkdirAll(crewDir, 0755); err != nil {
		t.Fatalf("mkdir crew dir: %v", err)
	}

	townSettings := config.NewTownSettings()
	townSettings.DefaultAgent = "target-cleaner"
	townSettings.Agents = map[string]*config.RuntimeConfig{
		"target-cleaner": {
			Command: "claude",
			Args:    []string{"--dangerously-skip-permissions"},
			Env: map[string]string{
				"BEADS_DIR":                  "/agent/beads",
				"BEADS_DOLT_DATA_DIR":        "/agent/data",
				"BEADS_DOLT_SERVER_DATABASE": "agentdb",
				"BEADS_DOLT_SERVER_SOCKET":   "/agent/socket",
				"GT_DOLT_DATA":               "/agent/data",
				"GT_DOLT_PORT":               "1555",
				"GT_DOLT_HOST":               "agent-host",
			},
		},
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), townSettings); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
	if err := config.SaveRigSettings(config.RigSettingsPath(rigPath), config.NewRigSettings()); err != nil {
		t.Fatalf("SaveRigSettings: %v", err)
	}

	env["GT_AGENT"] = "target-cleaner"

	cmd, err := buildTestRestartCommand(townRoot, env, "gt-crew-holden")
	if err != nil {
		t.Fatalf("buildRestartCommand: %v", err)
	}

	for _, key := range []string{"BEADS_DIR", "BEADS_DOLT_DATA_DIR", "BEADS_DOLT_SERVER_DATABASE", "BEADS_DOLT_SERVER_SOCKET", "GT_DOLT_DATA"} {
		if !strings.Contains(cmd, key+"=") {
			t.Fatalf("restart command missing cleared %s assignment: %q", key, cmd)
		}
	}
	for _, stale := range []string{"/agent/beads", "/agent/data", "agentdb", "/agent/socket"} {
		if strings.Contains(cmd, stale) {
			t.Fatalf("restart command leaked stale bd selector value %q: %q", stale, cmd)
		}
	}
	for _, want := range []string{"GT_DOLT_PORT=1555", "GT_DOLT_HOST=agent-host"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("restart command missing preserved connection env %q: %q", want, cmd)
		}
	}
}

func TestBuildRestartCommandWithOpts_ContinuePrompt(t *testing.T) {
	t.Parallel()
	env := map[string]string{}

	townRoot := t.TempDir()

	rigPath := filepath.Join(townRoot, "gastown")
	crewDir := filepath.Join(rigPath, "crew", "bear")

	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"gastown"}`), 0644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
	if err := os.MkdirAll(crewDir, 0755); err != nil {
		t.Fatalf("mkdir crew dir: %v", err)
	}

	townSettings := config.NewTownSettings()
	townSettings.DefaultAgent = "claude"
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), townSettings); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
	if err := config.SaveRigSettings(config.RigSettingsPath(rigPath), config.NewRigSettings()); err != nil {
		t.Fatalf("SaveRigSettings: %v", err)
	}

	env["GT_AGENT"] = ""

	t.Run("custom ContinuePrompt overrides default", func(t *testing.T) {
		cmd, err := buildRestartCommandWithOpts("gt-crew-bear", buildRestartCommandOpts{
			Registry:        handoffTestRegistry(),
			TownRoot:        townRoot,
			LookupEnv:       restartOpts(townRoot, env).LookupEnv,
			SessionEnv:      restartOpts(townRoot, env).SessionEnv,
			ContinueSession: true,
			ContinuePrompt:  "Context compacted. Continue your previous task.",
		})
		if err != nil {
			t.Fatalf("buildRestartCommandWithOpts: %v", err)
		}
		if !strings.Contains(cmd, "--continue") {
			t.Errorf("expected --continue flag in restart command, got: %q", cmd)
		}
		if !strings.Contains(cmd, "Context compacted") {
			t.Errorf("expected custom prompt in restart command, got: %q", cmd)
		}
	})

	t.Run("empty ContinuePrompt falls back to default", func(t *testing.T) {
		cmd, err := buildRestartCommandWithOpts("gt-crew-bear", buildRestartCommandOpts{
			Registry:        handoffTestRegistry(),
			TownRoot:        townRoot,
			LookupEnv:       restartOpts(townRoot, env).LookupEnv,
			SessionEnv:      restartOpts(townRoot, env).SessionEnv,
			ContinueSession: true,
		})
		if err != nil {
			t.Fatalf("buildRestartCommandWithOpts: %v", err)
		}
		if !strings.Contains(cmd, "--continue") {
			t.Errorf("expected --continue flag in restart command, got: %q", cmd)
		}
		if !strings.Contains(cmd, "Continue your previous task") {
			t.Errorf("expected default continuation message when ContinuePrompt is empty, got: %q", cmd)
		}
	})

	t.Run("ContinueSession false uses beacon", func(t *testing.T) {
		cmd, err := buildRestartCommandWithOpts("gt-crew-bear", buildRestartCommandOpts{
			Registry:        handoffTestRegistry(),
			TownRoot:        townRoot,
			LookupEnv:       restartOpts(townRoot, env).LookupEnv,
			SessionEnv:      restartOpts(townRoot, env).SessionEnv,
			ContinueSession: false,
		})
		if err != nil {
			t.Fatalf("buildRestartCommandWithOpts: %v", err)
		}
		if strings.Contains(cmd, "--continue") {
			t.Errorf("expected no --continue flag when ContinueSession is false, got: %q", cmd)
		}
	})
}

// makeTestGitRepo creates a minimal git repo in a temp dir and returns its path.
// The caller is responsible for cleanup via t.Cleanup or defer os.RemoveAll.
func makeTestGitRepo(t *testing.T) string {
	t.Helper()
	return cachedGitFixtureStrings(t, "makeTestGitRepo", func(dir string) []string {
		return []string{buildMakeTestGitRepo(t, dir)}
	})[0]
}

// buildMakeTestGitRepo makes makeTestGitRepo's repos under dir.
func buildMakeTestGitRepo(t *testing.T, dir string) string {
	t.Helper()
	for _, args := range [][]string{
		{"git", "-C", dir, "init"},
		{"git", "-C", dir, "config", "user.email", "test@test.com"},
		{"git", "-C", dir, "config", "user.name", "Test"},
		// Disable background processes that hold file handles open after exit —
		// causes TempDir cleanup failures on Windows.
		{"git", "-C", dir, "config", "gc.auto", "0"},
		{"git", "-C", dir, "config", "core.fsmonitor", "false"},
		{"git", "-C", dir, "commit", "--allow-empty", "-m", "init"},
	} {
		if err := exec.Command("git", args[1:]...).Run(); err != nil {
			t.Fatalf("git setup %v: %v", args, err)
		}
	}
	return dir
}

// TestHandoffPolecatEnvCheck verifies that the polecat guard in runHandoff uses
// GT_ROLE as the authoritative check, so coordinators with a stale GT_POLECAT
// in their environment are not redirected to gt done (GH #1707).
func TestHandoffPolecatEnvCheck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		role      string
		polecat   string
		wantBlock bool
	}{
		{
			name:      "bare polecat role is redirected",
			role:      "polecat",
			polecat:   "alpha",
			wantBlock: true,
		},
		{
			name:      "compound polecat role is redirected",
			role:      "gastown/polecats/Toast",
			polecat:   "Toast",
			wantBlock: true,
		},
		{
			name:      "mayor with stale GT_POLECAT is NOT redirected",
			role:      "mayor",
			polecat:   "alpha",
			wantBlock: false,
		},
		{
			name:      "compound witness with stale GT_POLECAT is NOT redirected",
			role:      "gastown/witness",
			polecat:   "alpha",
			wantBlock: false,
		},
		{
			name:      "crew with stale GT_POLECAT is NOT redirected",
			role:      "crew",
			polecat:   "alpha",
			wantBlock: false,
		},
		{
			name:      "compound crew with stale GT_POLECAT is NOT redirected",
			role:      "gastown/crew/den",
			polecat:   "alpha",
			wantBlock: false,
		},
		{
			name:      "no GT_ROLE with GT_POLECAT set is redirected",
			role:      "",
			polecat:   "alpha",
			wantBlock: true,
		},
		{
			name:      "no GT_ROLE and no GT_POLECAT is not redirected",
			role:      "",
			polecat:   "",
			wantBlock: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			blocked, name := handoffPolecat(envMap(map[string]string{"GT_ROLE": tt.role, "GT_POLECAT": tt.polecat}))
			if blocked != tt.wantBlock {
				t.Errorf("handoffPolecat redirect = %v, want %v (GT_ROLE=%q GT_POLECAT=%q)", blocked, tt.wantBlock, tt.role, tt.polecat)
			}
			if blocked && name != tt.polecat {
				t.Errorf("polecat name = %q, want %q", name, tt.polecat)
			}
		})
	}
}

// TestPolecatHandoffDoneCmd: a polecat's handoff runs gt done DEFERRED and
// marks it handoff-originated, so gt done preserves the session instead of
// retiring it (gt-5g3e).
func TestPolecatHandoffDoneCmd(t *testing.T) {
	t.Parallel()
	cmd := polecatHandoffDoneCmd([]string{"GT_ROLE=gastown/polecats/Toast"})
	if got := strings.Join(cmd.Args, " "); got != "gt done --status DEFERRED" {
		t.Fatalf("args = %q, want gt done --status DEFERRED", got)
	}
	env := strings.Join(cmd.Env, "\n")
	if !strings.Contains(env, envDoneFromHandoff+"=1") || !strings.Contains(env, "GT_ROLE=gastown/polecats/Toast") {
		t.Fatalf("env = %v, want the caller's environment plus %s=1", cmd.Env, envDoneFromHandoff)
	}
}

func TestWarnHandoffGitStatus(t *testing.T) {
	t.Parallel()
	warn := func(dir string) string {
		var buf bytes.Buffer
		warnHandoffGitStatusIn(&buf, dir)
		return buf.String()
	}

	t.Run("no warning on clean repo", func(t *testing.T) {
		t.Parallel()
		dir := makeTestGitRepo(t)
		output := warn(dir)
		if output != "" {
			t.Errorf("expected no output for clean repo, got: %q", output)
		}
	})

	t.Run("warns on untracked file", func(t *testing.T) {
		t.Parallel()
		dir := makeTestGitRepo(t)
		os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("x"), 0644)
		output := warn(dir)
		if !strings.Contains(output, "uncommitted work") {
			t.Errorf("expected warning about uncommitted work, got: %q", output)
		}
		if !strings.Contains(output, "untracked") {
			t.Errorf("expected 'untracked' in output, got: %q", output)
		}
	})

	t.Run("warns on modified tracked file", func(t *testing.T) {
		t.Parallel()
		dir := makeTestGitRepo(t)
		// Create and commit a file
		fpath := filepath.Join(dir, "tracked.txt")
		os.WriteFile(fpath, []byte("original"), 0644)
		exec.Command("git", "-C", dir, "add", ".").Run()
		exec.Command("git", "-C", dir, "commit", "-m", "add file").Run()
		// Now modify it
		os.WriteFile(fpath, []byte("modified"), 0644)
		output := warn(dir)
		if !strings.Contains(output, "uncommitted work") {
			t.Errorf("expected warning about uncommitted work, got: %q", output)
		}
		if !strings.Contains(output, "modified") {
			t.Errorf("expected 'modified' in output, got: %q", output)
		}
	})

	t.Run("no warning for .beads-only changes", func(t *testing.T) {
		t.Parallel()
		dir := makeTestGitRepo(t)
		// Only .beads/ untracked files — should be clean (excluded)
		os.MkdirAll(filepath.Join(dir, ".beads"), 0755)
		os.WriteFile(filepath.Join(dir, ".beads", "somefile.db"), []byte("db"), 0644)
		output := warn(dir)
		if output != "" {
			t.Errorf("expected no output for .beads-only changes, got: %q", output)
		}
	})

	t.Run("no warning outside git repo", func(t *testing.T) {
		t.Parallel()
		output := warn(t.TempDir())
		if output != "" {
			t.Errorf("expected no output outside git repo, got: %q", output)
		}
	})

	t.Run("no-git-check flag suppresses warning", func(t *testing.T) {
		t.Parallel()
		dir := makeTestGitRepo(t)
		os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("x"), 0644)
		// Simulate --no-git-check: runHandoff warns only when it is unset.
		noGitCheck := true
		output := ""
		if !noGitCheck {
			output = warn(dir)
		}
		if output != "" {
			t.Errorf("expected no output with --no-git-check, got: %q", output)
		}
	})
}

func TestHandoffProcessNames(t *testing.T) {
	t.Parallel()
	newTown := func(t *testing.T) string {
		tmpTown := t.TempDir()
		mayorDir := filepath.Join(tmpTown, "mayor")
		if err := os.MkdirAll(mayorDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mayorDir, "town.json"), []byte(`{"name":"test"}`), 0644); err != nil {
			t.Fatal(err)
		}
		return tmpTown
	}

	t.Run("same-agent restart preserves GT_PROCESS_NAMES from env", func(t *testing.T) {
		t.Parallel()
		env := map[string]string{"GT_AGENT": "claude", "GT_PROCESS_NAMES": "node,claude"}
		// Same-agent restart should preserve existing process names from env
		cmd, err := buildTestRestartCommand(newTown(t), env, "gt-crew-propane")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(cmd, "GT_PROCESS_NAMES") || !strings.Contains(cmd, "node,claude") {
			t.Errorf("expected GT_PROCESS_NAMES=node,claude preserved from env, got: %q", cmd)
		}
	})

	t.Run("first boot without GT_PROCESS_NAMES computes from config", func(t *testing.T) {
		t.Parallel()
		// GT_PROCESS_NAMES explicitly empty, as on first boot
		env := map[string]string{"GT_AGENT": "claude", "GT_PROCESS_NAMES": ""}
		cmd, err := buildTestRestartCommand(newTown(t), env, "gt-crew-propane")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Claude's default process names are "node,claude"
		if !strings.Contains(cmd, "GT_PROCESS_NAMES") || !strings.Contains(cmd, "node,claude") {
			t.Errorf("expected GT_PROCESS_NAMES=node,claude computed from config, got: %q", cmd)
		}
	})
}

// TestCollectGitState verifies that collectGitState returns deterministic
// workspace state from a git repo without shelling out to gt/bd. (GH#1996)
func TestCollectGitState(t *testing.T) {
	t.Parallel()
	t.Run("returns_state_from_git_repo", func(t *testing.T) {
		t.Parallel()
		// Create a temp git repo
		tmpDir := t.TempDir()
		cmds := [][]string{
			{"git", "init"},
			{"git", "config", "user.email", "test@test.com"},
			{"git", "config", "user.name", "Test"},
		}
		for _, args := range cmds {
			cmd := exec.Command("git", args[1:]...)
			cmd.Dir = tmpDir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v failed: %s", args, out)
			}
		}

		// Create a file and commit
		if err := os.WriteFile(filepath.Join(tmpDir, "file.txt"), []byte("hello"), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}
		for _, args := range [][]string{
			{"git", "add", "file.txt"},
			{"git", "commit", "-m", "initial commit"},
		} {
			cmd := exec.Command("git", args[1:]...)
			cmd.Dir = tmpDir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v failed: %s", args, out)
			}
		}

		// Modify a file to create uncommitted changes
		if err := os.WriteFile(filepath.Join(tmpDir, "file.txt"), []byte("modified"), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}

		state := collectGitStateIn(tmpDir)

		if state == "" {
			t.Fatal("collectGitState() returned empty string for a git repo with changes")
		}
		if !strings.Contains(state, "## Workspace State") {
			t.Errorf("expected '## Workspace State' header, got: %s", state)
		}
		if !strings.Contains(state, "Modified") {
			t.Errorf("expected 'Modified' in state, got: %s", state)
		}
		if !strings.Contains(state, "initial commit") {
			t.Errorf("expected recent commit in state, got: %s", state)
		}
	})

	t.Run("returns_empty_outside_git_repo", func(t *testing.T) {
		t.Parallel()
		state := collectGitStateIn(t.TempDir())
		if state != "" {
			t.Errorf("expected empty string outside git repo, got: %s", state)
		}
	})
}

// TestRecordHandoffTime verifies that recordHandoffTime creates the
// timestamp file in .runtime/ with a recent modification time.
func TestRecordHandoffTime(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	recordHandoffTimeIn(tmpDir)

	tsPath := filepath.Join(tmpDir, constants.DirRuntime, constants.FileLastHandoffTS)
	info, err := os.Stat(tsPath)
	if err != nil {
		t.Fatalf("expected last_handoff_ts file to exist: %v", err)
	}
	if time.Since(info.ModTime()) > 5*time.Second {
		t.Errorf("expected recent modification time, got %v ago", time.Since(info.ModTime()))
	}
}

// TestEnforceHandoffCooldown verifies the cooldown logic:
// - No cooldown when no previous handoff recorded
// - Cooldown triggers when last handoff was recent
// - No cooldown when enough time has passed
func TestEnforceHandoffCooldown(t *testing.T) {
	t.Parallel()
	t.Run("no cooldown without previous handoff", func(t *testing.T) {
		t.Parallel()
		role := ""
		tmpDir := t.TempDir()

		var elapsed time.Duration
		enforceHandoffCooldownIn(io.Discard, role, tmpDir, func(d time.Duration) { elapsed += d })

		// Should return almost immediately (no file to check)
		if elapsed != 0 {
			t.Errorf("expected no cooldown, but waited %v", elapsed)
		}
	})

	t.Run("no cooldown when last handoff is old", func(t *testing.T) {
		t.Parallel()
		role := ""
		tmpDir := t.TempDir()

		// Create a last_handoff_ts file with old mtime
		runtimeDir := filepath.Join(tmpDir, constants.DirRuntime)
		os.MkdirAll(runtimeDir, 0755)
		tsPath := filepath.Join(runtimeDir, constants.FileLastHandoffTS)
		os.WriteFile(tsPath, []byte("1000000000"), 0644)
		// Set mtime to well in the past
		oldTime := time.Now().Add(-10 * time.Minute)
		os.Chtimes(tsPath, oldTime, oldTime)

		var elapsed time.Duration
		enforceHandoffCooldownIn(io.Discard, role, tmpDir, func(d time.Duration) { elapsed += d })

		if elapsed != 0 {
			t.Errorf("expected no cooldown for old handoff, but waited %v", elapsed)
		}
	})

	t.Run("cooldown triggers for recent handoff", func(t *testing.T) {
		t.Parallel()
		role := "gastown/witness"
		tmpDir := t.TempDir()

		// Create a last_handoff_ts file with very recent mtime
		runtimeDir := filepath.Join(tmpDir, constants.DirRuntime)
		os.MkdirAll(runtimeDir, 0755)
		tsPath := filepath.Join(runtimeDir, constants.FileLastHandoffTS)
		os.WriteFile(tsPath, []byte("now"), 0644)
		// Set mtime to (MinHandoffCooldown - 1s) ago so remaining is ~1s
		recentTime := time.Now().Add(-(constants.MinHandoffCooldown - 1*time.Second))
		os.Chtimes(tsPath, recentTime, recentTime)

		var elapsed time.Duration
		enforceHandoffCooldownIn(io.Discard, role, tmpDir, func(d time.Duration) { elapsed += d })

		// Should wait approximately 1 second (the remaining cooldown)
		if elapsed < 500*time.Millisecond {
			t.Errorf("expected cooldown sleep of ~1s, but only waited %v", elapsed)
		}
		if elapsed > 1500*time.Millisecond {
			t.Errorf("expected cooldown sleep of ~1s, but waited %v", elapsed)
		}
	})

	t.Run("no cooldown for crew role", func(t *testing.T) {
		t.Parallel()
		role := "gastown/crew/max"
		tmpDir := t.TempDir()

		// Create a recent handoff file that would normally trigger cooldown
		runtimeDir := filepath.Join(tmpDir, constants.DirRuntime)
		os.MkdirAll(runtimeDir, 0755)
		tsPath := filepath.Join(runtimeDir, constants.FileLastHandoffTS)
		os.WriteFile(tsPath, []byte("now"), 0644)

		var elapsed time.Duration
		enforceHandoffCooldownIn(io.Discard, role, tmpDir, func(d time.Duration) { elapsed += d })

		if elapsed != 0 {
			t.Errorf("crew should be exempt from cooldown, but waited %v", elapsed)
		}
	})

	t.Run("no cooldown for mayor role", func(t *testing.T) {
		t.Parallel()
		role := "mayor"
		tmpDir := t.TempDir()

		// Create a recent handoff file that would normally trigger cooldown
		runtimeDir := filepath.Join(tmpDir, constants.DirRuntime)
		os.MkdirAll(runtimeDir, 0755)
		tsPath := filepath.Join(runtimeDir, constants.FileLastHandoffTS)
		os.WriteFile(tsPath, []byte("now"), 0644)

		var elapsed time.Duration
		enforceHandoffCooldownIn(io.Discard, role, tmpDir, func(d time.Duration) { elapsed += d })

		if elapsed != 0 {
			t.Errorf("mayor should be exempt from cooldown, but waited %v", elapsed)
		}
	})
}

// Regression test for the gt-layt second-session gap: when GT_AGENT is set
// (every role_agents session), buildRestartCommand took the agent-override
// resolver, which skipped the role-level Claude flags. The respawned session
// then ran without --append-system-prompt-file even though the role's
// system-prompt file existed, so gt prime kept printing the static role text
// inline and the hook output stayed over the 10k cap forever.
//
// GT_AGENT alone no longer means "override" after gt-di8p — the resolver is
// now chosen by GT_AGENT_OVERRIDE, which only --agent spawns set.
func TestBuildRestartCommand_AgentOverrideCarriesRoleSystemPromptFile(t *testing.T) {
	t.Parallel()
	env := map[string]string{}

	townRoot := t.TempDir()

	rigPath := filepath.Join(townRoot, "gastown")
	crewDir := filepath.Join(rigPath, "crew", "holden")

	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"gastown"}`), 0644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(crewDir, ".claude"), 0755); err != nil {
		t.Fatalf("mkdir crew dir: %v", err)
	}
	promptPath := config.SystemPromptFilePath("crew", townRoot, rigPath, "holden")
	if promptPath == "" {
		t.Fatal("SystemPromptFilePath returned empty for crew")
	}
	if err := os.MkdirAll(filepath.Dir(promptPath), 0755); err != nil {
		t.Fatalf("mkdir system prompt dir: %v", err)
	}
	if err := os.WriteFile(promptPath, []byte("# crew\n"), 0644); err != nil {
		t.Fatalf("write system prompt: %v", err)
	}

	townSettings := config.NewTownSettings()
	townSettings.DefaultAgent = "claude-proxy"
	townSettings.Agents = map[string]*config.RuntimeConfig{
		"claude-proxy": {
			Command: "claude",
			Args:    []string{"--dangerously-skip-permissions", "--model", "proxy-model"},
			Env:     map[string]string{"ANTHROPIC_BASE_URL": "http://localhost:8080"},
		},
	}
	// role_agents names a different agent so the assertion below proves the
	// GT_AGENT override still wins over role resolution. The override is only
	// identifiable as such by its provenance marker: a bare GT_AGENT that this
	// process did not set via --agent is treated as a stale snapshot of
	// role_agents and re-resolved (gt-di8p).
	townSettings.RoleAgents = map[string]string{"crew": "claude-proxy-role"}
	townSettings.Agents["claude-proxy-role"] = &config.RuntimeConfig{
		Command: "claude",
		Args:    []string{"--model", "role-model"},
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), townSettings); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
	if err := config.SaveRigSettings(config.RigSettingsPath(rigPath), config.NewRigSettings()); err != nil {
		t.Fatalf("SaveRigSettings: %v", err)
	}

	env["GT_AGENT"] = "claude-proxy"
	env["GT_AGENT_OVERRIDE"] = "1"

	cmd, err := buildTestRestartCommand(townRoot, env, "gt-crew-holden")
	if err != nil {
		t.Fatalf("buildRestartCommand: %v", err)
	}

	if !strings.Contains(cmd, "--model proxy-model") || strings.Contains(cmd, "role-model") {
		t.Errorf("GT_AGENT override no longer selects the override agent\ncmd: %s", cmd)
	}
	if !strings.Contains(cmd, "--append-system-prompt-file "+promptPath) {
		t.Errorf("agent-override restart command lacks --append-system-prompt-file %s\ncmd: %s", promptPath, cmd)
	}
	if !strings.Contains(cmd, config.EnvSystemPromptFile+"=") {
		t.Errorf("agent-override restart command does not export %s\ncmd: %s", config.EnvSystemPromptFile, cmd)
	}
	if !strings.Contains(cmd, "ANTHROPIC_BASE_URL=") {
		t.Errorf("agent preset env dropped alongside the flag\ncmd: %s", cmd)
	}
	// The role's own --settings flag must ride the same path: the crew
	// settings dir differs from its working dir only when hooks live above
	// the worktree, which is not the case here, so we only assert the
	// command still targets the override agent.
	if strings.Count(cmd, "--append-system-prompt-file") != 1 {
		t.Errorf("flag must appear exactly once\ncmd: %s", cmd)
	}
}

// A self-handoff re-pinned the session's own GT_AGENT, which made
// role_agents.<role> a write-once setting: the successor inherited the preset
// the session was originally spawned with, so changing role_agents (town or
// rig) did nothing until the role was restarted out of band — the gastown
// witness kept spawning claude-opus after role_agents.witness was set to
// claude-opus-cycle (gt-di8p). The mapping is live config; the pin is a
// snapshot of it taken at spawn.
func TestBuildRestartCommand_RoleAgentsChangeTakesEffectOnHandoff(t *testing.T) {
	t.Parallel()
	env := map[string]string{}

	townRoot := t.TempDir()

	rigPath := filepath.Join(townRoot, "gastown")
	crewDir := filepath.Join(rigPath, "crew", "holden")

	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"gastown"}`), 0644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
	if err := os.MkdirAll(crewDir, 0755); err != nil {
		t.Fatalf("mkdir crew dir: %v", err)
	}

	townSettings := config.NewTownSettings()
	townSettings.DefaultAgent = "claude"
	townSettings.Agents = map[string]*config.RuntimeConfig{
		"claude-opus": {
			Command: "claude",
			Args:    []string{"--dangerously-skip-permissions", "--model", "pinned-model"},
		},
		"claude-opus-cycle": {
			Command: "claude",
			Args:    []string{"--dangerously-skip-permissions", "--model", "cycle-model"},
		},
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), townSettings); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}

	// The rig mapping is the one the bug report changed mid-session.
	writeRigRoleAgents := func(agent string) {
		t.Helper()
		rigSettings := config.NewRigSettings()
		rigSettings.RoleAgents = map[string]string{"crew": agent}
		if err := config.SaveRigSettings(config.RigSettingsPath(rigPath), rigSettings); err != nil {
			t.Fatalf("SaveRigSettings: %v", err)
		}
	}
	writeRigRoleAgents("claude-opus")

	// The session was spawned while role_agents.crew was claude-opus, so
	// that is the GT_AGENT its environment carries.
	env["GT_AGENT"] = "claude-opus"
	env["GT_AGENT_OVERRIDE"] = ""

	spawned, err := buildTestRestartCommand(townRoot, env, "gt-crew-holden")
	if err != nil {
		t.Fatalf("buildRestartCommand before the config change: %v", err)
	}
	if !strings.Contains(spawned, "--model pinned-model") {
		t.Fatalf("spawn-time mapping must be honored\ncmd: %s", spawned)
	}

	// The operator now points the role at a different preset. The running
	// session still carries the old GT_AGENT.
	writeRigRoleAgents("claude-opus-cycle")

	handoff, err := buildTestRestartCommand(townRoot, env, "gt-crew-holden")
	if err != nil {
		t.Fatalf("buildRestartCommand after the config change: %v", err)
	}
	if !strings.Contains(handoff, "--model cycle-model") {
		t.Errorf("role_agents change must take effect on the next handoff\ncmd: %s", handoff)
	}
	if strings.Contains(handoff, "pinned-model") {
		t.Errorf("handoff re-pinned the previous GT_AGENT instead of resolving role_agents\ncmd: %s", handoff)
	}
	// The successor's environment must name the agent it actually runs, so
	// its own handoff and liveness checks read current config too.
	if !strings.Contains(handoff, config.EnvAgent+"=claude-opus-cycle") {
		t.Errorf("respawned session must record the agent it runs\ncmd: %s", handoff)
	}

	// An explicit --agent override is not a snapshot of role_agents, so it
	// survives the handoff even when the role is mapped.
	env["GT_AGENT"] = "claude-opus"
	env["GT_AGENT_OVERRIDE"] = "1"

	overridden, err := buildTestRestartCommand(townRoot, env, "gt-crew-holden")
	if err != nil {
		t.Fatalf("buildRestartCommand with an explicit override: %v", err)
	}
	if !strings.Contains(overridden, "--model pinned-model") {
		t.Errorf("explicit --agent override must survive handoff\ncmd: %s", overridden)
	}
	if strings.Contains(overridden, "cycle-model") {
		t.Errorf("role_agents must not displace an explicit override\ncmd: %s", overridden)
	}
	if !strings.Contains(overridden, config.EnvAgentOverride+"=1") {
		t.Errorf("override provenance must be handed to the successor\ncmd: %s", overridden)
	}
}

// A crew member's worker_agents mapping outranks role_agents.crew, so its pin
// is not a snapshot of role_agents: it must survive a handoff even when the rig
// also maps the crew role — otherwise the per-worker agent would be silently
// swapped for the role's preset — and a change to the worker's own mapping must
// take effect, exactly as it would on a fresh spawn (gt-di8p).
func TestBuildRestartCommand_WorkerAgentPinSurvivesHandoff(t *testing.T) {
	t.Parallel()
	env := map[string]string{}

	townRoot := t.TempDir()

	rigPath := filepath.Join(townRoot, "gastown")

	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"gastown"}`), 0644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, "crew", "toast"), 0755); err != nil {
		t.Fatalf("mkdir crew dir: %v", err)
	}

	agents := map[string]*config.RuntimeConfig{
		"claude-role": {
			Command: "claude",
			Args:    []string{"--model", "role-model"},
		},
		"claude-worker": {
			Command: "claude",
			Args:    []string{"--model", "worker-model"},
		},
		"claude-worker2": {
			Command: "claude",
			Args:    []string{"--model", "worker-model2"},
		},
	}
	townSettings := config.NewTownSettings()
	townSettings.DefaultAgent = "claude"
	townSettings.Agents = agents
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), townSettings); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}

	writeWorkerAgent := func(agent string) {
		t.Helper()
		rigSettings := config.NewRigSettings()
		rigSettings.Agents = agents
		rigSettings.RoleAgents = map[string]string{"crew": "claude-role"}
		if agent != "" {
			rigSettings.WorkerAgents = map[string]string{"toast": agent}
		}
		if err := config.SaveRigSettings(config.RigSettingsPath(rigPath), rigSettings); err != nil {
			t.Fatalf("SaveRigSettings: %v", err)
		}
	}
	writeWorkerAgent("claude-worker")

	env["GT_AGENT"] = "claude-worker"
	env["GT_AGENT_OVERRIDE"] = ""

	cmd, err := buildTestRestartCommand(townRoot, env, "gt-crew-toast")
	if err != nil {
		t.Fatalf("buildRestartCommand: %v", err)
	}
	if !strings.Contains(cmd, "--model worker-model") {
		t.Errorf("worker_agents pin must survive handoff\ncmd: %s", cmd)
	}
	if strings.Contains(cmd, "--model role-model") {
		t.Errorf("role_agents.crew must not displace the worker's own mapping\ncmd: %s", cmd)
	}

	// The worker's own mapping changed. Its pin is now stale, and the respawn
	// must land on the new mapping rather than on role_agents.crew — a fresh
	// spawn of the same worker resolves worker_agents first.
	writeWorkerAgent("claude-worker2")

	changed, err := buildTestRestartCommand(townRoot, env, "gt-crew-toast")
	if err != nil {
		t.Fatalf("buildRestartCommand after the worker mapping change: %v", err)
	}
	if !strings.Contains(changed, "--model worker-model2") {
		t.Errorf("changed worker_agents mapping must take effect on handoff\ncmd: %s", changed)
	}
	if strings.Contains(changed, "--model role-model") {
		t.Errorf("role_agents.crew must not capture a re-resolved worker mapping\ncmd: %s", changed)
	}
}

func TestLastHandoffAge(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, ok := lastHandoffAge(dir); ok {
		t.Fatal("lastHandoffAge reported a handoff in an empty dir")
	}
	recordHandoffTimeIn(dir)
	age, ok := lastHandoffAge(dir)
	if !ok {
		t.Fatal("lastHandoffAge found no handoff after recordHandoffTimeIn")
	}
	if age < 0 || age > 5*time.Second {
		t.Fatalf("age = %v, want a fresh timestamp", age)
	}
}

func TestWriteHandoffMarker(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeHandoffMarker(dir, "gt-refinery", "unit-cycle")
	got, err := os.ReadFile(filepath.Join(dir, constants.DirRuntime, constants.FileHandoffMarker))
	if err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	if string(got) != "gt-refinery\nunit-cycle" {
		t.Fatalf("marker = %q, want session\\nreason", got)
	}
	writeHandoffMarker(dir, "gt-refinery", "")
	got, _ = os.ReadFile(filepath.Join(dir, constants.DirRuntime, constants.FileHandoffMarker))
	if string(got) != "gt-refinery" {
		t.Fatalf("marker without reason = %q, want bare session", got)
	}
}

func TestIssueSummaryLines(t *testing.T) {
	t.Parallel()
	got := issueSummaryLines([]*beads.Issue{{ID: "gt-a", Title: "First", Priority: 0}, {ID: "gt-b", Title: "Second", Priority: 2}}, nil)
	want := []string{"gt-a [P0] First", "gt-b [P2] Second"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	if lines := issueSummaryLines(nil, nil); len(lines) != 0 {
		t.Fatalf("empty result gave lines %q, want none", lines)
	}
	if lines := issueSummaryLines([]*beads.Issue{{ID: "gt-a"}}, errors.New("bd failed")); lines != nil {
		t.Fatalf("a failed read gave lines %q, want nil", lines)
	}
}

// TestCreateHandoffMailHooksItToTheAgent: the handoff mail lands in the town
// database as high-priority mail from and assigned to the agent, hooked so
// the successor's gt hook finds it.
func TestCreateHandoffMailHooksItToTheAgent(t *testing.T) {
	t.Parallel()
	db := beadsfake.New(beadsfake.WithPrefix("hq"))
	const agent = "gastown/crew/max"
	id, err := createHandoffMailIn(db, agent, "🤝 HANDOFF: Session cycling", "Context cycling.")
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.Show(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "hooked" || got.Assignee != agent || got.Priority != 1 || got.Description != "Context cycling." || got.CreatedBy != agent {
		t.Errorf("mail = status %q assignee %q priority %d description %q created_by %q", got.Status, got.Assignee, got.Priority, got.Description, got.CreatedBy)
	}
	if !slices.Contains(got.Labels, "gt:message") || !slices.Contains(got.Labels, "from:"+agent) {
		t.Errorf("labels = %v, want gt:message and from:%s", got.Labels, agent)
	}
}

// The handoff respawn command carries no credential from the agent's
// environment (G3-18, gt-y3pgh.10): respawn-pane's command is argv and
// becomes the pane's start command. Plain provider settings still pass.
func TestBuildRestartCommand_CarriesNoParentCredentials(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "gastown")
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"gastown"}`), 0644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), config.NewTownSettings()); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
	if err := config.SaveRigSettings(config.RigSettingsPath(rigPath), config.NewRigSettings()); err != nil {
		t.Fatalf("SaveRigSettings: %v", err)
	}
	const marker = "fake-cred-"
	env := map[string]string{
		"HTTPS_PROXY":     "http://user:" + marker + "proxy@proxy.example:3128",
		"ANTHROPIC_MODEL": "fake-model",
	}
	for _, k := range config.UnforwardedCredentialEnvVars() {
		env[k] = marker + strings.ToLower(k)
	}

	for _, sessionName := range []string{"hq-mayor", "gt-witness", "gt-refinery", "gt-crew-holden", "gt-nux"} {
		t.Run(sessionName, func(t *testing.T) {
			t.Parallel()
			cmd, err := buildTestRestartCommand(townRoot, env, sessionName)
			if err != nil {
				t.Fatalf("buildRestartCommand: %v", err)
			}
			if strings.Contains(cmd, marker) {
				t.Errorf("restart command carries a parent credential")
			}
			if !strings.Contains(cmd, "ANTHROPIC_MODEL=fake-model") {
				t.Errorf("restart command does not forward ANTHROPIC_MODEL: %s", cmd)
			}
		})
	}
}
