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
	// LinesSkipped counts the lines that were not a readable record, so a
	// caller can tell a quiet window from a ledger that no longer parses.
	LinesSkipped int                    `json:"lines_skipped"`
	ByCaller     map[string]CallerSpend `json:"by_caller"`
}

// ReadSpend sums the ledger lines stamped from through to, inclusive, pricing
// their tokens at usdPerBillion; a missing ledger is an empty report, not an
// error.
func ReadSpend(path string, from, to time.Time, usdPerBillion float64) (SpendReport, error) {
	rep := SpendReport{ByCaller: map[string]CallerSpend{}}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return rep, nil
	}
	if err != nil {
		return rep, fmt.Errorf("opening the usage ledger: %w", err)
	}
	defer f.Close()

	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadString('\n')
		if text := strings.TrimSpace(line); text != "" {
			var rec LedgerRecord
			// A write cut short leaves a partial line, and a hand-edited one
			// can lose its stamp; neither names a call the window can price.
			if err := json.Unmarshal([]byte(text), &rec); err != nil || rec.TS.IsZero() {
				rep.LinesSkipped++
			} else if !rec.TS.Before(from) && !rec.TS.After(to) {
				rep.add(rec, usdPerBillion)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return rep, fmt.Errorf("reading the usage ledger: %w", readErr)
		}
	}
	return rep, nil
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
