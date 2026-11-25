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
// failed. The lines match the messages the gate recipe prints, so wiring the command keeps the output unchanged.
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
			line, note, code := verdict(tc.unit, tc.vet, 42)
			if line != tc.line || note != "" || code != tc.code {
				t.Errorf("verdict(%d, %d, 42) = %q, %q, %d; want %q, no note, %d", tc.unit, tc.vet, line, note, code, tc.line, tc.code)
			}
		})
	}
}

// TestVerdictNegativeWall: a wall read earlier at the end of the run than at its start (the
// clock stepped back) prints as 0 so the line keeps the format the gate's readers parse, and a
// note on stderr says why (gt-5qfrbu).
func TestVerdictNegativeWall(t *testing.T) {
	t.Parallel()
	const steppedBack = "gate: the clock stepped back during the run; the wall reads 0s"
	tests := []struct {
		name      string
		unit, vet int
		wall      int
		line      string
		note      string
		code      int
	}{
		{"a negative wall passes", 0, 0, -1, "gate: PASSED in 0s wall", steppedBack, 0},
		{"a negative wall fails on the unit tier", 1, 0, -5, "gate: FAILED at unit tier (Go suite, exit 1) after 0s wall", steppedBack, 1},
		{"a negative wall fails at the integration vet", 0, 2, -3, "gate: FAILED at integration vet (exit 2) after 0s wall", steppedBack, 1},
		{"a zero wall is unchanged", 0, 0, 0, "gate: PASSED in 0s wall", "", 0},
		{"a positive wall is unchanged", 0, 0, 42, "gate: PASSED in 42s wall", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			line, note, code := verdict(tc.unit, tc.vet, tc.wall)
			if line != tc.line || note != tc.note || code != tc.code {
				t.Errorf("verdict(%d, %d, %d) = %q, %q, %d; want %q, %q, %d", tc.unit, tc.vet, tc.wall, line, note, code, tc.line, tc.note, tc.code)
			}
		})
	}
}
