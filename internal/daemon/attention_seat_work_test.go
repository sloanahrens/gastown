package daemon

import (
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// TestSeatHoldsOpenWork is gt-kd866f: a bead submitted for landing is not work
// in hand, so a seat whose only bead carries gt:ready-to-land holds nothing,
// while a plain bead in a sling status keeps it working.
func TestSeatHoldsOpenWork(t *testing.T) {
	t.Parallel()
	submitted := &beads.Issue{Status: "hooked", Labels: []string{land.LabelReadyToLand}}
	plainHooked := &beads.Issue{Status: "hooked"}
	plainProgress := &beads.Issue{Status: "in_progress"}
	plainOpen := &beads.Issue{Status: "open"}

	listOf := func(byStatus map[string][]*beads.Issue) func(string) ([]*beads.Issue, error) {
		return func(status string) ([]*beads.Issue, error) { return byStatus[status], nil }
	}

	tests := []struct {
		name string
		list func(string) ([]*beads.Issue, error)
		want bool
	}{
		{
			name: "submitted bead alone is not work in hand",
			list: listOf(map[string][]*beads.Issue{"hooked": {submitted}}),
			want: false,
		},
		{
			name: "nothing assigned holds nothing",
			list: listOf(nil),
			want: false,
		},
		{
			name: "plain hooked bead is work in hand",
			list: listOf(map[string][]*beads.Issue{"hooked": {plainHooked}}),
			want: true,
		},
		{
			name: "plain in_progress bead is work in hand",
			list: listOf(map[string][]*beads.Issue{"in_progress": {plainProgress}}),
			want: true,
		},
		{
			name: "plain open bead is work in hand",
			list: listOf(map[string][]*beads.Issue{"open": {plainOpen}}),
			want: true,
		},
		{
			name: "a submitted bead does not hide another plain bead",
			list: listOf(map[string][]*beads.Issue{
				"hooked":      {submitted},
				"in_progress": {plainProgress},
			}),
			want: true,
		},
		{
			name: "a submitted bead and a plain bead in the same status",
			list: listOf(map[string][]*beads.Issue{"hooked": {submitted, plainHooked}}),
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := seatHoldsOpenWork(tt.list)
			if err != nil {
				t.Fatalf("seatHoldsOpenWork: %v", err)
			}
			if got != tt.want {
				t.Errorf("seatHoldsOpenWork = %v, want %v", got, tt.want)
			}
		})
	}
}

// A list that cannot answer is an error and false, never "no work": the
// collector keeps its previous items rather than clearing a real stall.
func TestSeatHoldsOpenWorkListError(t *testing.T) {
	t.Parallel()
	submitted := &beads.Issue{Status: "hooked", Labels: []string{land.LabelReadyToLand}}
	boom := errors.New("bd is down")

	t.Run("first status fails", func(t *testing.T) {
		t.Parallel()
		got, err := seatHoldsOpenWork(func(string) ([]*beads.Issue, error) { return nil, boom })
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want %v", err, boom)
		}
		if got {
			t.Error("seatHoldsOpenWork reported work in hand alongside the error")
		}
	})

	t.Run("later status fails after a submitted bead", func(t *testing.T) {
		t.Parallel()
		got, err := seatHoldsOpenWork(func(status string) ([]*beads.Issue, error) {
			if status == "hooked" {
				return []*beads.Issue{submitted}, nil
			}
			return nil, boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want %v", err, boom)
		}
		if got {
			t.Error("seatHoldsOpenWork reported work in hand alongside the error")
		}
	})
}

// failingListBeads fails every read, standing in for a bd that will not answer.
type failingListBeads struct{ err error }

func (f failingListBeads) Show(string) (*beads.Issue, error) { return nil, f.err }

func (f failingListBeads) List(beads.ListOptions) ([]*beads.Issue, error) { return nil, f.err }

// attentionSeatWork names the seat in the read's error, so the stall log says
// which polecat could not be judged.
func TestAttentionSeatWorkKeepsTheSeatOnError(t *testing.T) {
	t.Parallel()
	boom := errors.New("bd is down")
	d := &Daemon{
		config:        &Config{TownRoot: t.TempDir()},
		logger:        log.New(io.Discard, "", 0),
		openWorkBeads: func([]string, time.Duration) workBeadReader { return failingListBeads{err: boom} },
	}
	holds, err := d.attentionSeatWork(attentionRig, "opal")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the failed read", err)
	}
	if holds {
		t.Error("attentionSeatWork reported work in hand alongside the error")
	}
	if !strings.Contains(err.Error(), attentionRig+"/polecats/opal") {
		t.Errorf("err = %q, want it to name the seat that could not be read", err)
	}
}

// gt-kd866f: collectPolecatStall raises no item for a quiet seat whose only
// bead is submitted for landing. It drives the collector through the same
// helper production uses, so dropping the submitted-work filter turns the
// item back on.
func TestPolecatStallSkipsSubmittedWork(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	quiet := now.Add(-attentionPolecatStall - time.Minute)
	f := newAttentionFixture(t, now)
	f.src.seats = func() ([]townhealth.Seat, error) {
		return []townhealth.Seat{{
			Rig:     attentionRig,
			Name:    "polecat/opal",
			Run:     true,
			Sampled: now.Add(-time.Minute),
			Changed: quiet,
		}}, nil
	}

	submitted := &beads.Issue{Status: "hooked", Labels: []string{land.LabelReadyToLand}}
	byStatus := map[string][]*beads.Issue{"hooked": {submitted}}
	f.src.seatWork = func(rig, name string) (bool, error) {
		if rig != attentionRig || name != "opal" {
			t.Fatalf("seatWork(%q, %q), want the fixture seat", rig, name)
		}
		return seatHoldsOpenWork(func(status string) ([]*beads.Issue, error) {
			return byStatus[status], nil
		})
	}
	if items := f.collect(t, f.src.collectPolecatStall); len(items) != 0 {
		t.Fatalf("items = %+v, want none for a seat whose only bead is submitted for landing", items)
	}

	// The same seat with a plain bead is still a stall: the filter is what
	// cleared the item, not the seat going quiet.
	byStatus["hooked"] = []*beads.Issue{{Status: "hooked"}}
	items := f.collect(t, f.src.collectPolecatStall)
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one stall for a seat holding plain work", items)
	}
	if !strings.Contains(items[0].Summary, "work in hand") {
		t.Errorf("summary = %q, want the work-in-hand stall", items[0].Summary)
	}
}
