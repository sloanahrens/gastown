package ui

import (
	"strings"
	"testing"
)

func fixedWidth() int { return 80 }

func TestRenderMarkdown_AgentMode(t *testing.T) {
	t.Parallel()
	// Agent mode returns raw markdown even when color is forced on.
	e := fakeEnv(true, map[string]string{"GT_AGENT_MODE": "1", "CLICOLOR_FORCE": "1"})
	markdown := "# Test Header\n\nSome content"
	if result := e.renderMarkdown(markdown, fixedWidth); result != markdown {
		t.Errorf("RenderMarkdown() in agent mode should return raw markdown, got %q", result)
	}
}

// With color disabled, RenderMarkdown returns its input unchanged, whatever
// the input holds.
func TestRenderMarkdown_ColorDisabledReturnsRaw(t *testing.T) {
	t.Parallel()
	e := fakeEnv(true, map[string]string{"NO_COLOR": "1"})
	cases := map[string]string{
		"SimpleText":          "Simple text without formatting",
		"EmptyString":         "",
		"GracefulDegradation": "# Test\n\nContent with **bold** and *italic*",
		"LongContent":         strings.Repeat("word ", 1000),
		"Newlines":            "Line 1\n\nLine 2\n\nLine 3",
		"CodeBlocks":          "```go\nfunc main() {}\n```",
		"Links":               "[link text](https://example.com)",
	}
	for name, markdown := range cases {
		if result := e.renderMarkdown(markdown, fixedWidth); result != markdown {
			t.Errorf("%s: RenderMarkdown() with color disabled = %q, want the raw input", name, result)
		}
	}
}

// With color on, the markdown goes through glamour (which styles it for the
// detected terminal, plain on a non-TTY): the text survives and the output is
// laid out rather than passed through.
func TestRenderMarkdown_ColorEnabledRenders(t *testing.T) {
	t.Parallel()
	e := fakeEnv(false, map[string]string{"CLICOLOR_FORCE": "1"})
	widthAsked := false
	markdown := "# Header\n\nSome **bold** text"
	result := e.renderMarkdown(markdown, func() int { widthAsked = true; return 40 })
	if !widthAsked {
		t.Error("renderMarkdown did not ask for the wrap width")
	}
	if !strings.Contains(result, "Header") || !strings.Contains(result, "bold") {
		t.Errorf("rendered output lost the text: %q", result)
	}
	if result == markdown {
		t.Errorf("rendered output = the raw input %q, want glamour's layout", result)
	}
}
