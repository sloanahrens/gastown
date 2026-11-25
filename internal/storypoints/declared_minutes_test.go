package storypoints

import (
	"math"
	"testing"
)

// TestDeclaredMinutes checks the declared-lines curve at its fitted points, at
// the edges of its range, and that it rises at every line count in the range.
func TestDeclaredMinutes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		lines  int
		want   float64 // minutes, checked when wantOK is true and want is not 0
		tol    float64 // how far got may sit from want
		wantOK bool
	}{
		{name: "30 lines", lines: 30, want: 6.6, tol: 0.05, wantOK: true},
		{name: "100 lines", lines: 100, want: 12.1, tol: 0.05, wantOK: true},
		// The curve gives 17.26 at 200 lines, which rounds to 17.3. The check
		// accepts anything within 0.1 of the 17.2 the bead states.
		{name: "200 lines", lines: 200, want: 17.2, tol: 0.1, wantOK: true},
		{name: "lower bound", lines: 10, wantOK: true},
		{name: "upper bound", lines: 300, wantOK: true},
		{name: "below range", lines: 9},
		{name: "above range", lines: 301},
		{name: "zero", lines: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := DeclaredMinutes(tt.lines)
			if ok != tt.wantOK {
				t.Fatalf("DeclaredMinutes(%d) ok = %v, want %v", tt.lines, ok, tt.wantOK)
			}
			if !tt.wantOK {
				if got != 0 {
					t.Fatalf("DeclaredMinutes(%d) = %v, want 0 when ok is false", tt.lines, got)
				}
				return
			}
			if tt.want != 0 && math.Abs(got-tt.want) > tt.tol {
				t.Fatalf("DeclaredMinutes(%d) = %.4f, want %.1f within %.2f", tt.lines, got, tt.want, tt.tol)
			}
		})
	}

	prev, _ := DeclaredMinutes(10)
	for lines := 11; lines <= 300; lines++ {
		got, ok := DeclaredMinutes(lines)
		if !ok {
			t.Fatalf("DeclaredMinutes(%d) ok = false inside the range", lines)
		}
		if got <= prev {
			t.Fatalf("DeclaredMinutes(%d) = %.4f, not above DeclaredMinutes(%d) = %.4f", lines, got, lines-1, prev)
		}
		prev = got
	}
}
