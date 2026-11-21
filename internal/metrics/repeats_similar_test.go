package metrics

import (
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

// TestSimilarFindings is gt-jv5sje's acceptance: two findings on one file with
// enough shared words match, the same words on another file do not, and
// findings below the threshold do not.
func TestSimilarFindings(t *testing.T) {
	t.Parallel()
	const gate = "internal/land/gate.go"
	a := land.ReviewFindingText{File: gate, Title: "the gate ignores a failed read", Claim: "a failed read is reported as no findings and the gate passes"}
	tests := []struct {
		name string
		b    land.ReviewFindingText
		want bool
	}{
		{"restatement on the same file", land.ReviewFindingText{File: gate, Title: "the gate ignores a failed read", Claim: "a failed read still reports no findings and passes the gate"}, true},
		{"the same words on another file", land.ReviewFindingText{File: "internal/metrics/panel.go", Title: a.Title, Claim: a.Claim}, false},
		{"one file with too few shared words", land.ReviewFindingText{File: gate, Title: "the column is renamed", Claim: "the reader shows the old name"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := SimilarFindings(a, tt.b); got != tt.want {
				t.Errorf("SimilarFindings(%+v, %+v) = %v, want %v", a, tt.b, got, tt.want)
			}
		})
	}
}
