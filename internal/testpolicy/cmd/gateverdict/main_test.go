package main

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// TestVerdict: each branch of the gate's verdict, with the unit tier named first when both stages
// failed. The lines are the ones the gate has always printed.
func TestVerdict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		unit, vet int
		line      string
		code      int
	}{
		{"both stages pass", 0, 0, "gate: PASSED in 42s wall", 0},
		{"the unit tier fails", 1, 0, "gate: FAILED at unit tier (Go suite, exit 1) after 42s wall", 1},
		{"the integration vet fails", 0, 2, "gate: FAILED at integration vet (exit 2) after 42s wall", 1},
		{"both fail: the unit tier is named", 1, 2, "gate: FAILED at unit tier (Go suite, exit 1) after 42s wall", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			line, code := verdict(tc.unit, tc.vet, 42)
			if line != tc.line || code != tc.code {
				t.Errorf("verdict(%d, %d) = %q, %d; want %q, %d", tc.unit, tc.vet, line, code, tc.line, tc.code)
			}
		})
	}
}
