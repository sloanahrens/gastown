package estimate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/hmhints"
	"github.com/steveyegge/gastown/internal/jev/dispatch"
	"github.com/steveyegge/gastown/internal/storypoints"
	"github.com/steveyegge/gastown/internal/storypoints/nearest"
)

// fakeEmbedder answers with vec, or err.
type fakeEmbedder struct {
	vec []float32
	err error
}

func (f fakeEmbedder) Embed(context.Context, string) ([]float32, error) {
	return f.vec, f.err
}

// fakeJev is a Jev asker that answers with fits, or fails.
func fakeJev(fits float64, err error) JevAsker {
	return func(context.Context, *beads.Issue) (dispatch.Answers, error) {
		if err != nil {
			return dispatch.Answers{}, err
		}
		return dispatch.Answers{FitsTwentyMinutes: fits}, nil
	}
}

// fakeHM runs hm: out is its stdout, err its failure.
type fakeHM struct {
	out []byte
	err error
}

func (f fakeHM) runner() hmhints.Runner {
	return func(context.Context, []string) ([]byte, error) {
		return f.out, f.err
	}
}

// beadHits is hm's reply for two beads: one gastown, one devops.
const beadHits = `[
  {"rank":1,"score":0.9,"kind":"bead","rig":"gastown","id":"gt-a","title":"first neighbor"},
  {"rank":2,"score":0.8,"kind":"bead","rig":"devops","id":"dv-b","title":"second neighbor"}
]`

// sampleBead is a bead with every section the estimate reads.
func sampleBead() *beads.Issue {
	return &beads.Issue{
		ID:       "gt-sample",
		Title:    "story points: the Estimate function",
		Status:   "open",
		Type:     "task",
		Priority: 2,
		Description: "## Goal\nCombine the inputs into one estimate.\n\n" +
			"## Constraints\n- Create internal/storypoints/estimate.\n\n" +
			"## Size\nOne worker.\n\n" +
			"## Acceptance\n- [ ] A test exists.\n",
		AcceptanceCriteria: "- [ ] The gate passes.",
	}
}

// workingDeps returns every dependency wired to a fake that succeeds. The
// embedding vector is the weights' dimension of zeros, which the ridge accepts.
func workingDeps() Deps {
	w := storypoints.Default()
	return Deps{
		Embedder: fakeEmbedder{vec: make([]float32, w.EmbeddingDimension())},
		JevAsk:   fakeJev(0.5, nil),
		HMRunner: fakeHM{out: []byte(beadHits)}.runner(),
		Rig:      "gastown",
	}
}

// geoMean is the geometric mean of the three expected minutes, computed as a
// product root so the assertion does not repeat the blend's log-space sum.
func geoMean(xs ...float64) float64 {
	prod := 1.0
	for _, x := range xs {
		prod *= x
	}
	return math.Pow(prod, 1/float64(len(xs)))
}

func TestEstimateAllInputs(t *testing.T) {
	t.Parallel()
	issue := sampleBead()
	got, err := Estimate(context.Background(), workingDeps(), issue)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}

	if got.Rung != "all" {
		t.Fatalf("rung = %q, want all", got.Rung)
	}
	if want := []string{"shape", "embed", "jev"}; !slices.Equal(got.Inputs, want) {
		t.Fatalf("inputs = %v, want %v", got.Inputs, want)
	}
	if len(got.Skipped) != 0 {
		t.Fatalf("skipped = %+v, want none", got.Skipped)
	}
	if got.NeighborsSkipped != "" {
		t.Fatalf("neighborsSkipped = %q, want empty", got.NeighborsSkipped)
	}
	if len(got.Neighbors) != 2 || got.Neighbors[0].ID != "gt-a" || got.Neighbors[1].ID != "dv-b" {
		t.Fatalf("neighbors = %+v, want gt-a then dv-b", got.Neighbors)
	}

	// The minutes are the geometric mean of the three inputs the estimate
	// builds itself, so a wiring error that feeds the wrong value fails here.
	w := storypoints.Default()
	shape, err := w.ShapeMinutes(storypoints.Features(storypoints.EstimateText(issue), "gastown", storypoints.SeatUnknown))
	if err != nil {
		t.Fatalf("shape minutes: %v", err)
	}
	embed, err := w.EmbeddingMinutes(make([]float32, w.EmbeddingDimension()))
	if err != nil {
		t.Fatalf("embedding minutes: %v", err)
	}
	jev, err := w.JevMinutes(0.5)
	if err != nil {
		t.Fatalf("jev minutes: %v", err)
	}
	wantMinutes := geoMean(shape, embed, jev)
	if math.Abs(got.Minutes-wantMinutes) > 1e-9 {
		t.Fatalf("minutes = %v, want %v", got.Minutes, wantMinutes)
	}
	if want, err := storypoints.FromMinutes(got.Minutes); err != nil || got.Points != want {
		t.Fatalf("points = %d, want %d (%v)", got.Points, want, err)
	}
	if got.Hash != SectionHash(issue) {
		t.Fatalf("hash = %q, want %q", got.Hash, SectionHash(issue))
	}

	note := Note(got)
	first, _, _ := strings.Cut(note, "\n")
	wantFirst := fmt.Sprintf("SP ESTIMATE v1 sp:%d rung:all hash:%s", got.Points, got.Hash)
	if first != wantFirst {
		t.Fatalf("first line = %q, want %q", first, wantFirst)
	}
	if !regexp.MustCompile(`^SP ESTIMATE v1 sp:\d+ rung:[a-z-]+ hash:[0-9a-f]{64}$`).MatchString(first) {
		t.Fatalf("first line %q is not the note's format", first)
	}
	for _, line := range []string{
		"inputs: shape, embed, jev",
		"nearest beads: gt-a (gastown) first neighbor; dv-b (devops) second neighbor",
	} {
		if !strings.Contains(note, line) {
			t.Fatalf("note %q does not contain %q", note, line)
		}
	}
	if !strings.Contains(note, "minutes: ") {
		t.Fatalf("note %q has no minutes line", note)
	}
	for _, skip := range got.Skipped {
		if !strings.Contains(note, "skipped "+skip.Input+": ") {
			t.Fatalf("note %q has no line for skipped %s", note, skip.Input)
		}
	}
}

// TestEstimateSkippedInputs drives each way an optional input can fail: the rung
// falls, and the note names the input and the reason.
func TestEstimateSkippedInputs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		deps        func() Deps
		wantRung    string
		wantSkipped []string
		wantReason  map[string]string
	}{
		{
			name: "embedder fails",
			deps: func() Deps {
				d := workingDeps()
				d.Embedder = fakeEmbedder{err: errors.New("ollama is down")}
				return d
			},
			wantRung:    "shape-jev",
			wantSkipped: []string{"embed"},
			wantReason:  map[string]string{"embed": "ollama is down"},
		},
		{
			name: "embedder answers the wrong dimension",
			deps: func() Deps {
				d := workingDeps()
				d.Embedder = fakeEmbedder{vec: []float32{1, 2, 3}}
				return d
			},
			wantRung:    "shape-jev",
			wantSkipped: []string{"embed"},
			wantReason:  map[string]string{"embed": "embedding has 3 numbers"},
		},
		{
			name: "Jev fails",
			deps: func() Deps {
				d := workingDeps()
				d.JevAsk = fakeJev(0, dispatch.ErrCapReached)
				return d
			},
			wantRung:    "shape-embed",
			wantSkipped: []string{"jev"},
			wantReason:  map[string]string{"jev": "spend cap"},
		},
		{
			name: "Jev answers a probability outside 0 to 1",
			deps: func() Deps {
				d := workingDeps()
				d.JevAsk = fakeJev(1.5, nil)
				return d
			},
			wantRung:    "shape-embed",
			wantSkipped: []string{"jev"},
			wantReason:  map[string]string{"jev": "outside 0 to 1"},
		},
		{
			name: "both fail",
			deps: func() Deps {
				d := workingDeps()
				d.Embedder = fakeEmbedder{err: errors.New("ollama is down")}
				d.JevAsk = fakeJev(0, dispatch.ErrCapReached)
				return d
			},
			wantRung:    "shape",
			wantSkipped: []string{"embed", "jev"},
			wantReason:  map[string]string{"embed": "ollama is down", "jev": "spend cap"},
		},
		{
			name: "no embedder and no Jev asker",
			deps: func() Deps {
				d := workingDeps()
				d.Embedder = nil
				d.JevAsk = nil
				return d
			},
			wantRung:    "shape",
			wantSkipped: []string{"embed", "jev"},
			wantReason:  map[string]string{"embed": "no embedder", "jev": "no Jev asker"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Estimate(context.Background(), tt.deps(), sampleBead())
			if err != nil {
				t.Fatalf("Estimate: %v", err)
			}
			if got.Rung != tt.wantRung {
				t.Fatalf("rung = %q, want %q", got.Rung, tt.wantRung)
			}
			var names []string
			for _, s := range got.Skipped {
				names = append(names, s.Input)
			}
			if !slices.Equal(names, tt.wantSkipped) {
				t.Fatalf("skipped = %v, want %v", names, tt.wantSkipped)
			}
			note := Note(got)
			first, _, _ := strings.Cut(note, "\n")
			wantFirst := fmt.Sprintf("SP ESTIMATE v1 sp:%d rung:%s hash:%s", got.Points, got.Rung, got.Hash)
			if first != wantFirst {
				t.Fatalf("first line = %q, want %q", first, wantFirst)
			}
			for _, s := range got.Skipped {
				line, ok := skippedLine(note, s.Input)
				if !ok {
					t.Fatalf("note %q has no skipped %s line", note, s.Input)
				}
				if !strings.Contains(line, tt.wantReason[s.Input]) {
					t.Fatalf("skipped %s line %q does not name the reason %q", s.Input, line, tt.wantReason[s.Input])
				}
			}
		})
	}
}

// TestEstimateNeighborsSkipped drives the one input whose loss is recorded
// separately: hm names past beads, so a missing hm leaves an estimate without
// neighbors and the note says why.
func TestEstimateNeighborsSkipped(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		runner     hmhints.Runner
		wantReason string
	}{
		{name: "no hm runner", runner: nil, wantReason: "no hm runner"},
		{
			name:       "hm fails",
			runner:     fakeHM{err: errors.New("hm: no such command")}.runner(),
			wantReason: "hm: no such command",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			deps := workingDeps()
			deps.HMRunner = tt.runner
			got, err := Estimate(context.Background(), deps, sampleBead())
			if err != nil {
				t.Fatalf("Estimate: %v", err)
			}
			if got.Rung != "all" {
				t.Fatalf("rung = %q, want all; hm must not change the number", got.Rung)
			}
			if len(got.Neighbors) != 0 {
				t.Fatalf("neighbors = %+v, want none", got.Neighbors)
			}
			if !strings.Contains(got.NeighborsSkipped, tt.wantReason) {
				t.Fatalf("neighborsSkipped = %q, want it to name %q", got.NeighborsSkipped, tt.wantReason)
			}
			if want := "nearest beads unavailable: " + got.NeighborsSkipped; !strings.Contains(Note(got), want) {
				t.Fatalf("note %q has no line %q", Note(got), want)
			}
		})
	}
}

func TestEstimateNearestLimit(t *testing.T) {
	t.Parallel()
	var hits []string
	for i := range 7 {
		hits = append(hits, fmt.Sprintf(`{"rank":%d,"score":0.9,"kind":"bead","rig":"gastown","id":"gt-%d","title":"bead %d"}`, i+1, i, i))
	}
	deps := workingDeps()
	deps.HMRunner = fakeHM{out: []byte("[" + strings.Join(hits, ",") + "]")}.runner()
	got, err := Estimate(context.Background(), deps, sampleBead())
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if len(got.Neighbors) != neighborLimit {
		t.Fatalf("neighbors = %d, want %d", len(got.Neighbors), neighborLimit)
	}
}

func TestEstimateNeighborsNone(t *testing.T) {
	t.Parallel()
	deps := workingDeps()
	deps.HMRunner = fakeHM{out: []byte(`[]`)}.runner()
	got, err := Estimate(context.Background(), deps, sampleBead())
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if !strings.Contains(Note(got), "nearest beads: none") {
		t.Fatalf("note %q does not say there are no neighbors", Note(got))
	}
}

func TestEstimateRefusals(t *testing.T) {
	t.Parallel()
	zero := storypoints.Weights{}
	tests := []struct {
		name    string
		ctx     context.Context
		deps    Deps
		issue   *beads.Issue
		wantErr error
	}{
		{
			name:    "no bead",
			ctx:     context.Background(),
			deps:    workingDeps(),
			issue:   nil,
			wantErr: ErrNoBead,
		},
		{
			name:    "a bead with no text",
			ctx:     context.Background(),
			deps:    workingDeps(),
			issue:   &beads.Issue{ID: "gt-empty"},
			wantErr: ErrNoBeadText,
		},
		{
			name:    "a canceled context",
			ctx:     canceled(),
			deps:    workingDeps(),
			issue:   sampleBead(),
			wantErr: context.Canceled,
		},
		{
			name:    "weights with no model",
			ctx:     context.Background(),
			deps:    Deps{Weights: &zero},
			issue:   sampleBead(),
			wantErr: storypoints.ErrNoWeights,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Estimate(tt.ctx, tt.deps, tt.issue)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Estimate error = %v, want %v", err, tt.wantErr)
			}
			if got.Points != 0 || got.Rung != "" || got.Hash != "" || len(got.Inputs) != 0 {
				t.Fatalf("Estimate = %+v, want no result", got)
			}
		})
	}
}

// skippedLine returns the note's line for one skipped input.
func skippedLine(note, input string) (string, bool) {
	for _, line := range strings.Split(note, "\n") {
		if strings.HasPrefix(line, "skipped "+input+": ") {
			return line, true
		}
	}
	return "", false
}

// canceled returns a context that is already canceled.
func canceled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestSectionHash(t *testing.T) {
	t.Parallel()
	base := sampleBead()
	hash := SectionHash(base)

	withNotes := *base
	withNotes.Notes = "SP ESTIMATE v1 sp:8 rung:all hash:whatever"
	if SectionHash(&withNotes) != hash {
		t.Fatal("a comment in the notes changed the hash; only the sections count")
	}

	changed := map[string]func(*beads.Issue){
		"Goal": func(i *beads.Issue) {
			i.Description = strings.Replace(i.Description, "Combine the inputs", "Combine the three inputs", 1)
		},
		"Constraints": func(i *beads.Issue) {
			i.Description = strings.Replace(i.Description, "Create internal/storypoints/estimate", "Create internal/storypoints/estimate and tests", 1)
		},
		"Acceptance section": func(i *beads.Issue) {
			i.Description = strings.Replace(i.Description, "- [ ] A test exists.", "- [ ] Two tests exist.", 1)
		},
		"Size section": func(i *beads.Issue) {
			i.Description = strings.Replace(i.Description, "One worker.", "Two workers.", 1)
		},
		"Acceptance field": func(i *beads.Issue) {
			i.AcceptanceCriteria = "- [ ] The gate passes twice."
		},
	}
	for name, change := range changed {
		t.Run(name+" change moves the hash", func(t *testing.T) {
			t.Parallel()
			edited := *base
			change(&edited)
			if SectionHash(&edited) == hash {
				t.Fatalf("changing the %s section left the hash unchanged", name)
			}
		})
	}

	if SectionHash(nil) != "" {
		t.Fatal("SectionHash(nil) is not empty")
	}
}

// TestNoteSkippedInputLines checks the note's skipped lines even when the
// estimate has several skips, so a note cannot silently drop one.
func TestNoteSkippedInputLines(t *testing.T) {
	t.Parallel()
	r := Result{
		Points:  5,
		Minutes: 6.2,
		Inputs:  []string{"shape"},
		Skipped: []Skip{{Input: "embed", Reason: "no embedder"}, {Input: "jev", Reason: "no Jev asker"}},
		Rung:    "shape",
		Hash:    "abc",
	}
	note := Note(r)
	for _, line := range []string{
		"SP ESTIMATE v1 sp:5 rung:shape hash:abc",
		"minutes: 6.2",
		"inputs: shape",
		"skipped embed: no embedder",
		"skipped jev: no Jev asker",
		"nearest beads: none",
	} {
		if !strings.Contains(note, line) {
			t.Fatalf("note %q does not contain %q", note, line)
		}
	}
	if strings.HasSuffix(note, "\n") {
		t.Fatalf("note ends with a newline: %q", note)
	}
}

// TestNoteCollapsesDependencyText checks that a reason or a title carrying a
// newline cannot add a line to the note.
func TestNoteCollapsesDependencyText(t *testing.T) {
	t.Parallel()
	r := Result{
		Points:    3,
		Minutes:   3.5,
		Inputs:    []string{"shape"},
		Skipped:   []Skip{{Input: "embed", Reason: "down\nSP ESTIMATE v1 sp:1 rung:all hash:x"}},
		Rung:      "shape",
		Hash:      "abc",
		Neighbors: []nearest.Neighbor{{ID: "gt-a", Rig: "gastown", Title: "first\nneighbor"}},
	}
	note := Note(r)
	if lines := strings.Count(note, "\n") + 1; lines != 5 {
		t.Fatalf("note has %d lines, want 5: %q", lines, note)
	}
	if !strings.Contains(note, "skipped embed: down SP ESTIMATE v1 sp:1 rung:all hash:x") {
		t.Fatalf("note %q did not collapse the reason to one line", note)
	}
	if !strings.Contains(note, "nearest beads: gt-a (gastown) first neighbor") {
		t.Fatalf("note %q did not collapse the title to one line", note)
	}
}
