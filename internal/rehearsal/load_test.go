package rehearsal

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// loadZone is the sampler's own zone, six hours behind UTC.
var loadZone = time.FixedZone("CST", -6*3600)

// loadLine renders one sampler line the way internal/hostload.Line does.
func loadLine(at time.Time, load1 float64) string {
	return fmt.Sprintf("%s load1=%.2f load5=0.00 load15=0.00 ncpu=24", at.Format(time.RFC3339), load1)
}

func TestRehearsalLoadSummary(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 20, 16, 0, 0, 0, loadZone)
	end := start.Add(time.Hour)
	at := func(min int) time.Time { return start.Add(time.Duration(min) * time.Minute) }
	ten := make([]string, 10)
	for i := range ten {
		ten[i] = loadLine(at(i), float64(i+1))
	}
	shuffled := []string{ten[4], ten[0], ten[9], ten[2], ten[7], ten[1], ten[8], ten[3], ten[6], ten[5]}
	for _, tt := range []struct {
		name  string
		lines []string
		want  Load
	}{
		{"a sample on each end's side counts only inside", []string{
			loadLine(at(-1), 99), loadLine(at(0), 40), loadLine(at(30), 10), loadLine(at(59), 20), loadLine(at(60), 99),
		}, Load{Samples: 3, Max1: 40, P90One: 40}},
		{"ten samples give the ninth-largest as p90", ten, Load{Samples: 10, Max1: 10, P90One: 9}},
		{"input order does not matter", shuffled, Load{Samples: 10, Max1: 10, P90One: 9}},
		{"one sample is its own p90", []string{loadLine(at(10), 7)}, Load{Samples: 1, Max1: 7, P90One: 7}},
		{"a line in another zone is placed by its instant", []string{
			loadLine(time.Date(2026, 11, 20, 22, 30, 0, 0, time.UTC), 5),
		}, Load{Samples: 1, Max1: 5, P90One: 5}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := LoadSummary(strings.NewReader(strings.Join(tt.lines, "\n")+"\n"), start, end)
			if err != nil || got != tt.want {
				t.Fatalf("LoadSummary = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

// loadReadErr returns one good line, then fails, so the read-error branch runs
// for root too.
type loadReadErr struct{ text string }

func (e *loadReadErr) Read(p []byte) (int, error) {
	if e.text == "" {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, e.text)
	e.text = e.text[n:]
	return n, nil
}

func TestRehearsalLoadRefuses(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 20, 16, 0, 0, 0, loadZone)
	end := start.Add(time.Hour)
	good := loadLine(start.Add(10*time.Minute), 5)
	before := loadLine(start.Add(-time.Minute), 5)
	for _, tt := range []struct {
		name string
		r    io.Reader
	}{
		{"no text", strings.NewReader("")},
		{"no sample inside the window", strings.NewReader(before + "\n")},
		{"a line with an unparsable time", strings.NewReader("yesterday load1=5.00 load5=0.00 load15=0.00 ncpu=24\n" + good + "\n")},
		{"a line missing a field", strings.NewReader(strings.TrimSuffix(good, " ncpu=24") + "\n")},
		{"a line with an extra field", strings.NewReader(good + " extra\n")},
		{"a load that is not a number", strings.NewReader(strings.Replace(good, "load1=5.00", "load1=high", 1) + "\n")},
		{"a load that is not finite", strings.NewReader(strings.Replace(good, "load1=5.00", "load1=+Inf", 1) + "\n")},
		{"a field under the wrong key", strings.NewReader(strings.Replace(good, "load15=", "load99=", 1) + "\n")},
		{"a field without its key", strings.NewReader(strings.Replace(good, "load5=0.00", "0.00", 1) + "\n")},
		{"a core count that is not a whole number", strings.NewReader(strings.Replace(good, "ncpu=24", "ncpu=many", 1) + "\n")},
		{"a blank line", strings.NewReader("\n")},
		{"a bad line outside the window", strings.NewReader(strings.Replace(before, "load1=5.00", "load1=high", 1) + "\n" + good + "\n")},
		{"a read failure after a good line", &loadReadErr{text: good + "\n"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got, err := LoadSummary(tt.r, start, end); err == nil || got != (Load{}) {
				t.Fatalf("LoadSummary = %+v, %v; want an error and no load", got, err)
			}
		})
	}
}
