package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/doltpause"
)

// composeCall is one docker call the fake runner received.
type composeCall struct {
	argv []string
	env  []string
}

// composeTestEnviron is the environment the fake deps hand the compose path. It
// is the only environment those calls inherit, so no test reads the host's.
var composeTestEnviron = []string{"GT_DOLT_IMAGE=dolt@sha256:" + strings.Repeat("b", 64), "GT_BD_TOKEN_DIR=/tokens"}

// fakeComposeDeps returns deps whose docker call records into calls and writes
// out to stdout, and whose backup root is a path under townRoot.
func fakeComposeDeps(townRoot string, calls *[]composeCall, out []byte, runErr error) doltComposeDeps {
	return doltComposeDeps{
		run: func(argv, env []string) ([]byte, error) {
			*calls = append(*calls, composeCall{argv: argv, env: env})
			return out, runErr
		},
		environ:    func() []string { return composeTestEnviron },
		backupRoot: func() (string, error) { return filepath.Join(townRoot, "backups"), nil },
	}
}

// composeRef is a digest reference for an image, in the form the values file
// holds.
func composeRef(image string) string {
	return "registry.internal:5000/gt/" + image + "@sha256:" + strings.Repeat("a", 64)
}

// composeTown makes a town whose Dolt config file is in place, and whose values
// file names bd (when bd is non-empty) or has no bd key (when bd is empty). It
// returns the town root and the values file's path.
func composeTown(t *testing.T, bd string) string {
	t.Helper()
	townRoot := t.TempDir()
	dataDir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte("log_level: warning\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values := "daemon: " + composeRef("gastown-daemon") + "\n" +
		"polecat: " + composeRef("gastown-polecat") + "\n" +
		"commit: " + strings.Repeat("0", 40) + "\n"
	if bd != "" {
		values += "bd: " + bd + "\n"
	}
	writeComposeValues(t, townRoot, values)
	return townRoot
}

// writeComposeValues writes the values file under the town's gastown directory,
// where the compose path reads it.
func writeComposeValues(t *testing.T, townRoot, body string) {
	t.Helper()
	dir := filepath.Join(townRoot, "gastown", "deploy", "values")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "images.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// lastEnv returns the value env gives key. The last entry wins, as it does for a
// process environment.
func lastEnv(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// TestDoltComposeStartStop: with the compose unit selected, start and stop hand
// docker compose the gt-data project, the compose file, and the host paths and
// bd image the values file names, and nothing else changes the call.
func TestDoltComposeStartStop(t *testing.T) {
	t.Parallel()
	bd := composeRef("gastown-bd")
	townRoot := composeTown(t, bd)
	file := filepath.Join(townRoot, "gastown", "deploy", "data", "docker-compose.yml")
	dataDir := filepath.Join(townRoot, ".dolt-data")

	tests := []struct {
		name string
		op   func(deps doltComposeDeps) error
		argv []string
	}{
		{
			name: "start",
			op:   func(deps doltComposeDeps) error { return doltComposeStart(townRoot, deps) },
			argv: []string{"docker", "compose", "-p", "gt-data", "-f", file, "up", "-d", "--wait", "--wait-timeout", "120"},
		},
		{
			name: "stop",
			op:   func(deps doltComposeDeps) error { return doltComposeStop(townRoot, deps) },
			argv: []string{"docker", "compose", "-p", "gt-data", "-f", file, "stop"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []composeCall
			if err := tt.op(fakeComposeDeps(townRoot, &calls, nil, nil)); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			if len(calls) != 1 {
				t.Fatalf("%s made %d docker calls, want 1", tt.name, len(calls))
			}
			if got := calls[0].argv; strings.Join(got, "\x00") != strings.Join(tt.argv, "\x00") {
				t.Errorf("%s argv = %q, want %q", tt.name, got, tt.argv)
			}
			for key, want := range map[string]string{
				"BD_IMAGE":        bd,
				"GT_DOLT_DATA":    dataDir,
				"GT_DOLT_CONFIG":  filepath.Join(dataDir, "config.yaml"),
				"GT_DOLT_BACKUPS": filepath.Join(townRoot, "backups"),
				"GT_DOLT_IMAGE":   "dolt@sha256:" + strings.Repeat("b", 64),
				// The town's token directory, set over the inherited /tokens.
				"GT_BD_TOKEN_DIR": filepath.Join(townRoot, ".runtime", "bd-gateway", "tokens"),
			} {
				if got, ok := lastEnv(calls[0].env, key); !ok || got != want {
					t.Errorf("%s env %s = %q (set %v), want %q", tt.name, key, got, ok, want)
				}
			}
		})
	}
}

// TestDoltComposeNeedsBdKey: a town whose values file names no bd image makes the
// compose start refuse by name, and hands docker nothing.
func TestDoltComposeNeedsBdKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		setup   func(t *testing.T) string
		wantMsg string
	}{
		{
			name: "values file without a bd key",
			setup: func(t *testing.T) string {
				return composeTown(t, "")
			},
			wantMsg: "has no bd key",
		},
		{
			name: "no values file",
			setup: func(t *testing.T) string {
				townRoot := composeTown(t, "")
				if err := os.Remove(filepath.Join(townRoot, "gastown", "deploy", "values", "images.yaml")); err != nil {
					t.Fatal(err)
				}
				return townRoot
			},
			wantMsg: "images.yaml",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			townRoot := tt.setup(t)
			var calls []composeCall
			err := doltComposeStart(townRoot, fakeComposeDeps(townRoot, &calls, nil, nil))
			if err == nil {
				t.Fatal("doltComposeStart succeeded, want a refusal")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error %q does not name %q", err, tt.wantMsg)
			}
			if len(calls) != 0 {
				t.Errorf("refused start still made %d docker calls, want 0", len(calls))
			}
		})
	}
}

// TestDoltComposeRefusesWithoutConfigFile: compose would bind-mount a missing
// Dolt config into the container as a directory, so the start refuses first.
func TestDoltComposeRefusesWithoutConfigFile(t *testing.T) {
	t.Parallel()
	townRoot := composeTown(t, composeRef("gastown-bd"))
	if err := os.Remove(filepath.Join(townRoot, ".dolt-data", "config.yaml")); err != nil {
		t.Fatal(err)
	}
	var calls []composeCall
	err := doltComposeStart(townRoot, fakeComposeDeps(townRoot, &calls, nil, nil))
	if err == nil || !strings.Contains(err.Error(), "config.yaml") {
		t.Fatalf("doltComposeStart error = %v, want a refusal naming config.yaml", err)
	}
	if len(calls) != 0 {
		t.Errorf("refused start still made %d docker calls, want 0", len(calls))
	}
}

// TestDoltComposeRefusesWhilePaused: a Dolt pause marker that holds stops the
// compose start, as it stops the single-container start.
func TestDoltComposeRefusesWhilePaused(t *testing.T) {
	t.Parallel()
	townRoot := composeTown(t, composeRef("gastown-bd"))
	now := time.Now()
	if err := doltpause.Write(townRoot, doltpause.Marker{
		Actor:  "overseer",
		Reason: "test",
		Since:  now,
		Until:  now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	var calls []composeCall
	err := doltComposeStart(townRoot, fakeComposeDeps(townRoot, &calls, nil, nil))
	if err == nil {
		t.Fatal("doltComposeStart succeeded under a pause marker, want a refusal")
	}
	if len(calls) != 0 {
		t.Errorf("paused start still made %d docker calls, want 0", len(calls))
	}
}

// TestDoltComposeSurfacesDockerFailure: a docker call that fails is the start's
// error, named by the call, with its exit status.
func TestDoltComposeSurfacesDockerFailure(t *testing.T) {
	t.Parallel()
	townRoot := composeTown(t, composeRef("gastown-bd"))
	var calls []composeCall
	dockerErr := errors.New("docker exited with status 1")
	err := doltComposeStart(townRoot, fakeComposeDeps(townRoot, &calls, nil, dockerErr))
	if err == nil || !strings.Contains(err.Error(), "docker compose up") {
		t.Fatalf("doltComposeStart error = %v, want the docker compose up failure", err)
	}
}

// TestDoltComposeStatusPrintsServices: status writes the unit's services as
// docker compose ps reports them.
func TestDoltComposeStatusPrintsServices(t *testing.T) {
	t.Parallel()
	townRoot := composeTown(t, composeRef("gastown-bd"))
	var calls []composeCall
	ps := []byte("dolt running healthy\nbd running healthy\n")
	var w bytes.Buffer
	if err := doltComposeStatus(townRoot, fakeComposeDeps(townRoot, &calls, ps, nil), &w); err != nil {
		t.Fatalf("doltComposeStatus: %v", err)
	}
	if len(calls) != 1 || !composeArgvHas(calls[0].argv, "ps") {
		t.Fatalf("status made calls %q, want one docker compose ps", calls)
	}
	if !strings.Contains(w.String(), "dolt running healthy") {
		t.Errorf("status output %q does not carry the services", w.String())
	}
}

// composeArgvHas reports whether argv holds arg as one element.
func composeArgvHas(argv []string, arg string) bool {
	for _, a := range argv {
		if a == arg {
			return true
		}
	}
	return false
}

// TestDoltDefaultPathUnchanged: without --compose-unit the start runs the
// single-container path it always ran. Here the town names a container and
// docker cannot run, so the error is the container start's, not the compose
// path's values refusal.
//
//testpolicy:allow parallel — the dolt commands resolve the town root from process-wide cwd and GT_TOWN_ROOT
func TestDoltDefaultPathUnchanged(t *testing.T) {
	townRoot := externalContainerTownRoot(t, "gt-dolt")
	//testpolicy:allow no-chdir — the dolt commands resolve the town root by walking up from the process cwd
	t.Chdir(t.TempDir())
	//testpolicy:allow no-env — GT_TOWN_ROOT is the fallback the commands read when the cwd is outside a town
	t.Setenv("GT_TOWN_ROOT", townRoot)
	//testpolicy:allow no-env — PATH is the lookup that makes docker unfindable, so the test runs no engine
	t.Setenv("PATH", t.TempDir())

	err := runDoltStart(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "docker start") {
		t.Fatalf("gt dolt start without --compose-unit: error = %v, want the container start's docker failure", err)
	}
}

// TestDoltComposeFlagSelectsUnit: --compose-unit on gt dolt start and stop
// selects the compose unit. In a town with no values file the command refuses
// by name; the default path would start the town's container instead.
//
//testpolicy:allow parallel — the dolt commands resolve the town root from process-wide cwd and GT_TOWN_ROOT
func TestDoltComposeFlagSelectsUnit(t *testing.T) {
	townRoot := externalContainerTownRoot(t, "gt-dolt")
	//testpolicy:allow no-chdir — the dolt commands resolve the town root by walking up from the process cwd
	t.Chdir(t.TempDir())
	//testpolicy:allow no-env — GT_TOWN_ROOT is the fallback the commands read when the cwd is outside a town
	t.Setenv("GT_TOWN_ROOT", townRoot)
	//testpolicy:allow no-env — PATH makes docker unfindable, so a path that reaches docker fails here
	t.Setenv("PATH", t.TempDir())

	for _, c := range []struct {
		name string
		cmd  *cobra.Command
		run  func(cmd *cobra.Command) error
	}{
		{"start", doltStartCmd, func(cmd *cobra.Command) error { return runDoltStart(cmd, nil) }},
		{"stop", doltStopCmd, func(cmd *cobra.Command) error { return runDoltStop(cmd, nil) }},
		{"status", doltStatusCmd, func(cmd *cobra.Command) error {
			return runDoltStatusWith(&bytes.Buffer{}, doltStatusReads{townRoot: func() (string, error) { return townRoot, nil }})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.cmd.Flags().Parse([]string{"--compose-unit"}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := c.cmd.Flags().Set("compose-unit", "false"); err != nil {
					t.Errorf("resetting --compose-unit on gt dolt %s: %v", c.name, err)
				}
			})

			err := c.run(c.cmd)
			if err == nil || !strings.Contains(err.Error(), "images.yaml") {
				t.Fatalf("gt dolt %s --compose-unit: error = %v, want the compose unit's values refusal", c.name, err)
			}
		})
	}
}
