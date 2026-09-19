package tmux

import "testing"

// TestIsVolatilePaneLine covers the strings that made a healthy polecat look
// stalled (gt-xb27) alongside the pane content that must survive filtering —
// tool-call lines are the primary evidence of real work.
func TestIsVolatilePaneLine(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		volatile bool
	}{
		// Claude Code's per-turn spinner. Its parenthesized elapsed time is what
		// the witness read as "no output 30+ min" on a polecat mid-turn.
		{"spinner with elapsed", "✻ Imagining… (8h 54m 38s)", true},
		{"spinner with elapsed and tokens", "✻ Cogitating… (12m 4s · ↑ 3.2k tokens)", true},
		{"bare spinner", "· Churning…", true},
		{"spinner, other glyph", "⏺ Percolating…", true},
		{"short elapsed", "✢ Working… (3s)", true},
		{"interrupt hint", "  esc to interrupt", true},
		{"interrupt hint, ctrl-c", "Ctrl-C to interrupt", true},
		{"progress percentage", "  Context left until auto-compact: 12%", true},
		{"blank", "   ", true},

		// Real content must never be filtered: this is the evidence the patrol
		// is supposed to weigh.
		{"prose from the lapis case",
			"Test passes. Let me also check the other occurrence at ~line 619 for consistency", false},
		{"file read", "⏺ Read(internal/reaper/reaper.go)", false},
		{"tool call with duration-looking arg", "⏺ Bash(go test ./internal/reaper/ -run TestReap)", false},
		{"markdown bullet", "- it is the same pattern and should use the same filter", false},
		{"prose starting with glyph, no ellipsis", "✻ Imagining that this is prose", false},
		{"test result", "PASS", false},
		{"git output", "3a253ea Merge polecat/marble/gt-h1tq into main", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isVolatilePaneLine(tt.line); got != tt.volatile {
				t.Errorf("isVolatilePaneLine(%q) = %v, want %v", tt.line, got, tt.volatile)
			}
		})
	}
}

// TestPaneContentSignature_IgnoresSpinnerChrome is the gt-xb27 regression
// guard: a pane that does nothing but tick its spinner must hash identically
// across captures, so no detector can mistake the tick for output.
func TestPaneContentSignature_IgnoresSpinnerChrome(t *testing.T) {
	first := "⏺ Read(internal/reaper/reaper.go)\n" +
		"3a253ea Merge polecat/marble into main\n" +
		"✻ Imagining… (8h 54m 38s)\n" +
		"  esc to interrupt"

	second := "⏺ Read(internal/reaper/reaper.go)\n" +
		"3a253ea Merge polecat/marble into main\n" +
		"✻ Imagining… (8h 56m 12s)\n" +
		"  esc to interrupt"

	if a, b := paneContentSignature(first), paneContentSignature(second); a != b {
		t.Errorf("spinner chrome changed the signature: %s != %s", a, b)
	}
}

// TestPaneContentSignature_TracksRealOutput is the converse: genuine work
// changing the screen must change the signature.
func TestPaneContentSignature_TracksRealOutput(t *testing.T) {
	before := "⏺ Read(internal/reaper/reaper.go)\nPASS"
	after := "⏺ Read(internal/reaper/reaper.go)\n⏺ Edit(internal/reaper/reaper.go)\nPASS"

	if paneContentSignature(before) == paneContentSignature(after) {
		t.Error("real output did not change the signature")
	}
}

// TestPaneContentSignature_StableForIdenticalContent pins the no-change case.
func TestPaneContentSignature_StableForIdenticalContent(t *testing.T) {
	content := "⏺ Bash(make test)\nok  internal/witness"
	if paneContentSignature(content) != paneContentSignature(content) {
		t.Error("signature is not deterministic")
	}
}
