package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// defaultPrefixKeys is `tmux list-keys -T prefix` (trimmed) on a server with
// tmux's builtin table: what getKeyBinding sees before gt binds anything.
const defaultPrefixKeys = `bind-key    -T prefix Space   next-layout
bind-key    -T prefix n       next-window
bind-key    -T prefix p       previous-window
bind-key    -T prefix s       choose-tree -Zs
bind-key -r -T prefix Up      select-pane -U`

func keysTmux(listKeys string) (*Tmux, *scripted) {
	s := newScripted(bySub(map[string]reply{"list-keys": ok(listKeys)}))
	return unitTmux(s, nil), s
}

func TestGetKeyBinding_NoExistingBinding(t *testing.T) {
	t.Parallel()
	tm, _ := keysTmux(defaultPrefixKeys)
	if got := tm.getKeyBinding("prefix", "F12"); got != "" {
		t.Errorf("expected empty string for unbound key, got %q", got)
	}
}

// TestGetKeyBinding_CapturesDefaultBinding: prefix-n's builtin command is
// captured so gt can keep it as the if-shell fallback. (The live-server
// version skipped under load in flakes.md: the shared server was down.)
func TestGetKeyBinding_CapturesDefaultBinding(t *testing.T) {
	t.Parallel()
	tm, s := keysTmux(defaultPrefixKeys)
	if got := tm.getKeyBinding("prefix", "n"); got != "next-window" {
		t.Errorf("expected 'next-window' for default prefix-n binding, got %q", got)
	}
	if c := s.find("list-keys"); len(c) != 1 || !c[0].has("-T", "prefix") {
		t.Errorf("list-keys calls = %v", c)
	}
}

func TestGetKeyBinding_CapturesDefaultBindingWithArgs(t *testing.T) {
	t.Parallel()
	tm, _ := keysTmux(defaultPrefixKeys)
	if got := tm.getKeyBinding("prefix", "s"); got != "choose-tree -Zs" {
		t.Errorf("prefix-s binding = %q, want choose-tree -Zs", got)
	}
	if got := tm.getKeyBinding("prefix", "Up"); got != "select-pane -U" {
		t.Errorf("repeatable prefix-Up binding = %q, want select-pane -U", got)
	}
}

func TestGetKeyBinding_ListKeysFailureReadsAsUnbound(t *testing.T) {
	t.Parallel()
	s := newScripted(bySub(map[string]reply{"list-keys": fail("no server running")}))
	if got := unitTmux(s, nil).getKeyBinding("prefix", "n"); got != "" {
		t.Errorf("getKeyBinding with list-keys failing = %q, want empty", got)
	}
}

func TestGetKeyBinding_SkipsGasTownBindings(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		`bind-key    -T prefix F11     if-shell "echo '#{session_name}' | grep -Eq '^(gt|hq)-'" "run-shell 'gt agents menu'" :`,
		`bind-key    -T prefix F11     run-shell "gt agents menu"`,
		`bind-key    -T prefix F11     run-shell "gt rig menu"`,
	} {
		tm, _ := keysTmux(line)
		if got := tm.getKeyBinding("prefix", "F11"); got != "" {
			t.Errorf("getKeyBinding(%q) = %q, want empty for a Gas Town binding", line, got)
		}
	}
}

func TestGetKeyBinding_CapturesUserBinding(t *testing.T) {
	t.Parallel()
	tm, _ := keysTmux(`bind-key    -T prefix F11     display-message hello`)
	if got := tm.getKeyBinding("prefix", "F11"); got != "display-message hello" {
		t.Errorf("user binding = %q, want display-message hello", got)
	}
}

func TestIsGTBinding_DetectsGasTownBindings(t *testing.T) {
	t.Parallel()
	for line, want := range map[string]bool{
		`bind-key    -T prefix F11     display-message hello`: false,
		`bind-key    -T prefix F11     if-shell "echo '#{session_name}' | grep -Eq '^(gt|hq)-'" "run-shell 'gt agents menu'" "display-message hello"`: true,
		`bind-key    -T prefix F11     run-shell "gt rig menu"`: true,
		``: false,
	} {
		tm, _ := keysTmux(line)
		if got := tm.isGTBinding("prefix", "F11"); got != want {
			t.Errorf("isGTBinding(%q) = %v, want %v", line, got, want)
		}
	}
}

// TestSetBindings_PreserveFallbackOnRepeatedCalls: a second configuration
// pass over a key gt already bound neither rebinds it nor loses the user's
// original command, which lives on as the if-shell fallback.
func TestSetBindings_PreserveFallbackOnRepeatedCalls(t *testing.T) {
	t.Parallel()
	bound := `bind-key    -T prefix F11     if-shell "echo '#{session_name}' | grep -Eq '^(gt|hq)-'" "run-shell 'gt agents menu'" "display-message custom-user-cmd"`
	tm, _ := keysTmux(bound)
	if !tm.isGTBinding("prefix", "F11") {
		t.Fatal("expected isGTBinding=true after first configuration")
	}
	if got := tm.getKeyBinding("prefix", "F11"); got != "" {
		t.Errorf("getKeyBinding over our own binding = %q, want empty (no re-wrap)", got)
	}
	if raw := tm.lookupKeyBinding("prefix", "F11"); !strings.Contains(raw, "custom-user-cmd") {
		t.Errorf("original user fallback not found in binding: %q", raw)
	}
}

func TestSessionPrefixPattern_AlwaysIncludesGTAndHQ(t *testing.T) {
	t.Parallel()
	for _, root := range []string{"", t.TempDir()} {
		if got := sessionPrefixPatternFor(root); got != "^(gt|hq)-" {
			t.Errorf("sessionPrefixPatternFor(%q) = %q, want ^(gt|hq)-", root, got)
		}
	}
}

// TestSessionPrefixPattern_WithTownRoot reads rig prefixes from a town's
// mayor/rigs.json (was skipped unless the host had a live GT_TOWN_ROOT).
func TestSessionPrefixPattern_WithTownRoot(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	rigs := `{"version":1,"rigs":{
		"alpha":{"git_url":"https://example.com/a.git","beads":{"repo":"local","prefix":"al-"}},
		"beta":{"git_url":"https://example.com/b.git","beads":{"repo":"local","prefix":"be"}},
		"bad":{"git_url":"https://example.com/c.git","beads":{"repo":"local","prefix":"x|y"}}}}`
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte(rigs), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := sessionPrefixPatternFor(town); got != "^(al|be|gt|hq)-" {
		t.Errorf("sessionPrefixPatternFor = %q, want ^(al|be|gt|hq)- (unsafe prefix dropped)", got)
	}
}

func TestSessionPrefixPattern_FallsBackToTownRootEnv(t *testing.T) {
	t.Parallel()
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	// workspace.TownRootFromEnv is the one reader, and GT_TOWN_ROOT is the one
	// name: the GT_ROOT alias no longer names a town root (gt-syhch).
	if got := townRootFrom(env(map[string]string{"GT_TOWN_ROOT": "/b", "GT_ROOT": "/a"})); got != "/b" {
		t.Errorf("both set: town root = %q, want GT_TOWN_ROOT /b", got)
	}
	if got := townRootFrom(env(map[string]string{"GT_ROOT": "/a"})); got != "" {
		t.Errorf("GT_ROOT alone: town root = %q, want empty", got)
	}
	if got := townRootFrom(env(nil)); got != "" {
		t.Errorf("neither set: town root = %q, want empty", got)
	}
}

func TestGetSessionActivity(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	s := f.addSession("gt-x", "")
	f.with(func() { s.activity = 1_700_000_000; s.panes[0].activity = 1_700_000_042 })
	tm, _ := f.tmux(nil)
	if got, err := tm.GetSessionActivity("gt-x"); err != nil || got.Unix() != 1_700_000_000 {
		t.Errorf("GetSessionActivity = %v, %v", got, err)
	}
	if got, err := tm.GetWindowActivity("gt-x"); err != nil || got.Unix() != 1_700_000_042 {
		t.Errorf("GetWindowActivity = %v, %v", got, err)
	}
}

func TestGetSessionActivity_NonexistentSession(t *testing.T) {
	t.Parallel()
	tm, _ := newFakeServer().tmux(nil)
	if _, err := tm.GetSessionActivity("nonexistent-session-xyz-12345"); err == nil {
		t.Error("GetSessionActivity on nonexistent session should return error")
	}
	if _, err := tm.GetWindowActivity("nonexistent-session-xyz-12345"); err == nil {
		t.Error("GetWindowActivity on nonexistent session should return error")
	}
}

func TestGetWindowActivity_Unparseable(t *testing.T) {
	t.Parallel()
	s := newScripted(bySub(map[string]reply{"display-message": ok("not-a-number")}))
	tm := unitTmux(s, nil)
	if _, err := tm.GetWindowActivity("gt-x"); err == nil {
		t.Error("GetWindowActivity parsed garbage")
	}
	if _, err := tm.GetSessionActivity("gt-x"); err == nil {
		t.Error("GetSessionActivity parsed garbage")
	}
}

func TestCheckSessionHealth_NonexistentSession(t *testing.T) {
	t.Parallel()
	tm, _ := newFakeServer().tmux(nil)
	if status := tm.CheckSessionHealth("nonexistent-session-xyz", 0); status != SessionDead {
		t.Errorf("CheckSessionHealth(nonexistent) = %v, want SessionDead", status)
	}
}

func TestCheckSessionHealth_ZombieSession(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	f.addSession("gt-x", "bash")
	tm, _ := f.tmux(nil)
	if status := tm.CheckSessionHealth("gt-x", 0); status != AgentDead {
		t.Errorf("CheckSessionHealth(shell-only) = %v, want AgentDead", status)
	}
}

// TestCheckSessionHealth_ActivityCheck: a live agent is healthy until its
// window has been silent past maxInactivity, then hung.
func TestCheckSessionHealth_ActivityCheck(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	s := f.addSession("gt-x", "claude")
	clk := clockwork.NewFakeClockAt(time.Unix(1_700_000_000, 0))
	f.with(func() { s.panes[0].activity = clk.Now().Unix() })
	tm, _ := f.tmux(clk)
	if status := tm.CheckSessionHealth("gt-x", time.Minute); status != SessionHealthy {
		t.Errorf("fresh activity: %v, want SessionHealthy", status)
	}
	clk.Advance(2 * time.Minute)
	if status := tm.CheckSessionHealth("gt-x", time.Minute); status != AgentHung {
		t.Errorf("silent past maxInactivity: %v, want AgentHung", status)
	}
	if status := tm.CheckSessionHealth("gt-x", 0); status != SessionHealthy {
		t.Errorf("activity check disabled: %v, want SessionHealthy", status)
	}
}

// TestTmuxEnvironmentIsInjected: the boundary values methods read (the town
// root for the prefix pattern and the agent preset) come through the Tmux's
// environment seam, never the host's.
func TestTmuxEnvironmentIsInjected(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	rigs := `{"version":1,"rigs":{"alpha":{"git_url":"https://example.com/a.git","beads":{"repo":"local","prefix":"al-"}}}}`
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte(rigs), 0o644); err != nil {
		t.Fatal(err)
	}
	tm := unitTmux(newScripted(nil), nil)
	if got := tm.sessionPrefixPattern(); got != "^(gt|hq)-" {
		t.Errorf("empty environment: pattern = %q, want ^(gt|hq)-", got)
	}
	tm.getenv = func(k string) string {
		if k == "GT_TOWN_ROOT" {
			return town
		}
		return ""
	}
	if got := tm.sessionPrefixPattern(); got != "^(al|gt|hq)-" {
		t.Errorf("GT_TOWN_ROOT=%s: pattern = %q, want ^(al|gt|hq)-", town, got)
	}
}
