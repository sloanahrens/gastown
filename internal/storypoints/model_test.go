package storypoints

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"testing"
)

const initialWeightsPath = "../../docs/plans/2026-11-11-story-points-initial-weights.json"

// trainingMeanIsHaiku is the mean of is_haiku in the checked-in weights file.
const trainingMeanIsHaiku = 0.389538

func TestDefaultLoadsEmbeddedWeights(t *testing.T) {
	t.Parallel()
	w := Default()
	if got, ok := w.ShapeMean("is_haiku"); !ok || got != trainingMeanIsHaiku {
		t.Errorf("ShapeMean(is_haiku) = %v, %v; want %v, true", got, ok, trainingMeanIsHaiku)
	}
	if _, ok := w.ShapeMean("not_a_feature"); ok {
		t.Errorf("ShapeMean(not_a_feature) reports ok; want false")
	}
}

func TestEmbeddedWeightsAreTheInitialFile(t *testing.T) {
	t.Parallel()
	want, err := os.ReadFile(initialWeightsPath)
	if err != nil {
		t.Fatalf("read initial weights: %v", err)
	}
	if !bytes.Equal(embeddedWeights, want) {
		t.Errorf("embedded weights.json differs from %s: %d bytes, want %d identical bytes",
			initialWeightsPath, len(embeddedWeights), len(want))
	}
}

func TestShapeMinutesMatchFixtures(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(shapeFixturesPath)
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var fixtures []struct {
		Bead             string  `json:"bead"`
		Rig              string  `json:"rig"`
		SeatClass        string  `json:"seat_class"`
		Text             string  `json:"text"`
		PredictedMinutes float64 `json:"predicted_minutes"`
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("parse fixtures: %v", err)
	}
	if len(fixtures) != 6 {
		t.Fatalf("fixtures = %d beads, want 6", len(fixtures))
	}
	w := Default()
	for _, fx := range fixtures {
		t.Run(fx.Bead, func(t *testing.T) {
			t.Parallel()
			seat := SeatFlash
			if fx.SeatClass == "haiku" {
				seat = SeatHaiku
			}
			got := w.ShapeMinutes(Features(fx.Text, fx.Rig, seat))
			if math.Abs(got-fx.PredictedMinutes) > 0.01 {
				t.Errorf("ShapeMinutes = %v, want %v within 0.01", got, fx.PredictedMinutes)
			}
		})
	}
}

func TestShapeMinutesIsExpOfLinearPredictor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		shape Shape
		mut   func(f *weightsFile)
		want  float64
	}{
		{
			name:  "intercept alone",
			shape: Shape{Boxes: 3},
			mut: func(f *weightsFile) {
				clear(f.Shape.Coef)
				f.Shape.Intercept = math.Log(7)
			},
			want: 7,
		},
		{
			name:  "one term: coef * (x - mean) / scale",
			shape: Shape{Boxes: 3},
			mut: func(f *weightsFile) {
				clear(f.Shape.Coef)
				f.Shape.Intercept = 0
				f.Shape.Coef[0] = 0.5
				f.Shape.Mean[0] = 1
				f.Shape.Scale[0] = 2
			},
			want: math.Exp(0.5),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := loadWeightsFrom(t, weightsWith(t, tc.mut))
			if got := w.ShapeMinutes(tc.shape); math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("ShapeMinutes = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoadWeightsAcceptsTheEmbeddedFile(t *testing.T) {
	t.Parallel()
	if _, err := LoadWeights(bytes.NewReader(embeddedWeights)); err != nil {
		t.Errorf("LoadWeights(embedded) = %v, want nil", err)
	}
	// A re-marshalled copy of the typed file must load too, so the rejection
	// rows below fail for their one change and not for the round trip.
	if _, err := LoadWeights(bytes.NewReader(weightsWith(t, func(*weightsFile) {}))); err != nil {
		t.Errorf("LoadWeights(round trip) = %v, want nil", err)
	}
}

func TestLoadWeightsRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data func(t *testing.T) []byte
	}{
		{"feature name changed", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Shape.Features[0] = "box" })
		}},
		{"feature dropped", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Shape.Features = f.Shape.Features[:11] })
		}},
		{"coefficient list too short", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Shape.Coef = f.Shape.Coef[:11] })
		}},
		{"mean list too long", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Shape.Mean = append(f.Shape.Mean, 0) })
		}},
		{"zero scale", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Shape.Scale[3] = 0 })
		}},
		{"negative scale", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Shape.Scale[3] = -1 })
		}},
		{"NaN in the file", func(t *testing.T) []byte {
			return replaceOnce(t, `"intercept": 2.16992`, `"intercept": NaN`)
		}},
		{"number out of float64 range", func(t *testing.T) []byte {
			return replaceOnce(t, `"intercept": 2.16992`, `"intercept": 1e400`)
		}},
		{"knot table unsorted", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) {
				last := len(f.Knots) - 1
				f.Knots[0], f.Knots[last] = f.Knots[last], f.Knots[0]
			})
		}},
		{"knot probability above 1", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Knots[0][0] = 1.5 })
		}},
		{"knot minutes zero", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Knots[1][1] = 0 })
		}},
		{"knot row without two numbers", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Knots[2] = []float64{0.1} })
		}},
		{"knot table empty", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Knots = nil })
		}},
		{"embedding dim differs from its coefficients", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Embedding.Dim = 2559 })
		}},
		{"embedding model name empty", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Embedding.Model = "" })
		}},
		{"unknown version", func(t *testing.T) []byte {
			return weightsWith(t, func(f *weightsFile) { f.Version = 2 })
		}},
		{"data after the JSON object", func(t *testing.T) []byte {
			return append(bytes.Clone(embeddedWeights), "{}"...)
		}},
		{"not JSON", func(t *testing.T) []byte { return []byte("not json") }},
		{"empty input", func(t *testing.T) []byte { return nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := LoadWeights(bytes.NewReader(tc.data(t))); err == nil {
				t.Errorf("LoadWeights accepted the file; want an error")
			}
		})
	}
}

// weightsWith returns the embedded weights file after mutate has changed its
// typed form, re-marshalled.
func weightsWith(t *testing.T, mutate func(*weightsFile)) []byte {
	t.Helper()
	var f weightsFile
	if err := json.Unmarshal(embeddedWeights, &f); err != nil {
		t.Fatalf("decode embedded weights: %v", err)
	}
	mutate(&f)
	out, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("encode weights: %v", err)
	}
	return out
}

// replaceOnce returns the embedded weights file with the one occurrence of old
// replaced by new. It fails the test when old is not exactly once in the file,
// so a rejection row cannot pass against an unchanged file.
func replaceOnce(t *testing.T, old, new string) []byte {
	t.Helper()
	if n := bytes.Count(embeddedWeights, []byte(old)); n != 1 {
		t.Fatalf("%q occurs %d times in the embedded weights, want 1", old, n)
	}
	return bytes.Replace(embeddedWeights, []byte(old), []byte(new), 1)
}

func loadWeightsFrom(t *testing.T, data []byte) Weights {
	t.Helper()
	w, err := LoadWeights(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("LoadWeights: %v", err)
	}
	return w
}
