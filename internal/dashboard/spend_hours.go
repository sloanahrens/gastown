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

// spendLedgerLineMax bounds one ledger line the reader keeps. A longer line is
// counted as malformed and its tail discarded, so one line cannot grow the read
// without limit (gt-idfec).
const spendLedgerLineMax = 64 << 10

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
// spendHours whole hours ending with now's local hour, oldest first, and how many
// malformed lines it skipped. A missing ledger is no samples, so every hour is a
// gap; a row that does not parse is skipped and counted, as the spend tool skips
// it.
//
// The hours are local-time hours, so a half-hour or a DST-shifted zone still
// draws each hour where the ledger's own naive local times put it. Spend is the
// fall of the balance between two consecutive samples, counted in the hour of
// the later one. A rise is money added (a top-up or a grant), so it is not spend
// and it is not netted against a later fall.
func ReadSpendHours(path string, now time.Time) ([]SpendHour, int, error) {
	samples, skipped, err := readBalanceSamples(path)
	if err != nil {
		return nil, 0, err
	}
	hours := spendHourBuckets(now.In(time.Local))
	for i, s := range samples {
		b := hourBucket(hours, s.at)
		if b >= 0 {
			hours[b].Samples++
		}
		if i == 0 {
			continue
		}
		fall := samples[i-1].balance - s.balance
		if b >= 0 && fall > 0 {
			hours[b].Spent += fall
		}
	}
	return hours, skipped, nil
}

// spendHourBuckets is the last spendHours local hours ending with now's hour, each
// starting on the hour as the local clock reads it.
func spendHourBuckets(now time.Time) []SpendHour {
	y, m, d := now.Date()
	h := now.Hour()
	hours := make([]SpendHour, spendHours)
	for i := range hours {
		hours[i].Start = time.Date(y, m, d, h-spendHours+1+i, 0, 0, 0, time.Local)
	}
	return hours
}

// hourBucket is the index of the hour that holds at, or -1 when at falls outside
// every hour. The hours run from each Start to the next, the last to the hour
// after now.
func hourBucket(hours []SpendHour, at time.Time) int {
	for i := range hours {
		end := time.Time{}
		if i+1 < len(hours) {
			end = hours[i+1].Start
		} else {
			end = hours[i].Start.Add(time.Hour)
		}
		if !at.Before(hours[i].Start) && at.Before(end) {
			return i
		}
	}
	return -1
}

// readBalanceSamples parses the ledger's rows in file order and counts the
// malformed ones. A missing file is no rows.
func readBalanceSamples(path string) ([]balanceSample, int, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("spend ledger: %w", err)
	}
	defer f.Close()
	// A reader, not a scanner: a scanner stops at its token limit. A line longer
	// than the buffer is malformed here, and the rest of it is discarded.
	r := bufio.NewReaderSize(f, spendLedgerLineMax)
	skipped := 0
	var out []balanceSample
	for {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			skipped++
			err = discardLine(r)
		} else {
			text := strings.TrimRight(string(line), "\r\n")
			if s, ok := parseBalanceSample(text); ok {
				out = append(out, s)
			} else if strings.TrimSpace(text) != "" {
				skipped++
			}
		}
		if errors.Is(err, io.EOF) {
			return out, skipped, nil
		}
		if err != nil {
			return nil, 0, fmt.Errorf("spend ledger: %w", err)
		}
	}
}

// discardLine drops the rest of the current line: it reads until a newline, end
// of file, or a read error, and returns the error that ended it (nil at a
// newline).
func discardLine(r *bufio.Reader) error {
	for {
		_, err := r.ReadSlice('\n')
		if !errors.Is(err, bufio.ErrBufferFull) {
			return err
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
