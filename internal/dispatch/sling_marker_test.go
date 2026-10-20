package dispatch

import "testing"

func TestCountSlingHooked(t *testing.T) {
	t.Parallel()
	hooked := "  ✓ " + SlingHookedLine("furiosa") + "\n"
	tests := []struct {
		name string
		out  string
		want int
	}{
		{"empty", "", 0},
		{"one sling", hooked, 1},
		{"two slings amid other output", "spawning\n" + hooked + "more\n" + hooked, 2},
		{"color sequences around the glyph", "  \x1b[1m✓\x1b[0m " + SlingHookedLine("nux") + "\n", 1},
		{"no trailing newline", "  ✓ " + SlingHookedLine("nux"), 1},
		{"legacy gt sling line", "✓ Work attached to hook (status=hooked)\n", 0},
		{"gt hook lines", "✓ Work attached to gastown/polecats/p's hook\n✓ Work attached to hook (hooked bead)\n", 0},
		{"the prefix inside a line", "note: Work attached to p\n", 0},
		{"the prefix with no polecat", "  ✓ " + SlingHookedMarker + "\n", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := CountSlingHooked([]byte(tt.out)); got != tt.want {
				t.Errorf("CountSlingHooked(%q) = %d, want %d", tt.out, got, tt.want)
			}
		})
	}
}
