package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// TestDirectiveHelp_NamesTheResolvedLayerPaths: `gt directive --help` names the
// central and rig layer directories that the reader resolves and that
// 'gt directive list' prints, built by the config package so the help cannot
// drift from the layout (gt-ivnbx).
func TestDirectiveHelp_NamesTheResolvedLayerPaths(t *testing.T) {
	t.Parallel()

	help := directiveCmd.Long
	for _, want := range []string{
		config.CentralDirectiveDir("<townRoot>") + "/<role>.md",
		config.RigDirectiveDir("<townRoot>", "<rig>") + "/<role>.md",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help does not name %q:\n%s", want, help)
		}
	}
}

// TestDirectiveHelp_NamesTheLegacyDirectoriesOnlyAsFallback: the pre-gt-oz73l
// directories still reach an operator, but only in the paragraph that says a
// file there is read as a fallback, never as the layout itself (gt-ivnbx).
func TestDirectiveHelp_NamesTheLegacyDirectoriesOnlyAsFallback(t *testing.T) {
	t.Parallel()

	help := directiveCmd.Long
	named := false
	for _, para := range strings.Split(help, "\n\n") {
		if !strings.Contains(para, "<townRoot>/directives/") &&
			!strings.Contains(para, "<townRoot>/<rig>/directives/") {
			continue
		}
		named = true
		if !strings.Contains(para, "Legacy fallback") {
			t.Errorf("legacy directive directory named outside the fallback paragraph:\n%s", para)
		}
	}
	if !named {
		t.Errorf("help does not name the legacy directive directories at all:\n%s", help)
	}
}
