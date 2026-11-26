package specsplit

import (
	"reflect"
	"strings"
	"testing"
)

// specWith is a spec over testDirs with the given Constraints text and boxes.
func specWith(constraints string, list ...Box) Spec {
	return Spec{Constraints: constraints, Acceptance: acceptanceText(list...), Dirs: testDirs}
}

// threeBoxSpec is a one-package bead with three acceptance boxes.
func threeBoxSpec() (Spec, []Box) {
	list := []Box{
		{Text: "The setting is read. Run: go test ./internal/config -run TestEnv", Run: "go test ./internal/config -run TestEnv"},
		{Text: "The default is kept. Run: go test ./internal/config -run TestDefault", Run: "go test ./internal/config -run TestDefault"},
		{Text: "The guard holds. Run: go test ./internal/config -run TestGuard", Run: "go test ./internal/config -run TestGuard"},
	}
	constraints := strings.Join([]string{
		"- Edit internal/config/env.go to read the new setting.",
		"- Leave internal/config/defaults.go as it is.",
	}, "\n")
	return specWith(constraints, list...), list
}

// TestGroupByBoxes: a spec whose bullets name one package or none splits into a
// first group holding every bullet and the first box and a second holding the
// remaining boxes and no bullets, both in that one package and passing
// checkGroups.
func TestGroupByBoxes(t *testing.T) {
	t.Parallel()
	onePackage, list := threeBoxSpec()
	tests := []struct {
		name            string
		spec            Spec
		wantPackage     string
		wantConstraints []string
	}{
		{
			name:            "bullets name one package",
			spec:            onePackage,
			wantPackage:     "internal/config",
			wantConstraints: []string{"Edit internal/config/env.go to read the new setting.", "Leave internal/config/defaults.go as it is."},
		},
		{
			name:            "bullets name no package",
			spec:            specWith("- Make the documentation better.", list...),
			wantPackage:     ".",
			wantConstraints: []string{"Make the documentation better."},
		},
		{
			name:        "no bullets",
			spec:        specWith("", list...),
			wantPackage: ".",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := GroupByBoxes(tc.spec)
			if err != nil {
				t.Fatalf("GroupByBoxes() error = %v, want none", err)
			}
			if err := checkGroups(tc.spec, got); err != nil {
				t.Fatalf("checkGroups() error = %v, want the groups to pass unchanged", err)
			}
			if len(got) != 2 {
				t.Fatalf("GroupByBoxes() returned %d groups, want 2", len(got))
			}
			if got[0].Package != tc.wantPackage || got[1].Package != tc.wantPackage {
				t.Errorf("GroupByBoxes() packages = %q, %q, want both %q", got[0].Package, got[1].Package, tc.wantPackage)
			}
			if !reflect.DeepEqual(got[0].Constraints, tc.wantConstraints) {
				t.Errorf("group 1 Constraints = %q, want %q", got[0].Constraints, tc.wantConstraints)
			}
			if !reflect.DeepEqual(got[0].Boxes, list[:1]) {
				t.Errorf("group 1 Boxes = %+v, want only the first box %+v", got[0].Boxes, list[:1])
			}
			if len(got[1].Constraints) != 0 {
				t.Errorf("group 2 Constraints = %q, want none", got[1].Constraints)
			}
			if !reflect.DeepEqual(got[1].Boxes, list[1:]) {
				t.Errorf("group 2 Boxes = %+v, want the remaining boxes %+v", got[1].Boxes, list[1:])
			}
		})
	}
}

// TestGroupByBoxesRefuses: a spec without two boxes, a box without a go test
// Run command, or bullets naming more than one package (or the root package) is
// an error naming its cause.
func TestGroupByBoxesRefuses(t *testing.T) {
	t.Parallel()
	_, list := threeBoxSpec()
	rootDirs := append([]string{"."}, testDirs...)
	tests := []struct {
		name    string
		spec    Spec
		wantErr string
	}{
		{
			name:    "one box",
			spec:    specWith("- Edit internal/config/env.go.", list[0]),
			wantErr: "needs at least two",
		},
		{
			name:    "a box with no Run command",
			spec:    specWith("- Edit internal/config/env.go.", list[0], Box{Text: "A test shows the setting."}),
			wantErr: "no Run command",
		},
		{
			name: "a box whose Run names no go test command",
			spec: specWith("- Edit internal/config/env.go.", list[0],
				Box{Text: "A test shows the setting. Run: make gate"}),
			wantErr: "names no go test command",
		},
		{
			name: "bullets naming two packages",
			spec: specWith("- Edit internal/config/env.go.\n- Edit internal/specsplit/specsplit.go.",
				list[0], list[1]),
			wantErr: "is for Group",
		},
		{
			name:    "a bullet naming the root package",
			spec:    Spec{Constraints: "- Update the root package .", Acceptance: acceptanceText(list[0], list[1]), Dirs: rootDirs},
			wantErr: "names the root package .",
		},
		{
			name:    "a spec that names no directories",
			spec:    Spec{Constraints: "- Edit internal/config/env.go.", Acceptance: acceptanceText(list[0], list[1])},
			wantErr: "names no package directories",
		},
		{
			name:    "the same box twice",
			spec:    specWith("- Edit internal/config/env.go.", list[0], list[0]),
			wantErr: "more than one file group",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := GroupByBoxes(tc.spec)
			if err == nil {
				t.Fatalf("GroupByBoxes() = %+v, nil; want an error naming %q", got, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("GroupByBoxes() error = %q, want it to name %q", err, tc.wantErr)
			}
			if got != nil {
				t.Errorf("GroupByBoxes() groups = %+v, want none beside the error", got)
			}
		})
	}
}
