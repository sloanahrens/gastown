package specsplit

import (
	"strings"
	"testing"
)

// acceptanceText is a bead's acceptance section holding the boxes.
func acceptanceText(list ...Box) string {
	lines := make([]string, len(list))
	for i, box := range list {
		lines[i] = "- [ ] " + box.Text
	}
	return strings.Join(lines, "\n")
}

// twoPieceSpec is a two-package bead whose B bullet cites a file and a symbol A
// names, and whose A box carries a go vet guard.
func twoPieceSpec() Spec {
	return Spec{
		Constraints: strings.Join([]string{
			"- Add internal/config/env.go with `func Read() string`.",
			"- Create internal/specsplit/plan.go, which calls `Read()` with internal/config/env.go.",
		}, "\n"),
		Acceptance: strings.Join([]string{
			"- [ ] The setting is read. Run: go test ./internal/config -run TestEnv; go vet ./...",
			"- [ ] The plan is built. Run: go test ./internal/specsplit -run TestPlan",
		}, "\n"),
		Dirs:      testDirs,
		BaseFiles: map[string]bool{},
	}
}

// TestSplitPlanTwoPieces: a two-package bead becomes two pieces, A for the
// first group and B for the second, each with its own boxes and a Gate of its
// own tests plus the guard, B's Constraints citing A's file and symbol.
func TestSplitPlanTwoPieces(t *testing.T) {
	t.Parallel()
	spec := twoPieceSpec()
	groups, err := Group(spec)
	if err != nil {
		t.Fatalf("Group() error = %v, want none", err)
	}
	plan, err := Plan(spec, groups)
	if err != nil {
		t.Fatalf("Plan() error = %v, want none", err)
	}

	if plan.A.Name != "A" || plan.A.DependsOn != "" {
		t.Errorf("piece A = %q depending on %q, want A depending on nothing", plan.A.Name, plan.A.DependsOn)
	}
	if plan.B.Name != "B" || plan.B.DependsOn != "A" {
		t.Errorf("piece B = %q depending on %q, want B depending on A", plan.B.Name, plan.B.DependsOn)
	}
	if got, want := strings.Join(plan.A.Packages, ","), "internal/config"; got != want {
		t.Errorf("piece A packages = %q, want %q", got, want)
	}
	if got, want := strings.Join(plan.B.Packages, ","), "internal/specsplit"; got != want {
		t.Errorf("piece B packages = %q, want %q", got, want)
	}

	wantABoxes := []Box{{
		Text: "The setting is read. Run: go test ./internal/config -run TestEnv; go vet ./...",
		Run:  "go test ./internal/config -run TestEnv; go vet ./...",
	}}
	wantBBoxes := []Box{{
		Text: "The plan is built. Run: go test ./internal/specsplit -run TestPlan",
		Run:  "go test ./internal/specsplit -run TestPlan",
	}}
	if len(plan.A.Boxes) != 1 || plan.A.Boxes[0] != wantABoxes[0] {
		t.Errorf("piece A boxes = %+v, want %+v", plan.A.Boxes, wantABoxes)
	}
	if len(plan.B.Boxes) != 1 || plan.B.Boxes[0] != wantBBoxes[0] {
		t.Errorf("piece B boxes = %+v, want %+v", plan.B.Boxes, wantBBoxes)
	}

	if want := "## Gate\n\ngo test ./internal/config -run TestEnv && go vet ./...\n\n"; !strings.Contains(plan.A.Text, want) {
		t.Errorf("piece A text missing gate %q:\n%s", want, plan.A.Text)
	}
	if want := "## Gate\n\ngo test ./internal/specsplit -run TestPlan && go vet ./...\n\n"; !strings.Contains(plan.B.Text, want) {
		t.Errorf("piece B text missing gate %q:\n%s", want, plan.B.Text)
	}
	if strings.Contains(plan.A.Text, "TestPlan") {
		t.Errorf("piece A text names piece B's test:\n%s", plan.A.Text)
	}
	if strings.Contains(plan.B.Text, "TestEnv") {
		t.Errorf("piece B text names piece A's test:\n%s", plan.B.Text)
	}
	for _, want := range []string{
		"Deliver piece A: the internal/config file group of the split bead",
		"Piece B (internal/specsplit) and anything outside piece A's file group.",
		"- [ ] The setting is read. Run: go test ./internal/config -run TestEnv; go vet ./...",
	} {
		if !strings.Contains(plan.A.Text, want) {
			t.Errorf("piece A text missing %q:\n%s", want, plan.A.Text)
		}
	}
	for _, want := range []string{
		"Deliver piece B: the internal/specsplit file group of the split bead",
		"Piece A (internal/config) and anything outside piece B's file group.",
		"- internal/config/env.go is created by bead PIECE_A.",
		"- `Read` is created by bead PIECE_A.",
		"- [ ] The plan is built. Run: go test ./internal/specsplit -run TestPlan",
	} {
		if !strings.Contains(plan.B.Text, want) {
			t.Errorf("piece B text missing %q:\n%s", want, plan.B.Text)
		}
	}
}

// TestSplitPlanKeepsBoxes: the two piece texts each carry their package's boxes
// and together hold every original box exactly once.
func TestSplitPlanKeepsBoxes(t *testing.T) {
	t.Parallel()
	spec := Spec{
		Constraints: strings.Join([]string{
			"- Edit internal/config/env.go.",
			"- Edit internal/config/load.go.",
			"- Edit internal/specsplit/group.go.",
			"- Edit internal/specsplit/plan.go.",
		}, "\n"),
		Acceptance: strings.Join([]string{
			"- [ ] The environment is read. Run: go test ./internal/config -run TestEnv",
			"- [ ] The settings load. Run: go test ./internal/config -run TestLoad",
			"- [ ] The grouping holds. Run: go test ./internal/specsplit -run TestGroup",
			"- [ ] The plan holds. Run: go test ./internal/specsplit -run TestPlan",
		}, "\n"),
		Dirs:      testDirs,
		BaseFiles: map[string]bool{},
	}
	groups, err := Group(spec)
	if err != nil {
		t.Fatalf("Group() error = %v, want none", err)
	}
	plan, err := Plan(spec, groups)
	if err != nil {
		t.Fatalf("Plan() error = %v, want none", err)
	}
	original := boxes(spec.Acceptance)
	if got := len(plan.A.Boxes) + len(plan.B.Boxes); got != len(original) {
		t.Fatalf("pieces hold %d boxes, want the original %d", got, len(original))
	}
	for _, box := range original {
		if got := strings.Count(plan.A.Text, box.Text) + strings.Count(plan.B.Text, box.Text); got != 1 {
			t.Errorf("box %q appears %d times across the pieces, want exactly once", box.Text, got)
		}
	}
}

// TestSplitPlanFirstHalfRoundedUp: with three groups, A takes two and B takes
// one.
func TestSplitPlanFirstHalfRoundedUp(t *testing.T) {
	t.Parallel()
	original := []Box{
		{Text: "The environment is read. Run: go test ./internal/config -run TestEnv", Run: "go test ./internal/config -run TestEnv"},
		{Text: "The plan holds. Run: go test ./internal/specsplit -run TestPlan", Run: "go test ./internal/specsplit -run TestPlan"},
		{Text: "The check holds. Run: go test ./internal/hygiene -run TestCheck", Run: "go test ./internal/hygiene -run TestCheck"},
	}
	spec := Spec{Acceptance: acceptanceText(original...), Dirs: testDirs, BaseFiles: map[string]bool{}}
	groups := []FileGroup{
		{Package: "internal/config", Boxes: original[:1]},
		{Package: "internal/specsplit", Boxes: original[1:2]},
		{Package: "internal/hygiene", Boxes: original[2:]},
	}
	plan, err := Plan(spec, groups)
	if err != nil {
		t.Fatalf("Plan() error = %v, want none", err)
	}
	if got, want := strings.Join(plan.A.Packages, ","), "internal/config,internal/specsplit"; got != want {
		t.Errorf("piece A packages = %q, want %q", got, want)
	}
	if got, want := strings.Join(plan.B.Packages, ","), "internal/hygiene"; got != want {
		t.Errorf("piece B packages = %q, want %q", got, want)
	}
}

// TestSplitPlanRefuses: each group set the pieces cannot carry is an error
// naming the reason.
func TestSplitPlanRefuses(t *testing.T) {
	t.Parallel()
	boxA := Box{Text: "The setting is read. Run: go test ./internal/config -run TestEnv", Run: "go test ./internal/config -run TestEnv"}
	boxB := Box{Text: "The plan is built. Run: go test ./internal/specsplit -run TestPlan", Run: "go test ./internal/specsplit -run TestPlan"}
	spec := Spec{Acceptance: acceptanceText(boxA, boxB), Dirs: testDirs, BaseFiles: map[string]bool{}}
	tests := []struct {
		name    string
		spec    Spec
		groups  []FileGroup
		wantErr string
	}{
		{
			name:    "one group",
			spec:    spec,
			groups:  []FileGroup{{Package: "internal/config", Boxes: []Box{boxA}}},
			wantErr: "at least two file groups",
		},
		{
			name: "a group with no package",
			spec: spec,
			groups: []FileGroup{
				{Package: "", Boxes: []Box{boxA}},
				{Package: "internal/specsplit", Boxes: []Box{boxB}},
			},
			wantErr: "names no package",
		},
		{
			name: "a group with no boxes",
			spec: spec,
			groups: []FileGroup{
				{Package: "internal/config", Boxes: []Box{boxA}},
				{Package: "internal/specsplit"},
			},
			wantErr: "has no acceptance boxes",
		},
		{
			name: "a box with no command",
			spec: Spec{Acceptance: acceptanceText(boxA, Box{Text: "The plan is built."}), Dirs: testDirs, BaseFiles: map[string]bool{}},
			groups: []FileGroup{
				{Package: "internal/config", Boxes: []Box{boxA}},
				{Package: "internal/specsplit", Boxes: []Box{{Text: "The plan is built."}}},
			},
			wantErr: "with no Run command",
		},
		{
			name: "a box whose Run is not a go test",
			spec: Spec{Acceptance: acceptanceText(boxA, Box{
				Text: "The plan is built. Run: make gate",
				Run:  "make gate",
			}), Dirs: testDirs, BaseFiles: map[string]bool{}},
			groups: []FileGroup{
				{Package: "internal/config", Boxes: []Box{boxA}},
				{Package: "internal/specsplit", Boxes: []Box{{Text: "The plan is built. Run: make gate", Run: "make gate"}}},
			},
			wantErr: "names no go test command",
		},
		{
			name: "a group box the spec does not hold",
			spec: Spec{Acceptance: acceptanceText(boxA), Dirs: testDirs, BaseFiles: map[string]bool{}},
			groups: []FileGroup{
				{Package: "internal/config", Boxes: []Box{boxA}},
				{Package: "internal/specsplit", Boxes: []Box{boxB}},
			},
			wantErr: "which the spec does not",
		},
		{
			name: "a spec box the groups do not hold",
			spec: Spec{Acceptance: acceptanceText(boxA, boxB, Box{
				Text: "The check holds. Run: go test ./internal/hygiene -run TestCheck",
				Run:  "go test ./internal/hygiene -run TestCheck",
			}), Dirs: testDirs, BaseFiles: map[string]bool{}},
			groups: []FileGroup{
				{Package: "internal/config", Boxes: []Box{boxA}},
				{Package: "internal/specsplit", Boxes: []Box{boxB}},
			},
			wantErr: "do not hold the spec box",
		},
		{
			name:    "a box held twice",
			spec:    spec,
			groups:  []FileGroup{{Package: "internal/config", Boxes: []Box{boxA}}, {Package: "internal/specsplit", Boxes: []Box{boxA, boxB}}},
			wantErr: "more than one file group",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Plan(tc.spec, tc.groups); err == nil {
				t.Fatalf("Plan() error = nil, want one naming %q", tc.wantErr)
			} else if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Plan() error = %q, want it to name %q", err, tc.wantErr)
			}
		})
	}
}

// TestSplitPlanCreatedByNeedsACreation: B says a file "is created by bead
// PIECE_A" only when the file is absent from the base branch. A note on a file
// that already exists is the spec lint's trap, so an edited file gets none.
func TestSplitPlanCreatedByNeedsACreation(t *testing.T) {
	t.Parallel()
	const note = "internal/config/env.go is created by bead PIECE_A."
	tests := []struct {
		name      string
		baseFiles map[string]bool
		wantNote  bool
	}{
		{"a file absent from the base branch is created", map[string]bool{}, true},
		{"a file A edits that exists on the base branch is not created", map[string]bool{"internal/config/env.go": true}, false},
		{"another existing file does not hide the creation", map[string]bool{"internal/specsplit/specsplit.go": true}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := twoPieceSpec()
			spec.BaseFiles = tc.baseFiles
			groups, err := Group(spec)
			if err != nil {
				t.Fatalf("Group() error = %v, want none", err)
			}
			plan, err := Plan(spec, groups)
			if err != nil {
				t.Fatalf("Plan() error = %v, want none", err)
			}
			if got := strings.Contains(plan.B.Text, note); got != tc.wantNote {
				t.Errorf("piece B created-by note for internal/config/env.go = %v, want %v:\n%s", got, tc.wantNote, plan.B.Text)
			}
		})
	}
}

// symbolSpec is a two-package bead whose A bullets and B bullets are the ones a
// row passes; both boxes are as in twoPieceSpec.
func symbolSpec(aBullets, bBullets []string) Spec {
	spec := twoPieceSpec()
	spec.Constraints = strings.Join(append(append([]string{}, aBullets...), bBullets...), "\n")
	spec.BaseFiles = map[string]bool{}
	return spec
}

// TestSplitPlanSymbolCreatedByNeedsADeclaration: B says a symbol "is created by
// bead PIECE_A" only when A's bullets declare it in a Go declaration code span
// and B cites it in a code span. A symbol A only calls, and the same word in B's
// prose, cite nothing.
func TestSplitPlanSymbolCreatedByNeedsADeclaration(t *testing.T) {
	t.Parallel()
	const readNote = "`Read` is created by bead PIECE_A."
	const configNote = "`Config` is created by bead PIECE_A."
	declare := []string{"- Add internal/config/env.go with `func Read() string` and `type Config struct{}`."}
	tests := []struct {
		name     string
		a, b     []string
		want     []string
		wantNone []string
	}{
		{"a declared function cited in a code span of B",
			declare,
			[]string{"- Create internal/specsplit/plan.go, which calls `Read()`."},
			[]string{readNote}, []string{configNote}},
		{"a declared type cited in a code span of B",
			declare,
			[]string{"- Create internal/specsplit/plan.go with a field of type `Config`."},
			[]string{configNote}, []string{readNote}},
		{"a symbol A only calls is not created",
			[]string{"- Edit internal/config/env.go: it calls `Read()`."},
			[]string{"- Create internal/specsplit/plan.go, which calls `Read()`."},
			nil, []string{readNote}},
		{"the word in B's prose cites nothing",
			append(append([]string{}, declare...), "- Callers in internal/config/env.go use `Read()`."),
			[]string{"- Create internal/specsplit/plan.go and Read the setting once."},
			nil, []string{readNote, configNote}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := symbolSpec(tc.a, tc.b)
			groups, err := Group(spec)
			if err != nil {
				t.Fatalf("Group() error = %v, want none", err)
			}
			plan, err := Plan(spec, groups)
			if err != nil {
				t.Fatalf("Plan() error = %v, want none", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(plan.B.Text, want) {
					t.Errorf("piece B text lacks %q:\n%s", want, plan.B.Text)
				}
			}
			for _, none := range tc.wantNone {
				if strings.Contains(plan.B.Text, none) {
					t.Errorf("piece B text holds %q, which nothing here creates:\n%s", none, plan.B.Text)
				}
			}
		})
	}
}

// TestSplitPlanRequiresBaseFiles: a nil BaseFiles is an error, since a caller
// that forgot it would get the created-by trap with no signal; an empty non-nil
// map is the legitimate "nothing exists" input.
func TestSplitPlanRequiresBaseFiles(t *testing.T) {
	t.Parallel()
	spec := twoPieceSpec()
	groups, err := Group(spec)
	if err != nil {
		t.Fatalf("Group() error = %v, want none", err)
	}
	spec.BaseFiles = nil
	if _, err := Plan(spec, groups); err == nil || !strings.Contains(err.Error(), "BaseFiles not supplied") {
		t.Errorf("Plan() with a nil BaseFiles error = %v, want one naming %q", err, "BaseFiles not supplied")
	}
	spec.BaseFiles = map[string]bool{}
	if _, err := Plan(spec, groups); err != nil {
		t.Errorf("Plan() with an empty BaseFiles error = %v, want none", err)
	}
}
