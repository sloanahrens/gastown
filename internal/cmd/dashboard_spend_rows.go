package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/deepseekspend"
)

// The Cost panel's DeepSeek spend, read from the town's deepseek_balance rows
// before any file. The rows are what the balance patrol writes now (gt-a5c8ld),
// so a pod with no Mac ledger still draws the panel; a file source answers only
// while the town has no usable row, or when the row read failed or filled its
// cap (gt-u3fp8i).

// deepSeekRowScanLimit caps one poll's read of the town's deepseek_balance rows.
// The spend report folds the whole series - spent_ledger, the credits list and
// the gaps are all-time figures - so the read takes the whole table rather than
// a window, and this cap is its bound. A read that fills it is refused, never
// folded: the rows that fell off it are the series' start, so a report built
// from them would be short without saying so.
const deepSeekRowScanLimit = 20000

// deepSeekRowSource names the rows in the spend source log line.
const deepSeekRowSource = "the town's deepseek_balance rows"

// The file sources named in the spend source log line beside the reason the rows
// did not answer. The Mac branch that runs the tool before the ledger names both,
// since that choice is made inside the poll.
const (
	spendFileSourceLedger         = "the balance ledger"
	spendFileSourceTool           = "the spend tool"
	spendFileSourceToolThenLedger = "the spend tool, else the balance ledger"
	spendFileSourceNone           = "no source"
)

// dashSpendSource keys the DeepSeek spend source in the log's last map.
const dashSpendSource = "deepseek spend source"

// spendSource logs which source the DeepSeek spend read from, once per change:
// source is the rows or the file source that answered, or no source when neither
// could, and why is the reason a preferred source did not, "" otherwise. A nil
// log records nothing.
func (l *dashWhyLog) spendSource(source, why string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := source
	if why != "" {
		msg = source + " (" + why + ")"
	}
	if l.last[dashSpendSource] == msg {
		return
	}
	l.last[dashSpendSource] = msg
	l.logf("deepseek spend source: %s", msg)
}

// deepSeekRowReport reads the town's deepseek_balance rows and builds the spend
// report they hold. ReadCostSamples answers newest first and deepseekspend.Build
// folds the series oldest first, so the rows are reversed before they go in. A
// row with no total is not a reading and is skipped, never folded as a zero
// balance; its count comes back so the caller can name the rows it could not
// use. A read that fills the scan cap is refused for the same reason the claude
// plan rows are. A nil report with no error is a town with no usable row; a
// failed read is an error, never an empty series.
func deepSeekRowReport(read costSamplesRead, now time.Time) (json.RawMessage, int, error) {
	rows, err := read(beads.SampleKindDeepSeekBalance, time.Time{}, deepSeekRowScanLimit)
	if err != nil {
		return nil, 0, err
	}
	if len(rows) == deepSeekRowScanLimit {
		return nil, 0, fmt.Errorf("the read filled its cap of %d rows, so the series' start is not known to be in the rows", deepSeekRowScanLimit)
	}
	unusable := 0
	ledger := make([]deepseekspend.Row, 0, len(rows))
	var events []string
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if row.Total == nil {
			unusable++
			continue
		}
		ledger = append(ledger, deepseekspend.Row{
			At:      row.At,
			Balance: *row.Total,
			Granted: row.Granted,
			Topped:  row.ToppedUp,
		})
		events = append(events, noteLines(row.Note)...)
	}
	if len(ledger) == 0 {
		return nil, unusable, nil
	}
	report := deepseekspend.Build(deepseekspend.Input{
		Constants: deepseekspend.CurrentConstants(time.Local),
		Rows:      ledger,
		Events:    events,
		Now:       now,
		Days:      deepseekspend.DefaultDays,
	})
	out, err := json.Marshal(report)
	if err != nil {
		return nil, unusable, err
	}
	return out, unusable, nil
}

// noteLines is the alert lines a deepseek_balance row's note carries: the patrol
// writes the reading's alert lines as the note, joined by newlines.
func noteLines(note string) []string {
	if note == "" {
		return nil
	}
	return strings.Split(note, "\n")
}

// deepSeekSpendRowsFirst is the spend source when the town may have
// deepseek_balance rows: every poll reads them before the file source, and a
// town with usable rows is answered from them. file answers only while the town
// has no usable row, when the row read failed, or when it filled its cap; the
// reason goes to the log beside the source that answered, so a pane that fell
// back is not read as a town with no rows. A nil rows read is a hub with no row
// store wired and leaves the file source exactly as it was.
func deepSeekSpendRowsFirst(rows costSamplesRead, file func() json.RawMessage, fileSource string, whyLog *dashWhyLog) func() json.RawMessage {
	if rows == nil {
		return file
	}
	return func() json.RawMessage {
		out, unusable, err := deepSeekRowReport(rows, time.Now())
		switch {
		case err != nil:
			whyLog.spendSource(fileSource, "reading the deepseek_balance rows: "+dashWhy(err))
		case out != nil:
			why := ""
			if unusable > 0 {
				why = fmt.Sprintf("unusable deepseek_balance rows: %d", unusable)
			}
			whyLog.spendSource(deepSeekRowSource, why)
			return out
		case unusable > 0:
			whyLog.spendSource(fileSource, fmt.Sprintf("unusable deepseek_balance rows: %d", unusable))
		default:
			whyLog.spendSource(fileSource, "no deepseek_balance rows")
		}
		if file == nil {
			return nil
		}
		return file()
	}
}
