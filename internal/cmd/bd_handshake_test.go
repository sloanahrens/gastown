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
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/deps"
	"github.com/steveyegge/gastown/internal/townconfig"
)

// testTownStartGate is a gate over a fixed town whose config check and
// handshake are fn and config, counting the handshakes it runs.
func testTownStartGate(handshake func(ctx context.Context) (*deps.BDHandshake, error), config func() error) (*townStartGate, *int) {
	calls := 0
	g := &townStartGate{
		townRoot: func() string { return "/town" },
		config:   func(string) error { return config() },
		handshake: func(ctx context.Context, _ string) (*deps.BDHandshake, error) {
			calls++
			return handshake(ctx)
		},
	}
	return g, &calls
}

func configOK() error { return nil }

// brokenTownConfig is what the town config check returns for a file that
// does not parse.
func brokenTownConfig() error {
	return &config.ParseError{Path: "/town/settings/config.json", Offset: 12, Line: 1, Column: 12, Err: errors.New("invalid character '}'")}
}

func findCommand(t *testing.T, path string) *cobra.Command {
	t.Helper()
	c := lookupCommand(rootCmd, strings.Fields(path)[1:])
	if c == nil || c.CommandPath() != path {
		t.Fatalf("command %q not in the tree (found %v)", path, c)
	}
	return c
}

// TestBDHandshakeGatedCommandsExist fails when a gated command is renamed or
// removed, so the gate can never silently stop covering a town-running path.
func TestBDHandshakeGatedCommandsExist(t *testing.T) {
	t.Parallel()
	for path := range bdHandshakeGatedCommands {
		findCommand(t, path)
	}
	for _, want := range []string{"gt up", "gt daemon start", "gt daemon run", "gt sling", "gt crew start", "gt mayor start", "gt session start"} {
		if !bdHandshakeGatedCommands[want] {
			t.Errorf("%q must be gated by the bd handshake", want)
		}
	}
}

func TestBDHandshakeGate_OnlyTownRunningCommands(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path  string
		gated bool
	}{
		{"gt up", true},
		{"gt daemon run", true},
		{"gt crew start", true},
		{"gt status", false},
		{"gt show", false},
		{"gt mayor status", false},
		{"gt daemon status", false},
		{"gt doctor", false},
	} {
		if got := requiresBDHandshake(findCommand(t, tc.path)); got != tc.gated {
			t.Errorf("requiresBDHandshake(%q) = %v, want %v", tc.path, got, tc.gated)
		}
	}
}

func TestPersistentPreRunRefusesGatedCommandOnFailedHandshake(t *testing.T) {
	t.Parallel()
	refusal := fmt.Errorf("%w: no contract_version", deps.ErrBDHandshake)
	g, calls := testTownStartGate(func(context.Context) (*deps.BDHandshake, error) { return nil, refusal }, configOK)

	err := gateTownCommand(findCommand(t, "gt up"), g)
	if !errors.Is(err, deps.ErrBDHandshake) {
		t.Fatalf("gateTownCommand(gt up) = %v, want the handshake refusal", err)
	}
	if *calls != 1 {
		t.Errorf("handshake ran %d times, want 1", *calls)
	}
}

// TestRequireBDHandshakeCachesOnlySuccess: a long-lived process (the daemon
// starts sessions for hours) must recover once bd or Dolt is fixed, so a
// refusal is re-checked on the next call and only a pass is remembered.
func TestRequireBDHandshakeCachesOnlySuccess(t *testing.T) {
	t.Parallel()
	fail := true
	g, calls := testTownStartGate(func(context.Context) (*deps.BDHandshake, error) {
		if fail {
			return nil, fmt.Errorf("%w: store unavailable", deps.ErrBDHandshake)
		}
		return &deps.BDHandshake{DBSchema: 66}, nil
	}, configOK)
	if err := g.requireHandshake(); err == nil {
		t.Fatal("first call: want refusal")
	}
	fail = false
	if err := g.requireHandshake(); err != nil {
		t.Fatalf("after bd was fixed: %v", err)
	}
	fail = true
	if err := g.requireHandshake(); err != nil {
		t.Fatalf("a pass must be cached: %v", err)
	}
	if *calls != 2 {
		t.Errorf("handshake ran %d times, want 2 (refusal re-checked, pass cached)", *calls)
	}
}

func TestPersistentPreRunSkipsHandshakeForReadOnlyCommands(t *testing.T) {
	t.Parallel()
	g, calls := testTownStartGate(func(context.Context) (*deps.BDHandshake, error) {
		return nil, errors.New("must not run")
	}, configOK)
	if err := gateTownCommand(findCommand(t, "gt mayor status"), g); err != nil {
		t.Fatalf("gateTownCommand(gt mayor status) = %v", err)
	}
	if *calls != 0 {
		t.Fatalf("read-only command ran the handshake %d times", *calls)
	}
}

// TestBDHandshakeClassifiesEveryTownVerb: every start/run-style command is
// either gated or exempt with a stated reason, so a new town-running command
// fails this test until someone decides.
func TestBDHandshakeClassifiesEveryTownVerb(t *testing.T) {
	t.Parallel()
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
	g, handshakes := testTownStartGate(func(context.Context) (*deps.BDHandshake, error) {
		return &deps.BDHandshake{DBSchema: 66}, nil
	}, brokenTownConfig)
	for path := range bdHandshakeGatedCommands {
		if err := gateTownCommand(findCommand(t, path), g); !errors.Is(err, config.ErrUnparseable) {
			t.Errorf("%q with an unparseable town config: gateTownCommand = %v, want the config refusal", path, err)
		}
	}
	if *handshakes != 0 {
		t.Errorf("handshake ran %d times; the config refusal must come first", *handshakes)
	}
}

// TestDefaultBDHandshakeRefusesOutsideATown: with no town root the handshake
// has no town database to read, so it refuses rather than reading whatever
// database the working directory resolves to.
func TestDefaultBDHandshakeRefusesOutsideATown(t *testing.T) {
	t.Parallel()
	_, err := runBDHandshake(context.Background(), "")
	if !errors.Is(err, deps.ErrBDHandshake) || !strings.Contains(err.Error(), "not in a Gas Town workspace") {
		t.Fatalf("runBDHandshake outside a town = %v", err)
	}
}

// TestSessionGateRefusesAnUnparseableTownConfig: the gate every agent
// session start calls refuses on a broken config file, every time (not
// cached), even after the handshake passed.
func TestSessionGateRefusesAnUnparseableTownConfig(t *testing.T) {
	t.Parallel()
	broken := false
	g, _ := testTownStartGate(func(context.Context) (*deps.BDHandshake, error) { return &deps.BDHandshake{DBSchema: 66}, nil }, func() error {
		if broken {
			return brokenTownConfig()
		}
		return nil
	})
	if err := g.require(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	broken = true
	if err := g.require(); !errors.Is(err, config.ErrUnparseable) {
		t.Fatalf("config broken after a pass: require() = %v, want the config refusal", err)
	}
}

// writeTownFiles writes files (relative path to body) under a new town root.
func writeTownFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	town := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(town, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return town
}

// TestSessionGateRefusesSpawnOnARealBrokenSettingsFile is G3-04 end to end
// with the real check: a broken settings/config.json refuses the spawn and
// names the file, instead of moving the role to the default agent.
func TestSessionGateRefusesSpawnOnARealBrokenSettingsFile(t *testing.T) {
	t.Parallel()
	town := writeTownFiles(t, map[string]string{
		"mayor/town.json":      `{"type":"town","version":2,"name":"t"}`,
		"settings/config.json": "{\"role_agents\": {\"polecat\": \"deepseek-flash\",}}",
	})
	err := checkTownConfig(town)
	if !errors.Is(err, config.ErrUnparseable) || !strings.Contains(err.Error(), filepath.Join("settings", "config.json")) || !strings.Contains(err.Error(), "offset") {
		t.Fatalf("checkTownConfig = %v, want a refusal naming settings/config.json and the offset", err)
	}
}

// TestDefaultTownConfigCheckRefusesOutsideATown: with no town root there are
// no config files to check, and "nothing to check" must not read as a pass.
func TestDefaultTownConfigCheckRefusesOutsideATown(t *testing.T) {
	t.Parallel()
	if err := checkTownConfig(""); err == nil || !strings.Contains(err.Error(), "not in a Gas Town workspace") {
		t.Fatalf("checkTownConfig outside a town = %v, want a refusal", err)
	}
}

// TestDefaultTownConfigCheckPassesFromInsideATown: a town with valid config
// files passes.
func TestDefaultTownConfigCheckPassesFromInsideATown(t *testing.T) {
	t.Parallel()
	town := writeTownFiles(t, map[string]string{
		"mayor/town.json":      `{"type":"town","version":2,"name":"t"}`,
		"mayor/daemon.json":    `{"type":"daemon-patrol-config","version":1}`,
		"settings/config.json": `{"type":"town-settings","version":1}`,
	})
	if err := checkTownConfig(town); err != nil {
		t.Fatalf("checkTownConfig = %v", err)
	}
}

// TestDefaultTownConfigCheckRefusesARootWithoutTownJSON: a root found by its
// mayor/ directory alone is not a town the gate lets run (gt-y3pgh.1).
func TestDefaultTownConfigCheckRefusesARootWithoutTownJSON(t *testing.T) {
	t.Parallel()
	town := writeTownFiles(t, map[string]string{
		"mayor/daemon.json": `{"type":"daemon-patrol-config","version":1}`,
	})
	if err := checkTownConfig(town); !errors.Is(err, townconfig.ErrNotATown) {
		t.Fatalf("checkTownConfig without town.json = %v, want ErrNotATown", err)
	}
}
