package scorecard

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// lowTailItem is one fixture line: its id, the probability Jev gave the noul
// question, and the label the fixture gives it.
type lowTailItem struct {
	id    string
	value float64
	label bool
}

// lowTailQuestion writes items as a fixture log under a temp town, builds the
// report and returns the question named by id.
func lowTailQuestion(t *testing.T, items []lowTailItem, id QuestionID) QuestionReport {
	t.Helper()
	town := t.TempDir()
	var lines []string
	labels := map[string]map[QuestionID]bool{}
	for i, it := range items {
		lines = append(lines, fmt.Sprintf(`{"ts":"2026-10-18T%02d:00:00Z","id":"%s","answers":{"%s":{"noul":%g}}}`, i, it.id, id, it.value))
		labels[it.id] = map[QuestionID]bool{id: it.label}
	}
	if err := WriteFixture(town, lines, labels); err != nil {
		t.Fatal(err)
	}
	rep, err := Build(context.Background(), fixture{town: town}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range rep.Questions {
		if q.ID == string(id) {
			return q
		}
	}
	t.Fatalf("the report has no %s question: %+v", id, rep.Questions)
	return QuestionReport{}
}

// wantFloat checks a metric is set and equals want to a tenth of a percent, or
// is nil when want is nil.
func wantFloat(t *testing.T, name string, got *float64, want *float64) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("%s = %v, want nil", name, *got)
	case want != nil && got == nil:
		t.Errorf("%s = nil, want %v", name, *want)
	case want != nil && (*got-*want > 0.001 || *want-*got > 0.001):
		t.Errorf("%s = %v, want %v", name, *got, *want)
	}
}

func ptrTo(v float64) *float64 { return &v }

// A base rate over 80% is read for the false label. The lowest band holds three
// labeled lines, one of them false; the other false line sits in the 0.4-0.6 band.
func TestLowTailReadsTheFalseLabelOverAHighBaseRate(t *testing.T) {
	t.Parallel()
	q := lowTailQuestion(t, []lowTailItem{
		{"a", 0.10, false}, {"b", 0.15, true}, {"c", 0.50, false},
		{"d", 0.90, true}, {"e", 0.95, true}, {"f", 0.85, true},
		{"g", 0.70, true}, {"h", 0.30, true}, {"i", 0.60, true},
		{"j", 0.90, true}, {"k", 0.05, true},
	}, "needs")
	if q.LowTail == nil {
		t.Fatal("LowTail = nil, want the low tail of a 81.8% base")
	}
	lt := q.LowTail
	if lt.Band != "0.0-0.2" || lt.Label != false || lt.Labeled != 3 {
		t.Errorf("LowTail = %+v, want band 0.0-0.2, minority label false over 3 labeled lines", lt)
	}
	// Two false lines in all: a third of the band is false, and the band holds
	// half of the false lines.
	wantFloat(t, "precision", lt.Precision, ptrTo(1.0/3))
	wantFloat(t, "recall", lt.Recall, ptrTo(0.5))
}

// A base rate under 20% is read for the true label.
func TestLowTailReadsTheTrueLabelUnderALowBaseRate(t *testing.T) {
	t.Parallel()
	q := lowTailQuestion(t, []lowTailItem{
		{"a", 0.90, true}, {"b", 0.10, false}, {"c", 0.05, false},
		{"d", 0.15, false}, {"e", 0.30, false}, {"f", 0.40, false},
		{"g", 0.20, false}, {"h", 0.25, false}, {"i", 0.60, false},
		{"j", 0.70, false}, {"k", 0.10, false}, {"l", 0.12, true},
	}, "needs")
	if q.LowTail == nil {
		t.Fatal("LowTail = nil, want the low tail of a 16.7% base")
	}
	lt := q.LowTail
	if lt.Label != true || lt.Labeled != 5 {
		t.Errorf("LowTail = %+v, want minority label true over 5 labeled lines in the band", lt)
	}
	// One true line in the band of five labeled lines, and two true lines in all.
	wantFloat(t, "precision", lt.Precision, ptrTo(0.2))
	wantFloat(t, "recall", lt.Recall, ptrTo(0.5))
}

// A base rate of exactly 80% or 20%, or a balanced one, has no low tail.
func TestLowTailIsAbsentForABalancedQuestion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		items []lowTailItem
	}{
		{"exactly 80 percent", []lowTailItem{
			{"a", 0.1, false}, {"b", 0.9, true}, {"c", 0.9, true}, {"d", 0.9, true}, {"e", 0.9, true},
		}},
		{"exactly 20 percent", []lowTailItem{
			{"a", 0.9, true}, {"b", 0.1, false}, {"c", 0.1, false}, {"d", 0.1, false}, {"e", 0.1, false},
		}},
		{"half and half", []lowTailItem{
			{"a", 0.1, false}, {"b", 0.9, true}, {"c", 0.1, false}, {"d", 0.9, true},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if q := lowTailQuestion(t, c.items, "needs"); q.LowTail != nil {
				t.Errorf("LowTail = %+v, want none for a balanced question", q.LowTail)
			}
		})
	}
}

// The band can hold no labeled line: its precision has no value, and recall is
// still read over the minority label.
func TestLowTailPrecisionIsNilWhenTheBandIsEmpty(t *testing.T) {
	t.Parallel()
	q := lowTailQuestion(t, []lowTailItem{
		{"a", 0.9, true}, {"b", 0.8, true}, {"c", 0.9, true}, {"d", 0.9, true}, {"e", 0.9, true},
		{"f", 0.9, true}, {"g", 0.9, true}, {"h", 0.9, true}, {"i", 0.9, true}, {"j", 0.5, false},
	}, "needs")
	if q.LowTail == nil {
		t.Fatal("LowTail = nil, want the low tail of a 90% base")
	}
	if q.LowTail.Labeled != 0 {
		t.Errorf("band labeled %d, want 0", q.LowTail.Labeled)
	}
	wantFloat(t, "precision", q.LowTail.Precision, nil)
	wantFloat(t, "recall", q.LowTail.Recall, ptrTo(0))
}

// A base rate of 100% has no minority line, so recall has no value.
func TestLowTailRecallIsNilWhenTheMinorityIsAbsent(t *testing.T) {
	t.Parallel()
	q := lowTailQuestion(t, []lowTailItem{
		{"a", 0.1, true}, {"b", 0.9, true}, {"c", 0.9, true}, {"d", 0.9, true}, {"e", 0.9, true},
	}, "needs")
	if q.LowTail == nil {
		t.Fatal("LowTail = nil, want the low tail of a 100% base")
	}
	if q.LowTail.Label != false {
		t.Errorf("minority label = %v, want false", q.LowTail.Label)
	}
	wantFloat(t, "precision", q.LowTail.Precision, ptrTo(0))
	wantFloat(t, "recall", q.LowTail.Recall, nil)
}

// A choice question has no noul value to bin, so it never gets a low tail,
// whatever its base rate.
func TestLowTailIsOnlyForNoulQuestions(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var lines []string
	labels := map[string]map[QuestionID]bool{}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("c%d", i)
		lines = append(lines, fmt.Sprintf(`{"ts":"2026-10-18T%02d:00:00Z","id":"%s","answers":{"route":{"choice":"yes","probabilities":{"yes":0.1,"no":0.9},"positive":"yes"}}}`, i, id))
		labels[id] = map[QuestionID]bool{"route": true}
	}
	if err := WriteFixture(town, lines, labels); err != nil {
		t.Fatal(err)
	}
	rep, err := Build(context.Background(), fixture{town: town}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range rep.Questions {
		if q.ID == "route" && q.LowTail != nil {
			t.Errorf("choice question has a low tail %+v", q.LowTail)
		}
	}
}
