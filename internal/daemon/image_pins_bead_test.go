package daemon

import (
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The pins the builder tests name, with a distinct tip per pin so a description
// that pairs one pin's commit with another pin's tip fails the check.
var (
	pinBD     = strings.Repeat("a", 40)
	pinOM     = strings.Repeat("b", 40)
	pinHM     = strings.Repeat("c", 40)
	pinDevops = strings.Repeat("d", 40)
	tipBD     = strings.Repeat("1", 40)
	tipOM     = strings.Repeat("2", 40)
	tipHM     = strings.Repeat("3", 40)
	tipDevops = strings.Repeat("4", 40)
)

// refreshPin is one pin as the reader hands it to the planner.
func refreshPin(name, pin, tip string, behind int) ImagePinState {
	return ImagePinState{Name: name, Pin: pin, Tip: tip, Behind: behind}
}

// TestImagePinsRefreshBeadNamesEachPin: the bead names every pin the planner
// named with the commit it is pinned at and its source tip, in one bullet per
// pin, and carries the label the open-bead lookup reads.
func TestImagePinsRefreshBeadNamesEachPin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		pins []ImagePinState
	}{
		{"one pin", []ImagePinState{refreshPin("bd", pinBD, tipBD, 22)}},
		{"two pins", []ImagePinState{
			refreshPin("bd", pinBD, tipBD, 22),
			refreshPin("devops", pinDevops, tipDevops, 47),
		}},
		{"all four pins", []ImagePinState{
			refreshPin("bd", pinBD, tipBD, 22),
			refreshPin("om", pinOM, tipOM, 21),
			refreshPin("hm", pinHM, tipHM, 25),
			refreshPin("devops", pinDevops, tipDevops, 47),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			draft, err := imagePinsRefreshBead(tt.pins)
			if err != nil {
				t.Fatalf("imagePinsRefreshBead: %v", err)
			}
			for _, pin := range tt.pins {
				want := "- " + pin.Name + ": pinned at " + pin.Pin + "; its source's origin/main is " + pin.Tip
				if !strings.Contains(draft.Description, want) {
					t.Errorf("description does not carry %q:\n%s", want, draft.Description)
				}
				if count := fmt.Sprintf("%s by %d commits", pin.Name, pin.Behind); !strings.Contains(draft.Title, count) {
					t.Errorf("title %q does not say %q", draft.Title, count)
				}
			}
			if len(draft.Labels) != 1 || draft.Labels[0] != imagePinsRefreshLabel {
				t.Errorf("labels = %v, want [%s]", draft.Labels, imagePinsRefreshLabel)
			}
		})
	}
}

// TestImagePinsRefreshBeadPassesSpecLint: the built text is the shape the
// dispatcher reads, with the same boxes in the field and in the description and
// no refusal the dispatcher would hold the bead for.
func TestImagePinsRefreshBeadPassesSpecLint(t *testing.T) {
	t.Parallel()
	draft, err := imagePinsRefreshBead([]ImagePinState{
		refreshPin("bd", pinBD, tipBD, 22),
		refreshPin("devops", pinDevops, tipDevops, 47),
	})
	if err != nil {
		t.Fatalf("imagePinsRefreshBead: %v", err)
	}
	spec := specdispatch.Spec{
		Title:       draft.Title,
		Type:        "task",
		Status:      "open",
		Priority:    2,
		Labels:      draft.Labels,
		Description: draft.Description,
		Acceptance:  draft.Acceptance,
		Rig:         "gastown",
		TownRigs:    []string{"beads", "devops", "gastown", "hm", "om"},
		Target: &specdispatch.TargetTree{
			Branch: "main",
			Files: map[string]struct{}{
				"deploy/daemon/image-pins": {},
				"deploy/polecat/seed-pins": {},
			},
		},
	}
	got := specdispatch.Lint(spec, specdispatch.Template{Sections: specdispatch.DefaultSections, Source: "built-in"})
	if got.Route != specdispatch.RouteDispatch {
		t.Fatalf("lint route = %q (%s: %s), refusals = %v, want %q", got.Route, got.Field, got.Reason, got.Refusals, specdispatch.RouteDispatch)
	}
}

// TestImagePinsRefreshBeadCarriesTheRefreshCommand: the description tells the
// polecat the command, the two files it writes and the four checkouts it reads.
func TestImagePinsRefreshBeadCarriesTheRefreshCommand(t *testing.T) {
	t.Parallel()
	draft, err := imagePinsRefreshBead([]ImagePinState{refreshPin("devops", pinDevops, tipDevops, 47)})
	if err != nil {
		t.Fatalf("imagePinsRefreshBead: %v", err)
	}
	for _, want := range []string{
		"gt daemon image pins", "--apply", "--pins", "--seed-pins",
		"--bd-src", "--om-src", "--hm-src", "--devops-src",
		"deploy/daemon/image-pins", "deploy/polecat/seed-pins",
	} {
		if !strings.Contains(draft.Description, want) {
			t.Errorf("description does not carry %q:\n%s", want, draft.Description)
		}
	}
}

// TestImagePinsRefreshBeadRefusesAPinItCannotName: a bead is never built from a
// pin it cannot print, and a refusal returns no draft at all.
func TestImagePinsRefreshBeadRefusesAPinItCannotName(t *testing.T) {
	t.Parallel()
	good := refreshPin("bd", pinBD, tipBD, 22)
	tests := []struct {
		name string
		pins []ImagePinState
		want string
	}{
		{"no pin at all", nil, "named no pin"},
		{"a pin with no name", []ImagePinState{{Pin: pinBD, Tip: tipBD, Behind: 22}}, "has no name"},
		{"a pin with no commit", []ImagePinState{{Name: "bd", Tip: tipBD, Behind: 22}}, "has no pinned commit"},
		{"a pin with no tip", []ImagePinState{{Name: "bd", Pin: pinBD, Behind: 22}}, "has no source tip"},
		{"a good pin beside a blank one", []ImagePinState{good, {Name: "om", Pin: pinOM, Behind: 30}}, "has no source tip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			draft, err := imagePinsRefreshBead(tt.pins)
			if err == nil {
				t.Fatalf("imagePinsRefreshBead(%v) = %q, want an error", tt.pins, draft.Title)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to say %q", err, tt.want)
			}
			if draft.Title != "" || draft.Description != "" || draft.Acceptance != "" || draft.Labels != nil {
				t.Errorf("draft = %+v, want no draft beside the error", draft)
			}
		})
	}
}
