package jev

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
)

// CallerSpend is one caller's share of a window's spend.
type CallerSpend struct {
	Calls int     `json:"calls"`
	USD   float64 `json:"usd"`
}

// SpendReport is the priced usage of the ledger lines stamped inside a window.
type SpendReport struct {
	Calls        int     `json:"calls"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	USD          float64 `json:"usd"`
	// LinesSkipped counts the lines the report could not price, so a caller
	// can tell a quiet window from a ledger that no longer parses.
	LinesSkipped int                    `json:"lines_skipped"`
	ByCaller     map[string]CallerSpend `json:"by_caller"`
}

// ReadSpend sums the ledger lines stamped from through to, inclusive, pricing
// their tokens at usdPerBillion; a missing ledger is an empty report, not an
// error.
func ReadSpend(path string, from, to time.Time, usdPerBillion float64) (SpendReport, error) {
	rep := SpendReport{ByCaller: map[string]CallerSpend{}}
	skipped, err := scanLedger(path, func(rec LedgerRecord) {
		if !rec.TS.Before(from) && !rec.TS.After(to) {
			rep.add(rec, usdPerBillion)
		}
	})
	rep.LinesSkipped = skipped
	return rep, err
}

// SpendPeriods is the dollars of four windows ending now, for a dashboard row
// that shows more than one: the last hour, today since local midnight,
// yesterday (midnight to midnight), and the last 168 hours.
type SpendPeriods struct {
	Last1hUSD    float64 `json:"last_1h_usd"`
	TodayUSD     float64 `json:"today_usd"`
	YesterdayUSD float64 `json:"yesterday_usd"`
	Last7dUSD    float64 `json:"last_7d_usd"`
}

// ReadSpendPeriods prices the ledger once into the four windows of
// SpendPeriods, days cut at now's local midnight. A missing ledger is all
// zeros, as in ReadSpend.
func ReadSpendPeriods(path string, now time.Time, usdPerBillion float64) (SpendPeriods, error) {
	var sp SpendPeriods
	y, m, d := now.Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	prevMidnight := midnight.AddDate(0, 0, -1)
	_, err := scanLedger(path, func(rec LedgerRecord) {
		if rec.TS.After(now) {
			return
		}
		usd := float64(rec.InputTokens+rec.OutputTokens) * usdPerBillion / 1e9
		if !rec.TS.Before(now.Add(-time.Hour)) {
			sp.Last1hUSD += usd
		}
		if !rec.TS.Before(midnight) {
			sp.TodayUSD += usd
		} else if !rec.TS.Before(prevMidnight) {
			sp.YesterdayUSD += usd
		}
		if !rec.TS.Before(now.Add(-7 * 24 * time.Hour)) {
			sp.Last7dUSD += usd
		}
	})
	return sp, err
}

// scanLedger calls visit with every priceable record of the ledger at path and
// returns how many lines it skipped. A write cut short leaves a partial line,
// and a hand-edited one can lose its stamp or carry a negative count; none of
// them names a call a window can price. A missing ledger is no records.
func scanLedger(path string, visit func(LedgerRecord)) (int, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("opening the usage ledger: %w", err)
	}
	defer f.Close()

	skipped := 0
	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadString('\n')
		if text := strings.TrimSpace(line); text != "" {
			var rec LedgerRecord
			if err := json.Unmarshal([]byte(text), &rec); err != nil || rec.TS.IsZero() ||
				rec.InputTokens < 0 || rec.OutputTokens < 0 {
				skipped++
			} else {
				visit(rec)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return skipped, fmt.Errorf("reading the usage ledger: %w", readErr)
		}
	}
	return skipped, nil
}

// add folds one record into the report.
func (s *SpendReport) add(rec LedgerRecord, usdPerBillion float64) {
	usd := float64(rec.InputTokens+rec.OutputTokens) * usdPerBillion / 1e9
	s.Calls++
	s.InputTokens += rec.InputTokens
	s.OutputTokens += rec.OutputTokens
	s.USD += usd

	caller := rec.Caller
	if caller == "" {
		caller = defaultCaller
	}
	c := s.ByCaller[caller]
	c.Calls++
	c.USD += usd
	s.ByCaller[caller] = c
}
