package daemon

import (
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/seatship"
)

func seatEndComment(t *testing.T, p seatship.Payload) string {
	t.Helper()
	c, err := seatship.Encode(p)
	if err != nil {
		t.Fatalf("Encode(%+v): %v", p, err)
	}
	return c
}

func TestSeatEndRecordRead(t *testing.T) {
	t.Parallel()
	seat := seatship.Payload{Rig: "gastown", Polecat: "toast", Bead: "gt-a", Submitted: true}
	tests := []struct {
		name     string
		comments []string
		bead     string
		want     seatship.Payload
		found    bool
		wantErr  bool
	}{
		{"found", []string{seatEndComment(t, seat)}, "gt-a", seat, true, false},
		{"newest wins", []string{seatEndComment(t, seatship.Payload{Rig: "gastown", Polecat: "toast", Bead: "gt-a"}), seatEndComment(t, seat)}, "gt-a", seat, true, false},
		{"foreign comment skipped", []string{seatEndComment(t, seat), "SP ESTIMATE v1 {}"}, "gt-a", seat, true, false},
		{"malformed record skipped", []string{seatEndComment(t, seat), "SEAT-END v1 {}"}, "gt-a", seat, true, false},
		{"other rig skipped", []string{seatEndComment(t, seatship.Payload{Rig: "other", Polecat: "toast", Bead: "gt-a"})}, "gt-a", seatship.Payload{}, false, false},
		{"other polecat skipped", []string{seatEndComment(t, seatship.Payload{Rig: "gastown", Polecat: "nux", Bead: "gt-a"})}, "gt-a", seatship.Payload{}, false, false},
		{"other bead skipped", []string{seatEndComment(t, seatship.Payload{Rig: "gastown", Polecat: "toast", Bead: "gt-b"})}, "gt-a", seatship.Payload{}, false, false},
		{"none", nil, "gt-a", seatship.Payload{}, false, false},
		{"missing bead", nil, "gt-missing", seatship.Payload{}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bd := beadsfake.New()
			bd.Seed(beads.Issue{ID: "gt-a", Title: "seat"})
			for _, text := range tt.comments {
				if err := bd.AddComment("gt-a", text); err != nil {
					t.Fatalf("AddComment: %v", err)
				}
			}
			got, found, err := readSeatEndRecord(bd, "gastown", "toast", tt.bead)
			if (err != nil) != tt.wantErr || found != tt.found || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("readSeatEndRecord = %+v (found %v, err %v); want %+v (found %v, err %v)", got, found, err, tt.want, tt.found, tt.wantErr)
			}
		})
	}
}
