package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/seatfacts"
)

// TestAgentBeadRetireWorkBead is gt-lsrs78: the choice answers the record's
// work_bead, keeps a finished turn's empty one, falls back to the agent bead's
// hook_bead only for a record that predates the intent fields, and refuses a
// record it could not read rather than answering the bead.
func TestAgentBeadRetireWorkBead(t *testing.T) {
	t.Parallel()
	readErr := errors.New("reading seat facts: record unreadable")
	cases := []struct {
		name     string
		facts    seatfacts.Facts
		readErr  error
		beadHook string
		want     string
		wantErr  error
	}{
		{
			name:     "the record's work bead",
			facts:    seatfacts.Facts{WorkBead: "gt-intent-bead"},
			beadHook: "gt-bead-hook",
			want:     "gt-intent-bead",
		},
		{
			name:     "a finished turn keeps the record's empty work bead",
			facts:    seatfacts.Facts{TurnState: "done"},
			beadHook: "gt-bead-hook",
			want:     "",
		},
		{
			name:     "a record predating the intent fields answers the bead's hook",
			facts:    seatfacts.Facts{},
			beadHook: "gt-bead-hook",
			want:     "gt-bead-hook",
		},
		{
			name:     "an unreadable record is an error, not the bead",
			readErr:  readErr,
			beadHook: "gt-bead-hook",
			want:     "",
			wantErr:  readErr,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := poolSeatWorkBead(tc.facts, tc.readErr, tc.beadHook)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("poolSeatWorkBead() error = %v, want %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("poolSeatWorkBead() error = %v, want nil", err)
			}
			if got != tc.want {
				t.Fatalf("poolSeatWorkBead() = %q, want %q", got, tc.want)
			}
		})
	}
}
