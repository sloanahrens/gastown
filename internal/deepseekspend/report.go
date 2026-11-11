// Package deepseekspend builds the DeepSeek spend report from the balance ledger
// the spend watcher appends to. It is the ledger-only port of the report
// definitions in deepseek-spend.py: there is no live balance call, so the
// dashboard pod needs no DeepSeek key.
package deepseekspend

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// DefaultDays is how many days per_day shows: deepseek-spend.py's --days default.
const DefaultDays = 7

// gapAfter is the silence between two samples the report lists as a gap.
const gapAfter = 15 * time.Minute

// alertKeep is how many of the latest events the report's alerts list.
const alertKeep = 5

// ledgerLayout is the naive local timestamp the watcher writes.
const ledgerLayout = "2006-01-02T15:04:05"

// noLiveBalance is the live reason every report carries: the pod makes no
// balance call.
const noLiveBalance = "no live balance in the dashboard"

// Constants are the figures deepseek-spend.py keeps at the top of the tool.
type Constants struct {
	// TopUpAt is when the last top-up landed, as naive local time.
	TopUpAt time.Time
	// TopUp is the balance that top-up read.
	TopUp float64
}

// CurrentConstants is the last top-up, read in loc. Update on the next top-up.
func CurrentConstants(loc *time.Location) Constants {
	return Constants{
		TopUpAt: time.Date(2026, time.September, 29, 20, 50, 0, 0, loc),
		TopUp:   114.05,
	}
}

// Row is one balance.log line. Granted and Topped are the optional split the
// watcher writes; they are nil when the line carries no complete split.
type Row struct {
	At      time.Time
	Balance float64
	Granted *float64
	Topped  *float64
}

// Input is what Build reads: the ledger rows, the events log, the clock and the
// number of days per_day shows.
type Input struct {
	Constants
	Rows   []Row
	Events []string
	Now    time.Time
	// Days is how many of the latest days per_day lists; 0 lists all of them.
	Days int
}

// Report is the --json report of deepseek-spend.py, with the same keys. A ledger
// with no rows is a zero report: the balance and the sample stamps are null.
type Report struct {
	Live        Live     `json:"live"`
	Balance     *float64 `json:"balance"`
	TopUp       float64  `json:"topup"`
	SpentTotal  float64  `json:"spent_total"`
	Cash        *float64 `json:"cash"`
	Credit      *float64 `json:"credit"`
	SpentLedger float64  `json:"spent_ledger"`
	LedgerSince *string  `json:"ledger_since"`
	Credits     []Credit `json:"credits"`
	Samples     int      `json:"samples"`
	// WatcherPID is always null: the pod runs no watcher to ask.
	WatcherPID *int     `json:"watcher_pid"`
	LastSample *string  `json:"last_sample"`
	Pace1h     *Pace    `json:"pace_1h"`
	Pace6h     *Pace    `json:"pace_6h"`
	Pace24h    *Pace    `json:"pace_24h"`
	RunwayDays *float64 `json:"runway_days_at_24h_pace"`
	// CreditRunwayDays is always null: it divides the live granted balance, which
	// the pod never reads.
	CreditRunwayDays *float64       `json:"credit_runway_days_at_24h_pace"`
	PerDay           map[string]Day `json:"per_day"`
	Gaps             []Gap          `json:"gaps"`
	Alerts           []string       `json:"alerts"`
}

// Live is the live balance read. The pod never makes it, so OK is always false.
type Live struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// Credit is one balance rise: a grant, a top-up, or both.
type Credit struct {
	At     string  `json:"at"`
	Amount float64 `json:"amount"`
	Kind   string  `json:"kind"`
}

// Pace is the spend over one window, and its rate in dollars an hour.
type Pace struct {
	Spent   float64  `json:"spent"`
	PerHour *float64 `json:"per_hour"`
}

// Day is one calendar day: the first and last balance, and the spend within it.
type Day struct {
	Open  float64 `json:"open"`
	Close float64 `json:"close"`
	Spent float64 `json:"spent"`
}

// Gap is a stretch with no samples longer than gapAfter.
type Gap struct {
	From    string  `json:"from"`
	To      string  `json:"to"`
	Minutes int     `json:"minutes"`
	Spent   float64 `json:"spent"`
}

// ParseLedger reads balance.log text with naive local timestamps in loc. A line
// that does not parse is skipped. A granted/topped tail that does not parse
// drops only the split: the row keeps its balance, as the tool does.
func ParseLedger(text string, loc *time.Location) []Row {
	var rows []Row
	for _, line := range splitLines(text) {
		if row, ok := parseRow(line, loc); ok {
			rows = append(rows, row)
		}
	}
	return rows
}

// parseRow reads one balance.log line, and reports false for a line that is not
// a row.
func parseRow(line string, loc *time.Location) (Row, bool) {
	parts := strings.Split(line, " balance=")
	if len(parts) != 2 {
		return Row{}, false
	}
	at, err := time.ParseInLocation(ledgerLayout, strings.TrimSpace(parts[0]), loc)
	if err != nil {
		return Row{}, false
	}
	fields := strings.Fields(parts[1])
	if len(fields) == 0 {
		return Row{}, false
	}
	bal, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return Row{}, false
	}
	granted, topped := parseSplit(fields[1:])
	return Row{At: at, Balance: bal, Granted: granted, Topped: topped}, true
}

// splitLines splits a file's text into its lines, without a final empty line for
// a trailing newline, as reading the file line by line does.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	text = strings.ReplaceAll(strings.TrimSuffix(text, "\n"), "\r\n", "\n")
	return strings.Split(text, "\n")
}

// parseSplit reads the granted=G topped=T tail. Both must parse for either to be
// kept, so a split is never half a row.
func parseSplit(fields []string) (granted, topped *float64) {
	kv := map[string]string{}
	for _, f := range fields {
		if k, v, ok := strings.Cut(f, "="); ok {
			kv[k] = v
		}
	}
	g, gok := kv["granted"]
	t, tok := kv["topped"]
	if !gok || !tok {
		return nil, nil
	}
	gv, gerr := strconv.ParseFloat(g, 64)
	tv, terr := strconv.ParseFloat(t, 64)
	if gerr != nil || terr != nil {
		return nil, nil
	}
	return &gv, &tv
}

// Build returns the report for the ledger rows. A ledger with no rows is a zero
// report, not an absent one: it has no balance, and its figures are zero or null.
func Build(in Input) Report {
	r := Report{
		Live:    Live{OK: false, Error: noLiveBalance},
		TopUp:   in.TopUp,
		Credits: []Credit{},
		Alerts:  alertLines(in.Events),
		PerDay:  map[string]Day{},
		Gaps:    []Gap{},
	}
	rows := in.Rows
	if len(rows) == 0 {
		return r
	}
	last := rows[len(rows)-1]
	r.Balance = ptr(last.Balance)
	r.SpentTotal = round(drops(since(rows, in.TopUpAt)), 2)
	r.SpentLedger = round(drops(rows), 2)
	r.LedgerSince = ptr(stamp(rows[0].At))
	r.Credits = lastCredits(rises(rows), 5)
	r.Samples = len(rows)
	r.LastSample = ptr(stamp(last.At))
	r.Pace1h = pace(rows, in.Now, time.Hour)
	r.Pace6h = pace(rows, in.Now, 6*time.Hour)
	r.Pace24h = pace(rows, in.Now, 24*time.Hour)
	// The runway divides by the rounded 24h rate, as the tool's report does.
	if p := r.Pace24h; p != nil && p.PerHour != nil && *p.PerHour > 0 && last.Balance != 0 {
		r.RunwayDays = ptr(round(last.Balance/(*p.PerHour*24), 1))
	}
	r.PerDay = perDay(rows, in.Days)
	r.Gaps = gapList(rows)
	return r
}

// since is the rows at or after t, in order.
func since(rows []Row, t time.Time) []Row {
	var out []Row
	for _, r := range rows {
		if !r.At.Before(t) {
			out = append(out, r)
		}
	}
	return out
}

// drops is the spend over consecutive samples: only the falls count. A rise is a
// grant or a top-up, never negative spend.
func drops(rows []Row) float64 {
	var sum float64
	for i := 1; i < len(rows); i++ {
		sum += math.Max(0, rows[i-1].Balance-rows[i].Balance)
	}
	return sum
}

// rises lists every balance rise above a cent. A split on both samples says what
// the rise was: granted rose is a credit, topped rose is a top-up.
func rises(rows []Row) []Credit {
	out := []Credit{}
	for i := 1; i < len(rows); i++ {
		a, b := rows[i-1], rows[i]
		if b.Balance-a.Balance <= 0.01 {
			continue
		}
		kind := "unclassified"
		if a.Granted != nil && a.Topped != nil && b.Granted != nil && b.Topped != nil {
			dg := *b.Granted - *a.Granted
			dt := *b.Topped - *a.Topped
			switch {
			case dg > 0.01 && dt <= 0.01:
				kind = "credit"
			case dt > 0.01 && dg <= 0.01:
				kind = "top-up"
			case dg > 0.01:
				kind = "credit+top-up"
			}
		}
		out = append(out, Credit{At: stamp(b.At), Amount: round(b.Balance-a.Balance, 2), Kind: kind})
	}
	return out
}

// lastCredits keeps the latest n credits, as the report's credits list does.
func lastCredits(credits []Credit, n int) []Credit {
	if len(credits) > n {
		return credits[len(credits)-n:]
	}
	return credits
}

// pace is the spend between the first sample in the window that ends at now and
// the last sample, over the span those samples cover. It is nil with fewer than
// two samples in the window, and its rate is nil over a zero-length span.
func pace(rows []Row, now time.Time, window time.Duration) *Pace {
	in := since(rows, now.Add(-window))
	if len(in) < 2 {
		return nil
	}
	spent := drops(in)
	hours := in[len(in)-1].At.Sub(in[0].At).Hours()
	p := &Pace{Spent: round(spent, 2)}
	if hours != 0 {
		p.PerHour = ptr(round(spent/hours, 3))
	}
	return p
}

// perDay lists each day that has samples, in ledger order, keeping the latest
// days when days is positive. Spend counts only falls between a day's own
// samples, so the drop across midnight is in spent_ledger but in no day.
func perDay(rows []Row, days int) map[string]Day {
	var order []string
	byDay := map[string][]Row{}
	for _, r := range rows {
		d := r.At.Format("2006-01-02")
		if _, ok := byDay[d]; !ok {
			order = append(order, d)
		}
		byDay[d] = append(byDay[d], r)
	}
	if days > 0 && len(order) > days {
		order = order[len(order)-days:]
	}
	out := make(map[string]Day, len(order))
	for _, d := range order {
		rs := byDay[d]
		out[d] = Day{
			Open:  rs[0].Balance,
			Close: rs[len(rs)-1].Balance,
			Spent: round(drops(rs), 2),
		}
	}
	return out
}

// gapList lists each pair of samples further apart than gapAfter.
func gapList(rows []Row) []Gap {
	out := []Gap{}
	for i := 1; i < len(rows); i++ {
		a, b := rows[i-1], rows[i]
		if b.At.Sub(a.At) > gapAfter {
			out = append(out, Gap{
				From:    stamp(a.At),
				To:      stamp(b.At),
				Minutes: int(b.At.Sub(a.At) / time.Minute),
				Spent:   round(math.Max(0, a.Balance-b.Balance), 2),
			})
		}
	}
	return out
}

// alertLines keeps the latest events, right-trimmed, as the tool does.
func alertLines(events []string) []string {
	if len(events) > alertKeep {
		events = events[len(events)-alertKeep:]
	}
	out := []string{}
	for _, e := range events {
		out = append(out, strings.TrimRightFunc(e, unicode.IsSpace))
	}
	return out
}

// stamp is a timestamp to the minute, as the tool writes it.
func stamp(t time.Time) string {
	return t.Format("2006-01-02T15:04")
}

// round is Python's round(x, places): the value is rounded on its exact binary
// form, ties to even, so the report's figures match the tool's.
func round(x float64, places int) float64 {
	v, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', places, 64), 64)
	if err != nil {
		return x
	}
	return v
}

func ptr[T any](v T) *T {
	return &v
}

// File is an open watcher file: the part of it the reader reads. *os.File is one.
type File interface {
	fs.File
	io.Seeker
}

// Opener opens the watcher file at name. OpenFile is the default; a test passes
// one that counts the bytes it reads.
type Opener func(name string) (File, error)

// OpenFile opens name from the operating system.
func OpenFile(name string) (File, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// headBytes is how much of a watcher file's start the reader keeps. Each poll
// reads these bytes again: a file rewritten in place that grew past the old
// offset has a different start, and is read from the top.
const headBytes = 64

// cursor is how far a watcher file has been read: the file it was read from, its
// size and modification time at the last poll, the offset just past its last
// complete line, and the first bytes of the file as they were read.
type cursor struct {
	info   fs.FileInfo
	size   int64
	offset int64
	head   []byte
}

// Reader is the spend report read incrementally across polls. It keeps the
// parsed ledger rows and the latest events, and each poll reads only the bytes
// appended since the last one. A file that shrank, was replaced, or was rewritten
// in place is read from its start. A line still being written waits for its
// newline; a final line with no newline is reported, without being kept, once
// the file has not grown since the poll before, and is kept once its newline
// arrives. The reader is not safe for concurrent use.
type Reader struct {
	ledgerPath, eventsPath string
	loc                    *time.Location
	open                   Opener
	constants              Constants
	days                   int
	ledger, events         cursor
	rows                   []Row
	alerts                 []string
}

// NewReader reads the ledger at ledgerPath and the events log at eventsPath, with
// naive timestamps in loc. days is how many days per_day lists; 0 lists all.
func NewReader(ledgerPath, eventsPath string, loc *time.Location, c Constants, days int, open Opener) *Reader {
	return &Reader{
		ledgerPath: ledgerPath,
		eventsPath: eventsPath,
		loc:        loc,
		open:       open,
		constants:  c,
		days:       days,
	}
}

// Report reads what was appended since the last call and returns the report at
// now. A ledger that cannot be read is an error. A missing events log has no
// alerts; one that exists and cannot be read is an error.
func (r *Reader) Report(now time.Time) (Report, error) {
	text, pending, next, restarted, err := readAppended(r.ledgerPath, r.open, r.ledger)
	if err != nil {
		return Report{}, err
	}
	if restarted {
		r.rows = nil
	}
	r.ledger = next
	r.rows = append(r.rows, ParseLedger(text, r.loc)...)
	rows := r.rows
	if pending != "" {
		// The pending row is shown for this poll only: it is kept once its newline
		// arrives, so it is never counted twice.
		rows = append(rows[:len(rows):len(rows)], ParseLedger(pending, r.loc)...)
	}

	text, pending, next, restarted, err = readAppended(r.eventsPath, r.open, r.events)
	events := r.alerts
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.events, r.alerts, events = cursor{}, nil, nil
	case err != nil:
		return Report{}, err
	default:
		if restarted {
			r.alerts = nil
		}
		r.events = next
		r.alerts = keepLatestAlerts(append(r.alerts, splitLines(text)...))
		events = r.alerts
		if pending != "" {
			events = keepLatestAlerts(append(events[:len(events):len(events)], splitLines(pending)...))
		}
	}
	return Build(Input{Constants: r.constants, Rows: rows, Events: events, Now: now, Days: r.days}), nil
}

// keepLatestAlerts keeps the latest alertKeep events.
func keepLatestAlerts(events []string) []string {
	if over := len(events) - alertKeep; over > 0 {
		return events[over:]
	}
	return events
}

// readAppended reads the complete lines the file at path has past from's offset,
// and returns their text with the cursor just past them. restarted is set when
// the file is not the one from was read from, is shorter than its offset, has
// first bytes that changed, or has an earlier modification time: the read starts
// at the top, and the caller drops what it had kept. The text is empty for a file
// that has no complete line past the offset.
//
// pending is a final line with no newline. It is returned only when the file has
// not grown since from was read: a line still being written is left for the poll
// that finds its newline.
func readAppended(path string, open Opener, from cursor) (text, pending string, next cursor, restarted bool, err error) {
	f, err := open(path)
	if err != nil {
		return "", "", from, false, err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			text, pending, next, restarted, err = "", "", from, false, closeErr
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return "", "", from, false, err
	}
	rewritten := from.info == nil || !os.SameFile(from.info, info) || info.Size() < from.offset || info.ModTime().Before(from.info.ModTime())
	if !rewritten {
		same, err := headMatches(f, from.head)
		if err != nil {
			return "", "", from, false, err
		}
		rewritten = !same
	}
	next = from
	if rewritten {
		next = cursor{}
		restarted = true
	}
	if _, err := f.Seek(next.offset, io.SeekStart); err != nil {
		return "", "", from, false, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", "", from, false, err
	}
	end := bytes.LastIndexByte(data, '\n') + 1
	if end < len(data) && !restarted && info.Size() == from.size {
		pending = string(data[end:])
	}
	head := append(append([]byte(nil), next.head...), data[:end]...)
	if len(head) > headBytes {
		head = head[:headBytes]
	}
	next.info = info
	next.size = info.Size()
	next.offset += int64(end)
	next.head = head
	return string(data[:end]), pending, next, restarted, nil
}

// headMatches reports whether the file's first bytes are head, the bytes a read
// kept. An empty head matches any file.
func headMatches(f File, head []byte) (bool, error) {
	if len(head) == 0 {
		return true, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	buf := make([]byte, len(head))
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}
	return n == len(head) && bytes.Equal(buf, head), nil
}
