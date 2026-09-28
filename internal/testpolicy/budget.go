package testpolicy

import (
	"bufio"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"
)

type TestTime struct {
	Name    string
	Elapsed time.Duration
}

type Overrun struct {
	Package string
	Elapsed time.Duration
	Slowest []TestTime
}

type testEvent struct {
	Action  string
	Package string
	Test    string
	Elapsed float64
	Output  string
}

// WatchBudget copies the human-readable output of a `go test -json` stream to w
// and returns every package, outside exempt, whose test binary ran longer than
// budget. Package names are reported relative to module.
func WatchBudget(r io.Reader, w io.Writer, budget time.Duration, exempt map[string]bool, module string) ([]Overrun, error) {
	tests := map[string][]TestTime{}
	var over []Overrun
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var ev testEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			// go test prints build failures as plain text; pass them through.
			_, _ = io.WriteString(w, sc.Text()+"\n")
			continue
		}
		if ev.Action == "output" {
			_, _ = io.WriteString(w, ev.Output)
			continue
		}
		if ev.Action != "pass" && ev.Action != "fail" {
			continue
		}
		d := time.Duration(ev.Elapsed * float64(time.Second))
		pkg := strings.TrimPrefix(strings.TrimPrefix(ev.Package, module), "/")
		if ev.Test != "" {
			if !strings.Contains(ev.Test, "/") {
				tests[pkg] = append(tests[pkg], TestTime{ev.Test, d})
			}
			continue
		}
		if d > budget && !exempt[pkg] {
			ts := tests[pkg]
			sort.Slice(ts, func(i, j int) bool { return ts[i].Elapsed > ts[j].Elapsed })
			if len(ts) > 3 {
				ts = ts[:3]
			}
			over = append(over, Overrun{pkg, d, ts})
		}
	}
	return over, sc.Err()
}
