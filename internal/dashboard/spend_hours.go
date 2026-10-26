package dashboard

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"
)

// spendHours is how many whole hours the hourly spend chart draws.
const spendHours = 24

// spendLedgerLayout is the form the spend watcher writes each ledger row's
// time in: naive local time, to the second.
const spendLedgerLayout = "2006-01-02T15:04:05"

// SpendHour is one hour of DeepSeek spend read from the balance ledger: how
// far the balance fell over the hour's samples, and how many samples the hour
// holds. An hour with no sample is a gap, not zero spend, so Samples is what
// tells them apart.
type SpendHour struct {
	Start   time.Time `json:"start"`
	Spent   float64   `json:"spent"`
	Samples int       `json:"samples"`
}

// balanceSample is one ledger row: when the watcher took the balance and what it
// was.
type balanceSample struct {
	at      time.Time
	balance float64
}

// ReadSpendHours reads the balance ledger at path and returns the last
// spendHours whole hours ending at now's hour, oldest first. A missing ledger is
// no samples, so every hour is a gap; a row that does not parse is skipped, as
// the spend tool skips it.
//
// Spend is the fall of the balance between two consecutive samples, counted in
// the hour of the later one. A rise is money added (a top-up or a grant), so it
// is not spend and it is not netted against a later fall.
func ReadSpendHours(path string, now time.Time) ([]SpendHour, int, error) {
	samples, skipped, err := readBalanceSamples(path)
	if err != nil {
		return nil, 0, err
	}
	hourEnd := now.Truncate(time.Hour).Add(time.Hour)
	first := hourEnd.Add(-spendHours * time.Hour)
	hours := make([]SpendHour, spendHours)
	for i := range hours {
		hours[i].Start = first.Add(time.Duration(i) * time.Hour)
	}
	bucket := func(at time.Time) int {
		if at.Before(first) || !at.Before(hourEnd) {
			return -1
		}
		return int(at.Sub(first) / time.Hour)
	}
	for i, s := range samples {
		if b := bucket(s.at); b >= 0 {
			hours[b].Samples++
		}
		if i == 0 {
			continue
		}
		fall := samples[i-1].balance - s.balance
		if b := bucket(s.at); b >= 0 && fall > 0 {
			hours[b].Spent += fall
		}
	}
	return hours, skipped, nil
}

// readBalanceSamples parses the ledger's rows in file order. A missing file is
// no rows.
func readBalanceSamples(path string) ([]balanceSample, int, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("spend ledger: %w", err)
	}
	defer f.Close()
	// A reader, not a scanner: a scanner stops at its token limit, and one long
	// line would then fail the read of the whole ledger.
	r := bufio.NewReader(f)
	skipped := 0
	var out []balanceSample
	for {
		line, err := r.ReadString('\n')
		text := strings.TrimRight(line, "\r\n")
		if s, ok := parseBalanceSample(text); ok {
			out = append(out, s)
		} else if strings.TrimSpace(text) != "" {
			skipped++
		}
		if errors.Is(err, io.EOF) {
			return out, skipped, nil
		}
		if err != nil {
			return nil, 0, fmt.Errorf("spend ledger: %w", err)
		}
	}
}

// parseBalanceSample reads one ledger row: a local timestamp, then
// balance=<amount> and the optional grant tail. A row without a balance is not
// a sample.
func parseBalanceSample(line string) (balanceSample, bool) {
	ts, rest, ok := strings.Cut(line, " balance=")
	if !ok {
		return balanceSample{}, false
	}
	at, err := time.ParseInLocation(spendLedgerLayout, strings.TrimSpace(ts), time.Local)
	if err != nil {
		return balanceSample{}, false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return balanceSample{}, false
	}
	bal, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return balanceSample{}, false
	}
	return balanceSample{at: at, balance: bal}, true
}
