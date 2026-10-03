package dashboard

import (
	"testing"
	"time"
)

// The trend worker fills State.Trend, and the machine poll feeds the load
// sample to the writer.
func TestTrendWorkerFillsStateAndSamplesLoad(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 16, 0, 0, 0, time.UTC)
	var sampled []float64
	h := NewHub(Config{
		Machine:    func() (Machine, error) { return Machine{Load1: 3.5, At: now}, nil },
		LoadSample: func(at time.Time, load float64) { sampled = append(sampled, load) },
		Trend:      func() *Trend { return &Trend{Hours: []TrendHour{{Hour: now, Landed: 2}}} },
		Now:        func() time.Time { return now },
	})
	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	h.pollMachine()
	h.pollTrend()

	st := h.State()
	if st.Trend == nil || len(st.Trend.Hours) != 1 || st.Trend.Hours[0].Landed != 2 {
		t.Fatalf("trend = %+v", st.Trend)
	}
	if len(sampled) != 1 || sampled[0] != 3.5 {
		t.Fatalf("load samples = %v, want the machine's sample", sampled)
	}

	// A reader that has nothing leaves the part out rather than showing zeroes.
	h2 := NewHub(Config{Trend: func() *Trend { return nil }})
	h2.pollTrend()
	if h2.State().Trend != nil {
		t.Errorf("a nil trend must leave State.Trend unset, got %+v", h2.State().Trend)
	}
}
