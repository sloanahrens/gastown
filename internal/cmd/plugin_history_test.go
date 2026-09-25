package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/plugin"
	"github.com/steveyegge/gastown/internal/style"
)

// TestPluginHistoryGlyph verifies `gt plugin history` picks a distinct glyph
// per result, and in particular that ResultWarning does not collapse onto the
// ResultSuccess checkmark (gt-hrt9): a warning is a run that found something
// and reported it, not a quiet success.
func TestPluginHistoryGlyph(t *testing.T) {
	cases := []struct {
		result    plugin.RunResult
		wantIcon  string
		wantStyle string // style var name, for the collision check below
	}{
		{plugin.ResultSuccess, "✓", "Success"},
		{plugin.ResultFailure, "✗", "Error"},
		{plugin.ResultSkipped, "○", "Dim"},
		{plugin.ResultWarning, "!", "Warning"},
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

		var wantStyle string
		switch c.wantStyle {
		case "Success":
			wantStyle = style.Success.Render("x")
		case "Error":
			wantStyle = style.Error.Render("x")
		case "Dim":
			wantStyle = style.Dim.Render("x")
		case "Warning":
			wantStyle = style.Warning.Render("x")
		}
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
}
