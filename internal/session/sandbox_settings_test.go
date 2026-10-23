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
}
