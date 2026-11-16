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

// deepSeekRowsReport is the spend report built from the town's rows, plus the
// instant its balance was read. The watch writer stamps spend.json with
// BalanceAt, so a patrol that stopped leaves its last reading with its own age
// downstream rather than passing as this poll's live rate (gt-u3fp8i).
type deepSeekRowsReport struct {
	deepseekspend.Report
	BalanceAt time.Time `json:"balance_at"`
}

// deepSeekRowSeries is the town's deepseek_balance rows folded so far. The store
// is written only by appending a row (internal/beads exposes no sample update or
// delete), so the first poll reads the whole series and each later poll reads
// only the rows written since the newest it holds, folding them onto the series
// it kept. Every poll folds the whole series into its report, because
// spent_ledger, the credits and the gaps are all-time figures.
//
// A read that fails or fills its cap leaves the series as it was, so the next
// poll asks again from the same cursor; a failed poll never partly folds the
// rows it did read.
type deepSeekRowSeries struct {
	read costSamplesRead
	// rows is every usable row read so far, oldest first.
	rows []deepseekspend.Row
	// events is the alert lines those rows carry, oldest first.
	events []string
	// newest is the instant of the newest row read so far, usable or not, and
	// the cursor the next read starts after.
	newest time.Time
	// unusable counts the rows read so far that carried no total.
	unusable int
	// started is whether the first, whole-series read has answered.
	started bool
}

// report folds the series at now. A nil report with no error is a town with no
// usable row; a failed read is an error, never an empty series. The unusable
// count comes back so the caller can name the rows it could not use.
func (s *deepSeekRowSeries) report(now time.Time) (json.RawMessage, int, error) {
	if err := s.poll(); err != nil {
		return nil, s.unusable, err
	}
	if len(s.rows) == 0 {
		return nil, s.unusable, nil
	}
	out, err := json.Marshal(deepSeekRowsReport{
		Report: deepseekspend.Build(deepseekspend.Input{
			Constants: deepseekspend.CurrentConstants(time.Local),
			Rows:      s.rows,
			Events:    s.events,
			Now:       now,
			Days:      deepseekspend.DefaultDays,
		}),
		BalanceAt: s.rows[len(s.rows)-1].At,
	})
	if err != nil {
		return nil, s.unusable, err
	}
	return out, s.unusable, nil
}

// poll reads the rows the series does not hold yet and folds them on. The read
// answers newest first, so it is folded oldest first. On an incremental read the
// since filter may return the cursor row itself, so a row at or before the
// cursor is dropped as already folded; a second row at the cursor's whole-second
// instant is the one row this cannot tell from it. A row written late with an
// instant before the cursor is dropped the same way, so a backfilled row is
// never folded (accepted limitation: the store only appends). A row with no
// total is not a reading and is skipped, never folded as a zero balance. A read
// that fills the cap is refused whole: on the first poll the series' start is
// not known to be in the rows, and on a later one the rows written since the
// cursor are not known to be all there.
func (s *deepSeekRowSeries) poll() error {
	since := time.Time{}
	if s.started {
		since = s.newest
	}
	rows, err := s.read(beads.SampleKindDeepSeekBalance, since, deepSeekRowScanLimit)
	if err != nil {
		return err
	}
	if len(rows) == deepSeekRowScanLimit {
		return fmt.Errorf("the read filled its cap of %d rows, so the series' start is not known to be in the rows", deepSeekRowScanLimit)
	}
	incremental := s.started
	var added []deepseekspend.Row
	var newEvents []string
	unusable := 0
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if incremental && !row.At.After(s.newest) {
			// The cursor row, returned again by a since filter that includes it.
			continue
		}
		if row.Total == nil {
			unusable++
			continue
		}
		added = append(added, deepseekspend.Row{
			At:      row.At,
			Balance: *row.Total,
			Granted: row.Granted,
			Topped:  row.ToppedUp,
		})
		newEvents = append(newEvents, noteLines(row.Note)...)
	}
	s.rows = append(s.rows, added...)
	s.events = append(s.events, newEvents...)
	s.unusable += unusable
	s.started = true
	if len(rows) > 0 && rows[0].At.After(s.newest) {
		s.newest = rows[0].At
	}
	return nil
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
// deepseek_balance rows: every poll folds them before the file source, and a
// town with usable rows is answered from them. file answers only while the town
// has no usable row, when the row read failed, or when it filled its cap; the
// reason goes to the log beside the source that answered, so a pane that fell
// back is not read as a town with no rows. A nil rows read is a hub with no row
// store wired and leaves the file source exactly as it was. The clock is
// injected so a test drives the report's pace windows.
func deepSeekSpendRowsFirst(rows costSamplesRead, file func() json.RawMessage, fileSource string, now func() time.Time, whyLog *dashWhyLog) func() json.RawMessage {
	if rows == nil {
		return file
	}
	series := &deepSeekRowSeries{read: rows}
	return func() json.RawMessage {
		out, unusable, err := series.report(now())
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
