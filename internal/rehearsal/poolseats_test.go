package rehearsal

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestRehearsalFreeSeats(t *testing.T) {
	t.Parallel()
	page := func(seat, total string) string {
		return "sbx-flash  " + seat + "  open\ntotal  " + total + "\nread: polecat_pool.seats, max_total\n"
	}
	cases := []struct {
		name, page, wantErrText string
		runErr                  error
		want                    int
		wantErr                 bool
	}{
		{"the seat's count when the total is roomier", page("3/8 (3 live)", "4/12"), "", nil, 5, false},
		{"the total's count when it is smaller", page("1/8", "9/10"), "", nil, 1, false},
		{"0 when the seat is over its cap", page("9/8", "1/10") + "  held: dead\n", "", nil, 0, false},
		{"0 when the total is over its cap", page("1/8", "11/10"), "", nil, 0, false},
		{"a total with no cap leaves the seat's count", page("2/8", "3 live"), "", nil, 6, false},
		{"a failed run", page("1/8", "1/8"), "gt pool show", errors.New("exit status 1"), 0, true},
		{"no line for the agent", "other  1/8  open\ntotal  1/8\n", "sbx-flash", nil, 0, true},
		{"no total line", "sbx-flash  1/8  open\n", "no total line", nil, 0, true},
		{"the agent's count is not held/cap", page("cap 8", "1/8"), "not held/cap", nil, 0, true},
		{"the total's count is not held/cap", page("1/8", "cap 8"), "not held/cap", nil, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := Town{Run: func(_ context.Context, argv []string) ([]byte, error) {
				if !slices.Equal(argv, []string{"gt", "pool", "show"}) {
					t.Errorf("FreeSeats ran %q, want gt pool show", argv)
				}
				return []byte(tc.page), tc.runErr
			}}
			free, err := town.FreeSeats(context.Background(), "sbx-flash")
			switch {
			case !tc.wantErr && (err != nil || free != tc.want):
				t.Fatalf("FreeSeats = %d, %v; want %d, nil", free, err, tc.want)
			case tc.wantErr && err == nil:
				t.Fatalf("FreeSeats = %d, nil; want an error", free)
			case tc.wantErr && !strings.Contains(err.Error(), tc.wantErrText):
				t.Errorf("FreeSeats error = %v, want it to name %q", err, tc.wantErrText)
			case tc.wantErr && tc.runErr != nil && !errors.Is(err, tc.runErr):
				t.Errorf("FreeSeats error = %v, want it to wrap %v", err, tc.runErr)
			}
		})
	}
}
