package metrics

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// The golden cases were captured from the reference implementation before it
// was removed.
type replicateGolden struct {
	Binom []struct {
		K    int     `json:"k"`
		N    int     `json:"n"`
		P    float64 `json:"p"`
		Want float64 `json:"want"`
	} `json:"binom"`
	Holm []struct {
		P    []float64 `json:"p"`
		Want []float64 `json:"want"`
	} `json:"holm"`
	Matches []struct {
		Cube Cube              `json:"cube"`
		Row  map[string]string `json:"row"`
		Want bool              `json:"want"`
	} `json:"matches"`
	Describe []struct {
		Cube Cube   `json:"cube"`
		Want string `json:"want"`
	} `json:"describe"`
}

func loadReplicateGolden(t *testing.T) replicateGolden {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "replicate_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g replicateGolden
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

const replicateTol = 1e-9

func TestReplicateBinomTwoSidedMatchesTheGoldenCases(t *testing.T) {
	t.Parallel()
	g := loadReplicateGolden(t)
	if len(g.Binom) == 0 {
		t.Fatal("no binomial cases in the golden file")
	}
	for _, c := range g.Binom {
		if got := BinomTwoSided(c.K, c.N, c.P); math.Abs(got-c.Want) > replicateTol {
			t.Errorf("BinomTwoSided(%d, %d, %v) = %v, want %v", c.K, c.N, c.P, got, c.Want)
		}
	}
}

func TestReplicateBinomTwoSidedEdges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		k, n int
		p    float64
		want float64
	}{
		{"no trials is no evidence", 0, 0, 0.5, 1},
		{"p of zero with no successes", 0, 5, 0, 1},
		{"p of zero with a success is impossible", 1, 5, 0, 0},
		{"p of one with all successes", 5, 5, 1, 1},
		{"p of one with a miss is impossible", 3, 5, 1, 0},
		{"a negative p behaves as zero", 0, 5, -0.1, 1},
		{"a p above one behaves as one", 5, 5, 1.5, 1},
	}
	for _, tc := range tests {
		if got := BinomTwoSided(tc.k, tc.n, tc.p); got != tc.want {
			t.Errorf("%s: BinomTwoSided(%d, %d, %v) = %v, want %v", tc.name, tc.k, tc.n, tc.p, got, tc.want)
		}
	}
	for _, k := range []int{-1, 6} {
		if got := BinomTwoSided(k, 5, 0.5); !math.IsNaN(got) {
			t.Errorf("BinomTwoSided(%d, 5, 0.5) = %v, want NaN for a k outside [0, n]", k, got)
		}
	}
}

func TestReplicateHolmMatchesTheGoldenCases(t *testing.T) {
	t.Parallel()
	g := loadReplicateGolden(t)
	for _, c := range g.Holm {
		got := Holm(c.P)
		if len(got) != len(c.Want) {
			t.Errorf("Holm(%v) has %d values, want %d", c.P, len(got), len(c.Want))
			continue
		}
		for i := range got {
			if math.Abs(got[i]-c.Want[i]) > replicateTol {
				t.Errorf("Holm(%v)[%d] = %v, want %v", c.P, i, got[i], c.Want[i])
			}
		}
	}
}

func TestReplicateMatchesAndDescribeMatchTheGoldenCases(t *testing.T) {
	t.Parallel()
	g := loadReplicateGolden(t)
	for _, c := range g.Matches {
		if got := Matches(c.Cube, c.Row); got != c.Want {
			t.Errorf("Matches(%v, %v) = %v, want %v", c.Cube, c.Row, got, c.Want)
		}
	}
	for _, c := range g.Describe {
		if got := Describe(c.Cube); got != c.Want {
			t.Errorf("Describe(%v) = %q, want %q", c.Cube, got, c.Want)
		}
	}
}

// The golden file must keep the awkward cases that make the port non-trivial:
// the test above passing on a file without them would prove little.
func TestReplicateGoldenFileKeepsTheEdgeCases(t *testing.T) {
	t.Parallel()
	g := loadReplicateGolden(t)
	var noTrials, pZero, pOne, atBound bool
	for _, c := range g.Binom {
		noTrials = noTrials || c.N == 0
		pZero = pZero || c.P == 0
		pOne = pOne || c.P == 1
		atBound = atBound || c.K == c.N
	}
	var ties, empty bool
	for _, c := range g.Holm {
		empty = empty || len(c.P) == 0
		for i := 1; i < len(c.P); i++ {
			ties = ties || c.P[i] == c.P[i-1]
		}
	}
	var unparseable, blank, nan, padded bool
	for _, c := range g.Matches {
		for _, v := range c.Row {
			unparseable = unparseable || v == "abc"
			blank = blank || v == ""
			nan = nan || v == "nan"
			padded = padded || v == " 0.75 "
		}
	}
	for name, ok := range map[string]bool{"n = 0": noTrials, "p = 0": pZero, "p = 1": pOne, "k = n": atBound,
		"holm ties": ties, "holm empty": empty, "unparseable cell": unparseable, "blank cell": blank,
		"nan cell": nan, "padded cell": padded} {
		if !ok {
			t.Errorf("the golden file has no %s case", name)
		}
	}
}
