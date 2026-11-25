package patrolscan

import (
	"errors"
	"testing"
)

// TestClosedPoolResumeNote pins the note a recovery restart appends about its
// pool: a closed pool is named, an open or absent cap says nothing, and a
// failed read is named rather than read as an open pool (gt-anlj5v).
func TestClosedPoolResumeNote(t *testing.T) {
	t.Parallel()
	const key = "polecat_pool.seats.deepseek-flash.cap"
	errRead := errors.New("reading town settings: permission denied")
	cases := []struct {
		name string
		cap  *SeatCap
		err  error
		want string
	}{
		{"closed pool names the pool", &SeatCap{Live: 1, Closed: true, Key: key}, nil, "resumed on closed pool " + key + " (cap 0)"},
		{"open pool adds nothing", &SeatCap{Live: 1, Limit: 3, Key: "polecat_pool.max_seats"}, nil, ""},
		{"pool with no cap adds nothing", &SeatCap{Live: 5}, nil, ""},
		{"no SeatCap wired adds nothing", nil, nil, ""},
		{"failed read names the error", nil, errRead, "cap read failed: " + errRead.Error()},
		{"failed read outranks the cap it returned", &SeatCap{Live: 1, Closed: true, Key: key}, errRead, "cap read failed: " + errRead.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := closedPoolResumeNote(tc.cap, tc.err); got != tc.want {
				t.Fatalf("closedPoolResumeNote(%+v, %v) = %q, want %q", tc.cap, tc.err, got, tc.want)
			}
		})
	}
}

// The caller joins the note after its reason with "; ", so a failed read reads "; cap read failed: <error>".
func TestClosedPoolResumeNoteJoinsAfterTheReason(t *testing.T) {
	t.Parallel()
	note := closedPoolResumeNote(nil, errors.New("permission denied"))
	if got, want := "resumed; "+note, "resumed; cap read failed: permission denied"; got != want {
		t.Fatalf("detail = %q, want %q", got, want)
	}
}
