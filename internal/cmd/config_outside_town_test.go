package cmd

import (
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/workspace"
)

// The cwd config environment must resolve the town through the GT_TOWN_ROOT
// aware lookup. workspace.FindFromCwd returns an empty root with no error
// outside a town, which made `gt config set` write the relative path
// settings/config.json under the cwd and report success (gt-da3uh). The
// GT_TOWN_ROOT fallback itself is covered by workspace.TestFindFromCwdOrError.
func TestCwdConfigCmdEnvFindsTownThroughGTTownRoot(t *testing.T) {
	t.Parallel()
	got := reflect.ValueOf(cwdConfigCmdEnv().findTown).Pointer()
	want := reflect.ValueOf(workspace.FindFromCwdOrError).Pointer()
	if got != want {
		t.Error("cwdConfigCmdEnv().findTown is not workspace.FindFromCwdOrError: outside a town it would resolve an empty root instead of GT_TOWN_ROOT or an error")
	}
}

// A town lookup that fails (no town from the cwd, no GT_TOWN_ROOT) stops every
// config command with an error before it resolves a settings path, so nothing
// is written under the cwd.
func TestConfigCommandsRefuseWhenNoTownIsFound(t *testing.T) {
	t.Parallel()
	for name, run := range map[string]func(configCmdEnv) error{
		"set":           func(e configCmdEnv) error { return configSet(e, []string{"cli_theme", "dark"}) },
		"get":           func(e configCmdEnv) error { return configGet(e, []string{"cli_theme"}) },
		"agent list":    func(e configCmdEnv) error { return configAgentList(e) },
		"agent set":     func(e configCmdEnv) error { return configAgentSet(e, []string{"x", "echo"}) },
		"default-agent": func(e configCmdEnv) error { return configDefaultAgent(e, []string{"claude"}) },
		"cost-tier":     func(e configCmdEnv) error { return configCostTier(e, []string{"standard"}) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := configCmdEnv{
				findTown: func() (string, error) { return "", workspace.ErrNotFound },
				out:      io.Discard,
			}
			err := run(e)
			if !errors.Is(err, workspace.ErrNotFound) {
				t.Fatalf("err = %v, want it to wrap workspace.ErrNotFound", err)
			}
		})
	}
}

// With the town found (the GT_TOWN_ROOT case), set writes that town's settings.
func TestConfigSetWritesTheFoundTownsSettings(t *testing.T) {
	t.Parallel()
	townRoot := setupTestTownForConfig(t)
	if err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"cli_theme", "dark"}); err != nil {
		t.Fatalf("configSet: %v", err)
	}
	settings, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		t.Fatalf("load town settings: %v", err)
	}
	if settings.CLITheme != "dark" {
		t.Errorf("town CLITheme = %q, want dark", settings.CLITheme)
	}
}
