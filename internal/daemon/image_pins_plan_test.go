package daemon

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// TestPlanImagePins is the filing decision's table: which pins a refresh bead
// names, and which conditions it raises, for each distance from the tip and
// each state of the open refresh bead.
func TestPlanImagePins(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	state := func(name string, behind int) ImagePinState {
		return ImagePinState{Name: name, Pin: "pin-" + name, Tip: "tip-" + name, Behind: behind}
	}
	// A failed read's Behind is not trusted: the state still carries a stale
	// count so the row fails if the error is ignored.
	failed := ImagePinState{Name: "om", Behind: 34, Err: errors.New("ls-remote exited 128")}

	tests := []struct {
		name      string
		pins      []ImagePinState
		open      *ImagePinRefreshBead
		wantFile  []string
		wantItems []string
	}{
		{
			name: "every pin fresh",
			pins: []ImagePinState{state("bd", 0), state("om", 0), state("hm", 0), state("devops", 0)},
		},
		{
			name: "one commit short of the file threshold",
			pins: []ImagePinState{state("bd", 19)},
		},
		{
			name:     "at the file threshold",
			pins:     []ImagePinState{state("bd", 20)},
			wantFile: []string{"bd"},
		},
		{
			name:     "exactly the pins at or over the threshold",
			pins:     []ImagePinState{state("bd", 19), state("om", 20), state("hm", 34), state("devops", 3)},
			wantFile: []string{"om", "hm"},
		},
		{
			name:     "a failed read is never filed",
			pins:     []ImagePinState{state("bd", 34), failed},
			wantFile: []string{"bd"},
		},
		{
			name: "an open bead blocks a second",
			pins: []ImagePinState{state("bd", 25)},
			open: &ImagePinRefreshBead{ID: "gt-refresh7", OpenedAt: at.Add(-time.Hour)},
		},
		{
			name: "an open bead at the stuck bound",
			pins: []ImagePinState{state("bd", 25)},
			open: &ImagePinRefreshBead{ID: "gt-refresh7", OpenedAt: at.Add(-imagePinsRefreshStuck)},
		},
		{
			name:      "an open bead past the stuck bound",
			pins:      []ImagePinState{state("bd", 25)},
			open:      &ImagePinRefreshBead{ID: "gt-refresh7", OpenedAt: at.Add(-imagePinsRefreshStuck - time.Second)},
			wantItems: []string{imagePinsRefreshStuckKey},
		},
		{
			name:      "an open bead past the stuck bound with nothing behind",
			pins:      []ImagePinState{state("bd", 0)},
			open:      &ImagePinRefreshBead{ID: "gt-refresh7", OpenedAt: at.Add(-4 * time.Hour)},
			wantItems: []string{imagePinsRefreshStuckKey},
		},
		{
			name:      "an open bead with an unknown age",
			pins:      []ImagePinState{state("bd", 25)},
			open:      &ImagePinRefreshBead{ID: "gt-refresh7"},
			wantItems: []string{imagePinsRefreshStuckKey},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PlanImagePins(tt.pins, tt.open, at)
			var names []string
			for _, pin := range got.File {
				names = append(names, pin.Name)
			}
			if !slices.Equal(names, tt.wantFile) {
				t.Errorf("File names %v, want %v", names, tt.wantFile)
			}
			var keys []string
			for _, it := range got.Items {
				keys = append(keys, it.Key)
			}
			if !slices.Equal(keys, tt.wantItems) {
				t.Errorf("item keys %v, want %v", keys, tt.wantItems)
			}
		})
	}
}

// TestPlanImagePinsFailedReadRaisesNothingYet pins both halves of a failed
// read's decision: the pin files nothing and raises no item. The
// image-pins-unreadable:<name> item arrives with the stale item (gt-3ycfvf),
// and that landing rewrites this test to expect it.
func TestPlanImagePinsFailedReadRaisesNothingYet(t *testing.T) {
	t.Parallel()
	// Behind is over the file threshold so a planner that ignored Err would
	// file this pin.
	pin := ImagePinState{Name: "om", Pin: "pin-om", Behind: 34, Err: errors.New("ls-remote exited 128")}

	got := PlanImagePins([]ImagePinState{pin}, nil, time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC))

	if len(got.File) != 0 {
		t.Errorf("File = %+v, want no pin filed on a failed read", got.File)
	}
	if len(got.Items) != 0 {
		t.Errorf("Items = %+v, want none: the unreadable item is not raised yet", got.Items)
	}
}

// TestPlanImagePinsFileCarriesThePinState: the filer names each pin's commit
// and tip, so the decision hands over the whole state it decided on, not just
// the name.
func TestPlanImagePinsFileCarriesThePinState(t *testing.T) {
	t.Parallel()
	pin := ImagePinState{Name: "hm", Pin: "pin-hm", Tip: "tip-hm", Behind: 20}

	got := PlanImagePins([]ImagePinState{pin}, nil, time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC))

	if len(got.File) != 1 || got.File[0] != pin {
		t.Fatalf("File = %+v, want exactly the pin state %+v", got.File, pin)
	}
}

// TestPlanImagePinsStuckItemNamesTheBeadAndAge: the item is one the overseer
// reads without a lookup, so it carries the open bead's id and how long it has
// sat.
func TestPlanImagePinsStuckItemNamesTheBeadAndAge(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	open := &ImagePinRefreshBead{ID: "gt-refresh7", OpenedAt: at.Add(-4 * time.Hour)}

	got := PlanImagePins(nil, open, at)

	want := ImagePinItem{
		Key:      imagePinsRefreshStuckKey,
		Severity: attention.SeverityMedium,
		Summary:  "refresh bead gt-refresh7 open 4h: the image pins wait for it to land",
	}
	if len(got.Items) != 1 || got.Items[0] != want {
		t.Fatalf("Items = %+v, want exactly %+v", got.Items, want)
	}
}
