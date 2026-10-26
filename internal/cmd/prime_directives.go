package cmd

import (
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

// renderDirectiveSection is the operator directives that render for a role, as
// the system-prompt file carries them: one "## Directive: <scope> <name>"
// heading per source, broadest first, then the file's full text. No truncation
// (gt-qwcdz): the hook's capped copy is the only other route, and a polecat in a
// sandbox cannot read the file the cap points it to.
func renderDirectiveSection(directives []config.RoleDirective) string {
	var b strings.Builder
	for _, d := range directives {
		b.WriteString("\n## Directive: " + d.Scope + " " + d.Name + "\n\n")
		b.WriteString(d.Text)
		if !strings.HasSuffix(d.Text, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}
