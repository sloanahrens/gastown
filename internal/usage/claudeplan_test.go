package usage

import (
	"context"
	"errors"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

// memStore is an in-memory Store. Its failures are switches, so a test drives
// a read or write error without any other behaviour changing.
type memStore struct {
	rows     []ClaudePlan
	readErr  error
	writeErr error
}

func (m *memStore) NewestClaudePlan(context.Context) (*ClaudePlan, error) {
	if m.readErr != nil {
		return nil, m.readErr
	}
	if len(m.rows) == 0 {
		return nil, nil
	}
	p := m.rows[len(m.rows)-1]
	return &p, nil
}

func (m *memStore) WriteClaudePlan(_ context.Context, p ClaudePlan) error {
	if m.writeErr != nil {
		return m.writeErr
	}
	m.rows = append(m.rows, p)
	return nil
}

func plan(at time.Time, five, seven float64) ClaudePlan {
	return ClaudePlan{
		At:               at,
		FiveHourPct:      five,
		FiveHourResetsAt: t0.Add(5 * time.Hour),
		SevenDayPct:      seven,
		SevenDayResetsAt: t0.Add(72 * time.Hour),
	}
}

func TestParseStatusLineReadsBothWindows(t *testing.T) {
	t.Parallel()
	const doc = `{"model":{"id":"x"},"rate_limits":{
		"five_hour":{"used_percentage":41.5,"resets_at":1794400000},
		"seven_day":{"used_percentage":12,"resets_at":1794700000}}}`
	got, ok, err := ParseStatusLine([]byte(doc))
	if err != nil {
		t.Fatalf("ParseStatusLine: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want a reading")
	}
	if got.FiveHourPct != 41.5 || got.SevenDayPct != 12 {
		t.Errorf("percentages = %v, %v; want 41.5, 12", got.FiveHourPct, got.SevenDayPct)
	}
	if got.FiveHourResetsAt.Unix() != 1794400000 || got.SevenDayResetsAt.Unix() != 1794700000 {
		t.Errorf("resets = %v, %v; want epoch 1794400000, 1794700000", got.FiveHourResetsAt, got.SevenDayResetsAt)
	}
	if got.FiveHourResetsAt.Location() != time.UTC {
		t.Errorf("resets location = %v, want UTC", got.FiveHourResetsAt.Location())
	}
}

func TestParseStatusLineWithoutRateLimitsIsNoReading(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]string{
		"key absent":   `{"model":{"id":"x"}}`,
		"key null":     `{"rate_limits":null}`,
		"empty object": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, ok, err := ParseStatusLine([]byte(doc))
			if err != nil {
				t.Fatalf("err = %v, want nil: an absent rate_limits is not a failure", err)
			}
			if ok {
				t.Error("ok = true, want no reading")
			}
		})
	}
}

func TestParseStatusLineRefusesBrokenPayloads(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"not JSON":               `{"rate_limits":`,
		"empty input":            ``,
		"five_hour missing":      `{"rate_limits":{"seven_day":{"used_percentage":1,"resets_at":1}}}`,
		"seven_day missing":      `{"rate_limits":{"five_hour":{"used_percentage":1,"resets_at":1}}}`,
		"pct missing":            `{"rate_limits":{"five_hour":{"resets_at":1},"seven_day":{"used_percentage":1,"resets_at":1}}}`,
		"resets missing":         `{"rate_limits":{"five_hour":{"used_percentage":1},"seven_day":{"used_percentage":1,"resets_at":1}}}`,
		"pct below zero":         `{"rate_limits":{"five_hour":{"used_percentage":-1,"resets_at":1},"seven_day":{"used_percentage":1,"resets_at":1}}}`,
		"pct above hundred":      `{"rate_limits":{"five_hour":{"used_percentage":101,"resets_at":1},"seven_day":{"used_percentage":1,"resets_at":1}}}`,
		"rate_limits not object": `{"rate_limits":"soon"}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			_, ok, err := ParseStatusLine([]byte(doc))
			if err == nil {
				t.Fatal("err = nil, want a refusal: a broken payload must not read as absent")
			}
			if ok {
				t.Error("ok = true alongside an error")
			}
		})
	}
}

func TestParseStatusLineAcceptsPercentageBounds(t *testing.T) {
	t.Parallel()
	for _, pct := range []string{"0", "100"} {
		doc := `{"rate_limits":{"five_hour":{"used_percentage":` + pct + `,"resets_at":1},"seven_day":{"used_percentage":` + pct + `,"resets_at":1}}}`
		if _, ok, err := ParseStatusLine([]byte(doc)); err != nil || !ok {
			t.Errorf("pct %s: ok=%v err=%v, want a reading", pct, ok, err)
		}
	}
}

func TestShouldWriteDedupesWithinTenMinutes(t *testing.T) {
	t.Parallel()
	newest := plan(t0, 40, 25)
	cases := []struct {
		name  string
		next  ClaudePlan
		write bool
	}{
		{"same values one minute later", plan(t0.Add(time.Minute), 40, 25), false},
		{"same values just under ten minutes", plan(t0.Add(DedupeWindow-time.Second), 40, 25), false},
		{"same values at exactly ten minutes", plan(t0.Add(DedupeWindow), 40, 25), true},
		{"same values after ten minutes", plan(t0.Add(11*time.Minute), 40, 25), true},
		{"five-hour moved within window", plan(t0.Add(time.Minute), 41, 25), true},
		{"seven-day moved within window", plan(t0.Add(time.Minute), 40, 26), true},
		{"reset moved within window", func() ClaudePlan {
			p := plan(t0.Add(time.Minute), 40, 25)
			p.SevenDayResetsAt = p.SevenDayResetsAt.Add(time.Hour)
			return p
		}(), true},
		{"clock moved back is not recent", plan(t0.Add(-time.Minute), 40, 25), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ShouldWrite(&newest, c.next); got != c.write {
				t.Errorf("ShouldWrite = %v, want %v", got, c.write)
			}
		})
	}
}

func TestShouldWriteWithoutPriorRow(t *testing.T) {
	t.Parallel()
	if !ShouldWrite(nil, plan(t0, 1, 1)) {
		t.Error("ShouldWrite(nil, _) = false, want true: the first reading is always a row")
	}
}

func TestRecordWritesOneRowAcrossDedupeWindows(t *testing.T) {
	t.Parallel()
	store := &memStore{}
	feeds := []struct {
		at      time.Time
		wantNew bool
	}{
		{t0, true},
		{t0.Add(4 * time.Minute), false},
		{t0.Add(9 * time.Minute), false},
		{t0.Add(19 * time.Minute), true},
	}
	for _, f := range feeds {
		got, err := Record(context.Background(), store, plan(f.at, 40, 25))
		if err != nil {
			t.Fatalf("Record at %v: %v", f.at, err)
		}
		if got != f.wantNew {
			t.Errorf("Record at %v wrote %v, want %v", f.at, got, f.wantNew)
		}
	}
	if len(store.rows) != 2 {
		t.Errorf("rows = %d, want 2", len(store.rows))
	}
}

func TestRecordRefusesWhenNewestRowCannotBeRead(t *testing.T) {
	t.Parallel()
	store := &memStore{readErr: errors.New("bd sample list: boom")}
	wrote, err := Record(context.Background(), store, plan(t0, 1, 1))
	if err == nil {
		t.Fatal("err = nil, want the read failure: an unreadable newest row is not 'no row'")
	}
	if wrote || len(store.rows) != 0 {
		t.Errorf("wrote = %v rows = %d, want no write after a failed read", wrote, len(store.rows))
	}
}

func TestRecordReportsWriteFailure(t *testing.T) {
	t.Parallel()
	store := &memStore{writeErr: errors.New("bd sample add: boom")}
	wrote, err := Record(context.Background(), store, plan(t0, 1, 1))
	if err == nil {
		t.Fatal("err = nil, want the write failure")
	}
	if wrote {
		t.Error("wrote = true alongside a write error")
	}
}
