package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
)

// fakeCredentialMarker starts every fake credential value, so one substring
// search finds any of them in a produced command.
const fakeCredentialMarker = "fake-cred-"

// parentEnvWithFakeCredentials is a spawning process's environment holding a
// fake value for every credential the passthrough once forwarded, proxies
// carrying a fake password, and one plain provider setting.
func parentEnvWithFakeCredentials() map[string]string {
	env := map[string]string{
		"HTTP_PROXY":      "http://user:" + fakeCredentialMarker + "proxy@proxy.example:3128",
		"HTTPS_PROXY":     "http://user:" + fakeCredentialMarker + "proxy@proxy.example:3128",
		"ANTHROPIC_MODEL": "fake-model",
	}
	for _, k := range unforwardedCredentialEnvVars {
		env[k] = fakeCredentialMarker + strings.ToLower(k)
	}
	return env
}

// No spawn path carries a credential from the spawning process on a command
// line (G3-18, gt-y3pgh.10): not AgentEnv (tmux new-session -e) and not the
// startup command's exec env (tmux pane_start_command), for any role. The
// handoff respawn command is guarded in internal/cmd.
func TestSpawnCommandsCarryNoParentCredentials(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "gastown")
	if err := SaveTownSettings(TownSettingsPath(townRoot), NewTownSettings()); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
	if err := SaveRigSettings(RigSettingsPath(rigPath), NewRigSettings()); err != nil {
		t.Fatalf("SaveRigSettings: %v", err)
	}
	h := agentHost(parentEnvWithFakeCredentials())

	roles := []AgentEnvConfig{
		{Role: constants.RoleMayor, TownRoot: townRoot},
		{Role: "deacon", TownRoot: townRoot},
		{Role: "boot", TownRoot: townRoot},
		{Role: "dog", AgentName: "alpha", TownRoot: townRoot},
		{Role: "witness", Rig: "gastown", TownRoot: townRoot},
		{Role: "refinery", Rig: "gastown", TownRoot: townRoot},
		{Role: constants.RolePolecat, Rig: "gastown", AgentName: "nux", TownRoot: townRoot},
		{Role: constants.RoleCrew, Rig: "gastown", AgentName: "holden", TownRoot: townRoot},
	}
	for _, cfg := range roles {
		t.Run(cfg.Role, func(t *testing.T) {
			t.Parallel()
			cfg.Getenv = h.getenv
			env := AgentEnv(cfg)
			for k, v := range env {
				if strings.Contains(v, fakeCredentialMarker) {
					t.Errorf("AgentEnv[%s] carries a parent credential (tmux new-session -e)", k)
				}
			}
			if env["ANTHROPIC_MODEL"] != "fake-model" {
				t.Errorf("AgentEnv[ANTHROPIC_MODEL] = %q, want the parent's setting forwarded", env["ANTHROPIC_MODEL"])
			}

			rp := rigPath
			if cfg.Rig == "" {
				rp = ""
			}
			for name, build := range map[string]func() (string, error){
				"BuildStartupCommandFromConfig": func() (string, error) { return buildStartupCommandFromConfig(h, cfg, rp, "beacon", "") },
				"BuildAgentStartupCommand": func() (string, error) {
					return buildAgentStartupCommand(h, cfg.Role, cfg.Rig, townRoot, rp, "beacon")
				},
				"BuildAgentStartupCommandWithAgentOverride": func() (string, error) {
					return buildAgentStartupCommandWithAgentOverride(h, cfg.Role, cfg.Rig, townRoot, rp, "beacon", "claude")
				},
			} {
				cmd, err := build()
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if strings.Contains(cmd, fakeCredentialMarker) {
					t.Errorf("%s carries a parent credential in the startup command", name)
				}
				if !strings.Contains(cmd, "ANTHROPIC_MODEL=fake-model") {
					t.Errorf("%s does not forward the parent's ANTHROPIC_MODEL: %s", name, cmd)
				}
			}
		})
	}
}

// A key added to the forwarded list must not name a credential.
func TestProviderPassthroughNamesNoCredential(t *testing.T) {
	t.Parallel()
	for _, k := range providerPassthroughEnvVars {
		for _, m := range secretKeyMarkers {
			if strings.Contains(k, m) {
				t.Errorf("%s is forwarded from the spawning process but names a credential (%s)", k, m)
			}
		}
	}
}
