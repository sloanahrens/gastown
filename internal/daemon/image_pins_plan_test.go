package daemon

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/daemonimage"
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
			pins: []ImagePinState{state("bd", 14)},
		},
		{
			name:     "at the file threshold",
			pins:     []ImagePinState{state("bd", 15)},
			wantFile: []string{"bd"},
		},
		{
			name:     "nineteen behind files",
			pins:     []ImagePinState{state("bd", 19)},
			wantFile: []string{"bd"},
		},
		{
			name:      "exactly the pins at or over the threshold",
			pins:      []ImagePinState{state("bd", 14), state("om", 15), state("hm", 34), state("devops", 3)},
			wantFile:  []string{"om", "hm"},
			wantItems: []string{imagePinsStaleKey},
		},
		{
			name:      "one commit short of the stale bound",
			pins:      []ImagePinState{state("bd", 29)},
			wantFile:  []string{"bd"},
			wantItems: nil,
		},
		{
			name:      "at the stale bound",
			pins:      []ImagePinState{state("bd", 30)},
			wantFile:  []string{"bd"},
			wantItems: []string{imagePinsStaleKey},
		},
		{
			name:      "a failed read is never filed",
			pins:      []ImagePinState{state("bd", 34), failed},
			wantFile:  []string{"bd"},
			wantItems: []string{imagePinsStaleKey, imagePinsUnreadableKey},
		},
		{
			name:      "a failed read with nothing behind raises unreadable",
			pins:      []ImagePinState{{Name: "om", Pin: "pin-om", Behind: 0, Err: errors.New("ls-remote exited 128")}},
			wantItems: []string{imagePinsUnreadableKey},
		},
		{
			name: "an open bead blocks a second",
			pins: []ImagePinState{state("bd", 25)},
			open: &ImagePinRefreshBead{ID: "gt-refresh7", OpenedAt: at.Add(-time.Hour)},
		},
		{
			name:      "a stale pin under an open bead still raises stale",
			pins:      []ImagePinState{state("bd", 30)},
			open:      &ImagePinRefreshBead{ID: "gt-refresh7", OpenedAt: at.Add(-time.Hour)},
			wantItems: []string{imagePinsStaleKey},
		},
		{
			name:      "a stale pin under an open bead past the stuck bound",
			pins:      []ImagePinState{state("bd", 30)},
			open:      &ImagePinRefreshBead{ID: "gt-refresh7", OpenedAt: at.Add(-4 * time.Hour)},
			wantItems: []string{imagePinsStaleKey, imagePinsRefreshStuckKey},
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

// TestPlanImagePinsFilesAtTheFileThreshold: filing turns on imagePinsFileBehind,
// so a pin one commit short of it files nothing, and a pin at it or over it
// files.
func TestPlanImagePinsFilesAtTheFileThreshold(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		behind int
		want   bool
	}{
		{behind: imagePinsFileBehind - 1, want: false},
		{behind: imagePinsFileBehind, want: true},
		{behind: 19, want: true},
	}
	for _, tt := range tests {
		got := PlanImagePins([]ImagePinState{{Name: "bd", Behind: tt.behind}}, nil, at)
		if filed := len(got.File) == 1; filed != tt.want {
			t.Errorf("a pin %d behind: filed = %v, want %v (File %+v)", tt.behind, filed, tt.want, got.File)
		}
	}
}

// TestPlanImagePinsThresholdOrdering holds the three bounds apart: the planner
// files below the stale item, and both sit below the drift guard's bound, so a
// pin the guard would refuse a roll for has been filed for first. An open
// refresh bead still suppresses the file the threshold would make.
func TestPlanImagePinsThresholdOrdering(t *testing.T) {
	t.Parallel()
	if imagePinsFileBehind >= imagePinsStaleBehind {
		t.Errorf("file threshold %d is not below the stale bound %d", imagePinsFileBehind, imagePinsStaleBehind)
	}
	if imagePinsFileBehind >= daemonimage.PinRefreshBehind {
		t.Errorf("file threshold %d is not below the pins command's %d", imagePinsFileBehind, daemonimage.PinRefreshBehind)
	}
	if imagePinsStaleBehind >= daemonimage.MaxPinBehind {
		t.Errorf("stale bound %d is not below the drift guard's %d-commit bound", imagePinsStaleBehind, daemonimage.MaxPinBehind)
	}

	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	atThreshold := ImagePinState{Name: "bd", Pin: "pin-bd", Tip: "tip-bd", Behind: imagePinsFileBehind}
	open := &ImagePinRefreshBead{ID: "gt-refresh7", OpenedAt: at.Add(-time.Hour)}
	if got := PlanImagePins([]ImagePinState{atThreshold}, open, at); len(got.File) != 0 {
		t.Errorf("File = %+v under an open refresh bead, want none", got.File)
	}
}

// TestPlanImagePinsNamesAPinWhoseReadFailed: a failed read files nothing, and
// the plan names the pin, so it is not the output of an all-fresh tick.
func TestPlanImagePinsNamesAPinWhoseReadFailed(t *testing.T) {
	t.Parallel()
	// Behind is over the file threshold so a planner that ignored Err would
	// file this pin.
	failed := ImagePinState{Name: "om", Pin: "pin-om", Behind: 34, Err: errors.New("ls-remote exited 128")}
	fresh := ImagePinState{Name: "hm", Pin: "pin-hm", Behind: 0}
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

	got := PlanImagePins([]ImagePinState{failed, fresh}, nil, now)
	allFresh := PlanImagePins([]ImagePinState{fresh}, nil, now)

	if len(got.File) != 0 {
		t.Errorf("File = %+v, want no pin filed on a failed read", got.File)
	}
	if len(got.Unreadable) != 1 || got.Unreadable[0] != "om" {
		t.Errorf("Unreadable = %q, want [om]", got.Unreadable)
	}
	if len(allFresh.Unreadable) != 0 {
		t.Errorf("Unreadable = %q for a fresh pin, want none", allFresh.Unreadable)
	}
}

// TestPlanImagePinsOpenBeadWithoutAnAgeSaysItsAgeIsUnknown: a bead with no
// recorded open time raises the stuck item with a summary that names no
// duration, not one measured from the zero time.
func TestPlanImagePinsOpenBeadWithoutAnAgeSaysItsAgeIsUnknown(t *testing.T) {
	t.Parallel()
	open := &ImagePinRefreshBead{ID: "gt-refresh7"}

	got := PlanImagePins(nil, open, time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC))

	if len(got.Items) != 1 || got.Items[0].Key != imagePinsRefreshStuckKey {
		t.Fatalf("Items = %+v, want the one stuck item", got.Items)
	}
	if want := "refresh bead gt-refresh7 is open and its age is unknown: the image pins wait for it to land"; got.Items[0].Summary != want {
		t.Errorf("Summary = %q, want %q", got.Items[0].Summary, want)
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

// TestPlanImagePinsStaleAndUnreadableItemsNameTheirPin: each per-pin item names
// the pin it is about, so the patrol can raise and clear each one on its own.
func TestPlanImagePinsStaleAndUnreadableItemsNameTheirPin(t *testing.T) {
	t.Parallel()
	stale := ImagePinState{Name: "hm", Pin: "pin-hm", Behind: 30}
	failed := ImagePinState{Name: "om", Pin: "pin-om", Err: errors.New("ls-remote exited 128")}

	got := PlanImagePins([]ImagePinState{stale, failed}, nil, time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC))

	want := []ImagePinItem{
		{
			Key:      imagePinsStaleKey,
			Pin:      "hm",
			Severity: attention.SeverityMedium,
			Summary:  "image pin hm is 30 commits behind its source's origin/main, at or over the 30-commit stale bound",
		},
		{
			Key:      imagePinsUnreadableKey,
			Pin:      "om",
			Severity: attention.SeverityMedium,
			Summary:  "image pin om cannot be read: it is not counted as fresh, and no refresh bead is filed for it",
		},
	}
	if !slices.Equal(got.Items, want) {
		t.Fatalf("Items = %+v, want %+v", got.Items, want)
	}
}
