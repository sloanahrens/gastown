package metrics

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// ParseTS parses an ISO 8601 timestamp with these rules:
// a trailing Z is UTC, a fractional second and a numeric offset are read,
// and a timestamp with no zone (or only a date) is taken as UTC, which keeps
// every result a zoned time.
func ParseTS(ts string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02"} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid ISO timestamp %q", ts)
}

// WeekOf returns the Monday of ts's week as YYYY-MM-DD. The date is the one in
// ts's own offset, not UTC's.
func WeekOf(ts string) (string, error) {
	t, err := ParseTS(ts)
	if err != nil {
		return "", err
	}
	return weekOf(t), nil
}

// weekOf is WeekOf for a timestamp the caller already holds.
func weekOf(t time.Time) string {
	y, m, d := t.Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	sinceMonday := (int(day.Weekday()) + 6) % 7
	return day.AddDate(0, 0, -sinceMonday).Format("2006-01-02")
}

// Entropy is the Shannon entropy in bits of the positive weights. Weights that
// are zero or negative are ignored, and fewer than two positive weights is 0.
func Entropy(weights []float64) float64 {
	var w []float64
	total := 0.0
	for _, x := range weights {
		if x > 0 {
			w = append(w, x)
			total += x
		}
	}
	if len(w) <= 1 {
		return 0
	}
	sum := 0.0
	for _, x := range w {
		sum += (x / total) * math.Log2(x/total)
	}
	return -sum
}

// WriteCSV writes rows under columns in landings.csv's format: see
// WriteLandings, which it calls. A column a row does not carry is an empty cell.
func WriteCSV(w io.Writer, rows []Row, columns []string) error {
	return WriteLandings(w, columns, rows)
}

// ReadCSV reads a CSV whose first record is the header into one Row per later
// record. An empty input is no rows, as csv.DictReader reads it.
//
// A record whose width differs from the header is an error: ReadLandings reads
// the records, and csv.DictReader would pad a short row with None and gather a
// long row's extras under a None key.
func ReadCSV(r io.Reader) ([]Row, error) {
	br := bufio.NewReader(r)
	if _, err := br.Peek(1); err == io.EOF {
		return nil, nil
	}
	_, rows, err := ReadLandings(br)
	return rows, err
}

// FormatFloat formats v so a CSV cell written as 2.0 is still 2.0: the shortest
// digits that round-trip, a ".0" on an integer-valued float, and exponent form
// (1e+16, 1.5e-05) when the decimal exponent is below -4 or at least 16.
func FormatFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "nan"
	case math.IsInf(v, 1):
		return "inf"
	case math.IsInf(v, -1):
		return "-inf"
	}
	exp := strconv.FormatFloat(v, 'e', -1, 64)
	if i := strings.IndexByte(exp, 'e'); i >= 0 {
		if n, err := strconv.Atoi(exp[i+1:]); err == nil && (n < -4 || n >= 16) {
			return exp
		}
	}
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if !strings.ContainsAny(s, ".") {
		s += ".0"
	}
	return s
}

// DroppedItem is one refused row or unreadable source and why.
type DroppedItem struct {
	Source string
	Reason string
}

// Dropped records every row a loader refuses, so the row-count assertion and
// the footer can name it.
type Dropped struct {
	Items []DroppedItem
	// Failed lists the sources that could not be read at all, kept as a
	// structure so a caller never matches on a message's text.
	Failed []string
}

// Add records one refused row or gap.
func (d *Dropped) Add(source, reason string) {
	d.Items = append(d.Items, DroppedItem{Source: source, Reason: reason})
}

// Fail records a source that could not be read at all; it is also a logged gap.
func (d *Dropped) Fail(source, reason string) {
	d.Failed = append(d.Failed, source)
	d.Add(source, reason)
}

// Count is the number of items recorded.
func (d *Dropped) Count() int {
	return len(d.Items)
}
