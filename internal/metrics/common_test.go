package metrics

import (
	"bytes"
	"math"
	"reflect"
	"strings"
	"testing"
)

// floatTol is the tolerance for comparing a float the Python printed.
const floatTol = 1e-9

func TestCommonWeekOfIsMonday(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ts   string
		want string
	}{
		{"friday", "2026-10-23T18:49:02Z", "2026-10-19"},
		{"monday", "2026-10-19T00:00:00Z", "2026-10-19"},
		{"sunday with a fractional second", "2026-10-25T23:59:59.5Z", "2026-10-19"},
		{"the date in the timestamp's own offset", "2026-10-26T01:00:00+02:00", "2026-10-26"},
		{"no zone is read as UTC", "2026-10-23T18:49:02", "2026-10-19"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := WeekOf(tc.ts)
			if err != nil {
				t.Fatalf("WeekOf(%q): %v", tc.ts, err)
			}
			if got != tc.want {
				t.Errorf("WeekOf(%q) = %q, want %q", tc.ts, got, tc.want)
			}
		})
	}
}

func TestCommonWeekOfRejectsANonTimestamp(t *testing.T) {
	t.Parallel()
	if got, err := WeekOf("last tuesday"); err == nil {
		t.Errorf("WeekOf(%q) = %q, want an error", "last tuesday", got)
	}
}

func TestCommonEntropy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		weights []float64
		want    float64
	}{
		{"none", nil, 0},
		{"one weight", []float64{7}, 0},
		{"two equal", []float64{1, 1}, 1},
		{"four equal", []float64{1, 1, 1, 1}, 2},
		{"zero weights are ignored", []float64{0, 5}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Entropy(tc.weights); math.Abs(got-tc.want) > floatTol {
				t.Errorf("Entropy(%v) = %v, want %v", tc.weights, got, tc.want)
			}
		})
	}
}

func TestCommonCSVRoundTripKeepsColumnOrderAndEmpty(t *testing.T) {
	t.Parallel()
	rows := []Row{{"a": "1"}, {"a": "2", "b": "x,y"}}
	var buf bytes.Buffer
	if err := WriteCSV(&buf, rows, []string{"b", "a"}); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	if first, _, _ := strings.Cut(buf.String(), "\r\n"); first != "b,a" {
		t.Errorf("header = %q, want %q", first, "b,a")
	}
	got, err := ReadCSV(&buf)
	if err != nil {
		t.Fatalf("ReadCSV: %v", err)
	}
	want := []Row{{"b": "", "a": "1"}, {"b": "x,y", "a": "2"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadCSV = %v, want %v", got, want)
	}
}

func TestCommonReadCSVOfAnEmptyFileIsNoRows(t *testing.T) {
	t.Parallel()
	rows, err := ReadCSV(strings.NewReader(""))
	if err != nil || len(rows) != 0 {
		t.Errorf("ReadCSV(empty) = %v, %v; want no rows and no error", rows, err)
	}
}

func TestCommonDroppedCounts(t *testing.T) {
	t.Parallel()
	var d Dropped
	d.Add("ledger:gastown", "bad json")
	if d.Count() != 1 {
		t.Errorf("Count = %d, want 1", d.Count())
	}
	if want := []DroppedItem{{Source: "ledger:gastown", Reason: "bad json"}}; !reflect.DeepEqual(d.Items, want) {
		t.Errorf("Items = %v, want %v", d.Items, want)
	}
}

func TestCommonFailRecordsAStructuredFailedSource(t *testing.T) {
	t.Parallel()
	var d Dropped
	d.Fail("bd:/g", "bd list failed in /g")
	d.Add("ledger:x", "bad")
	if want := []string{"bd:/g"}; !reflect.DeepEqual(d.Failed, want) {
		t.Errorf("Failed = %v, want %v", d.Failed, want)
	}
	if d.Count() != 2 {
		t.Errorf("Count = %d, want 2: a failure is also a logged gap", d.Count())
	}
}

func TestCommonFormatFloat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		v    float64
		want string
	}{
		{2, "2.0"},
		{0, "0.0"},
		{1, "1.0"},
		{0.5, "0.5"},
		{1.584962500721156, "1.584962500721156"},
		{123456789012345, "123456789012345.0"},
		{1e16, "1e+16"},
		{1.5e-5, "1.5e-05"},
		{0.0001, "0.0001"},
		{-3, "-3.0"},
		{math.Copysign(0, -1), "-0.0"},
		{math.NaN(), "nan"},
		{math.Inf(1), "inf"},
		{math.Inf(-1), "-inf"},
	}
	for _, tc := range tests {
		if got := FormatFloat(tc.v); got != tc.want {
			t.Errorf("FormatFloat(%v) = %q, want %q", tc.v, got, tc.want)
		}
	}
}
