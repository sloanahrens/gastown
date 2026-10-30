package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
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

	tests := []struct {
		name        string
		sessionName string
		mkdirs      []string // created under the town root before the call
		wantDir     string   // relative to the town root; "" is the town root itself
		files       []string // regular files created under the town root before the call
		wantErr     bool
	}{
		{
			name:        "crew runs from crew subdirectory",
			sessionName: "gt-crew-holden",
			wantDir:     "gastown/crew/holden",
			wantErr:     false,
		},
		{
			// The overseer is the human operator at the town root; the
			// deacon dir it used to map to belongs to a retired role.
			name:        "overseer runs from town root",
			sessionName: "hq-overseer",
			wantDir:     "",
			wantErr:     false,
		},
		{
			// The polecat's git worktree is polecats/<name>/<rig>, not the
			// polecat directory above it.
			name:        "polecat runs from its worktree subdirectory",
			sessionName: "gt-furiosa",
			mkdirs:      []string{"gastown/polecats/furiosa/gastown"},
			wantDir:     "gastown/polecats/furiosa/gastown",
			wantErr:     false,
		},
		{
			name:        "legacy polecat clone at the polecat directory",
			sessionName: "gt-furiosa",
			mkdirs:      []string{"gastown/polecats/furiosa"},
			wantDir:     "gastown/polecats/furiosa",
			wantErr:     false,
		},
		{
			// A stat failure other than "not found" (here ENOTDIR, the
			// polecat directory being a file) is not the legacy layout.
			name:        "stat error other than not-exist is returned",
			sessionName: "gt-furiosa",
			files:       []string{"gastown/polecats/furiosa"},
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			for _, d := range tt.mkdirs {
				if err := os.MkdirAll(filepath.Join(townRoot, d), 0755); err != nil {
					t.Fatalf("mkdir %s: %v", d, err)
				}
			}
			for _, f := range tt.files {
				path := filepath.Join(townRoot, f)
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatalf("mkdir for %s: %v", f, err)
				}
				if err := os.WriteFile(path, nil, 0644); err != nil {
					t.Fatalf("write %s: %v", f, err)
				}
			}
			wantDir := filepath.Join(townRoot, tt.wantDir)

			gotDir, err := sessionWorkDir(handoffTestRegistry(), tt.sessionName, townRoot)
			if (err != nil) != tt.wantErr {
				t.Errorf("sessionWorkDir() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if gotDir != wantDir {
				t.Errorf("sessionWorkDir() = %q, want %q", gotDir, wantDir)
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

// TestPolecatHandoffDryRunDoesNotRunDone: a polecat's handoff --dry-run
// previews the gt done and does not run it; without --dry-run it runs once.
// A caller that is not a polecat is left to the rest of runHandoff.
func TestPolecatHandoffDryRunDoesNotRunDone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		env         map[string]string
		dryRun      bool
		wantHandled bool
		wantRuns    int
		wantOut     string
	}{
		{"dry run", map[string]string{"GT_ROLE": "gastown/polecats/Toast"}, true, true, 0, "Would execute: gt done --status DEFERRED"},
		{"real run", map[string]string{"GT_ROLE": "gastown/polecats/Toast"}, false, true, 1, "Polecat detected (Toast)"},
		{"not a polecat", map[string]string{"GT_ROLE": "gastown/crew/max"}, true, false, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			runs := 0
			handled, err := polecatHandoff(func(k string) string { return tt.env[k] }, nil, tt.dryRun, &out,
				func(*exec.Cmd) error { runs++; return nil })
			if err != nil {
				t.Fatalf("polecatHandoff: %v", err)
			}
			if handled != tt.wantHandled {
				t.Errorf("handled = %v, want %v", handled, tt.wantHandled)
			}
			if runs != tt.wantRuns {
				t.Errorf("gt done ran %d times, want %d", runs, tt.wantRuns)
			}
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("output = %q, want it to contain %q", out.String(), tt.wantOut)
			}
		})
	}
}

// TestRunHandoffPolecatDryRunDoesNotRunDone drives runHandoff itself, so it
// covers the wiring TestPolecatHandoffDryRunDoesNotRunDone cannot: that
// runHandoff passes --dry-run and the process environment to the polecat branch
// and reaches the done runner through runPolecatDone.
//
//testpolicy:allow parallel — runHandoff reads the --dry-run flag variable, GT_ROLE and runPolecatDone, all process-wide
func TestRunHandoffPolecatDryRunDoesNotRunDone(t *testing.T) {
	tests := []struct {
		name     string
		dryRun   bool
		wantRuns int
		wantOut  string
	}{
		{"dry run", true, 0, "Would execute: gt done --status DEFERRED"},
		{"real run", false, 1, "Polecat detected (Toast)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			//testpolicy:allow no-env — the process environment is the input under test: runHandoff reads GT_ROLE with os.Getenv
			t.Setenv("GT_ROLE", "gastown/polecats/Toast")
			prevDryRun, prevRun := handoffDryRun, runPolecatDone
			t.Cleanup(func() {
				//testpolicy:allow no-global-swap — restores the variables swapped below
				handoffDryRun, runPolecatDone = prevDryRun, prevRun
			})
			runs := 0
			//testpolicy:allow no-global-swap — runHandoff reads the flag variable and the done runner; neither is an argument
			handoffDryRun, runPolecatDone = tt.dryRun, func(*exec.Cmd) error { runs++; return nil }

			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			if err := runHandoff(cmd, nil); err != nil {
				t.Fatalf("runHandoff: %v", err)
			}
			if runs != tt.wantRuns {
				t.Errorf("gt done ran %d times, want %d", runs, tt.wantRuns)
			}
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("output = %q, want it to contain %q", out.String(), tt.wantOut)
			}
		})
	}
}

// fakeHandoffGit answers the handoff's workspace checks from canned values.
// A nil status with no statusErr is a clean worktree.
type fakeHandoffGit struct {
	notRepo   bool
	branch    string
	status    *git.UncommittedWorkStatus
	statusErr error
	log       string
}

func (f fakeHandoffGit) IsRepo() bool                      { return !f.notRepo }
func (f fakeHandoffGit) CurrentBranch() (string, error)    { return f.branch, nil }
func (f fakeHandoffGit) RecentCommits(int) (string, error) { return f.log, nil }
func (f fakeHandoffGit) CheckUncommittedWork() (*git.UncommittedWorkStatus, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	if f.status == nil {
		return &git.UncommittedWorkStatus{}, nil
	}
	return f.status, nil
}

// TestWarnHandoffGitStatus covers the warning's decisions over canned git
// state; TestIntegrationHandoffGitState reads a real repository.
func TestWarnHandoffGitStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		g    fakeHandoffGit
		want []string // substrings; nil means no output
	}{
		{name: "no warning on clean repo", g: fakeHandoffGit{}},
		{name: "no warning outside git repo", g: fakeHandoffGit{notRepo: true, status: &git.UncommittedWorkStatus{UntrackedFiles: []string{"x"}}}},
		{name: "no warning when the status cannot be read", g: fakeHandoffGit{statusErr: errors.New("git status failed")}},
		{
			name: "no warning for .beads-only changes",
			g:    fakeHandoffGit{status: &git.UncommittedWorkStatus{HasUncommittedChanges: true, UntrackedFiles: []string{".beads/somefile.db"}}},
		},
		{
			name: "warns on untracked file",
			g:    fakeHandoffGit{status: &git.UncommittedWorkStatus{HasUncommittedChanges: true, UntrackedFiles: []string{"dirty.txt"}}},
			want: []string{"uncommitted work", "untracked: dirty.txt", "--no-git-check"},
		},
		{
			name: "warns on modified tracked file",
			g:    fakeHandoffGit{status: &git.UncommittedWorkStatus{HasUncommittedChanges: true, ModifiedFiles: []string{"tracked.txt"}}},
			want: []string{"uncommitted work", "modified: tracked.txt"},
		},
		{
			name: "warns on unpushed commits",
			g:    fakeHandoffGit{status: &git.UncommittedWorkStatus{UnpushedCommits: 2}},
			want: []string{"uncommitted work", "2 unpushed commit(s)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			warnHandoffGitStatusWith(&buf, tt.g)
			out := buf.String()
			if tt.want == nil {
				if out != "" {
					t.Fatalf("expected no output, got: %q", out)
				}
				return
			}
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("output missing %q: %q", w, out)
				}
			}
		})
	}
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

// newHandoffTown writes a minimal town — mayor/town.json plus the crew worktree
// gt-crew-holden runs from — and returns its root.
func newHandoffTown(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"gastown"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown", "crew", "holden"), 0o755); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

// TestBuildRestartCommand_DoesNotPropagateTownRoot: the successor resolves its
// town root by walking up from its workdir, so the respawn command no longer
// exports GT_TOWN_ROOT. The tmux global environment's copy was handoff's
// fallback and went stale across a town move (gt-y3pgh.2.12).
func TestBuildRestartCommand_DoesNotPropagateTownRoot(t *testing.T) {
	t.Parallel()
	cmd, err := buildTestRestartCommand(newHandoffTown(t), map[string]string{}, "gt-crew-holden")
	if err != nil {
		t.Fatalf("buildRestartCommand: %v", err)
	}
	if strings.Contains(cmd, "GT_TOWN_ROOT") {
		t.Errorf("respawn command exports GT_TOWN_ROOT; the workspace walker is the one source\ncmd: %s", cmd)
	}
}

// TestDetectTownRootFromCwdUsesTheWalker: gt handoff's town root is whatever
// the workspace walker returns, and nothing else. A walk that errors or finds
// no town resolves to "": with the tmux global-env fallback deleted, GT_TOWN_ROOT
// read from that environment is no longer consulted, and no other source
// exists (gt-y3pgh.2.12).
func TestDetectTownRootFromCwdUsesTheWalker(t *testing.T) {
	t.Parallel()
	t.Run("returns the walker's town root", func(t *testing.T) {
		t.Parallel()
		walk := func() (string, error) { return "/towns/gastown", nil }
		if got := townRootFrom(walk); got != "/towns/gastown" {
			t.Errorf("townRootFrom() = %q, want the walker's %q", got, "/towns/gastown")
		}
	})
	t.Run("a failed walk resolves to nothing", func(t *testing.T) {
		t.Parallel()
		walk := func() (string, error) { return "", errors.New("not in a workspace") }
		if got := townRootFrom(walk); got != "" {
			t.Errorf("townRootFrom() = %q, want \"\" when the walk fails", got)
		}
	})
	t.Run("a walk with no town resolves to nothing", func(t *testing.T) {
		t.Parallel()
		walk := func() (string, error) { return "", nil }
		if got := townRootFrom(walk); got != "" {
			t.Errorf("townRootFrom() = %q, want \"\" when the walk finds no town", got)
		}
	})
}

// TestCollectGitState verifies that collectGitState renders deterministic
// workspace state from git without shelling out to gt/bd (GH#1996), over
// canned git state; TestIntegrationHandoffGitState reads a real repository.
func TestCollectGitState(t *testing.T) {
	t.Parallel()
	t.Run("renders branch, changes, stashes, unpushed and recent commits", func(t *testing.T) {
		t.Parallel()
		state := collectGitStateWith(fakeHandoffGit{
			branch: "main",
			status: &git.UncommittedWorkStatus{
				HasUncommittedChanges: true,
				ModifiedFiles:         []string{"file.txt"},
				UntrackedFiles:        []string{"new.txt"},
				StashCount:            1,
				UnpushedCommits:       3,
			},
			log: "abc1234 initial commit",
		})
		for _, want := range []string{
			"## Workspace State", "Branch: main", "Modified: file.txt", "Untracked: new.txt",
			"Stashes: 1", "Unpushed commits: 3", "Recent commits:\nabc1234 initial commit",
		} {
			if !strings.Contains(state, want) {
				t.Errorf("state missing %q:\n%s", want, state)
			}
		}
	})

	t.Run("caps long file lists", func(t *testing.T) {
		t.Parallel()
		var modified, untracked []string
		for i := 0; i < 12; i++ {
			modified = append(modified, fmt.Sprintf("m%d.go", i))
			untracked = append(untracked, fmt.Sprintf("u%d.go", i))
		}
		state := collectGitStateWith(fakeHandoffGit{status: &git.UncommittedWorkStatus{
			HasUncommittedChanges: true, ModifiedFiles: modified, UntrackedFiles: untracked,
		}})
		if !strings.Contains(state, "m9.go, ... (+2 more)") || strings.Contains(state, "m10.go") {
			t.Errorf("modified list not capped at 10:\n%s", state)
		}
		if !strings.Contains(state, "u4.go, ... (+7 more)") || strings.Contains(state, "u5.go") {
			t.Errorf("untracked list not capped at 5:\n%s", state)
		}
	})

	t.Run("returns_empty_outside_git_repo", func(t *testing.T) {
		t.Parallel()
		if state := collectGitStateWith(fakeHandoffGit{notRepo: true, branch: "main"}); state != "" {
			t.Errorf("expected empty string outside git repo, got: %s", state)
		}
	})

	t.Run("returns_empty_with_nothing_to_report", func(t *testing.T) {
		t.Parallel()
		if state := collectGitStateWith(fakeHandoffGit{}); state != "" {
			t.Errorf("expected empty string with nothing to report, got: %s", state)
		}
	})
}

// TestRecordHandoffTime verifies that recordHandoffTime creates the
// timestamp file in .runtime/ dated at the now it is given.
func TestRecordHandoffTime(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	recordHandoffTimeIn(tmpDir, now)

	tsPath := filepath.Join(tmpDir, constants.DirRuntime, constants.FileLastHandoffTS)
	info, err := os.Stat(tsPath)
	if err != nil {
		t.Fatalf("expected last_handoff_ts file to exist: %v", err)
	}
	if !info.ModTime().Equal(now) {
		t.Errorf("mtime = %v, want the injected now %v", info.ModTime(), now)
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
		enforceHandoffCooldownIn(io.Discard, role, tmpDir, time.Now, func(d time.Duration) { elapsed += d })

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
		enforceHandoffCooldownIn(io.Discard, role, tmpDir, time.Now, func(d time.Duration) { elapsed += d })

		if elapsed != 0 {
			t.Errorf("expected no cooldown for old handoff, but waited %v", elapsed)
		}
	})

	t.Run("cooldown triggers for recent handoff", func(t *testing.T) {
		t.Parallel()
		role := "gastown/witness"
		tmpDir := t.TempDir()
		now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

		// Create a last_handoff_ts file with very recent mtime
		runtimeDir := filepath.Join(tmpDir, constants.DirRuntime)
		os.MkdirAll(runtimeDir, 0755)
		tsPath := filepath.Join(runtimeDir, constants.FileLastHandoffTS)
		os.WriteFile(tsPath, []byte("now"), 0644)
		// One second short of the cooldown as of the injected now, so the
		// remaining wait is exactly that second.
		recentTime := now.Add(-(constants.MinHandoffCooldown - time.Second))
		os.Chtimes(tsPath, recentTime, recentTime)

		var elapsed time.Duration
		enforceHandoffCooldownIn(io.Discard, role, tmpDir, func() time.Time { return now }, func(d time.Duration) { elapsed += d })

		if want := time.Second; elapsed != want {
			t.Errorf("cooldown sleep = %v, want the remaining %v", elapsed, want)
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
		enforceHandoffCooldownIn(io.Discard, role, tmpDir, time.Now, func(d time.Duration) { elapsed += d })

		if elapsed != 0 {
			t.Errorf("crew should be exempt from cooldown, but waited %v", elapsed)
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
	if _, ok := lastHandoffAge(dir, time.Now()); ok {
		t.Fatal("lastHandoffAge reported a handoff in an empty dir")
	}
	recordHandoffTimeIn(dir, time.Now())
	age, ok := lastHandoffAge(dir, time.Now())
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

	for _, sessionName := range []string{"hq-overseer", "gt-witness", "gt-refinery", "gt-crew-holden", "gt-nux"} {
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

// TestHandoffSessionDryRunSkipsMoleculeCleanup: a self-handoff --dry-run only
// previews, so it must not close the attached molecule; a live one closes it
// once, before the session env update and the mail send (gt-fmhjf). The mail
// send fails in the live run so no pane is respawned.
//
// It drives handoffSession with fakes, not runHandoff: runHandoff needs a tmux
// session, TMUX_PANE and the handoff cooldown before it reaches the cleanup,
// and the dry-run flag reaches handoffSession as a field, not through
// handoffDryRun.
func TestHandoffSessionDryRunSkipsMoleculeCleanup(t *testing.T) {
	t.Parallel()
	mailErr := errors.New("dolt down")
	tests := []struct {
		name      string
		dryRun    bool
		wantCalls []string
		wantErr   error
	}{
		{"dry run leaves the molecule alone", true, nil, nil},
		{"live run closes it before the env update and the mail", false,
			[]string{"cleanupMolecule", "updateSessionEnv", "sendMail"}, mailErr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			steps := handoffSteps{
				buildPlan: func(string, buildRestartCommandOpts) (restartPlan, error) {
					return restartPlan{Command: "true"}, nil
				},
				updateSessionEnv: func(*session.PrefixRegistry, *tmux.Tmux, string, func(string) (string, bool)) {
					calls = append(calls, "updateSessionEnv")
				},
				cleanupMolecule: func() { calls = append(calls, "cleanupMolecule") },
				sendMail: func(string, string) (string, error) {
					calls = append(calls, "sendMail")
					return "", mailErr
				},
				logNoPersist: func(string, string, error) {},
			}
			err := handoffSession(sessionHandoff{
				reg:            handoffTestRegistry(),
				pane:           "%0",
				currentSession: "gt-crew-tester",
				targetSession:  "gt-crew-tester",
				dryRun:         tt.dryRun,
			}, steps)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if !slices.Equal(calls, tt.wantCalls) {
				t.Errorf("calls = %v, want %v", calls, tt.wantCalls)
			}
		})
	}
}

// A crew member who runs gt handoff from crew/max/src has a successor that
// starts in crew/max and reads the marker there. Both the marker and the
// cooldown go under the plan's workDir, not under the caller's cwd.
func TestHandoffMarkerWrittenInPlanWorkDir(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	srcDir := filepath.Join(townRoot, "gastown", "crew", "max", "src")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}

	plan, err := buildRestartPlan("gt-crew-max", restartOpts(townRoot, map[string]string{}))
	if err != nil {
		t.Fatalf("buildRestartPlan: %v", err)
	}
	crewDir := filepath.Join(townRoot, "gastown", "crew", "max")
	if plan.WorkDir != crewDir {
		t.Fatalf("plan.WorkDir = %q, want %q", plan.WorkDir, crewDir)
	}

	// srcDir is the caller's cwd, passed in rather than entered so the test
	// stays parallel (the testpolicy bans chdir).
	recordHandoffState(plan, srcDir, "gt-crew-max", "unit-cycle")

	for _, name := range []string{constants.FileHandoffMarker, constants.FileLastHandoffTS} {
		if _, err := os.Stat(filepath.Join(crewDir, constants.DirRuntime, name)); err != nil {
			t.Errorf("%s not under the successor's workDir: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(srcDir, constants.DirRuntime, name)); err == nil {
			t.Errorf("%s written under the caller's cwd %s", name, srcDir)
		}
	}
}

// A respawn exports the same git author first spawn does: the seat's bare
// name, never the compound address gt done's rework lease reads as another
// seat's divergence, so a handoff cannot break the successor's next commit
// (gt-7a955). GT_ROLE and BD_ACTOR stay compound.
//
// The fixture builds each session name from the registry's prefix and checks
// the expected author against config.AgentEnv, so it tests the stated
// criterion instead of a copied string, and no case can pass through
// DefaultPrefix.
//
// Witness, refinery, deacon and mayor are retired roles, but their session
// names still parse through this path, so the bare name must hold there too.
func TestRestartPlanExportsBareGitAuthorName(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	// Prefixes other than the default "gt": a fallback to DefaultPrefix
	// leaves a session name this registry cannot parse, failing the case.
	reg := session.NewPrefixRegistry()
	reg.Register("be", "beads")
	reg.Register("hq", "hq") // town prefix; with no rig behind it, it is its own rig

	tests := []struct {
		name    string
		role    string // config.AgentEnv's role, empty when it has no case for it
		rig     string
		seat    string
		session func(prefix, seat string) string
	}{
		{"polecat", constants.RolePolecat, "beads", "furiosa", session.PolecatSessionName},
		{"crew", constants.RoleCrew, "beads", "max", session.CrewSessionName},
		{"witness", "", "beads", "witness", session.PolecatSessionName},
		{"refinery", "", "beads", "refinery", session.PolecatSessionName},
		{"deacon", "", "hq", "deacon", session.PolecatSessionName},
		{"mayor", "", "hq", "mayor", session.PolecatSessionName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// First spawn's export is the criterion the respawn must meet.
			if tt.role != "" {
				env := config.AgentEnv(config.AgentEnvConfig{Role: tt.role, Rig: tt.rig, AgentName: tt.seat})
				if got := env["GIT_AUTHOR_NAME"]; got != tt.seat {
					t.Fatalf("config.AgentEnv exports GIT_AUTHOR_NAME=%q for a %s, want %q",
						got, tt.role, tt.seat)
				}
			}

			opts := restartOpts(townRoot, map[string]string{})
			opts.Registry = reg
			sessionName := tt.session(reg.PrefixForRig(tt.rig), tt.seat)
			plan, err := buildRestartPlan(sessionName, opts)
			if err != nil {
				t.Fatalf("buildRestartPlan(%q): %v", sessionName, err)
			}

			compound := tt.rig + "/polecats/" + tt.seat
			if tt.role == constants.RoleCrew {
				compound = tt.rig + "/crew/" + tt.seat
			}
			if want := "GIT_AUTHOR_NAME=" + tt.seat; !strings.Contains(plan.Command, want) {
				t.Errorf("command does not carry %s:\n%s", want, plan.Command)
			}
			if bad := "GIT_AUTHOR_NAME=" + compound; strings.Contains(plan.Command, bad) {
				t.Errorf("command carries the compound git author %s:\n%s", bad, plan.Command)
			}
			if want := "GT_ROLE=" + compound; !strings.Contains(plan.Command, want) {
				t.Errorf("command lost the compound %s:\n%s", want, plan.Command)
			}
		})
	}
}

// TestHandoffEnvIdentity checks the respawn exports the same four git identity
// variables the first spawn does: the seat's bare name as author and committer
// name and <name>@gastown.local as author and committer email, for a compound
// gtRole such as gastown/polecats/coma. They ride the environment, so no git
// config is written, and the compound address is never exported as the name.
func TestHandoffEnvIdentity(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	reg := session.NewPrefixRegistry()
	reg.Register("gt", "gastown")
	tests := []struct {
		name    string
		role    string
		rig     string
		seat    string
		session func(prefix, seat string) string
	}{
		{"polecat", constants.RolePolecat, "gastown", "coma", session.PolecatSessionName},
		{"crew", constants.RoleCrew, "gastown", "max", session.CrewSessionName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := restartOpts(townRoot, map[string]string{})
			opts.Registry = reg
			plan, err := buildRestartPlan(tt.session(reg.PrefixForRig(tt.rig), tt.seat), opts)
			if err != nil {
				t.Fatalf("buildRestartPlan: %v", err)
			}
			// First spawn's export is the criterion the respawn must meet.
			first := config.AgentEnv(config.AgentEnvConfig{Role: tt.role, Rig: tt.rig, AgentName: tt.seat})
			for _, key := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
				want := first[key]
				if want == "" {
					t.Fatalf("config.AgentEnv does not export %s for a %s", key, tt.role)
				}
				if !slices.Contains(strings.Fields(plan.Command), key+"="+want) {
					t.Errorf("respawn command does not carry %s=%s:\n%s", key, want, plan.Command)
				}
			}
			if want := tt.seat + "@gastown.local"; first["GIT_AUTHOR_EMAIL"] != want || first["GIT_COMMITTER_EMAIL"] != want {
				t.Errorf("first spawn emails = %q and %q, want %q", first["GIT_AUTHOR_EMAIL"], first["GIT_COMMITTER_EMAIL"], want)
			}
			compound := tt.rig + "/polecats/" + tt.seat
			if tt.role == constants.RoleCrew {
				compound = tt.rig + "/crew/" + tt.seat
			}
			for _, key := range []string{"GIT_COMMITTER_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
				if bad := key + "=" + compound; strings.Contains(plan.Command, bad) {
					t.Errorf("respawn command carries the compound address in %s:\n%s", key, plan.Command)
				}
			}
		})
	}
}

// TestHandoffEnvIdentityQuotesSeatName checks that a seat name needing shell
// quoting reaches the respawn command quoted, as the first spawn's export
// quotes it. The respawn builds the identity values into the shell command it
// runs, so an unquoted name would split or expand in that shell.
func TestHandoffEnvIdentityQuotesSeatName(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	reg := session.NewPrefixRegistry()
	reg.Register("gt", "gastown")
	const seat = "o'neil$x"
	opts := restartOpts(townRoot, map[string]string{})
	opts.Registry = reg
	plan, err := buildRestartPlan(session.PolecatSessionName(reg.PrefixForRig("gastown"), seat), opts)
	if err != nil {
		t.Fatalf("buildRestartPlan: %v", err)
	}
	quoted := config.ShellQuote(seat)
	for _, key := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		if want := key + "=" + quoted; !strings.Contains(plan.Command, want) {
			t.Errorf("respawn command does not carry %s:\n%s", want, plan.Command)
		}
		if bad := key + "=" + seat; strings.Contains(plan.Command, bad) {
			t.Errorf("respawn command exports %s unquoted:\n%s", bad, plan.Command)
		}
	}
}

// The plan's workDir wins; without one the state stays in cwd, and with
// neither there is no directory to write under.
func TestHandoffStateDir(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		plan restartPlan
		cwd  string
		want string
	}{
		{"plan workDir wins", restartPlan{WorkDir: "/town/gastown/crew/max"}, "/town/gastown/crew/max/src", "/town/gastown/crew/max"},
		{"empty plan falls back to cwd", restartPlan{}, "/town/gastown/crew/max/src", "/town/gastown/crew/max/src"},
		{"neither is empty", restartPlan{}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := handoffStateDir(tt.plan, tt.cwd); got != tt.want {
				t.Errorf("handoffStateDir() = %q, want %q", got, tt.want)
			}
		})
	}
}

// gt handoff --auto respawns nothing, but the post-compact prime still reads
// the marker from the session's workDir, not from where the command ran.
func TestAutoHandoffStateDir(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	srcDir := filepath.Join(townRoot, "gastown", "crew", "max", "src")
	crewDir := filepath.Join(townRoot, "gastown", "crew", "max")
	reg := handoffTestRegistry()

	tests := []struct {
		name     string
		session  string
		townRoot string
		want     string
	}{
		{"crew session resolves to its workDir", "gt-crew-max", townRoot, crewDir},
		{"unparseable session keeps cwd", "not-a-gt-session", townRoot, srcDir},
		{"no town root keeps cwd", "gt-crew-max", "", srcDir},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := autoHandoffStateDir(reg, tt.session, tt.townRoot, srcDir); got != tt.want {
				t.Errorf("autoHandoffStateDir() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The overseer's GT_ROLE is the role "overseer", not its session name. The
// handoff must resolve it to hq-overseer, or sessionWorkDir cannot parse it.
//
//testpolicy:allow parallel — getCurrentTmuxSession reads GT_ROLE from the process environment, which t.Setenv sets for the whole process
func TestGetCurrentTmuxSession_OverseerRole(t *testing.T) {
	//testpolicy:allow no-env — the process environment is the input under test: getCurrentTmuxSession takes no role argument
	t.Setenv("GT_ROLE", "overseer")

	got, err := getCurrentTmuxSession()
	if err != nil {
		t.Fatalf("getCurrentTmuxSession: %v", err)
	}
	if want := session.OverseerSessionName(); got != want {
		t.Fatalf("getCurrentTmuxSession = %q, want %q", got, want)
	}

	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"gastown"}`), 0644); err != nil {
		t.Fatal(err)
	}
	plan, err := buildRestartPlan(got, restartOpts(townRoot, map[string]string{}))
	if err != nil {
		t.Fatalf("buildRestartPlan(%q): %v", got, err)
	}
	if plan.WorkDir != townRoot {
		t.Errorf("plan.WorkDir = %q, want the town root %q", plan.WorkDir, townRoot)
	}
}

// resolveRoleToSession also parses the explicit target of handoff, hook and
// sling, so the overseer shortcut maps there too, not only for GT_ROLE.
func TestResolveRoleToSession_OverseerArgument(t *testing.T) {
	t.Parallel()
	reg := session.NewPrefixRegistry()
	for _, arg := range []string{"overseer", "Overseer"} {
		got, err := resolveRoleToSession(reg, arg)
		if err != nil {
			t.Fatalf("resolveRoleToSession(%q): %v", arg, err)
		}
		if want := session.OverseerSessionName(); got != want {
			t.Errorf("resolveRoleToSession(%q) = %q, want %q", arg, got, want)
		}
	}
}

// TestHandoffSessionNameNotTreatedAsBead: a crew or overseer session name has
// the shape of a short-prefix bead ID, but gt handoff <session-name> restarts
// that session; it must not take the bead branch and fail with "bead not
// found" (gt-ev685).
func TestHandoffSessionNameNotTreatedAsBead(t *testing.T) {
	t.Parallel()
	reg := handoffTestRegistry()
	for _, name := range []string{"gt-gastown-crew-max", "gt-crew-max", "zz-crew-max", "hq-overseer"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !looksLikeBeadID(name) {
				t.Fatalf("looksLikeBeadID(%q) = false: the case this test guards is gone", name)
			}
			if looksLikeHandoffBeadID(reg, name) {
				t.Errorf("looksLikeHandoffBeadID(%q) = true, want a session name", name)
			}
			got, err := resolveRoleToSession(reg, name)
			if err != nil || got != name {
				t.Errorf("resolveRoleToSession(%q) = %q, %v; want the name itself", name, got, err)
			}
		})
	}
	// A bead ID that merely contains "-crew-" is still a bead.
	for _, id := range []string{"gt-abc123", "bd-ka761", "hq-cv-abc", "ap-qtsup.16", "gt-fix-crew-1", "hq-wisp-crew-x", "gt-crew-", "gt-gastown-crew-"} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			if !looksLikeHandoffBeadID(reg, id) {
				t.Errorf("looksLikeHandoffBeadID(%q) = false, want a bead ID", id)
			}
		})
	}
}

// TestHandoffRemoteDryRunLeavesTargetEnv: a remote --dry-run only previews, so
// it must not rewrite the target's tmux env (the env update is the only
// SetEnvironment caller); a live one updates it once, after the target is
// found and before the respawn. A restart of another session is built from
// an environment without the caller's agent keys, so a handoff between agents
// of different kinds keeps the target's agent (gt-ev685).
func TestHandoffRemoteDryRunLeavesTargetEnv(t *testing.T) {
	t.Parallel()

	t.Run("dry run makes no env update", func(t *testing.T) {
		t.Parallel()
		var calls []string
		ops := recordingRemoteOps(&calls)
		err := runRemoteHandoff(ops, "gt-crew-max", restartPlan{Command: "true"}, true, true,
			func() { calls = append(calls, "updateEnv") })
		if err != nil {
			t.Fatalf("runRemoteHandoff: %v", err)
		}
		if want := []string{"hasSession", "sessionPane"}; !slices.Equal(calls, want) {
			t.Errorf("calls = %v, want %v", calls, want)
		}
	})

	t.Run("live run updates the env before the respawn", func(t *testing.T) {
		t.Parallel()
		var calls []string
		ops := recordingRemoteOps(&calls)
		err := runRemoteHandoff(ops, "gt-crew-max", restartPlan{Command: "true"}, false, true,
			func() { calls = append(calls, "updateEnv") })
		if err != nil {
			t.Fatalf("runRemoteHandoff: %v", err)
		}
		want := []string{"hasSession", "sessionPane", "updateEnv", "respawn", "switchClient"}
		if !slices.Equal(calls, want) {
			t.Errorf("calls = %v, want %v", calls, want)
		}
	})

	t.Run("a missing target gets no env update", func(t *testing.T) {
		t.Parallel()
		var calls []string
		ops := recordingRemoteOps(&calls)
		ops.hasSession = func(string) (bool, error) { return false, nil }
		err := runRemoteHandoff(ops, "gt-crew-max", restartPlan{Command: "true"}, false, false,
			func() { calls = append(calls, "updateEnv") })
		if err == nil {
			t.Fatal("runRemoteHandoff of a missing session = nil, want an error")
		}
		if len(calls) != 0 {
			t.Errorf("calls = %v, want none", calls)
		}
	})

	t.Run("handoffSession builds a remote target's restart and env without the caller's agent", func(t *testing.T) {
		t.Parallel()
		for _, dryRun := range []bool{true, false} {
			t.Run(fmt.Sprintf("dryRun=%v", dryRun), func(t *testing.T) {
				t.Parallel()
				var calls []string
				var buildLookup, updateLookup func(string) (string, bool)
				steps := handoffSteps{
					buildPlan: func(_ string, opts buildRestartCommandOpts) (restartPlan, error) {
						buildLookup = opts.LookupEnv
						return restartPlan{Command: "true"}, nil
					},
					updateSessionEnv: func(_ *session.PrefixRegistry, _ *tmux.Tmux, _ string, lookup func(string) (string, bool)) {
						calls = append(calls, "updateSessionEnv")
						updateLookup = lookup
					},
					remoteOps: func(*tmux.Tmux) remoteHandoffOps { return recordingRemoteOps(&calls) },
				}
				err := handoffSession(sessionHandoff{
					reg:            handoffTestRegistry(),
					pane:           "%0",
					currentSession: "gt-crew-tester",
					targetSession:  "gt-crew-max",
					dryRun:         dryRun,
				}, steps)
				if err != nil {
					t.Fatalf("handoffSession: %v", err)
				}
				wantCalls := []string{"hasSession", "sessionPane"}
				if !dryRun {
					wantCalls = append(wantCalls, "updateSessionEnv", "respawn")
				}
				if !slices.Equal(calls, wantCalls) {
					t.Errorf("calls = %v, want %v", calls, wantCalls)
				}
				lookups := map[string]func(string) (string, bool){"buildPlan": buildLookup}
				if !dryRun {
					lookups["updateSessionEnv"] = updateLookup
				}
				for who, lookup := range lookups {
					if lookup == nil {
						t.Errorf("%s got no env lookup for a remote target", who)
						continue
					}
					for _, key := range []string{"GT_AGENT", "GT_AGENT_OVERRIDE", "GT_PROCESS_NAMES"} {
						if v, ok := lookup(key); ok || v != "" {
							t.Errorf("%s env carries the caller's %s = %q", who, key, v)
						}
					}
				}
			})
		}
	})

	t.Run("a restart built from the remote env keeps the target's agent", func(t *testing.T) {
		t.Parallel()
		townRoot := newHandoffTown(t)
		caller := map[string]string{
			"GT_AGENT":         "claude",
			"GT_PROCESS_NAMES": "caller-procs",
		}
		opts := restartOpts(townRoot, caller)
		opts.LookupEnv = remoteHandoffEnv(opts.LookupEnv)
		opts.SessionEnv = func(_, key string) (string, error) {
			if key == "GT_AGENT" {
				return string(config.AgentGroqCompound), nil
			}
			return "", errors.New("unset")
		}
		cmd, err := buildRestartCommandWithOpts("gt-crew-holden", opts)
		if err != nil {
			t.Fatalf("buildRestartCommandWithOpts: %v", err)
		}
		if !strings.Contains(cmd, "GT_AGENT="+string(config.AgentGroqCompound)) {
			t.Errorf("restart does not keep the target's agent\ncmd: %s", cmd)
		}
		if strings.Contains(cmd, "caller-procs") {
			t.Errorf("restart carries the caller's GT_PROCESS_NAMES\ncmd: %s", cmd)
		}
	})
}

// recordingRemoteOps are remoteHandoffOps for a live session that append each
// call to calls.
func recordingRemoteOps(calls *[]string) remoteHandoffOps {
	return remoteHandoffOps{
		hasSession: func(string) (bool, error) {
			*calls = append(*calls, "hasSession")
			return true, nil
		},
		sessionPane: func(string) (string, error) {
			*calls = append(*calls, "sessionPane")
			return "%7", nil
		},
		respawn: func(string, string, restartPlan) error {
			*calls = append(*calls, "respawn")
			return nil
		},
		switchClient: func(string) error {
			*calls = append(*calls, "switchClient")
			return nil
		},
	}
}
