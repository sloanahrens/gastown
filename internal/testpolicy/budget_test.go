package testpolicy

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

const stream = `{"Action":"output","Package":"m/internal/a","Output":"ok\n"}
{"Action":"pass","Package":"m/internal/a","Test":"TestSlow","Elapsed":9.5}
{"Action":"pass","Package":"m/internal/a","Test":"TestFast","Elapsed":0.1}
{"Action":"pass","Package":"m/internal/a","Elapsed":12.0}
{"Action":"pass","Package":"m/internal/b","Elapsed":30.0}
{"Action":"pass","Package":"m/internal/c","Elapsed":1.0}
`

func TestWatchBudget(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	over, err := WatchBudget(strings.NewReader(stream), &out, 10*time.Second, map[string]bool{"internal/b": true}, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(over) != 1 || over[0].Package != "internal/a" || over[0].Elapsed != 12*time.Second {
		t.Fatalf("overruns = %+v, want internal/a at 12s only (b is exempt, c is under)", over)
	}
	if len(over[0].Slowest) == 0 || over[0].Slowest[0].Name != "TestSlow" {
		t.Fatalf("slowest = %+v, want TestSlow first", over[0].Slowest)
	}
	if out.String() != "ok\n" {
		t.Fatalf("output = %q, want the Output fields passed through", out.String())
	}
}
