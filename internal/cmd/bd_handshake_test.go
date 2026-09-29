package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/bdgate"
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
	return &calls
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
	for _, want := range []string{"gt up", "gt start", "gt daemon start", "gt daemon run", "gt sling", "gt witness start", "gt refinery start", "gt crew start", "gt mayor start", "gt deacon start", "gt session start"} {
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
