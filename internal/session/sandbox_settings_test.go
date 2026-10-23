package session

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

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
