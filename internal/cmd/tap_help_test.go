package cmd

import (
	"regexp"
	"testing"

	"github.com/spf13/cobra"
)

// matcherExampleRE pulls the value out of a `"matcher": "<value>"` line in
// help text — the example an agent copies into .claude/settings.json.
var matcherExampleRE = regexp.MustCompile(`"matcher":\s*"([^"]*)"`)

// TestTapHelpExamplesUseShellExecutingMatcher pins the copied example configs
// (gt-kxw6d): an example matcher that fires for "Bash" must also fire for
// "Monitor", since Claude Code's matcher only ever matches the tool name
// (gt-5ihs) and a bare "Bash" let a command run through Monitor reach a
// guard's target unguarded (gt-vx2mm). Examples that do not fire for Bash
// belong to another tool set and may name whatever tools they guard.
func TestTapHelpExamplesUseShellExecutingMatcher(t *testing.T) {
	t.Parallel()

	var cmds []*cobra.Command
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		cmds = append(cmds, c)
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(tapCmd)

	examples := 0
	for _, c := range cmds {
		for _, m := range matcherExampleRE.FindAllStringSubmatch(c.Long, -1) {
			matcher := m[1]
			if !preToolUseMatcherAppliesToTool(matcher, "Bash") {
				continue
			}
			examples++
			if !preToolUseMatcherAppliesToTool(matcher, "Monitor") {
				t.Errorf("%s help example uses matcher %q, which fires for Bash but not Monitor; a guard configured from it is bypassable (gt-vx2mm)", c.CommandPath(), matcher)
			}
		}
	}
	if examples == 0 {
		t.Error("no shell-executing matcher example found in the gt tap help texts; the extraction this test relies on has drifted")
	}
}
