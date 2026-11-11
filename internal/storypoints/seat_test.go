package storypoints

import "testing"

// TestSeatClass pins the seat class of each agent name. Names match exactly,
// except that any name starting deepseek is the flash class.
func TestSeatClass(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		agent string
		want  string
	}{
		{"sbx-haiku is haiku", "sbx-haiku", "haiku"},
		{"claude-haiku is haiku", "claude-haiku", "haiku"},
		{"sbx-flash is flash", "sbx-flash", "flash"},
		{"deepseek-claude is flash", "deepseek-claude", "flash"},
		{"bare deepseek is flash", "deepseek", "flash"},
		{"claude-sonnet is sonnet", "claude-sonnet", "sonnet"},
		{"empty name has no class", "", ""},
		{"claude-opus has no class", "claude-opus", ""},
		{"upper-case name does not match", "SBX-HAIKU", ""},
		{"haiku name with a suffix does not match", "sbx-haiku-2", ""},
		{"deepseek inside a name does not match", "xdeepseek", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := SeatClass(tt.agent); got != tt.want {
				t.Fatalf("SeatClass(%q) = %q, want %q", tt.agent, got, tt.want)
			}
		})
	}
}
