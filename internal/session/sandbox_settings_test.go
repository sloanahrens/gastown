package session

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// doltModeSettingsFixture is the dolt-mode seat's render of polecatHostSettings,
// checked in so that the render is pinned to fixed bytes rather than to a
// second call of the renderer under test.
const doltModeSettingsFixture = "testdata/polecat_settings_dolt.golden.json"

// openshellHostSettings is a host polecat settings.json: gt hooks at the
// host's gt, the host-only python path guard, and keys outside "hooks".
const openshellHostSettings = `{
  "theme": "dark",
  "permissions": {"defaultMode": "bypassPermissions"},
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [
        {"type": "command", "command": "/Users/x/.local/bin/gt tap guard pr-workflow"},
        {"type": "command", "command": "/Users/x/.local/bin/gt tap guard dangerous-command"}
      ]},
      {"matcher": "Edit|Write", "hooks": [
        {"type": "command", "command": "/usr/bin/python3 /Users/x/.claude/tools/polecat-path-guard.py"}
      ]}
    ],
    "SessionStart": [
      {"matcher": "", "hooks": [{"type": "command", "command": "/Users/x/.local/bin/gt prime --hook"}]}
    ]
  }
}`

// polecatHostSettings is a host polecat settings.json holding the hooks a
// file-mode seat must and must not run (gt-1m0kb F6): every guard, the prime
// hook, the nudge drain, the mail check and mail inbox, and the bead-reading
// stop hook.
const polecatHostSettings = `{
  "theme": "dark",
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash|Monitor", "hooks": [
        {"type": "command", "command": "/Users/x/.local/bin/gt tap guard pr-workflow"},
        {"type": "command", "command": "/Users/x/.local/bin/gt tap guard dangerous-command"},
        {"type": "command", "command": "/Users/x/.local/bin/gt tap guard container-suite"},
        {"type": "command", "command": "/Users/x/.local/bin/gt tap guard bd-close-invariant"},
        {"type": "command", "command": "/Users/x/.local/bin/gt tap guard bd-create-store"},
        {"type": "command", "command": "/Users/x/.local/bin/gt tap guard sandbox-secrets"}
      ]},
      {"matcher": "Edit|Write|MultiEdit|NotebookEdit", "hooks": [
        {"type": "command", "command": "/Users/x/.local/bin/gt tap guard polecat-paths"},
        {"type": "command", "command": "/Users/x/.local/bin/gt tap guard script-files"}
      ]},
      {"matcher": "AskUserQuestion", "hooks": [
        {"type": "command", "command": "/Users/x/.local/bin/gt tap guard question-tool"}
      ]},
      {"matcher": "Bash", "hooks": [
        {"type": "command", "command": "/usr/bin/python3 /Users/x/.claude/tools/polecat-path-guard.py"}
      ]}
    ],
    "PermissionRequest": [
      {"matcher": "Bash|Monitor", "hooks": [
        {"type": "command", "command": "/Users/x/.local/bin/gt tap guard permission-request"}
      ]}
    ],
    "SessionStart": [
      {"matcher": "", "hooks": [{"type": "command", "command": "/Users/x/.local/bin/gt prime --hook"}]}
    ],
    "PostToolUse": [
      {"matcher": "", "hooks": [{"type": "command", "command": "/Users/x/.local/bin/gt tap nudge-drain"}]}
    ],
    "Stop": [
      {"matcher": "", "hooks": [{"type": "command", "command": "/Users/x/.local/bin/gt tap polecat-stop-check"}]}
    ],
    "UserPromptSubmit": [
      {"matcher": "", "hooks": [
        {"type": "command", "command": "/Users/x/.local/bin/gt mail check --inject"},
        {"type": "command", "command": "/Users/x/.local/bin/gt mail inbox"}
      ]}
    ]
  }
}`

// polecatGuardCommands is every guard the polecat settings above carry.
var polecatGuardCommands = []string{
	"/usr/local/bin/gt tap guard pr-workflow",
	"/usr/local/bin/gt tap guard dangerous-command",
	"/usr/local/bin/gt tap guard container-suite",
	"/usr/local/bin/gt tap guard bd-close-invariant",
	"/usr/local/bin/gt tap guard bd-create-store",
	"/usr/local/bin/gt tap guard sandbox-secrets",
	"/usr/local/bin/gt tap guard polecat-paths",
	"/usr/local/bin/gt tap guard script-files",
	"/usr/local/bin/gt tap guard question-tool",
	"/usr/local/bin/gt tap guard permission-request",
}

func hookCommands(t *testing.T, settings []byte) map[string][]string {
	t.Helper()
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(settings, &doc); err != nil {
		t.Fatalf("rendered settings are not JSON: %v\n%s", err, settings)
	}
	out := map[string][]string{}
	for event, groups := range doc.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				out[event] = append(out[event], h.Command)
			}
		}
	}
	return out
}

// assertDoltModeHookSet checks settings still hold every hook a dolt-mode seat
// runs (gt-1m0kb F6), so a hook dropped from the render is caught even when the
// fixture was rewritten to match it.
func assertDoltModeHookSet(t *testing.T, settings []byte) {
	t.Helper()
	got := hookCommands(t, settings)
	for _, guard := range polecatGuardCommands {
		if !slices.Contains(got["PreToolUse"], guard) && !slices.Contains(got["PermissionRequest"], guard) {
			t.Errorf("dolt-mode seat lost the guard %q", guard)
		}
	}
	for _, want := range []struct {
		event, command string
	}{
		{"SessionStart", "/usr/local/bin/gt prime --hook"},
		{"PostToolUse", "/usr/local/bin/gt tap nudge-drain"},
		{"Stop", "/usr/local/bin/gt tap polecat-stop-check"},
		{"UserPromptSubmit", "/usr/local/bin/gt mail check --inject"},
		{"UserPromptSubmit", "/usr/local/bin/gt mail inbox"},
	} {
		if !slices.Contains(got[want.event], want.command) {
			t.Errorf("dolt-mode seat lost the %s hook %q, got %q", want.event, want.command, got[want.event])
		}
	}
}

func TestRenderSandboxSettingsKeepsGtHooksAndDropsOnlyThePathGuard(t *testing.T) {
	t.Parallel()
	out, err := renderSandboxSettings([]byte(openshellHostSettings))
	if err != nil {
		t.Fatalf("renderSandboxSettings: %v", err)
	}
	got := hookCommands(t, out)
	want := map[string][]string{
		"PreToolUse":   {"/usr/local/bin/gt tap guard pr-workflow", "/usr/local/bin/gt tap guard dangerous-command"},
		"SessionStart": {"/usr/local/bin/gt prime --hook"},
	}
	for event, cmds := range want {
		if !slices.Equal(got[event], cmds) {
			t.Errorf("%s hooks = %q, want %q", event, got[event], cmds)
		}
	}
	if len(got) != len(want) {
		t.Errorf("events = %v, want exactly %v (the group holding only the path guard must go)", got, want)
	}
	if strings.Contains(string(out), "/Users/") {
		t.Errorf("rendered settings still name a host path:\n%s", out)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["theme"] != "dark" || doc["permissions"] == nil {
		t.Errorf("keys outside hooks were not copied: %v", doc)
	}
}

func TestRenderSandboxSettingsRefusesAnUnknownHostOnlyHook(t *testing.T) {
	t.Parallel()
	host := strings.Replace(openshellHostSettings,
		"/usr/bin/python3 /Users/x/.claude/tools/polecat-path-guard.py",
		"/usr/bin/python3 /Users/x/.claude/tools/some-other-guard.py", 1)
	_, err := renderSandboxSettings([]byte(host))
	if err == nil || !strings.Contains(err.Error(), "some-other-guard.py") {
		t.Fatalf("renderSandboxSettings = %v, want an error naming the unknown hook", err)
	}
}

func TestRenderSandboxSettingsRefusesAHooksStructureItCannotInspect(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, settings string }{
		{"hooks is a list", `{"hooks": []}`},
		{"hooks is a string", `{"hooks": "none"}`},
		{"hooks is null", `{"hooks": null}`},
		{"an event is not a list", `{"hooks": {"PreToolUse": {}}}`},
		{"a group is not an object", `{"hooks": {"PreToolUse": ["gt prime"]}}`},
		{"a group has no hooks", `{"hooks": {"PreToolUse": [{"matcher": "Bash"}]}}`},
		{"a hook is not an object", `{"hooks": {"PreToolUse": [{"hooks": ["gt prime"]}]}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if out, err := renderSandboxSettings([]byte(tt.settings)); err == nil {
				t.Errorf("renderSandboxSettings accepted %s: %s", tt.name, out)
			}
		})
	}
}

func TestRenderSandboxSettingsRefusesANonCommandHook(t *testing.T) {
	t.Parallel()
	const host = `{"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "http", "url": "https://example.invalid/hook"}]}]}}`
	out, err := renderSandboxSettings([]byte(host))
	if err == nil || !strings.Contains(err.Error(), "PreToolUse") {
		t.Fatalf("renderSandboxSettings = %s, %v; want an error naming the event whose hook has no command", out, err)
	}
}

func TestRenderSandboxSettingsCopiesSettingsWithoutHooks(t *testing.T) {
	t.Parallel()
	out, err := renderSandboxSettings([]byte(`{"theme": "dark"}`))
	if err != nil {
		t.Fatalf("renderSandboxSettings: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["theme"] != "dark" {
		t.Errorf("settings without hooks were not copied: %v", doc)
	}
	if _, present := doc["hooks"]; present {
		t.Errorf("settings without hooks gained a hooks key: %v", doc)
	}
}

func TestRenderHookCommandMatchesTheProgramExactly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, cmd, want string
		drop, refuse    bool
	}{
		{name: "bare gt", cmd: "gt prime --hook", want: "/usr/local/bin/gt prime --hook"},
		{name: "host gt keeps its argv as written", cmd: "/Users/x/.local/bin/gt  tap guard  pr-workflow", want: "/usr/local/bin/gt  tap guard  pr-workflow"},
		{name: "leading whitespace does not survive", cmd: "  /Users/x/.local/bin/gt prime", want: "/usr/local/bin/gt prime"},
		{name: "a longer name is not gt", cmd: "/opt/gt/bin/gtx prime", refuse: true},
		{name: "a longer name is not python", cmd: "/usr/bin/pythonista /Users/x/.claude/tools/polecat-path-guard.py", refuse: true},
		{name: "the guard is dropped", cmd: "/usr/bin/python3 /Users/x/.claude/tools/polecat-path-guard.py", drop: true},
		{name: "a versioned python still runs the guard", cmd: "/usr/bin/python3.11 /Users/x/.claude/tools/polecat-path-guard.py", drop: true},
		{name: "a guard by that name elsewhere is host-only too", cmd: "/usr/bin/python3 /tmp/polecat-path-guard.py", refuse: true},
		{name: "an argument before the guard is not the guard", cmd: "/usr/bin/python3 -u /Users/x/.claude/tools/polecat-path-guard.py", refuse: true},
		{name: "empty command", cmd: "", refuse: true},
		{name: "whitespace command", cmd: "   ", refuse: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, drop, err := renderHookCommand(tt.cmd)
			switch {
			case tt.refuse:
				if err == nil {
					t.Fatalf("renderHookCommand(%q) = %q, drop %v; want a refusal", tt.cmd, got, drop)
				}
			case err != nil:
				t.Fatalf("renderHookCommand(%q): %v", tt.cmd, err)
			case drop != tt.drop:
				t.Fatalf("renderHookCommand(%q) dropped = %v, want %v", tt.cmd, drop, tt.drop)
			case got != tt.want:
				t.Errorf("renderHookCommand(%q) = %q, want %q", tt.cmd, got, tt.want)
			}
		})
	}
}

func TestRewriteTownRootChangesWholePathsOnly(t *testing.T) {
	t.Parallel()
	const from, to = "/h/gt", "/sandbox/gt"
	tests := []struct{ name, in, want string }{
		{"flag value", "claude --settings /h/gt/r/polecats/.claude/settings.json", "claude --settings /sandbox/gt/r/polecats/.claude/settings.json"},
		{"env assignment", "env GIT_CEILING_DIRECTORIES=/h/gt X=1", "env GIT_CEILING_DIRECTORIES=/sandbox/gt X=1"},
		{"quoted and backticked", "a '/h/gt/x' \"/h/gt\" `/h/gt/y`", "a '/sandbox/gt/x' \"/sandbox/gt\" `/sandbox/gt/y`"},
		{"colon list", "PATH=/h/gt/bin:/h/gt", "PATH=/sandbox/gt/bin:/sandbox/gt"},
		{"longer sibling name", "/h/gt-other/x /h/gtx", "/h/gt-other/x /h/gtx"},
		{"contained deeper in another path", "/x/h/gt/y", "/x/h/gt/y"},
		{"no mention", "claude --model m", "claude --model m"},
		{"the whole string", "/h/gt", "/sandbox/gt"},
		{"the whole string then a separator", "/h/gt/", "/sandbox/gt/"},
		{"in parentheses", "x(/h/gt)y", "x(/sandbox/gt)y"},
		{"tab before, newline after", "a\t/h/gt\nb", "a\t/sandbox/gt\nb"},
		{"repeated, second one embedded", "/h/gt/h/gt", "/sandbox/gt/h/gt"},
		{"two whole paths in a list", "/h/gt:/h/gt", "/sandbox/gt:/sandbox/gt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := RewriteTownRoot(tt.in, from, to); got != tt.want {
				t.Errorf("RewriteTownRoot(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
	if got := RewriteTownRoot("/h/gt/x", "", ""); got != "/h/gt/x" {
		t.Errorf("an empty root must leave the text alone, got %q", got)
	}
	if got := RewriteTownRoot("/h/gt/x", from, from); got != "/h/gt/x" {
		t.Errorf("an unchanged root must leave the text alone, got %q", got)
	}
}

// TestNoBeadAccessFileModeSeatSettingsDropMailAndKeepEveryGuard is the seat's
// rendered hook set in file mode (gt-1m0kb F6): the mail check, the mail inbox
// and the bead-reading stop hook are gone, and every guard, the prime hook and
// the nudge drain stay.
func TestNoBeadAccessFileModeSeatSettingsDropMailAndKeepEveryGuard(t *testing.T) {
	t.Parallel()
	out, err := renderSandboxSettingsForSeat([]byte(polecatHostSettings), config.SandboxBeadAccessFiles)
	if err != nil {
		t.Fatalf("renderSandboxSettingsForSeat(files): %v", err)
	}
	got := hookCommands(t, out)

	for event, commands := range got {
		for _, command := range commands {
			for _, gone := range []string{"mail check", "mail inbox", "polecat-stop-check"} {
				if strings.Contains(command, gone) {
					t.Errorf("file-mode seat still runs %q (%s)", command, event)
				}
			}
		}
	}
	for _, event := range []string{"Stop", "UserPromptSubmit"} {
		if _, present := got[event]; present {
			t.Errorf("file-mode seat keeps a %s hook: %q", event, got[event])
		}
	}
	for _, guard := range polecatGuardCommands {
		if !slices.Contains(got["PreToolUse"], guard) && !slices.Contains(got["PermissionRequest"], guard) {
			t.Errorf("file-mode seat lost the guard %q", guard)
		}
	}
	for event, want := range map[string]string{
		"SessionStart": "/usr/local/bin/gt prime --hook",
		"PostToolUse":  "/usr/local/bin/gt tap nudge-drain",
	} {
		if !slices.Contains(got[event], want) {
			t.Errorf("file-mode seat lost the %s hook %q, got %q", event, want, got[event])
		}
	}
}

// TestNoBeadAccessDoltModeSeatSettingsUnchanged pins that bead access other
// than files renders the settings exactly as before (gt-1m0kb F6): byte for
// byte the checked-in fixture, which holds the mail check, the mail inbox and
// the stop hook intact. The hook-presence check runs against the fixture too,
// so a hook edited out of both the render and the fixture still fails.
func TestNoBeadAccessDoltModeSeatSettingsUnchanged(t *testing.T) {
	t.Parallel()
	want, err := os.ReadFile(doltModeSettingsFixture)
	if err != nil {
		t.Fatalf("reading the dolt-mode fixture: %v", err)
	}
	assertDoltModeHookSet(t, want)
	for _, beadAccess := range []string{config.SandboxBeadAccessDolt, ""} {
		t.Run("bead_access="+beadAccess, func(t *testing.T) {
			t.Parallel()
			got, err := renderSandboxSettingsForSeat([]byte(polecatHostSettings), beadAccess)
			if err != nil {
				t.Fatalf("renderSandboxSettingsForSeat(%q): %v", beadAccess, err)
			}
			assertDoltModeHookSet(t, got)
			if !bytes.Equal(got, want) {
				t.Errorf("bead_access %q changed the rendered settings:\ngot:\n%s\nwant (%s):\n%s",
					beadAccess, got, doltModeSettingsFixture, want)
			}
		})
	}
}
