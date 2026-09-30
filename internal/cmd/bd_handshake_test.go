package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/bdgate"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/deps"
)

// stubBDHandshake replaces the handshake for one test and resets the
// per-process cache around it.
func stubBDHandshake(t *testing.T, fn func(ctx context.Context) (*deps.BDHandshake, error)) *int {
	t.Helper()
	calls := 0
	old := bdHandshakeCheck
	bdHandshakeCheck = func(ctx context.Context) (*deps.BDHandshake, error) {
		calls++
		return fn(ctx)
	}
	resetBDHandshakeCache()
	t.Cleanup(func() {
		bdHandshakeCheck = old
		resetBDHandshakeCache()
	})
	// The town config check reads the town the test's cwd sits in; a
	// handshake test must not depend on the live town's files.
	stubTownConfig(t, func() error { return nil })
	return &calls
}

// stubTownConfig replaces the town config parse check for one test.
func stubTownConfig(t *testing.T, fn func() error) *int {
	t.Helper()
	calls := 0
	old := townConfigCheck
	townConfigCheck = func() error {
		calls++
		return fn()
	}
	t.Cleanup(func() { townConfigCheck = old })
	return &calls
}

// brokenTownConfig is what the town config check returns for a file that
// does not parse.
func brokenTownConfig() error {
	return &config.ParseError{Path: "/town/settings/config.json", Offset: 12, Line: 1, Column: 12, Err: errors.New("invalid character '}'")}
}

func findCommand(t *testing.T, path string) *cobra.Command {
	t.Helper()
	args := strings.Fields(path)[1:]
	c, rest, err := rootCmd.Find(args)
	if err != nil || len(rest) != 0 || c.CommandPath() != path {
		t.Fatalf("command %q not in the tree (found %v, rest %v, err %v)", path, c, rest, err)
	}
	return c
}

// TestBDHandshakeGatedCommandsExist fails when a gated command is renamed or
// removed, so the gate can never silently stop covering a town-running path.
func TestBDHandshakeGatedCommandsExist(t *testing.T) {
	for path := range bdHandshakeGatedCommands {
		findCommand(t, path)
	}
	for _, want := range []string{"gt up", "gt daemon start", "gt daemon run", "gt sling", "gt witness start", "gt refinery start", "gt crew start", "gt mayor start", "gt deacon start", "gt session start"} {
		if !bdHandshakeGatedCommands[want] {
			t.Errorf("%q must be gated by the bd handshake", want)
		}
	}
}

func TestBDHandshakeGate_OnlyTownRunningCommands(t *testing.T) {
	for _, tc := range []struct {
		path  string
		gated bool
	}{
		{"gt up", true},
		{"gt daemon run", true},
		{"gt witness start", true},
		{"gt status", false},
		{"gt show", false},
		{"gt witness status", false},
		{"gt daemon status", false},
		{"gt doctor", false},
	} {
		if got := requiresBDHandshake(findCommand(t, tc.path)); got != tc.gated {
			t.Errorf("requiresBDHandshake(%q) = %v, want %v", tc.path, got, tc.gated)
		}
	}
}

func TestPersistentPreRunRefusesGatedCommandOnFailedHandshake(t *testing.T) {
	refusal := fmt.Errorf("%w: no contract_version", deps.ErrBDHandshake)
	calls := stubBDHandshake(t, func(context.Context) (*deps.BDHandshake, error) { return nil, refusal })

	err := persistentPreRun(findCommand(t, "gt up"), nil)
	if !errors.Is(err, deps.ErrBDHandshake) {
		t.Fatalf("persistentPreRun(gt up) = %v, want the handshake refusal", err)
	}
	if *calls != 1 {
		t.Errorf("handshake ran %d times, want 1", *calls)
	}
}

// TestRequireBDHandshakeCachesOnlySuccess: a long-lived process (the daemon
// starts sessions for hours) must recover once bd or Dolt is fixed, so a
// refusal is re-checked on the next call and only a pass is remembered.
func TestRequireBDHandshakeCachesOnlySuccess(t *testing.T) {
	fail := true
	calls := stubBDHandshake(t, func(context.Context) (*deps.BDHandshake, error) {
		if fail {
			return nil, fmt.Errorf("%w: store unavailable", deps.ErrBDHandshake)
		}
		return &deps.BDHandshake{DBSchema: 66}, nil
	})
	if err := requireBDHandshake(); err == nil {
		t.Fatal("first call: want refusal")
	}
	fail = false
	if err := requireBDHandshake(); err != nil {
		t.Fatalf("after bd was fixed: %v", err)
	}
	fail = true
	if err := requireBDHandshake(); err != nil {
		t.Fatalf("a pass must be cached: %v", err)
	}
	if *calls != 2 {
		t.Errorf("handshake ran %d times, want 2 (refusal re-checked, pass cached)", *calls)
	}
}

func TestPersistentPreRunSkipsHandshakeForReadOnlyCommands(t *testing.T) {
	calls := stubBDHandshake(t, func(context.Context) (*deps.BDHandshake, error) {
		return nil, errors.New("must not run")
	})
	_ = persistentPreRun(findCommand(t, "gt witness status"), nil)
	if *calls != 0 {
		t.Fatalf("read-only command ran the handshake %d times", *calls)
	}
}

// TestBDHandshakeClassifiesEveryTownVerb: every start/run-style command is
// either gated or exempt with a stated reason, so a new town-running command
// fails this test until someone decides.
func TestBDHandshakeClassifiesEveryTownVerb(t *testing.T) {
	for path, reason := range bdHandshakeNotTownRunning {
		findCommand(t, path)
		if reason == "" || bdHandshakeGatedCommands[path] {
			t.Errorf("%q: exempt entries need a reason and must not also be gated", path)
		}
	}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if bdHandshakeTownVerbs[c.Name()] {
			path := c.CommandPath()
			if !bdHandshakeGatedCommands[path] && bdHandshakeNotTownRunning[path] == "" {
				t.Errorf("%q starts or dispatches something: gate it (bdHandshakeGatedCommands) or exempt it with a reason (bdHandshakeNotTownRunning)", path)
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)

	// Every gated command also refuses an unparseable town config file, and
	// does so before the handshake runs (gt-fcxe9.10).
	handshakes := stubBDHandshake(t, func(context.Context) (*deps.BDHandshake, error) {
		return &deps.BDHandshake{DBSchema: 66}, nil
	})
	stubTownConfig(t, brokenTownConfig)
	for path := range bdHandshakeGatedCommands {
		if err := persistentPreRun(findCommand(t, path), nil); !errors.Is(err, config.ErrUnparseable) {
			t.Errorf("%q with an unparseable town config: persistentPreRun = %v, want the config refusal", path, err)
		}
	}
	if *handshakes != 0 {
		t.Errorf("handshake ran %d times; the config refusal must come first", *handshakes)
	}
}

func TestInstallSessionGateRoutesSessionStartsThroughTheHandshake(t *testing.T) {
	refusal := fmt.Errorf("%w: no contract_version", deps.ErrBDHandshake)
	stubBDHandshake(t, func(context.Context) (*deps.BDHandshake, error) { return nil, refusal })
	installSessionGate()
	t.Cleanup(func() { bdgate.Set(nil) })
	if err := bdgate.Require(); !errors.Is(err, deps.ErrBDHandshake) {
		t.Fatalf("bdgate.Require() = %v, want the handshake refusal", err)
	}
}

// TestDefaultBDHandshakeRefusesOutsideATown: with no town root the handshake
// has no town database to read, so it refuses rather than reading whatever
// database the working directory resolves to.
func TestDefaultBDHandshakeRefusesOutsideATown(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GT_TOWN_ROOT", "")
	t.Setenv("GT_ROOT", "")
	_, err := defaultBDHandshakeCheck(context.Background())
	if !errors.Is(err, deps.ErrBDHandshake) || !strings.Contains(err.Error(), "not in a Gas Town workspace") {
		t.Fatalf("defaultBDHandshakeCheck outside a town = %v", err)
	}
}

// TestSessionGateRefusesAnUnparseableTownConfig: the gate every agent
// session start calls refuses on a broken config file, every time (not
// cached), even after the handshake passed.
func TestSessionGateRefusesAnUnparseableTownConfig(t *testing.T) {
	stubBDHandshake(t, func(context.Context) (*deps.BDHandshake, error) { return &deps.BDHandshake{DBSchema: 66}, nil })
	broken := false
	stubTownConfig(t, func() error {
		if broken {
			return brokenTownConfig()
		}
		return nil
	})
	installSessionGate()
	t.Cleanup(func() { bdgate.Set(nil) })
	if err := bdgate.Require(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	broken = true
	if err := bdgate.Require(); !errors.Is(err, config.ErrUnparseable) {
		t.Fatalf("config broken after a pass: bdgate.Require() = %v, want the config refusal", err)
	}
}

// TestSessionGateRefusesSpawnOnARealBrokenSettingsFile is G3-04 end to end
// with the real check: a broken settings/config.json refuses the spawn and
// names the file, instead of moving the role to the default agent.
func TestSessionGateRefusesSpawnOnARealBrokenSettingsFile(t *testing.T) {
	stubBDHandshake(t, func(context.Context) (*deps.BDHandshake, error) { return &deps.BDHandshake{DBSchema: 66}, nil })
	townConfigCheck = defaultTownConfigCheck // stubBDHandshake's cleanup restores it

	town := t.TempDir()
	for rel, body := range map[string]string{
		"mayor/town.json":      `{"type":"town","version":2,"name":"t"}`,
		"settings/config.json": "{\"role_agents\": {\"polecat\": \"deepseek-flash\",}}",
	} {
		p := filepath.Join(town, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(town)
	t.Setenv("GT_TOWN_ROOT", "")
	t.Setenv("GT_ROOT", "")

	installSessionGate()
	t.Cleanup(func() { bdgate.Set(nil) })
	err := bdgate.Require()
	if !errors.Is(err, config.ErrUnparseable) || !strings.Contains(err.Error(), filepath.Join("settings", "config.json")) || !strings.Contains(err.Error(), "offset") {
		t.Fatalf("bdgate.Require() = %v, want a refusal naming settings/config.json and the offset", err)
	}
}

// TestDefaultTownConfigCheckRefusesOutsideATown: with no town root there are
// no config files to check, and "nothing to check" must not read as a pass.
func TestDefaultTownConfigCheckRefusesOutsideATown(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GT_TOWN_ROOT", "")
	t.Setenv("GT_ROOT", "")
	if err := defaultTownConfigCheck(); err == nil || !strings.Contains(err.Error(), "not in a Gas Town workspace") {
		t.Fatalf("defaultTownConfigCheck outside a town = %v, want a refusal", err)
	}
}

// TestDefaultTownConfigCheckPassesFromInsideATown: a gated command run from a
// rig or polecat worktree deep inside a town with valid config files passes;
// town-root resolution is the same one the handshake uses.
func TestDefaultTownConfigCheckPassesFromInsideATown(t *testing.T) {
	town := t.TempDir()
	for rel, body := range map[string]string{
		"mayor/town.json":      `{"type":"town","version":2,"name":"t"}`,
		"mayor/daemon.json":    `{"type":"daemon-patrol-config","version":1}`,
		"settings/config.json": `{"type":"town-settings","version":1}`,
	} {
		p := filepath.Join(town, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	worktree := filepath.Join(town, "gastown", "polecats", "onyx", "gastown")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(worktree)
	t.Setenv("GT_TOWN_ROOT", "")
	t.Setenv("GT_ROOT", "")
	if err := defaultTownConfigCheck(); err != nil {
		t.Fatalf("defaultTownConfigCheck from a polecat worktree = %v", err)
	}
}
