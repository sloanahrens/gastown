package land

import (
	"errors"
	"strings"
	"testing"
)

// TestRoundsOMNumbersRefusesARepeat: om's ParseRounds refuses a round number
// below 1 or repeated; the checker Render calls refuses the same and passes 1
// to n, so such a break degrades the landing instead of blocking it at om
// (gt-smmlw4).
func TestRoundsOMNumbersRefusesARepeat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		rounds []omRound
		want   string
	}{
		{"repeat", []omRound{{Round: 1}, {Round: 2}, {Round: 1}}, "round number 1 appears twice"},
		{"below 1", []omRound{{Round: 0}}, "round number 0 is below 1"},
		{"1 to n", []omRound{{Round: 1}, {Round: 2}, {Round: 3}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := checkOMRounds([]byte("[]"), tc.rounds)
			ok := errors.Is(err, ErrRoundsCorrupt) && strings.Contains(err.Error(), tc.want)
			if tc.want == "" {
				ok = err == nil && string(got) == "[]"
			}
			if !ok {
				t.Fatalf("checkOMRounds = %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}
