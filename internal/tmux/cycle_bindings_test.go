package tmux

import (
	"strings"
	"testing"
)

// stalePattern can never be current: sessionPrefixPattern always includes hq.
// (A stale pattern of ^(gt|hq)- is current on a host without GT_ROOT, which
// made the fixture depend on the environment.)
const stalePattern = "^(gt)-"

// staleCycleBindings is `tmux list-keys -T prefix` (trimmed to n and p) after
// an older gt bound C-b n with a prefix pattern that predates a rig add.
const staleCycleBindings = `bind-key    -T prefix n       if-shell "echo '#{session_name}' | grep -Eq '^(gt)-'" "run-shell 'gt cycle next --session #{session_name} --client #{client_tty}'" next-window
bind-key    -T prefix p       if-shell "echo '#{session_name}' | grep -Eq '^(gt)-'" "run-shell 'gt cycle prev --session #{session_name} --client #{client_tty}'" previous-window`

// TestIsGTBindingCurrent_DetectsStalePattern verifies that isGTBindingCurrent
// returns false when the baked-in pattern doesn't match the current pattern.
// This is the core of the gt rig add fix: after adding a rig, the prefix
// pattern changes and existing bindings become stale.
func TestIsGTBindingCurrent_DetectsStalePattern(t *testing.T) {
	t.Parallel()
	s := newScripted(bySub(map[string]reply{"list-keys": ok(staleCycleBindings)}))
	tm := unitTmux(s, nil)

	if !tm.isGTBindingWithClient("prefix", "n") {
		t.Fatal("expected isGTBindingWithClient to return true for the installed binding")
	}
	if tm.isGTBindingCurrent("prefix", "n", "^(gt|hq|qu)-") {
		t.Error("expected isGTBindingCurrent to return false for stale pattern")
	}
	if !tm.isGTBindingCurrent("prefix", "n", stalePattern) {
		t.Error("expected isGTBindingCurrent to return true for matching pattern")
	}
	for _, c := range s.find("list-keys") {
		if !c.has("-T", "prefix") {
			t.Errorf("list-keys call %v does not select the prefix table", c)
		}
	}
}

// TestSetCycleBindings_RefreshesStalePattern verifies that SetCycleBindings
// re-binds when the existing binding has a stale prefix pattern, even though
// it already has --client support.
func TestSetCycleBindings_RefreshesStalePattern(t *testing.T) {
	t.Parallel()
	s := newScripted(bySub(map[string]reply{"list-keys": ok(staleCycleBindings)}))
	tm := unitTmux(s, nil)

	if err := tm.SetCycleBindings("gt-x"); err != nil {
		t.Fatalf("SetCycleBindings: %v", err)
	}

	binds := s.find("bind-key")
	if len(binds) != 2 {
		t.Fatalf("bind-key calls = %v, want n and p rebound", binds)
	}
	pattern := tm.sessionPrefixPattern()
	for i, key := range []string{"n", "p"} {
		c := binds[i]
		if !c.has("-T", "prefix", key, "if-shell") {
			t.Errorf("bind-key %d = %v, want prefix %s if-shell", i, c, key)
		}
		if !strings.Contains(strings.Join(c.args, " "), pattern) {
			t.Errorf("bind-key %s = %v, want current pattern %q", key, c, pattern)
		}
	}
}

// TestSetCycleBindings_SkipsCurrentBinding is the other half: a binding that
// already has --client and the current pattern is left alone.
func TestSetCycleBindings_SkipsCurrentBinding(t *testing.T) {
	t.Parallel()
	current := strings.ReplaceAll(staleCycleBindings, stalePattern, "^(gt|hq)-")
	s := newScripted(bySub(map[string]reply{"list-keys": ok(current)}))
	if err := unitTmux(s, nil).SetCycleBindings("gt-x"); err != nil {
		t.Fatalf("SetCycleBindings: %v", err)
	}
	if got := s.find("bind-key"); len(got) != 0 {
		t.Fatalf("bind-key calls = %v, want none for a current binding", got)
	}
}
