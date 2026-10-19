package cmd

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/steveyegge/gastown/internal/plugin"
	"github.com/steveyegge/gastown/internal/style"
)

// TestPluginHistoryGlyph verifies `gt plugin history` picks a distinct glyph
// per result, and in particular that ResultWarning does not collapse onto the
// ResultSuccess checkmark (gt-hrt9): a warning is a run that found something
// and reported it, not a quiet success. The same holds for ResultPrinted: a run
// that only printed a plugin's instructions did no work and must not read as
// the success checkmark.
func TestPluginHistoryGlyph(t *testing.T) {
	t.Parallel()
	cases := []struct {
		result    plugin.RunResult
		wantIcon  string
		wantStyle lipgloss.Style
	}{
		{plugin.ResultSuccess, "✓", style.Success},
		{plugin.ResultFailure, "✗", style.Error},
		{plugin.ResultSkipped, "○", style.Dim},
		{plugin.ResultWarning, "!", style.Warning},
		{plugin.ResultPrinted, "▸", style.Info},
	}

	seenIcons := map[string]plugin.RunResult{}
	for _, c := range cases {
		gotStyle, gotIcon := pluginHistoryGlyph(c.result)
		if gotIcon != c.wantIcon {
			t.Errorf("pluginHistoryGlyph(%q) icon = %q, want %q", c.result, gotIcon, c.wantIcon)
		}
		if prev, ok := seenIcons[gotIcon]; ok && prev != c.result {
			t.Errorf("pluginHistoryGlyph(%q) reuses icon %q from %q — results must render distinctly", c.result, gotIcon, prev)
		}
		seenIcons[gotIcon] = c.result

		wantStyle := c.wantStyle.Render("x")
		if got := gotStyle.Render("x"); got != wantStyle {
			t.Errorf("pluginHistoryGlyph(%q) style = %q, want %q", c.result, got, wantStyle)
		}
	}

	// ResultWarning must not render identically to ResultSuccess: that was
	// the bug (both fell through to the default checkmark).
	successStyle, successIcon := pluginHistoryGlyph(plugin.ResultSuccess)
	warningStyle, warningIcon := pluginHistoryGlyph(plugin.ResultWarning)
	if successIcon == warningIcon && successStyle.Render("x") == warningStyle.Render("x") {
		t.Error("ResultWarning renders identically to ResultSuccess — a warning receipt would be invisible in `gt plugin history`")
	}

	// ResultPrinted is what `gt plugin run` records when it only printed the
	// plugin's instructions; a success checkmark would claim work never done.
	_, printedIcon := pluginHistoryGlyph(plugin.ResultPrinted)
	if printedIcon == successIcon {
		t.Error("ResultPrinted renders like ResultSuccess — a printed run would read as work done in `gt plugin history`")
	}
}
