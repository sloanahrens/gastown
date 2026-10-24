//go:build linux

package util

import "testing"

func TestParseProcStat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		line      string
		wantGroup int
		wantState byte
		wantOK    bool
	}{
		{"plain", "7389 (sh) S 1 7389 7389 0 -1 4194560", 7389, 'S', true},
		{"comm with spaces and parens", "412 (my (odd) cmd) R 1 412 412 0 -1 0", 412, 'R', true},
		{"zombie", "9 (sleep) Z 7389 7389 7389 0 -1 0", 7389, 'Z', true},
		{"no closing paren", "7389 sh S 1 7389", 0, 0, false},
		{"truncated after comm", "7389 (sh) S 1", 0, 0, false},
		{"non-numeric group", "7389 (sh) S 1 x 7389", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g, s, ok := parseProcStat(tt.line)
			if g != tt.wantGroup || s != tt.wantState || ok != tt.wantOK {
				t.Errorf("parseProcStat(%q) = %d, %q, %v; want %d, %q, %v", tt.line, g, s, ok, tt.wantGroup, tt.wantState, tt.wantOK)
			}
		})
	}
}
